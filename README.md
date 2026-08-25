# AISH — AI-shared terminal

A terminal wrapper that lets you and an AI agent (Claude Code, Codex, or any
MCP client) drive **one shared shell session**: both of you type into it,
both of you see everything. When you `ssh` somewhere inside the session, the
AI follows the session onto that host. No software needs to be installed on
the remote host.

## What it does

- The AI types into the same PTY you do, so it operates on whatever host the
  terminal is currently on. The AI can see the terminal, no cutting and pasting
  error messages.
- `sudo` prompts stay in the shared terminal. If the AI needs to run a privileged
  command, you see the command and you type the password. No sharing secrets with
  the AI. AISH does not inject commands while secret input is active.
- By default, file and exec operations are visible in the shared terminal.
- Out-of-band (hidden) operations can be enabled with the `--oob` command line argument
  or via the Ctrl-] menu. If oob is enabled, SSH connections opened inside that
  session are multiplexed and remote file/exec operations can use the
  out-of-band channel. This is convenient for code editing.

## Install

AISH is a single **Linux** binary (x86-64 or arm64). Windows and macOS aren't
supported natively — AISH relies on Linux-only PTY/termios constants and `/proc`.
On Windows, run it inside WSL2 (below).

**Runtime prerequisites** (needed to *run* it, not to install it):

- **OpenSSH client** (`ssh`) — for the ControlMaster remote features.
- **bash or zsh** as your shell — for the OSC 133 prompt integration (other
  shells work, with degraded output framing).

You can build from source or use prebuilt binaries.

### Build from source

Requires **git** and **Go ≥ 1.25**.

* Debian/Ubuntu `sudo apt install golang-go` (usually too old — use the tarball below)
* Fedora `sudo dnf install golang`
* Arch `sudo pacman -S go`

Distro Go packages are often older than 1.25; the reliable route is the official
tarball:

```sh
curl -LO https://go.dev/dl/go1.25.5.linux-amd64.tar.gz   # arm64: swap in linux-arm64
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.25.5.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin   # add to ~/.profile to persist
```

Then build and install:

```sh
git clone https://github.com/mkrzywonski/aish.git
cd aish
go install ./cmd/aish     # builds AND installs to ~/go/bin (put ~/go/bin on PATH)
# or: go build -o aish ./cmd/aish   (leaves ./aish in the clone; not on PATH)
aish version
```

### Prebuilt binary

If you'd rather not build it yourself, each release ships a static Linux binary.

```sh
# x86-64 (most machines):
curl -fsSL https://github.com/mkrzywonski/aish/releases/latest/download/aish_linux_amd64.tar.gz | tar -xz aish
# arm64 (Raspberry Pi 4/5, ARM servers): use .../aish_linux_arm64.tar.gz instead

sudo install -m 755 aish /usr/local/bin/aish && rm aish   # or: install -m 755 aish ~/.local/bin/aish (no sudo)
aish version
```

### Install script

`install.sh` wraps the two methods above into one command: it tries the
prebuilt binary first and falls back to building from source (installing Go
if needed) if no prebuilt binary fits the machine.

```sh
curl -fsSL https://raw.githubusercontent.com/mkrzywonski/aish/main/install.sh | bash
# no sudo, installs to ~/.local/bin instead of /usr/local/bin:
curl -fsSL https://raw.githubusercontent.com/mkrzywonski/aish/main/install.sh | bash -s -- --user
```

Run `install.sh --help` (or read the script) for `--source`/`--prebuilt`,
`--prefix`, `--version`, and `--update-rc` options.

### NixOS

This repo is a flake exporting the package and an overlay:

