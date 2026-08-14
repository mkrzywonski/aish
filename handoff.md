# Handoff - Windows targets after workstream D

Current coordination record for assistants continuing work in
`/home/mike/aish`. The design history and acceptance criteria are in
`windows-targets-plan.md`; this file states the current merge boundary,
verified behavior, constraints, and exact next work.

---

## Current state

Workstreams A, B, C, and D are complete. C was fast-forwarded into `main`
through the C closeout boundary. D is implemented on `d-oob-activity-log`, cut
from clean, pushed main, and is awaiting live acceptance and merge.

| Workstream | State |
|---|---|
| **A - linearize the ring** | complete; live-validated on Linux and ConPTY |
| **B - know the target** | complete; durable facts, passive/active identity, unknown-target safety, and MFA provenance are live-accepted |
| **C - SFTP probe and file transport** | complete; shell-first reads and atomic writes are automated-tested and live-accepted |
| **D - out-of-band activity log** | implemented on `d-oob-activity-log`; automated-tested and locally exercised, pending live acceptance and merge |

The C write implementation is in:

```text
70c0cc4  sshmux: preserve atomic writes over SFTP
becd5c0  sshmux: verify SFTP write modes
0f7b958  mcpserver: advertise SFTP file capabilities
5537411  sshmux: verify appended SFTP file modes
```

The earlier C checkpoints are merged on main through:

```text
4d72bc5  SFTP durable axis, explicit probe, retained client, MFA warning
2ea5409  read-only SFTP routing live-acceptance
e755008  handoff for the write checkpoint
```

The complete C code/documentation boundary `c4f1bcb` was built and installed
as `v0.2.2-39-gc4f1bcb`. Its build artifact and
`/usr/local/bin/aish` had identical SHA-256:

```text
34de802a56e0094ed3dfdaf1643e4efe399458e6d1a71eba87b1af0f0eba3044
```

The append mode verification in `5537411` was found during final review and is
included in that installed binary. A deployment-note-only commit after
`c4f1bcb` does not change runtime source; rebuild only when subsequent code
changes land.

---

## Workstream C result

### Routing policy

- Shell-first remains the default. A working POSIX channel handles file and
  command tools and does not open SFTP.
- SFTP is selected only after `ShellAxis` is conclusively/sticky down.
- An unknown SFTP axis may be opened once by the first eligible file operation.
  The open can trigger consent or MFA and is covered by the debounced
  `2FA MAY BE REQUESTED` status/title warning.
- SFTP success and failure are sticky. A dead retained client is retired and
  never reopened implicitly. Retry requires
  `probe_host{sftp:true,force:true}`, with explicit MFA guidance.
- SFTP path/platform evidence never establishes a command dialect and never
  enables POSIX framing.

### File contract

- `file_read`, `file_stat`, `directory_list`, and `file_download` use
  retained SFTP after conclusive shell failure.
- `file_write`, `file_upload`, `file_edit`, and `file_patch` use atomic
  retained-SFTP replacement when the server advertises
  `posix-rename@openssh.com`.
- `file_write{append:true}` uses the existing explicitly non-atomic append
  contract.
- `exec`, `file_grep`, and `file_search` remain shell-only. SFTP is file
  I/O, not remote computation.
- Tool-facing paths remain target-native. Windows drive paths and the observed
  slash-drive server form normalize at one mux boundary; relative, UNC,
  root-escaping, and cross-style ambiguous paths fail before host I/O.
- MCP never receives `pkg/sftp.Client`; `sshmux` exposes a narrow serialized,
  cancellation-aware retained-client API.

### Atomic replacement guarantees

- Requires `posix-rename@openssh.com`; remove-then-rename is refused because it
  would expose a missing-file window.
- Uses an exclusive cryptographically random `.aishtmp.<hex>` file in the
  destination directory.
- Refuses a destination symlink before staging and checks again immediately
  before rename.
- Preserves the existing mode unless an explicit mode was requested.
- Verifies required modes on the staged file before replacement. The final
  review extended the same false-success detection to explicit-mode append.
- Checks SHA-256 or mtime-size `if_match` immediately before rename.
- Cleans the temporary file on stale, symlink, mode, version, and rename errors.
- Treats transport loss as retained-client death and moves the SFTP axis to
  cached down.

This matches the pre-existing shell atomic-write contract. It is compare/check
immediately before atomic rename, not a server-side transactional CAS; another
writer can still race between the final check and rename.

### Windows mode caveat

