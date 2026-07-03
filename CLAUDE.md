# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`aish` is a terminal wrapper letting a human and an MCP client (Claude Code, Codex) drive one shared shell session on a single PTY. When the user types `ssh host` inside the session, the AI's tools follow along; OpenSSH ControlMaster multiplexing gives out-of-band file/exec access to the remote. See README.md for the user-facing behavior and tool list.

## Build & run

Go is NOT on PATH on this machine (NixOS) — wrap all go commands in nix-shell:

```sh
nix-shell -p go --run "go build -o aish ./cmd/aish"
nix-shell -p go --run "go vet ./..."
nix-shell -p go --run "go test ./internal/term/"   # single package (no tests exist yet)
```

## Testing changes without a real terminal

There are no unit tests yet; verification is done by driving a live session. The Bash tool has no TTY, so start a session under `script` with a FIFO holding stdin open (run in background):

```sh
mkfifo $SCRATCH/stdin.fifo
(sleep 900 > $SCRATCH/stdin.fifo &)
script -qec "./aish" /dev/null < $SCRATCH/stdin.fifo > $SCRATCH/session.log 2>&1
```

Then exercise it from another shell with the one-shot debug client:

```sh
./aish client --list
./aish client run_command '{"command":"false"}'      # expect exit_code 1, framing osc133
./aish client send_input '{"text":"vim\r"}'          # then read_screen shows alt_screen: true
```

Gotchas learned the hard way: piping input into interactive programs races (vim eats queued lines; its termios restore flushes typeahead) — pace input with sleeps or use `send_input` per step. Don't `pkill -f` patterns that appear in your own wrapper command line. When killing a test session, resolve the exact PID from its session id (`grep AISH_SESSION=<id> /proc/<pid>/environ`) — generic matches like "aish with a pts stdin" have killed the user's live session before. Killing the session with signals is the supported path (a handler cleans the runtime dir); stale dirs are swept at next startup.

`ssh localhost` works passwordlessly here and is the M4 test bed for ControlMaster paths.

## Architecture

One `aish` process per session. Everything hangs off a single data path in `cmd/aish/main.go:runMain`:

