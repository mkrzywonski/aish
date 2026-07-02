# aish — AI-shareable terminal

A terminal wrapper that lets you and an AI agent (Claude Code, Codex, or any
MCP client) drive **one shared shell session**: both of you type into it,
both of you see everything. When you `ssh` somewhere inside the session, the
AI comes along — no installation on the remote host, ever.

## Why

- **No remote installs**: the AI types into the same PTY you do, so an ssh
  session is transparent — its commands run on whatever host the terminal
  is on.
- **sudo without sharing your password**: the AI runs `sudo ...`, sudo
  prompts in the shared terminal, *you* type the password. Echo is off, so
  the password never enters anything the AI can read (and aish refuses to
  inject commands while a secret prompt is active).
- **Shared visibility**: everything either of you runs is in one scrollback.
  For genuinely invisible work there's an explicit out-of-band `exec` tool.
- **Remote superpowers via ControlMaster**: `ssh` invoked inside the session
  is transparently multiplexed. The AI gets file read/write and background
  command execution on the remote over the already-authenticated connection
  — without touching your interactive shell and without re-auth.

## Build

```sh
nix-shell -p go --run "go build -o aish ./cmd/aish"   # or just: go build -o aish ./cmd/aish
```

## Use

```sh
./aish                       # start a shared session (wraps your $SHELL)
```

Register the MCP server with Claude Code once (any directory):

```sh
claude mcp add aish -- /path/to/aish mcp-proxy
```

Then run `claude` (in another window, or even inside the aish session —
it auto-targets that session via $AISH_SESSION) and ask it to run commands.

Debug/poke without an AI:

```sh
./aish client --list
./aish client run_command '{"command":"uname -a"}'
./aish client read_screen
./aish client --session <id> session_status    # pick among several sessions
```

## MCP tools

| Tool | What it does |
|---|---|
| `run_command` | Run a command in the shared terminal; exact output + exit code (OSC 133 framing locally, invisible sentinel fallback inside remotes) |
| `send_input` / `send_keys` | Raw typing / named keys (ctrl_c, arrows, F-keys) |
| `read_screen` | Rendered screen text (works during vim/htop), cursor, alt-screen flag |
| `read_output` | Incremental scrollback with cursors; escape-stripped |
| `wait_idle` | Wait for output to go quiet |
| `session_status` | mode, host, cwd, foreground process, echo-off, routing |
| `file_read` / `file_write` | Files on the *current* host (local, or remote via multiplexed channel, or size-capped in-band fallback) |
| `file_upload` / `file_download` | Local ↔ remote copies over the multiplexed connection |
| `exec` / `exec_status` | Out-of-band (invisible) commands on the current host; background tasks with incremental polling |

## Visual indicators

Every aish session is visibly marked as shared:

- **Prompt badge**: a magenta `⧉` prefixes your shell prompt (bash/zsh).
- **Window title**: any title set by your shell — or by a remote host over
  ssh — is rewritten to start with `⧉ `, and switches to `⧉⚡ ` while an MCP
  client (an AI) is actually connected, reverting when it disconnects.

## How the ssh integration works

Inside a session, a PATH shim makes `ssh` resolve to aish itself, which
injects `-oControlMaster=auto -oControlPath=<session>/cm-<hash>
-oControlPersist=60s` and execs the real ssh. Your interactive connection
becomes the multiplexing master; file and exec tools open extra channels
over it. If you pass your own `-S`/`-o Control*` options, the shim backs
off entirely. Hosts without a usable channel degrade to in-band operation
through the shared terminal (marked `via: "in_band"` in results).

Shell integration (OSC 133/7) is injected via `--rcfile` (bash) / `ZDOTDIR`
(zsh), sourcing your own rc first. Unsupported shells still work — command
framing just falls back to the sentinel strategy.

Session runtime state lives in `$XDG_RUNTIME_DIR/aish/<session-id>/` and is
removed on exit; ControlPersist reaps orphaned masters within 60s even after
a hard kill.

## Notes / limits (v1)

- Nested ssh (host A → host B): out-of-band tools reach hop 1; deeper hops
  are in-band. (`ProxyJump` channel reuse is the planned fix.)
- Only ssh sessions started *inside* aish are multiplexed; existing
  connections elsewhere can't be adopted.
- bash and zsh get OSC 133 integration; fish falls back to sentinel framing.