The tested Windows OpenSSH SFTP server accepted `chmod` but continued to report
`0600`. AISH initially returned false success; `becd5c0` fixed that.

- Replacing an existing file without `mode` preserves its native reported
  mode and succeeds.
- Creating a file without `mode` permits the server's native creation mode.
- Requesting an unsupported POSIX mode fails explicitly.
- For atomic replacement, the destination remains unchanged on that failure.
- Append is non-atomic by contract: data may already be appended when the
  post-write mode verification reports that the requested mode was ignored.

Do not weaken this to make Windows appear more POSIX-like.

### Availability merge

When shell is conclusively down:

| SFTP state | File reads | Atomic write/edit/upload | exec/grep/search |
|---|---|---|---|
| unknown | unknown; first eligible operation may open once | unknown | unavailable |
| up + POSIX rename | available | available | unavailable |
| up without POSIX rename | available | unavailable with atomicity explanation | unavailable |
| cached down | unavailable with explicit force/MFA guidance | unavailable | unavailable |

Shell-up behavior is unchanged and remains shell-first.

---

## Workstream D result

### Middleware ordering

The ordering re-check required before coding resolved as follows against the
current server, and the answers are load-bearing:

- The logger is registered THIRD in `AddReceivingMiddleware`, which the SDK
  applies right-to-left, making it innermost.
- Innermost puts it inside `connAuthMiddleware`, so the client identity is
  resolved and every entry is attributable. A call rejected at the gate never
  reaches the logger and is not recorded: it executed nothing, and the human
  answered that prompt themselves.
- Innermost puts it inside `crossSession`, which returns before calling `next`
  when it forwards. A cross-session call is therefore recorded once, at the
  session that executes it, never duplicated at the relay.
- Innermost also means it holds the final `CallToolResult`. Typed handlers
  return their output as `json.RawMessage` in `StructuredContent`, so `via` and
  `host` are read off the result every routed handler already produces.
- `authproto.InternalTools` and `oob_log` itself are skipped. The auth tools
  carry keys, nonces and signatures and run before there is an identity; logging
  `oob_log` would let a polling client crowd out what it polls for.
- Failed and refused operations are recorded. The SDK converts handler errors
  into `IsError` results inside the tool wrapper, which is inside this
  middleware, so refusals arrive as results and are captured.

### Visibility classification

`controlTools` -> result `via` -> `terminalTools` -> `unresolved`.

`session_status` needs its exception because it REPORTS the route an operation
would take without taking it; reading `via` off its result would file every
status poll as out-of-band work.

The `unresolved` default was found by live testing and is the important one. A
routed tool refused BEFORE `route()` resolves returns no `via`, and the first
implementation defaulted that to visible - which hid the refused out-of-band
`sudo` that motivated the whole workstream from the human's default view.
Unknown routes now fail loud into the invisible view. Do not weaken this to
reduce noise.

### Content policy

`logTarget` is an explicit key allowlist: `command`, `path`, `pattern`,
`local_path`/`remote_path`, `task_id`. File content (`content`, `text`,
`old_text`, `new_text`, `patch`) and `send_input` text are never recorded.
Exec command lines are kept in full, capped at 1 KiB, since they are the point.

`Revoke` deliberately does NOT clear the log: withdrawing a client's access
should not erase the record of what it already did.

### Surfaces

- `oob_log` tool: cursor-based, cross-session capable, invisible operations by
  default with `include_visible` for the full call history.
- `Ctrl-]` -> `l`: recent invisible operations through the console, never the
  PTY.
- Pull, not push. No per-operation `Notify`.

Bounded at 500 entries, memory-only, dies with the session.

---

## Verification

Final required local verification:

```sh
make check
go test -race ./...
git diff --check
```

Automated coverage includes:

- target-native POSIX/Windows path normalization and rejection
- serialized concurrent use and concurrent probe single-flight
- bounded reads and large/binary read behavior
- subsystem-disabled and cached-down availability
- no-`posix-rename` refusal before server I/O
- exclusive temp creation, cleanup, and atomic rename
- initial and immediate pre-rename symlink refusal
- SHA-256 and mtime-size success, stale refusal, and missing versions
- existing/explicit mode preservation and ignored-mode refusal
- append path/mode handling
- transport-loss client retirement and force-only recovery
- shell/SFTP availability merge without enabling command tools
- B's unknown/advisory/non-POSIX framing guards

### Live acceptance

Passwordless POSIX:

- Ordinary file writes returned `via:"channel"`.
- Status showed zero SFTP attempts, preserving shell-first behavior.
- A direct protocol probe against Linux proved ordinary SFTP rename refuses an
  existing destination while `posix-rename@openssh.com` replaces it and
  preserves `0640`.

Windows OpenSSH with cmd.exe:

- One conclusive shell probe and one SFTP open were reused throughout.
- Atomic replacement with a current SHA-256 token succeeded via SFTP.
- Replaying the stale token was refused without clobbering the destination.
- An mtime-size token succeeded.
- `file_edit`, `file_patch`, `file_upload`, and append succeeded via SFTP.
- A Windows symlink was identified by `Lstat`; replacement was refused and the
  target remained unchanged.
- Explicit `0640` replacement failed because the server reported `0600`;
  the original destination remained intact and no temp file remained.
- No-mode replacement preserved the server-native `0600`.
- Final `session_status.oob_tools` reported all eight file tools available and
  kept `exec`, `file_grep`, and `file_search` unavailable.
- The disposable `C:\Users\mk31\aish-sftp-write-test` directory was removed
  and the shared `test` session was returned to local Bash.

Duo behavior was accepted in checkpoint 1: a pending SFTP open caused one
expected push, took over the status bar/title after the 500 ms debounce, and
restored the normal bar after approval. Cached/reused operations did not open a
new subsystem. Passwordless operations completed before the debounce and caused
no visible interruption.

PowerShell 5.1 and 7 shell classification were live-accepted in B. Once either
shell is conclusively down, C uses the same shell-independent SFTP protocol as
cmd.exe; no PowerShell syntax is introduced by the file path.

---

## Exact next work

Live-accept D, then merge `d-oob-activity-log` into `main`.

Local verification is complete: `make check`, `go test -race ./...`, and
`git diff --check` are clean, and the log was exercised against a live session
over both the local route and a `ssh localhost` ControlMaster route. Refused
escalation, `via: channel` file writes and exec, cursor paging, the
`include_visible` filter, and the `Ctrl-]` -> `l` console view were all
confirmed by hand, and no file content appeared in any entry.

Not yet done, and worth doing before merge:

- Live acceptance on a real remote and on an MFA host, confirming the log does
  not itself open channels or change probe behavior.
- Two concurrent AI clients against one session, which is the coordination case
  the feature is justified by and the one path automated tests do not cover.
- A restart of any long-lived AI proxy after install, so the new `oob_log` tool
  and the revised server instructions are actually loaded.

Deferred by choice: a status-bar counter (`oob: 12`) as an ambient hint. The bar
already arbitrates session badge, host, 2FA and input-required states, and the
value of another competitor for that row is unproven.

## Invariants

- The user's PTY remains byte-transparent. Console prompts/status/title are the
  sanctioned presentation path; never inject audit UI into the remote stream.
- `session_status` is a pure cache reader and must never open a channel or
  trigger MFA.
- Invisible work remains opt-in through `route()`; no new handler bypasses the
  OOB authorization gate.
- Unknown/advisory identity never enables POSIX `RunSentinel` framing.
- Identity axes and capability axes remain independent.
- New SSH slave/subsystem opens are sticky/cached and covered by MFA provenance
  warning; cache reads and retained-client reuse remain silent.
- Privilege escalation stays visible. Invisible `exec` refuses
  sudo/su/doas/pkexec/runuser.
- Writes remain wrong-host guarded, symlink-refusing, version-aware, and atomic
  where advertised.
- Client authorization is session-memory-only and kernel peer identity remains
  distinct from self-declared client descriptions.
- The activity log records what was touched, never what it contained, and never
  claims an operation was visible when its route is unknown. It is an audit
  trail for coordination and review, not a tamper-evident log and not a security
  boundary; do not describe it as stronger than that.

---

## Operational notes

- Install only to `/usr/local/bin/aish`; multiple copies previously caused
  version confusion.
- Make builds use `git describe`; every commit therefore has a distinguishable
  development version without manually bumping tags.
- Long-lived AI MCP proxies keep the version/tool descriptions loaded when they
  started. Restart the AI client after install to refresh them. Forwarding to a
  newer session server can work while proxy metadata remains stale.
- `aish sessions` is authoritative; do not assume a prior live session exists.
- Set `GIT_PAGER=cat` for captured Git output.

Known non-blocking cleanup outside workstream C:

- PSK reconnect notices can still be noisy; notify once per client key per
  session if that is addressed.
- An OOB operation timeout intentionally retires the persistent channel. A later
  explicit retry may open another channel and trigger another MFA request; the
  status warning now makes that provenance visible.