- `internal/session` owns the PTY (creack/pty): raw-mode stdin pump, SIGWINCH, and `WriteInput` — the one serialized entry point for BOTH human keystrokes and AI injections. Output fans out to registered taps.
- `internal/term` is the sole output tap: scrollback `Ring` (absolute monotonic offsets — all cursor semantics in the tool API derive from this), midterm-based `Screen` (what `read_screen` renders), and `OSCParser` (publishes OSC 133/7/7979 events with ring offsets to subscribers).
- `internal/state.Tracker` consumes parser events → mode (prompt/running/fullscreen), cwd/host from OSC 7, plus on-demand ioctls on the PTY master: TIOCGPGRP for foreground process, TCGETS for echo-off (= password prompt) detection.
- `internal/framing.Engine` implements `run_command`: OSC 133 C→D window when `Tracker.PromptReady()` (integration active), else bare-typed command with idle-quiescence framing (no exit code — user rejected visible wrapper text on remotes). `RunSentinel` (OSC-7979 nonce wrapper, exact + exit code, but its echo is visible) survives ONLY for in-band file/exec fallbacks in `tools_remote.go`; don't reintroduce it into user-visible run_command. The echoed wrapper never triggers the parser because the typed `\033` is literal text until the remote printf executes.
- `internal/shellintegration` injects the OSC-emitting snippets via generated `--rcfile` (bash) / `ZDOTDIR` (zsh) that source the user's rc first, then re-prepend the ssh shim to PATH (user rcs reorder PATH).
- `internal/sshmux` has two halves. Shim side: aish is exec'd AS `ssh` via a symlink (dispatch on `argv[0]` in main.go), uses `ssh -G` to resolve canonical host/user/port and detect user Control* config, writes a ConnInfo event file keyed by pid (pid survives the exec), injects ControlMaster options, execs real ssh. ControlMaster injection happens only when `$AISH_OOB=1` (session started with `--oob`); without it the ConnInfo event is still written (Sock empty) so `route()` keeps host awareness for in-band ops. It backs off entirely for `-O/-G/-V/-Q/-W/-S` or explicit `-o Control*` (ssh's first-obtained-wins would make our prepended options beat the user's). Daemon side (`Mux`): discovers the current remote from event files (liveness = /proc/pid/comm == "ssh"), runs out-of-band commands via `ssh -S <sock>`.
- `internal/mcpserver` serves MCP over the per-session Unix socket (each accepted conn = one MCP session via `mcp.IOTransport`). `tools_remote.go:route()` is the routing brain: controlmaster (live socket AND `Core.OOB`) → in_band (no `--oob`, or foreground is ssh but no socket; base64 through the terminal via the framing engine) → local (only with `--oob`). Every routed result carries `via` + `host`.
- Persistent OOB channel (`sshmux/channel.go`): on the controlmaster route, foreground exec and all file ops go through one long-lived `sh -s` per remote (`Mux.ChannelRun`), opened lazily — results report `via: "channel"`. Rationale: hosts with per-session MFA (login_duo) push once per channel open; one shared channel = one push per host per session (validated in oob.md). Framing is a printf'd nonce sentinel carrying `$?`; writes use a base64 heredoc (`sshmux.WriteScript` — base64's alphabet can't collide with the `@`-delimited marker); exec scripts are wrapped `</dev/null 2>&1` so they can't eat the channel's stdin. Dead/timed-out channels are dropped, never silently reopened — the error tells the caller a retry reopens (and may cost a push). Background exec still gets a dedicated channel (needs a concurrent stream).
- `internal/proxy` (`aish mcp-proxy`) is a dumb stdio↔socket byte pump — no MCP parsing. `Discover` resolves the target session: `--session`/`$AISH_SESSION` accept id, name, or unique id prefix; `$AISH_SOCKET` (set inside sessions) short-circuits; with no target and several live sessions the most recently active wins — safe because attachment is only a default, not a boundary (see cross-session below).
- Cross-session access (`mcpserver/crosssession.go`): every tool's args embed `SessionArg`; receiving middleware intercepts `tools/call`, and when `session` names another live session it forwards the raw call over that session's socket with the argument stripped (so it executes locally there — no loops, and the target's own guards apply). Handlers never read `.Session`.

Sessions have an immutable random id (keys the runtime dir, sockets, env) and an optional mutable name (`aish --name`, or the `set_session_name` tool) stored in `<session-dir>/name`. The prompt badge re-reads the name file each prompt via `$AISH_DIR`; the window-title label updates immediately through `TitleMarker.SetLabel`.

Runtime state lives in `$XDG_RUNTIME_DIR/aish/<session-id>/` (`internal/paths`): MCP socket, `name`, `bin/ssh` shim symlink, `cm-*` ControlMaster sockets, `ssh/*.json` connection events. The dir is removed on exit/signal; ControlPersist=60s reaps orphaned masters after SIGKILL.

## Invariants to preserve

- The user's terminal experience must stay byte-transparent: never write to the PTY or stdout except through `Session.WriteInput`/the output pump.
- Invisible activity is opt-in: without `--oob` (`Core.OOB` false, no `$AISH_OOB`), no code path may act outside the shared terminal — no ControlMaster channels, no direct local fs/exec from tools. Don't add tool paths that bypass `route()`.
- Secrets: echo-off means passwords never enter the Ring; `run_command` must keep refusing while `Tracker.EchoOff()` (locally) — don't weaken this when touching framing.
- Tool results use absolute ring cursors; anything returning output should keep `cursor_start`/`cursor_end`/`next_cursor` consistent with `Ring.ReadFrom` semantics (including `dropped_bytes` on ring wrap).
- The sentinel wrapper assumes a POSIX shell; commands from `exec`/`file_*` build remote command lines with `sshmux.Quote` for paths.