```sh
nix run github:mkrzywonski/aish               # try it without installing
nix profile install github:mkrzywonski/aish   # install into your profile
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

Update a pinned config by bumping the input and rebuilding:

```sh
nix flake update aish --flake /path/to/your/nix-config
sudo nixos-rebuild switch --flake /path/to/your/nix-config#<host>
```

### Windows 11 — via WSL2

AISH needs a Unix PTY and OpenSSH ControlMaster, so run it inside WSL2:

```powershell
wsl --install -d Ubuntu   # once, then reboot / open Ubuntu
```

Inside the Ubuntu shell, build from source or grab the prebuilt `linux_amd64`
binary as above. Your MCP client (e.g. Claude Code) must also run inside WSL to
reach the session's Unix socket.

### aishwin — driving a native Windows shell (experimental)

Separate from AISH's own PTY, `aishwin` lets an AI drive a native Windows
shell (cmd/PowerShell) directly, with everything visible in its own window.
It has two halves: `aishwin.exe` on Windows, and `aishwnd` on the Linux/WSL
side it connects to (by default via `wsl.exe`, or over `ssh` for a separate
remote Linux box).

It appears to the AI as a session with the `direct_host` backend, alongside any
`shared_terminal` sessions, and implements its own tool set — see
[Session backends](#session-backends).

```powershell
iwr https://raw.githubusercontent.com/mkrzywonski/aish/main/install.ps1 | iex
```

Then install `aishwnd` on the Linux side it will connect to (e.g. inside WSL):

```sh
curl -fsSL https://raw.githubusercontent.com/mkrzywonski/aish/main/install.sh | bash -s -- --components aishwnd
```

Building `aishwin.exe` from a checkout **must pass the subsystem flag**:

```powershell
go build -ldflags "-H=windowsgui" -o aishwin.exe ./cmd/aishwin
```

A plain `go build ./cmd/aishwin` links for the console subsystem, which holds
the shell that launched it until the window closes. The subsystem is a
link-time property with no source-level equivalent in Go, so every build
command carries the flag (`make aishwin`, the release build and `install.ps1`
all do). A binary built without it says so at startup.

## Running AISH

### Install the MCP Server in your AI TUI

Register the MCP server with your AI TUI once (this wires up the integration —
it does **not** install the binary, which you did above):

```sh
aish install            # register with every AI TUI found (Claude Code, Codex)
aish install claude     # ... or just one
aish uninstall          # remove it again
```

`install` registers the server as `aish mcp-proxy` at user/global scope,
replacing any previous entry. Then run `claude` or `codex` in another window
and point it at the session.

Equivalent manual command: `claude mcp add aish --scope user -- aish mcp-proxy`.

### Teach your AI to use AISH well

[`SKILL.md`](SKILL.md) in this repository is a guide written for the AI, not for
you: the workflow, the two backends, the visible/invisible choice, and the
handful of traps that reliably waste an agent's time. The MCP server sends every
client a 2 KB summary of the same rules automatically — this is the long form.

Point your assistant at the file and ask it to install it. The last section
tells it how for its own platform (it drops straight into
`~/.claude/skills/aish/SKILL.md` for Claude Code, or `~/.codex/AGENTS.md` for
Codex). Re-copy it after upgrading AISH.

### Launch a shared terminal for collaborating with the AI

```sh
aish                       # start a shared session (wraps your $SHELL)
aish --name myproject      # ... with a meaningful name
aish --oob                 # ... authorizing invisible out-of-band ops
```

You can run multiple sessions and share them with the AI. Every MCP tool accepts
a `session` argument (id or name); `list_sessions` enumerates them with their
backend and tool list. The proxy attaches to one session by default, but that is
only the default target, not a boundary. Use `AISH_SESSION=<id|name>` or
`--session <id|name>` in the proxy args to pick a default explicitly.

The proxy advertises the **union** of the tools across every live session, so a
tool belonging to only one backend is still reachable, and it re-derives that set
as sessions come and go (emitting `notifications/tools/list_changed`). It also
watches for renames: if a session's name changes under a client that has been
using it, the next result carries a notice saying so, rather than letting the AI
silently aim at the wrong target.

```sh
aish sessions              # list live sessions: id, name
```

Debug/poke without an AI:

```sh
aish client --list
aish client run_command '{"command":"uname -a"}'
aish client read_screen
aish client --session <id|name> session_status   # pick among several sessions
```

## MCP tools

| Tool | What it does |
|---|---|
| `run_command` | Run a command in the shared terminal; exact output + exit code with OSC 133 framing (integrated shells), or output-only via idle detection on shells without integration (nothing extra is ever typed) |
| `send_input` / `send_keys` | Raw typing / named keys (ctrl_c, arrows, F-keys) |
| `read_screen` | Rendered screen text (works during vim/htop), cursor, alt-screen flag |
| `read_output` | Incremental scrollback with cursors; escape-stripped |
| `wait_idle` | Wait for output to go quiet |
| `session_status` | mode, host, cwd, foreground process, echo-off, routing, session id/name, other live sessions, the MCP `clients` currently sharing the session, plus explicit remote identity status (`unknown`/`advisory`/`authoritative`), the three host fields (`interactive_host`, `oob_host`, `remote_hostname`), target confidence, cached SFTP status, and per-tool `oob_tools` availability (`unknown` until probed; never opens a channel) |
| `probe_host` | Initialize the OOB shell toolset, or explicitly diagnose identity (`deep=true`) or SFTP (`sftp=true`). Each fresh probe may prompt for OOB consent/MFA and caches its outcome; selectors are independent and `force=true` retries only the selected axis. After a conclusive shell failure, a retained SFTP client can serve file reads and atomic writes |
| `set_session_name` | Label the session after its purpose; shows in prompt badge and title, selectable by name |
| `file_read` / `file_write` | Read or replace files on the *current* host (local, remote OOB, or size-capped visible fallback). `file_read` returns a `version` token and optional line numbers; `file_write` takes an optional `if_match` and writes atomically |
| `file_edit` | Exact-match UTF-8 text replacement on the current host; rejects missing or ambiguous matches; OOB only. Atomic, with automatic staleness protection |
| `file_patch` | Apply a unified diff (multi-hunk) to a text file on the current host; applied in AISH, written atomically; OOB only |
| `file_grep` / `file_search` | Regex content search and name-glob file finding on the current host (ripgrep/grep/find, best-effort); OOB only |
| `file_stat` / `directory_list` | Native-style path metadata and directory browsing on the current host; OOB only |
| `directory_create` | `mkdir -p` on the current host; idempotent, and `created` says whether it already existed |
| `file_upload` / `file_download` | Local ↔ remote copies over the multiplexed connection |
| `exec` / `task_status` | Commands on the current host, with optional `cwd`; OOB background tasks with incremental polling |
| `oob_log` | Read what happened out of band — which client ran which tool, on which host and route, and how it ended. Incremental with cursors; invisible operations by default, `include_visible` for the full call history. Never records file contents |
| `list_sessions` / `version_info` | Served by the proxy, not a session: enumerate live sessions with their backend and actual tool list, and report every component's version |

Every tool also takes an optional `session` (id or name) to target another
live session on the machine.

Two fields are stamped on **every** result that describes an operation, by
middleware rather than by each tool, so a tool added later cannot forget them:

- **`visibility`** — `visible` (it happened in the shared terminal, the human
  saw it), `silent` (out of band, nothing appeared on screen), or `unknown`
  (the route never resolved — AISH claims nothing rather than guessing).
- **`target_confidence`** — whether AISH can confirm the operation landed on the
  machine the human is watching. See
  [Wrong-host protection](#wrong-host-protection).

### Session backends

`list_sessions` reports a **backend** per session, and the two backends do not
implement the same tools. Plan against the `tools` list it returns rather than
assuming the tool schema you loaded applies everywhere.

| Backend | What it is | Notable tools |
|---|---|---|
| `shared_terminal` | An AISH PTY session you and the AI both type into (`aish`) | the full set above, including `run_command`, `send_input`/`send_keys`, `read_screen`/`read_output`, `probe_host`, `oob_log` |
| `direct_host` | A native Windows shell driven through `aishwin` — no shared PTY, its own window | `capture_screen` (screenshot of the window), `read_console` (scrollback), plus `run_command`, the `file_*` suite, `directory_*` and `task_status` |

A tool absent from a session's list is genuinely not there; calling it returns a
capability error naming the backend and what that session does offer, rather
than a bare "unknown tool".

Out-of-band (invisible) operation of `exec`/`file_*` requires an OOB grant
(`--oob`, the Ctrl-] runtime toggle, or an interactive grant). Without one,
`file_read`/`file_write` and foreground `exec` can fall back in-band only when
the remote dialect is authoritatively identified as POSIX. Unknown, advisory,
and non-POSIX identities fail closed rather than receiving POSIX sentinel
framing. The safe fallback is typed visibly through the shared terminal and
size-capped, while `file_edit`,
`file_patch`, `file_grep`/`file_search`, `file_stat`, `directory_list`,
`file_upload`/`file_download`, and background `exec` refuse with guidance. For
remote OOB access, grant it before starting the SSH connection so the shim can
create the ControlMaster. Enabling OOB after SSH is already running does not
retrofit multiplexing onto that existing SSH process; it only affects later SSH
connections. See
[Security](#security).

### Oversized output

Tool results carry at most 16 KiB of command output inline — one bound shared by
every path, so output is never spilled at one threshold and trimmed at another.
Nothing is lost to it. `run_command` types into the shared terminal, so its full
output is in the scrollback and the result's cursors let you page the rest back
with `read_output`.

`exec` has no scrollback behind it, so oversized output is trimmed **from the
middle** (the conclusion of a command is usually its last line) and the complete
text is written to a file on the host that ran the command; the result reports
`truncated`, `output_bytes` and `output_path`. Retrieve it with `file_read`, or
search it with `file_grep` without reading it at all. One such file exists at a
time per session — the previous one is deleted when the next is written — so
collect it before running another command.

### Remote prerequisites

On POSIX targets the OOB file/exec tools run **stock commands over one
persistent `/bin/sh`** — nothing is installed or deployed. When that channel
cannot run because the login shell is non-POSIX, eligible file tools can use a
retained SFTP subsystem instead. AISH reports per-tool availability in
`session_status`
(`oob_tools`); a tool whose prerequisite is missing is disabled and returns a
clear error (with an install suggestion) instead of failing silently. A target
that isn't a POSIX shell is detected in seconds: Windows OpenSSH can expose the
file suite through SFTP, while command tools and targets without working SFTP
refuse with guidance. Use `run_command` to drive unsupported command work
visibly.

On a host it hasn't probed yet, `oob_tools` reads `unknown` for each tool —
`session_status` never opens a channel (so a status check can't trigger an MFA
prompt), so it can't yet know what the host supports. The `probe_host` tool is
the explicit "initialize" step: it opens the channel, runs the probe, and
returns the resolved availability so the AI can plan (and offer to install a
missing package) before acting. Tools also auto-probe on first use, so this is
optional — it just moves the one unavoidable channel-open earlier.

`probe_host` also has two explicit diagnostic selectors that never run
implicitly. `deep=true` identifies login-shell grammar. `sftp=true` opens one
bounded SFTP subsystem, runs `realpath(".")`, records its path style and server
extensions, and retains a successful client. Success and failure are cached
because either outcome may cost an MFA prompt; retry with both `sftp=true` and
`force=true`. After the shell axis fails conclusively, the retained client may
serve reads, downloads, uploads, writes, edits, and patches. Replacement
requires the server's `posix-rename@openssh.com` extension; AISH refuses a
remove-and-rename fallback because it would weaken atomicity. Explicit or
preserved modes are verified, so a server that accepts `chmod` but ignores the
requested mode returns an error rather than false success. `oob_tools` merges
the two axes: file tools report SFTP availability while `exec`, `file_grep`,
and `file_search` remain shell-only.

Commands used (POSIX/coreutils):

- **Core (all content tools):** `sh`, `base64`, `tail`, `head`, `mv`, `chmod`,
  `dirname` — universal on Linux.
- **Per tool:** `stat` (file_stat), `find` (directory_list, file_search),
  `grep` or `ripgrep` (file_grep), `sha256sum`/`shasum` (optional, for
  `if_match` staleness checks).

AISH adapts to the flavor it finds (GNU vs BusyBox vs BSD `stat`/`find`/`grep`,
`base64 -d` vs `-D`, `ripgrep` vs `grep`), so Debian/RHEL/Arch/Raspberry Pi OS
work fully; Alpine/BusyBox, BSD, and macOS work with best-effort fallbacks;
Windows OpenSSH can provide the file suite through SFTP after its non-POSIX
shell is identified. Network devices without a compatible shell or SFTP
subsystem are cleanly refused.

| Platform | OOB file/exec tools |
|---|---|
| Debian/Ubuntu, RHEL family, Arch, Raspberry Pi OS | full |
| Alpine/BusyBox, FreeBSD/OpenBSD, macOS | best-effort (some tools may need a package) |
| Windows OpenSSH | file read/write/stat/list/upload/download/edit/patch through SFTP; command/search tools refused |
| Cisco IOS / network devices | not supported (refused fast); use `run_command` |

## Security

This is mainly a visibility/consent tool, not a sandbox. The MCP endpoint is a
Unix socket under `$XDG_RUNTIME_DIR/aish/<id>/` (mode `0700`), not a TCP port.
Do not expose it over localhost TCP/HTTP/WebSocket. If code is already
running as your uid, AISH does not try to defend against it.

Prompts are shown and answered outside the shell input stream, so the response
does not go through the shell or land in scrollback.

### Client authorization

When an MCP client first tries to use a tool, AISH asks in your terminal:

```
claude wants to control this session — allow? [y/n]
```

- **y** grants that client access until the AISH session closes; reconnects
  must prove possession of the approved private key.
- **n** denies it — sticky, so a client can't re-prompt-spam you; reconnect to
  be asked again.
- **No answer** fails closed (denied).

The prompt names the connecting client from its MCP `clientInfo` — shown as
`claude` or `codex` for the bundled TUIs, or the raw client name otherwise.
Approvals are per client for the life of the AISH session. Reconnects use a
challenge/response check so an already-approved client can reconnect without a
new prompt. Client keys and grants are memory-only (or persisted to
  tmpfs when PSK auth is used — see below).

- `--no-auth`: never prompt for client approval.
- `--auto-approve`: keep the handshake, but auto-answer prompts. Useful for testing.
- `aish client`: treated as a client too, so it also goes through approval unless disabled.

### Pre-shared key (PSK) authentication

When the MCP host (Amazon Quick, an IDE, etc.) restarts the proxy process
between uses, ephemeral client keys are lost and every reconnect triggers a
new approval prompt. PSK auth solves this: the proxy derives a **deterministic**
Ed25519 keypair from a shared secret, so the session recognizes it across
restarts.

```sh
aish generate-psk          # prints a 32-byte random hex key
```

Pass the key to the proxy via the `AISH_PSK` environment variable:

```sh
AISH_PSK=<hex> aish mcp-proxy
```

The first connection to a new session still prompts for approval (you type
`y` once). After that, the session persists the grant — keyed by the PSK-derived
public key — to a file in its tmpfs session directory
(`/run/user/$UID/aish/<session-id>/grants.json`). On the next proxy restart,
the session recognizes the returning key and grants access silently.

Security properties:

- **Volatile storage.** The grants file lives on tmpfs. It is cleaned up when
  the session exits and wiped at logout/reboot.
- **Scoped.** Each session has its own grants; revoking one session does not
  affect others.
- **Revocable.** `Ctrl-]` → `k` (revoke) clears both in-memory and persisted
  grants. The next connection will prompt again.
- **Unknown clients still prompt.** A proxy without the PSK (or with a
  different PSK) generates a random ephemeral key and gets the normal
  interactive prompt.
- **The PSK never touches the Linux filesystem.** It lives in the MCP client's
  configuration (Windows-side for Quick, `~/.claude.json` for Claude Code, etc.).

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
  attempts an out-of-band-capable operation AISH asks:

  ```
  the AI wants out-of-band (invisible) access on <host> — allow? [y/n/a]
  ```

  **y** allows that one operation; **a** grants it for the rest of the session
  (and enables ControlMaster for future `ssh`, so later remote work can
  multiplex); it does not attach OOB to an SSH connection that is already
  running;
  **n** or a timeout does the operation visibly through the shared terminal
  instead. The grant is remembered once you've said **a**.

For hosts with MFA on new SSH channels, `--oob` uses one persistent shell
channel per host. That usually means one MFA prompt per host per session
instead of one per OOB operation. An explicit SFTP probe is a separate retained
channel and may cause another prompt. Lost channels are not reopened silently.

### Privilege escalation stays visible

`exec` **refuses** `sudo`, `su`, `doas`, `pkexec` and `runuser` on any invisible
route. A privileged command has to be one you saw and authorized with your own
password, so escalation has to go through `run_command`, which types into the
shared terminal. The in-band route is exempt for exactly that reason — it is
already visible.

On most hosts the refusal is also the practical answer: the out-of-band channel
has no TTY and its stdin is the null device, so a password prompt would fail
rather than reach anyone.

Like the SSH block, this is a **guardrail, not a boundary**: `bash -c 'sudo …'`
gets through, and anything running as your uid could escalate anyway. It has the
same standing as the tool annotations — it stops an AI from escalating by
accident, not an attacker from escalating on purpose.

### The out-of-band activity log

Consent governs *whether* invisible work may happen. The activity log is the
record of *what* did. This matters because out-of-band operations never touch
the shared terminal — `read_screen` and `read_output` cannot show them, by
definition.

Every tool call is recorded with the client that made it (the approved MCP
client name, plus the kernel-verified peer process), the tool, the route
(`channel`, `sftp`, `local`, `in_band`, `terminal`), the host, the identifying
argument, the outcome, and a monotonic sequence number. Two ways to read it:

- **`Ctrl-]` → `l`** prints the recent invisible operations in your terminal.
- **The `oob_log` tool** lets an AI client poll it incrementally, including
  across sessions.

Refused and failed operations are recorded too — an out-of-band `sudo` that the
escalation guard turned away is exactly the kind of thing worth seeing.

**File contents are never recorded.** Paths, command lines, byte counts and
outcomes only. A log that accumulated what was read and written would be a
secret store nobody asked for.

It also works as a coordination channel: if you have two assistants on one
session, each can check what the other already touched instead of clobbering it.

Two honest limits. The log is **memory-only and bounded** (the most recent few
hundred entries) and dies with the session. And it records **what was asked of
AISH and what came back, not ground truth on the host** — a bug or a path that
bypassed the tool layer would not appear. It is an audit *trail* for
coordination and review, with the same standing as the privilege-escalation
guardrail and the tool annotations. It is not tamper-evident, and it is not a
security boundary.

### Wrong-host protection

When you use one host as a jump box (`ssh a`, then `ssh b` from there), the
interactive shell can be on **b** while the out-of-band channel still points at
**a**. AISH guards against acting on the wrong host.

Three fields describe host identity and **only two of them are comparable**:

- **`interactive_host`** — what the *terminal* reports it is on, from OSC 7.
- **`remote_hostname`** — what the *out-of-band channel's* host reports, from
  the probe.
- **`target_confidence`** — compares those two. It answers the one question
  that matters: do invisible commands land on the machine you are looking at?

**`oob_host` is a different kind of thing** and is deliberately excluded from
the comparison: it is the connection target as *configured* — an alias, an
FQDN, a bare IP, a `ProxyJump` expression — with no obligation to equal any
hostname. Comparing it against `interactive_host` would manufacture
disagreement on exactly the setups where the question is hardest.

The three outcomes:

| `target_confidence` | Meaning | Reads | Writes |
|---|---|---|---|
| `same` | the two hostnames agree | proceed | proceed |
| `mismatch` | they genuinely differ | warned | **fail closed** |
| `unknown` | the remote reports no hostname, so AISH cannot tell | proceed, with a note | one confirmation per host, then a note on every later write |

`unknown` is not the same as `mismatch`, and the distinction is load-bearing. A
lean remote that emits no OSC 7 leaves the terminal reporting the stale *local*
hostname; comparing that against the probed remote would read as a real
disagreement and fail-close every write on the very hosts this is meant to
help. AISH recognises that case and says `unknown` instead.

Every routed result carries `target_confidence`, and while it is not `same`
every result also carries a note explaining what to do about it. The human is
prompted at most once per host so they are not nagged — but that quiet is for
the human, not for the AI: after a single "yes", an operation on an unverified
host must not look identical to one on a verified host.

The fix is one keystroke: **`Ctrl-]` → `p`** installs the AISH prompt on the
remote, `interactive_host` starts tracking, and `target_confidence` becomes
`same`. The note says so, and the write confirmation offers `[p]` inline.

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
  client (an AI) is actually connected, reverting when it disconnects. It also
  gains `[2FA?]` while AISH is opening an SSH session that may trigger MFA,
  including while a full-screen app hides the status bar.
- **Status bar**: a reserved bottom row showing the session badge, the host the
  terminal is on (`tty:`), and where out-of-band ops land (`oob: user@host`),
  with a `⚠` when those diverge — plus a `Ctrl-] menu` hint. It is painted from
  AISH's own state, so a stale `tty:` (e.g. still your old host after an `ssh`
  the remote never reported over OSC 7) is itself the warning. Apps get one
  fewer row; the bar steps aside inside full-screen apps like vim/htop. When a
  new SSH slave session remains pending for 500 ms, this entire row is replaced
  by `2FA MAY BE REQUESTED`, the operation, and `user@host`; verify those details
  before approving. Fast and cached operations never disturb the standard bar.
- **Input-required alert**: when AISH asks an authorization, confirmation, or
  menu question, it emits one terminal bell and replaces the status bar with
  `AISH INPUT REQUIRED` until the prompt is answered, cancelled, or times out.
  The title simultaneously gains `[INPUT?]`, so the state remains visible when
  the bar is disabled or a full-screen app owns the terminal. Terminals may map
  BEL to a sound, visual flash, or nothing; the persistent bar/title signal does
  not depend on bell settings.

### The prompt does double duty — and `ssh`/`su` drops it

The badge is not just decoration; the same prompt hook serves two audiences:

- **You** — at a glance it tells you that *this* terminal is a shared AISH
  session, which one (`<name>`), and which host/user it is on. With several
  terminals open, that badge is how you know where you're about to point the AI.
- **AISH** — the hook also emits an OSC 7 report each prompt, which is how AISH
  tracks the interactive host, keeps the AI aimed at the intended target, and
  can warn or fail closed when the host you're looking at and the host
  out-of-band writes land on have drifted apart (see
  [Wrong-host protection](#wrong-host-protection)).

AISH installs this in the session's *own* shell. But a shell you reach **later**
— after `ssh host`, or after `su - user` on a remote — comes up with that
host/user's plain default prompt: no badge, and no OSC 7, so AISH drops to
`unknown` host confidence and starts asking for a per-write confirmation. Restore
both with **`Ctrl-]` → `p`** (offered on any remote; hidden only on a local
session, where your own shell already has the badge), which types the one-time
badge + OSC 7 snippet into that shell. It is
session-only and per-shell, so you have to remember to do it after each hop —
AISH never auto-injects into a shell it didn't start.

**OSC 7 and OSC 133 are separate, and `p` only restores the first.** OSC 7 is
host reporting, which is what host confidence needs. OSC 133 is the prompt
marking behind `session_status`'s `mode` and `prompt_ready`, and those marks
come from the session's local shell — they cannot see past `ssh`. So for the
whole of a remote session `mode` reads `running` (correctly: `ssh` really is the
running foreground process) however idle the remote prompt is, and
`shell_integration` stays `true` because it describes the local shell.
`session_status` says so in `mode_note` whenever `mode` has stopped tracking the
shell you're talking to, and points at `last_output_ms_ago` or `wait_idle`
instead.

## The AISH menu

Press **`Ctrl-]`** at the shell to open the AISH menu (the keypress is caught
by AISH and doesn't reach the shell). `Esc` cancels the menu at any point. So
does a second **`Ctrl-]`** — which additionally passes one literal `Ctrl-]`
through to the shell, so you can still send the key to a program (e.g. `telnet`)
by pressing it twice.

- **`r` — rename this session.** Type a new name, Enter. The rename shows up in
  the prompt badge on the next prompt and in the window title immediately.
- **`o` — toggle out-of-band ops.** Flips invisible operation on/off for the
  running session. Turning it on is the same grant as `--oob` or answering
  `a` to an out-of-band prompt.
- **`m` — block new SSH sessions.** A stop button for MFA prompts. AISH opens
  one shared channel per host, so a protected host normally costs a single
  push — but a confused AI can keep paying it: a forced re-probe on a host with
  no channel, a deep or SFTP probe, a background command, or reopening a channel
  that timed out each start a new SSH session. Turn this on and AISH opens no
  more of them.

  Everything riding a channel that is *already* open keeps working — file reads
  and writes, grep, search, stat, directory listings and foreground `exec` — so
  you keep the out-of-band tooling you already paid for. Only the operations
  that would need a new session are refused, with an error that tells the AI
  retrying will not help. `run_command` is unaffected. Session-scoped, off by
  default, and nothing is written to disk.

  Like the `sudo` refusal this is a guardrail, not a boundary: it stops AISH
  from opening sessions, not you, and an AI that types `ssh` through
  `run_command` still reaches the host — visibly, in the shared terminal.
- **`c` — connected AI clients.** Lists every live MCP connection: the client
  name and version it reports, the identity it declared when it asked for
  access, the process the kernel verified on the other end of the socket, and
  how long it has been connected. Declared and verified are shown separately
  on purpose — the first is a claim, the second is not.
- **`v` — version info.** This session's AISH build and the binary it is
  running from, plus the version every connected client reports. When a client
  is verifiably an AISH process and its version differs from the session's, the
  view says so: that is the shape of the stale-proxy problem, where a long-lived
  AI client still serves the tool list it loaded before you installed a new
  build. Restarting the AI client clears it.
- **`l` — recent out-of-band activity.** Prints the last invisible operations:
  time, client, tool, route, host, the path or command, and how it ended.
  Everything else in the session you already watched happen; this is the part
  you didn't. See [the out-of-band activity log](#the-out-of-band-activity-log).
- **`k` — revoke client access.** Disconnects every connected client and clears
  all grants for this session, so the next client to act must be approved
  again. (No effect under `--no-auth`.)
- **`p` — set up the AISH prompt on the remote.** Offered whenever you're on a
  remote (SSH'd) — useful because a remote shell often reports no OSC 7, so AISH
  can't otherwise verify its host.
  Types one visible, one-time command that gives the remote shell AISH's badge
  prompt (`<name>⧉ [user@host:cwd]$`) plus OSC 7 host reporting — so the shared
  terminal shows you're on the remote and out-of-band writes stop asking for a
  per-host confirmation. Session-only (no dotfile edits); make it permanent in
  the remote `~/.bashrc` yourself.

## How the ssh integration works

Inside a session started with `--oob`, a PATH shim makes `ssh` resolve to
AISH itself, which injects `-oControlMaster=auto
-oControlPath=<session>/cm-<hash> -oControlPersist=60s` and execs the real
ssh. (Without `--oob` the shim only records which host you're on and execs
ssh untouched — no multiplexing, no extra channels.)

That injection happens when the `ssh` process starts. If you enable OOB only
after an SSH session is already open, that existing SSH process stays
untouched: AISH can still track the host, but remote OOB tools will not have a
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
backs off entirely. Hosts without a usable channel can use in-band operation
through the shared terminal (marked `via: "in_band"` in results), but framed
file/exec fallbacks require an authoritative POSIX dialect. `run_command`
remains available for other targets and types the caller's syntax verbatim.

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
- Only ssh sessions started *inside* AISH are multiplexed; existing
  connections elsewhere can't be adopted.
- bash and zsh get OSC 133 integration; fish and other unsupported shells
  fall back to idle detection, with commands typed bare and no exit code.
- OSC 133 does not survive `ssh`, so `mode` and `prompt_ready` describe the
  local shell for the whole of a remote session. `session_status` flags this in
  `mode_note`; use `last_output_ms_ago` or `wait_idle` instead.
- The out-of-band channel runs as the SSH login user (`oob_user`) and does not
  follow an interactive `su`/`sudo -i` in the shared shell. Check `oob_user`
  before ownership-sensitive work.
- `Ctrl-]` → `p` is per-shell and session-only, so it has to be repeated after
  each hop unless you add the snippet to the remote's rc yourself.
