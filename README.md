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
- **Shared visibility, by default total**: everything either of you runs is
  in one scrollback. Out of the box the AI *cannot* act invisibly — file and
  exec tools work by typing through the shared terminal where you see them.
- **Opt-in remote superpowers via ControlMaster** (`aish --oob`): `ssh`
  invoked inside the session is transparently multiplexed. The AI gets
  invisible file read/write and background command execution on the remote
  over the already-authenticated connection — without touching your
  interactive shell and without re-auth. Starting with `--oob` is your
  explicit authorization for that behind-the-scenes activity (it also
  avoids surprise MFA prompts on hosts where extra ssh channels re-trigger
  Duo-style push authentication).

## Build

### Prerequisites

- **Go ≥ 1.25** — build-time only. Distro packages are often older than
  this; when in doubt install from [go.dev/dl](https://go.dev/dl/).
- **git** — to clone this repo.
- **OpenSSH client** (`ssh`) — runtime, for the ControlMaster remote
  features. Already present on virtually every Linux.
- **bash or zsh** as your shell for OSC 133 integration (other shells work
  with degraded output framing).

### Debian / Ubuntu

```sh
sudo apt install git openssh-client

# Go from go.dev (apt's golang-go is usually too old):
curl -LO https://go.dev/dl/go1.25.5.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.25.5.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin   # add to ~/.profile to persist

git clone https://github.com/mkrzywonski/aish.git
cd aish
go build -o aish ./cmd/aish
```

### Fedora / Arch

Both ship a current Go:

```sh
sudo dnf install golang git openssh-clients   # Fedora
sudo pacman -S go git openssh                 # Arch

git clone https://github.com/mkrzywonski/aish.git
cd aish
go build -o aish ./cmd/aish
```

### NixOS

This repo is a flake exporting the package and an overlay:

```sh
nix run github:mkrzywonski/aish          # try it without installing
nix build github:mkrzywonski/aish       # or build ./result/bin/aish
nix develop                              # dev shell with Go (in a clone)
```

To install system-wide, consume it as a flake input in your NixOS config:

```nix
inputs.aish = {
  url = "github:mkrzywonski/aish";
  inputs.nixpkgs.follows = "nixpkgs";
};
# then add aish.overlays.default to nixpkgs.overlays
# and pkgs.aish to environment.systemPackages
```

**Updating to the latest version** (the flake.lock in your NixOS config pins
a commit; bump it and rebuild):

```sh
nix flake update aish --flake /path/to/your/nix-config
sudo nixos-rebuild switch --flake /path/to/your/nix-config#<host>
aish version   # prints the git revision it was built from
```

### Windows 11 — via WSL2

aish requires a Unix PTY and OpenSSH ControlMaster multiplexing, neither of
which exists natively on Windows, so run it inside WSL2:

```powershell
wsl --install -d Ubuntu   # once, then reboot / open Ubuntu
```

Inside the Ubuntu shell, follow the Debian/Ubuntu steps above. Windows
Terminal gives you tabs/titles, and `ssh` from WSL reaches the same hosts.
Your MCP client (e.g. Claude Code) must also run inside WSL to reach the
session's Unix socket.

### macOS

Not yet supported: the build fails on Linux-only termios constants, and
process tracking uses `/proc`. Port contributions welcome.

## Use

```sh
./aish                       # start a shared session (wraps your $SHELL)
./aish --name deploy-web     # ... with a meaningful name
./aish --oob                 # ... authorizing invisible out-of-band ops
```

Register the MCP server with Claude Code once (any directory):

```sh
claude mcp add aish -- /path/to/aish mcp-proxy
```

Then run `claude` in another window and ask it to run commands.

**The AI can reach every live session**: every tool accepts a `session`
argument (id or name) to run that call in another session; `session_status`
lists the others. The proxy attaches to one session by default — the most
recently active, or pick explicitly with `AISH_SESSION=<id|name> claude` or
`--session <id|name>` in the proxy args — but attachment is just the
default target, not a boundary. Each session has an immutable short id and
an optional mutable name (`--name` at start, or the AI names it via
`set_session_name`); both are shown in the prompt badge and accepted
anywhere a session is selected.

```sh
./aish sessions              # list live sessions: id, name
```

Debug/poke without an AI:

```sh
./aish client --list
./aish client run_command '{"command":"uname -a"}'
./aish client read_screen
./aish client --session <id|name> session_status   # pick among several sessions
```

## MCP tools

| Tool | What it does |
|---|---|
| `run_command` | Run a command in the shared terminal; exact output + exit code with OSC 133 framing (integrated shells), or output-only via idle detection on shells without integration (nothing extra is ever typed) |
| `send_input` / `send_keys` | Raw typing / named keys (ctrl_c, arrows, F-keys) |
| `read_screen` | Rendered screen text (works during vim/htop), cursor, alt-screen flag |
| `read_output` | Incremental scrollback with cursors; escape-stripped |
| `wait_idle` | Wait for output to go quiet |
| `session_status` | mode, host, cwd, foreground process, echo-off, routing, session id/name, other live sessions |
| `set_session_name` | Label the session after its purpose; shows in prompt badge and title, selectable by name |
| `file_read` / `file_write` | Files on the *current* host (local, or remote via multiplexed channel, or size-capped in-band fallback) |
| `file_upload` / `file_download` | Local ↔ remote copies over the multiplexed connection |
| `exec` / `exec_status` | Out-of-band (invisible) commands on the current host; background tasks with incremental polling |
| `authorize` | Internal token bypass for same-uid helpers; AI clients are approved via the terminal y/n prompt instead |

Every tool also takes an optional `session` (id or name) to run the call in
another live session on the machine; forwarded calls are executed by the
target session's own server, so its safety guards apply unchanged.

Out-of-band (invisible) operation of `exec`/`file_*` requires the session to
have been started with `--oob`; otherwise those tools run in-band — typed
visibly through the shared terminal, size-capped — and `file_upload`/
`file_download`/background `exec` refuse with guidance. See
[Security](#security).

## Security

### Threat model

aish's job is to keep the AI's activity **visible and consented** and to keep
untrusted *network* actors out — not to sandbox code that is already running
as you.

- **In scope.** Untrusted code with no local foothold — above all a malicious
  web page running JavaScript/WASM in your browser, the one place untrusted
  code executes on your machine routinely. Also: accidental or unintended
  clients (a misconfigured MCP client, attaching to the wrong session), and
  keeping every AI action either visible in the shared terminal or explicitly
  authorized.
- **Out of scope.** Any code already executing under your uid. If an attacker
  can run commands as you, they can read your files, `ptrace` your shell, and
  scrape your terminal directly — your shared tty is the least of your
  worries, and no in-process control could stop them. aish does not pretend
  to defend against this.

**The load-bearing decision is the transport.** aish's MCP endpoint is a
**Unix-domain socket** under `$XDG_RUNTIME_DIR/aish/<id>/` (mode `0700`), not
a TCP port. That single choice is what excludes the browser: JavaScript has
no API that can open a Unix socket — `fetch`, `WebSocket`, `WebRTC`, and
`WebTransport` all speak TCP/UDP to host:port and nothing else. A page cannot
reach the socket, full stop. The `0700` directory additionally blocks other
local users, so the only party that can even connect is your own uid.

> **Corollary — never bind a TCP port.** If aish ever exposed MCP over
> localhost TCP/HTTP/WebSocket, a malicious page could reach it via DNS
> rebinding / CORS attacks — and because MCP calls have side effects (running
> commands), even *write-only* access with no readable response would be
> catastrophic. Remote access, if ever wanted, must be an SSH-forwarded Unix
> socket, not a bound port. Likewise, nothing a browser can talk to (a
> localhost HTTP relay, a browser extension with native messaging) should be
> able to talk to the aish socket.

Everything below is layered *on top of* that transport boundary. Because the
only connecting party is already same-uid, the connection and OOB prompts are
**consent and awareness controls** — they ensure you know about and approved
what the AI does — rather than hard boundaries against a hostile local
process.

### How aish asks you

aish talks to you directly through the terminal — writing to your screen and
reading your keypress **out of band from the shell**, so nothing it asks ever
lands at your prompt or in the scrollback, and the shell never sees the
keystroke that answers. (This is the one sanctioned exception to aish's
byte-transparency, alongside the window-title marker.)

### MCP connection authorization

When an MCP client first tries to use a tool, aish asks in your terminal:

```
🔒 aish an AI client (claude-code) wants to control this session — allow? [y/n]
```

- **y** approves that client for the life of the connection.
- **n** denies it — sticky, so a client can't re-prompt-spam you; reconnect to
  be asked again.
- **No answer** fails closed (denied).

The prompt names the connecting client (from its MCP `clientInfo`) so you know
what's asking. This guarantees you're aware of every client driving the
session and prevents accidental attachments; it is *not* a barrier against
same-uid code (which is out of scope, above).

- **`--no-auth`** starts a session that never prompts — zero friction when you
  don't want to approve each connection.
- **Internal helpers** — cross-session forwarding and the debug CLI (`aish
  client`) — authenticate with a per-session token file (`token`, `0600`, in
  the session dir) and are never prompted. The token is same-uid convenience,
  not a security control: any process that could read it could already do
  worse.

### Out-of-band operation authorization

By default the AI **cannot act invisibly**. `file_read`/`file_write`/`exec`
work by typing through the shared terminal, visibly, where you watch them
happen; `file_upload`/`file_download` and background `exec` (which have no
visible form) simply refuse. No ControlMaster multiplexing is set up at all,
so no hidden channel to a remote host even exists.

Out-of-band (invisible) operation is opt-in, two ways:

- **`aish --oob`** authorizes it up front for the whole session — you've
  decided this session may act behind the scenes, so nothing prompts.
- **Interactive grant.** In a session *without* `--oob`, the first time the AI
  attempts an out-of-band-capable operation aish asks:

  ```
  🔒 aish the AI wants out-of-band (invisible) access on <host> — allow? [y/n/a]
  ```

  **y** allows that one operation; **a** grants it for the rest of the session
  (and enables ControlMaster for future `ssh`, so remote work multiplexes);
  **n** or a timeout does the operation *visibly* through the shared terminal
  instead. The grant is remembered so you're not re-asked once you've said
  **a**.

Beyond visibility, `--oob` also has an MFA benefit: aish routes all remote OOB
work through **one persistent ssh channel** per host (see
[How the ssh integration works](#how-the-ssh-integration-works)). On hosts
where each new ssh session re-triggers a Duo-style push, that means a single
push per host per session instead of one per operation — and aish never
silently reopens a dropped channel (which would cost another push); it tells
you, and your retry is the consent.

### What's protected, concretely

- **Your password** never reaches the AI: `sudo`/ssh password prompts turn
  terminal echo off, aish detects that and refuses to inject `run_command`
  while it's active, and echo-off input never enters the scrollback the AI can
  read.
- **The AI's visibility is the default.** Invisible file/exec activity
  requires `--oob` or an explicit y/n/a grant; without it, everything is in
  the one shared scrollback.
- **You approve every client** (unless `--no-auth`), and the approval is
  per-connection.

## Visual indicators

Every aish session is visibly marked as shared:

- **Prompt badge**: a magenta `⧉` plus the session's name (or id) prefixes
  your shell prompt (bash/zsh), e.g. `⧉deploy-web`. Renames show up at the
  next prompt.
- **Window title**: any title set by your shell — or by a remote host over
  ssh — is rewritten to start with `⧉<label> `, gaining a `⚡` while an MCP
  client (an AI) is actually connected, reverting when it disconnects.

## How the ssh integration works

Inside a session started with `--oob`, a PATH shim makes `ssh` resolve to
aish itself, which injects `-oControlMaster=auto
-oControlPath=<session>/cm-<hash> -oControlPersist=60s` and execs the real
ssh. (Without `--oob` the shim only records which host you're on and execs
ssh untouched — no multiplexing, no extra channels.)

Remote OOB operations share **one persistent channel** per remote: a
long-lived `sh -s` opened lazily over the master on the first OOB op, with
all foreground `exec` and `file_*` traffic streamed through it
(sentinel-framed, base64 for binary; results say `via: "channel"`). On
hosts where each new ssh channel re-triggers MFA (Duo-style per-session
push), this costs exactly one push per host per session instead of one per
operation. A lost channel is never reopened silently: the failed call says
so, and your retry is the consent for the (possibly push-triggering)
reopen. Background `exec` tasks need a concurrent stream and use a
dedicated channel each. Your interactive connection
becomes the multiplexing master; file and exec tools open extra channels
over it. If you pass your own `-S`/`-o Control*` options, the shim backs
off entirely. Hosts without a usable channel degrade to in-band operation
through the shared terminal (marked `via: "in_band"` in results).

Shell integration (OSC 133/7) is injected via `--rcfile` (bash) / `ZDOTDIR`
(zsh), sourcing your own rc first. Shells without integration (plain
remotes, fish) still work: `run_command` types the command bare and infers
completion from output quiescence — no exit code on that path, and add the
aish snippet to the remote's shell rc if you want exact framing there.

Session runtime state lives in `$XDG_RUNTIME_DIR/aish/<session-id>/` and is
removed on exit; ControlPersist reaps orphaned masters within 60s even after
a hard kill.

## Notes / limits (v1)

- Nested ssh (host A → host B): out-of-band tools reach hop 1; deeper hops
  are in-band. (`ProxyJump` channel reuse is the planned fix.)
- Only ssh sessions started *inside* aish are multiplexed; existing
  connections elsewhere can't be adopted.
- bash and zsh get OSC 133 integration; fish falls back to sentinel framing.
