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

Unit tests cover the connection-auth handshake (`internal/mcpserver/connauth_test.go`) and proxy resolution (`internal/proxy/proxy_test.go`); run them with `nix-shell -p go --run "go test ./..."`. Everything else is verified by driving a live session. The Bash tool has no TTY, so start a session under `script` with a FIFO holding stdin open (run in background):

```sh
mkfifo $SCRATCH/stdin.fifo
(sleep 900 > $SCRATCH/stdin.fifo &)
script -qec "./aish --auto-approve" /dev/null < $SCRATCH/stdin.fifo > $SCRATCH/session.log 2>&1
```

Use `--auto-approve` for test sessions: the one-shot debug client now runs the full Ed25519 approval handshake (it no longer bypasses consent with a token), so without it every `./aish client …` call blocks on a y/n prompt on the session's terminal — which for a FIFO-driven session means `printf y > $SCRATCH/stdin.fifo` before each command. `--auto-approve` auto-answers that prompt while still exercising the real handshake (request_access → auth_challenge → authenticate), so it's the faithful test path; `--no-auth` (which disables the gate entirely) also works if you don't care about the auth path. Then exercise it from another shell:

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
- `internal/framing.Engine` implements `run_command`: OSC 133 C→D window when `Tracker.PromptReady()` (integration active), else a bare-typed command with idle-quiescence framing (no exit code). `run_command` must never use wrapper/sentinel framing because its echoed command makes the human terminal experience noisy. `RunSentinel` (OSC 7979) remains in the current implementation for visible in-band `file_read`, `file_write`, and `exec` fallbacks; treat that as legacy behavior and don't extend it. Exact hidden operations belong on the OOB channel.
- `internal/shellintegration` injects the OSC-emitting snippets via generated `--rcfile` (bash) / `ZDOTDIR` (zsh) that source the user's rc first, then re-prepend the ssh shim to PATH (user rcs reorder PATH).
- `internal/sshmux` has two halves. Shim side: aish is exec'd AS `ssh` via a symlink (dispatch on `argv[0]` in main.go), uses `ssh -G` to resolve canonical host/user/port and detect user Control* config, writes a ConnInfo event file keyed by pid (pid survives the exec), injects ControlMaster options, execs real ssh. ControlMaster injection happens only when `$AISH_OOB=1` (session started with `--oob`); without it the ConnInfo event is still written (Sock empty) so `route()` keeps host awareness for in-band ops. It backs off entirely for `-O/-G/-V/-Q/-W/-S` or explicit `-o Control*` (ssh's first-obtained-wins would make our prepended options beat the user's). Daemon side (`Mux`): discovers the current remote from event files (liveness = /proc/pid/comm == "ssh"), runs out-of-band commands via `ssh -S <sock>`.
- `internal/mcpserver` serves MCP over the per-session Unix socket (each accepted conn = one MCP session via `mcp.IOTransport`). `tools_remote.go:route()` is the routing brain: controlmaster (live socket AND an OOB grant) → in_band (no grant, or foreground is ssh but no socket) → local (with a grant). Every routed result carries `via` + `host`. `file_read`/`file_write`/foreground `exec` retain a visible in-band fallback; the native-style `file_edit`, `file_stat`, and `directory_list` primitives require OOB and never add new visible wrappers. `exec.cwd` is absolute and applies to local, remote foreground, and background execution.
- Persistent OOB channel (`sshmux/channel.go`): on the controlmaster route, foreground exec and all file ops go through one long-lived `sh -s` per remote (`Mux.ChannelRun`), opened lazily — results report `via: "channel"`. Rationale: hosts with per-session MFA (login_duo) push once per channel open; one shared channel = one push per host per session (validated in oob.md). Its private, invisible protocol uses a printf'd nonce marker carrying `$?`; writes use a base64 heredoc (`sshmux.WriteScript` — base64's alphabet can't collide with the `@`-delimited marker); exec scripts are wrapped `</dev/null 2>&1` so they can't eat the channel's stdin. This protocol never appears in the shared terminal. Dead/timed-out channels are dropped, never silently reopened — the error tells the caller a retry reopens (and may cost a push). Background exec still gets a dedicated channel (needs a concurrent stream).
- `internal/proxy` (`aish mcp-proxy`, `aggregate.go`) is a durable, session-aware MCP SERVER — its lifetime is the AI TUI's, independent of any session. It presents one endpoint over stdio, mirrors the session tool set (fetched from a live session's `ListTools`, cached to `~/.cache/aish/tools.json` so it works with zero sessions; private authentication tools filtered out) plus a local `list_sessions`, and routes each `tools/call` to the session named in its `session` arg. It holds one connection per session in a pool and one memory-only client identity; first access requests an interactive grant, while reconnects prove possession of that grant without prompting again. `resolve` errors on ambiguity (several sessions, no `session` arg) — never guesses. `annotate`/`renameNotices` diff live session names against the last-seen set on every routed call and prepend a rename notice to the result (plus a `Log` notification), so the AI is told when a name it's been using moved — avoiding wrong-target confusion. `Discover`/`List`/`Resolve` in `proxy.go` do session enumeration.
- Cross-session access (`mcpserver/crosssession.go`): every tool's args embed `SessionArg`; receiving middleware intercepts `tools/call`, and when `session` names another live session it forwards the raw call over that session's socket with the argument stripped (so it executes locally there — no loops, and the target's own guards apply). Handlers never read `.Session`.
- Console (`session/console.go`): the sanctioned SECOND exception to byte-transparency (window-title marking was the first). `Notify`/`Prompt`/`PromptLine` write to the real terminal (os.Stdout) and, for a prompt, capture the user's keystroke(s) off the stdin pump before they reach the shell — so aish can ask the user for consent (or a session name) without a byte entering the session stream. `outMu` gates the output pump while a prompt owns the screen (shell output waits, flushes after, never interleaves); `promptMu` serializes prompts; `capturing`/`capCh` divert stdin; Esc cancels. The aish menu (Ctrl-], `Session.menuKey`/`SetMenu`) is opened from the input pump — the key is swallowed, `onMenu` runs on its own goroutine (so it can drive the capture). Pressing the menu key a second time while a prompt is up cancels it (like Esc) and passes one literal menu key through to the shell (the pump injects Esc to cancel, then `WriteInput`s the key). It offers (main.go): `[r]` rename (`paths.WriteName` + `TitleMarker.SetLabel`), `[o]` toggle out-of-band ops (`Core.SetOOB`/`OOBEnabled`, flipping the in-process grant and the persisted `oob` marker), and `[k]` revoke client access (`Core.Revoke`).
- Connection authorization (`mcpserver/connauth.go`): a new MCP connection can't call session tools until it requests access and the user approves it with a y/n prompt in the terminal. Approval creates a session-lifetime grant bound to the client's ephemeral Ed25519 public key and MCP client name. Reconnects use a 30-second, single-use nonce challenge and proof-of-possession signature; grants and keys are memory-only, and multiple clients have independent grants. Cross-session forwarding and the debug CLI use the same protocol and no longer bypass consent. `--no-auth` (`Core.NoAuth`) disables the gate entirely; `--auto-approve` (`Core.AutoApprove`) keeps the handshake but auto-answers the prompt with a `Notify` (for one-shot testing without an on-disk secret). `connAuthMiddleware` is registered outermost (before cross-session); initialization, tool listing, and the three private auth tools remain reachable while gated. `Core.Revoke` (Ctrl-] menu) clears all grants/challenges and disconnects live clients so the next access re-prompts — disconnect is required because a pooled client reuses its authorized connection and would never otherwise re-run the handshake.
- OOB authorization is now interactive when `--oob` was NOT passed: `route()` (the only prompting entry point — `session_status` uses non-prompting `capability()`) asks the user y/n/a on the first OOB-capable op. yes = this op, always = session grant (writes the OOB file so the shim honors it for future ssh), no/timeout = downgrade to visible in-band. `--oob` pre-writes the grant so nothing prompts.

