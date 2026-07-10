# aish — AI-shareable terminal

A terminal wrapper that lets you and an AI agent (Claude Code, Codex, or any
MCP client) drive **one shared shell session**: both of you type into it,
both of you see everything. When you `ssh` somewhere inside the session, the
AI follows the session onto that host. No software needs to be installed on
the remote host.

## What it does

- The AI types into the same PTY you do, so it operates on whatever host the
  terminal is currently on.
- `sudo` prompts stay in the shared terminal. You type the password there;
  aish does not inject commands while secret input is active.
- By default, file and exec operations are visible in the shared terminal.
- If the session starts with `--oob`, SSH connections opened inside that
  session are multiplexed and remote file/exec operations can use the
  out-of-band channel.

## Build

### Prerequisites

- **Go ≥ 1.25** — build-time only. Distro packages are often older than
  this; when in doubt install from [go.dev/dl](https://go.dev/dl/).
- **git** — to clone this repo.
- **OpenSSH client** (`ssh`) — runtime, for the ControlMaster remote
  features.
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

To update a pinned NixOS config, bump the flake input and rebuild:

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

Not supported currently: the build fails on Linux-only termios constants, and
process tracking uses `/proc`.

## Use

```sh
./aish                       # start a shared session (wraps your $SHELL)
./aish --name deploy-web     # ... with a meaningful name
./aish --oob                 # ... authorizing invisible out-of-band ops
```

Register the MCP server with your AI TUI once:

```sh
aish install            # register with every AI TUI found (Claude Code, Codex)
aish install claude     # ... or just one
aish uninstall          # remove it again
```

`install` registers the server as `aish mcp-proxy` at user/global scope,
replacing any previous entry. Then run `claude` or `codex` in another window
and point it at the session.

Equivalent manual command: `claude mcp add aish --scope user -- aish mcp-proxy`.

Every tool accepts a `session` argument (id or name). `session_status` lists
other live sessions. The proxy attaches to one session by default, but that is
only the default target, not a boundary. Use `AISH_SESSION=<id|name>` or
`--session <id|name>` in the proxy args to pick a default explicitly.

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
| `session_status` | mode, host, cwd, foreground process, echo-off, routing, session id/name, other live sessions, plus interactive/OOB host, target confidence, and probed remote capabilities |
| `set_session_name` | Label the session after its purpose; shows in prompt badge and title, selectable by name |
| `file_read` / `file_write` | Read or replace files on the *current* host (local, remote OOB, or size-capped visible fallback). `file_read` returns a `version` token and optional line numbers; `file_write` takes an optional `if_match` and writes atomically |
| `file_edit` | Exact-match UTF-8 text replacement on the current host; rejects missing or ambiguous matches; OOB only. Atomic, with automatic staleness protection |
| `file_patch` | Apply a unified diff (multi-hunk) to a text file on the current host; applied in aish, written atomically; OOB only |
| `file_grep` / `file_search` | Regex content search and name-glob file finding on the current host (ripgrep/grep/find, best-effort); OOB only |
| `file_stat` / `directory_list` | Native-style path metadata and directory browsing on the current host; OOB only |
| `file_upload` / `file_download` | Local ↔ remote copies over the multiplexed connection |
| `exec` / `exec_status` | Commands on the current host, with optional `cwd`; OOB background tasks with incremental polling |

Every tool also takes an optional `session` (id or name) to target another
live session on the machine.

Out-of-band (invisible) operation of `exec`/`file_*` requires an OOB grant
(`--oob`, the Ctrl-] runtime toggle, or an interactive grant). Without one,
`file_read`/`file_write` and foreground `exec` can fall back in-band — typed
visibly through the shared terminal, size-capped — while `file_edit`,
`file_patch`, `file_grep`/`file_search`, `file_stat`, `directory_list`,
`file_upload`/`file_download`, and background `exec` refuse with guidance. For
remote OOB access, grant it before starting the SSH connection so the shim can
create the ControlMaster. Enabling OOB after SSH is already running does not
retrofit multiplexing onto that existing SSH process; it only affects later SSH
connections. See
[Security](#security).

### Remote prerequisites

The OOB file/exec tools run **stock commands on the target over one persistent
`/bin/sh`** — nothing is installed or deployed. When the channel opens, aish
probes the host and reports per-tool availability in `session_status`
(`oob_tools`); a tool whose prerequisite is missing is disabled and returns a
clear error (with an install suggestion) instead of failing silently. A target
that isn't a POSIX shell at all (Windows, a network device, a restricted shell)
is detected in seconds and the tools refuse with guidance — use `run_command`
to drive it visibly instead.

Commands used (POSIX/coreutils):

- **Core (all content tools):** `sh`, `base64`, `tail`, `head`, `mv`, `chmod`,
  `dirname` — universal on Linux.
- **Per tool:** `stat` (file_stat), `find` (directory_list, file_search),
  `grep` or `ripgrep` (file_grep), `sha256sum`/`shasum` (optional, for
  `if_match` staleness checks).

aish adapts to the flavor it finds (GNU vs BusyBox vs BSD `stat`/`find`/`grep`,
`base64 -d` vs `-D`, `ripgrep` vs `grep`), so Debian/RHEL/Arch/Raspberry Pi OS
work fully; Alpine/BusyBox, BSD, and macOS work with best-effort fallbacks;
Windows and network devices are cleanly refused.

| Platform | OOB file/exec tools |
|---|---|
| Debian/Ubuntu, RHEL family, Arch, Raspberry Pi OS | full |
| Alpine/BusyBox, FreeBSD/OpenBSD, macOS | best-effort (some tools may need a package) |
| Windows, Cisco IOS / network devices | not supported (refused fast); use `run_command` |

## Security

This is mainly a visibility/consent tool, not a sandbox. The MCP endpoint is a
Unix socket under `$XDG_RUNTIME_DIR/aish/<id>/` (mode `0700`), not a TCP port.
Do not expose it over localhost TCP/HTTP/WebSocket. If code is already
running as your uid, aish does not try to defend against it.

Prompts are shown and answered outside the shell input stream, so the response
does not go through the shell or land in scrollback.

### Client authorization

When an MCP client first tries to use a tool, aish asks in your terminal:

```
claude wants to control this session — allow? [y/n]
```

- **y** grants that client access until the aish session closes; reconnects
  must prove possession of the approved private key.
- **n** denies it — sticky, so a client can't re-prompt-spam you; reconnect to
  be asked again.
- **No answer** fails closed (denied).

The prompt names the connecting client from its MCP `clientInfo` — shown as
`claude` or `codex` for the bundled TUIs, or the raw client name otherwise.
Approvals are per client for the life of the aish session. Reconnects use a
challenge/response check so an already-approved client can reconnect without a
new prompt. Client keys and grants are memory-only.

- `--no-auth`: never prompt for client approval.
- `--auto-approve`: keep the handshake, but auto-answer prompts. Useful for testing.
- `aish client`: treated as a client too, so it also goes through approval unless disabled.

### Out-of-band operation authorization

By default the AI does not use invisible operations. `file_read`/`file_write`/
foreground `exec` can work by typing through the shared terminal. Native-style
OOB-only operations (`file_edit`, `file_patch`, `file_grep`/`file_search`,
`file_stat`, `directory_list`, upload/download, and background exec) refuse with
guidance.
No ControlMaster multiplexing is set up at all, so no hidden channel to a
remote host even exists.

Out-of-band (invisible) operation is opt-in, two ways:

- **`aish --oob`** authorizes it up front for the whole session.
- **Interactive grant.** In a session *without* `--oob`, the first time the AI
  attempts an out-of-band-capable operation aish asks:

  ```
  the AI wants out-of-band (invisible) access on <host> — allow? [y/n/a]
  ```

  **y** allows that one operation; **a** grants it for the rest of the session
  (and enables ControlMaster for future `ssh`, so later remote work can
  multiplex); it does not attach OOB to an SSH connection that is already
  running;
  **n** or a timeout does the operation visibly through the shared terminal
  instead. The grant is remembered once you've said **a**.

For hosts with MFA on new SSH channels, `--oob` uses one persistent SSH
channel per host. That usually means one MFA prompt per host per session
instead of one per OOB operation. Lost channels are not reopened silently.

### Wrong-host protection

When you use one host as a jump box (`ssh a`, then `ssh b` from there), the
interactive shell can be on **b** while the out-of-band channel still points at
**a**. aish guards against writing to the wrong host: on the first probe it
records the OOB host and compares it to where the shell reports it is
(`session_status` shows `interactive_host`, `oob_host`, and `target_confidence`).
On a **detected mismatch** an OOB *write* (`file_write`/`file_edit`/`file_patch`/
`file_upload`) fails closed and a *read* is flagged with a warning; when the
host **can't be verified** (no shell host reporting) a write asks for a one-time
confirmation per host. This is an initial policy, not a final UX.

Out-of-band writes are also **atomic** (temp file + rename, preserving mode and
refusing to follow a symlink) and support optimistic concurrency: `file_read`
and `file_stat` return a `version` token you can pass back as `if_match` so a
write lands only if the file hasn't changed since — and `file_edit`/`file_patch`
do this automatically.

## Visual indicators

- **Prompt badge**: a magenta `⧉` plus the session's name (or id) prefixes
  your shell prompt (bash/zsh), e.g. `⧉deploy-web`. Renames show up at the
  next prompt.
- **Window title**: any title set by your shell — or by a remote host over
  ssh — is rewritten to start with `⧉<label> `, gaining a `⚡` while an MCP
  client (an AI) is actually connected, reverting when it disconnects.

## The aish menu

Press **`Ctrl-]`** at the shell to open the aish menu (the keypress is caught
by aish and doesn't reach the shell). `Esc` cancels the menu at any point. So
does a second **`Ctrl-]`** — which additionally passes one literal `Ctrl-]`
through to the shell, so you can still send the key to a program (e.g. `telnet`)
by pressing it twice.

- **`r` — rename this session.** Type a new name, Enter. The rename shows up in
  the prompt badge on the next prompt and in the window title immediately.
- **`o` — toggle out-of-band ops.** Flips invisible operation on/off for the
  running session. Turning it on is the same grant as `--oob` or answering
  `a` to an out-of-band prompt.
- **`k` — revoke client access.** Disconnects every connected client and clears
  all grants for this session, so the next client to act must be approved
  again. (No effect under `--no-auth`.)

## How the ssh integration works

Inside a session started with `--oob`, a PATH shim makes `ssh` resolve to
aish itself, which injects `-oControlMaster=auto
-oControlPath=<session>/cm-<hash> -oControlPersist=60s` and execs the real
ssh. (Without `--oob` the shim only records which host you're on and execs
ssh untouched — no multiplexing, no extra channels.)

That injection happens when the `ssh` process starts. If you enable OOB only
after an SSH session is already open, that existing SSH process stays
untouched: aish can still track the host, but remote OOB tools will not have a
ControlMaster route until you reconnect SSH after enabling OOB.

Remote OOB operations share **one persistent channel** per remote: a
long-lived `sh -s` opened lazily over the master on the first OOB op, with
all foreground `exec` and `file_*` traffic streamed through it. The private
channel protocol uses nonce-delimited responses and base64 for binary data;
none of that framing is typed into the shared terminal (results say
`via: "channel"`). On
hosts where each new ssh channel re-triggers MFA (Duo-style per-session
push), this costs exactly one push per host per session instead of one per
operation. A lost channel is never reopened silently: the failed call says
so, and your retry is the consent for the reopen. Background `exec` tasks need
a concurrent stream and use a dedicated channel each. Your interactive
connection becomes the multiplexing master; file and exec tools open extra
channels over it. If you pass your own `-S`/`-o Control*` options, the shim
backs off entirely. Hosts without a usable channel degrade to in-band
operation through the shared terminal (marked `via: "in_band"` in results).

Shell integration (OSC 133/7) is injected via `--rcfile` (bash) / `ZDOTDIR`
(zsh), sourcing your own rc first. Shells without integration (plain
remotes, fish) still work: `run_command` types the command bare and infers
completion from output quiescence. There is no exact exit code on that path.

Session runtime state lives in `$XDG_RUNTIME_DIR/aish/<session-id>/` and is
removed on exit; ControlPersist reaps orphaned masters within 60s even after
a hard kill.

## Notes / limits (v1)

- Nested ssh (host A → host B): out-of-band tools reach hop 1; deeper hops
  are in-band. (`ProxyJump` channel reuse is the planned fix.)
- Only ssh sessions started *inside* aish are multiplexed; existing
  connections elsewhere can't be adopted.
- bash and zsh get OSC 133 integration; fish and other unsupported shells
  fall back to idle detection, with commands typed bare and no exit code.