Sessions have an immutable random id (keys the runtime dir, sockets, env) and an optional mutable name (`aish --name`, or the `set_session_name` tool) stored in `<session-dir>/name`. The prompt badge re-reads the name file each prompt via `$AISH_DIR`; the window-title label updates immediately through `TitleMarker.SetLabel`.

Runtime state lives in `$XDG_RUNTIME_DIR/aish/<session-id>/` (`internal/paths`): MCP socket, `name`, `oob` (out-of-band grant marker, read by the shim), `bin/ssh` shim symlink, `cm-*` ControlMaster sockets, and `ssh/*.json` connection events. Client authorization grants exist only in process memory. The dir is removed on exit/signal; ControlPersist=60s reaps orphaned masters after SIGKILL.

## Invariants to preserve

- The user's terminal experience must stay byte-transparent: never write to the PTY or stdout except through `Session.WriteInput`/the output pump.
- Invisible activity is opt-in: without an OOB grant (`--oob` or a runtime 'always'), no code path may act outside the shared terminal — no ControlMaster channels, no direct local fs/exec from tools. Don't add tool paths that bypass `route()` (the prompting policy gate).
- Console output/input is the ONLY sanctioned way for aish to talk to the user outside the session stream; never write consent UI through `Session.WriteInput`. Prompts must fail closed (deny) on timeout.
- Secrets: echo-off means passwords never enter the Ring; `run_command` must keep refusing while `Tracker.EchoOff()` (locally) — don't weaken this when touching framing.
- Tool results use absolute ring cursors; anything returning output should keep `cursor_start`/`cursor_end`/`next_cursor` consistent with `Ring.ReadFrom` semantics (including `dropped_bytes` on ring wrap).
- MCP server instructions are the primary cross-client discovery mechanism: they must keep telling models that native client tools stay local and aish tools target the shared session's current host. Tool annotations are hints, not security boundaries.
- The OOB channel protocol assumes a POSIX shell; commands from `exec`/`file_*` build remote command lines with `sshmux.Quote` for paths.
