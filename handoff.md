# Handoff - Windows target work after `linearize-ring`

Current context for continuing the Windows-target work. The companion document
`windows-targets-plan.md` contains the original design for workstreams A-D; this
file records what has shipped, what remains, and the implementation constraints
established by live testing and the review of B phases 2-3.

---

## Current state

The completed `linearize-ring` work is merged into `main`. The repository is now
used natively from WSL at `/home/mike/aish`, rather than through DrvFs.

At the merge boundary:

- the worktree and pushed `linearize-ring` branch were clean
- `main` was a direct ancestor, so the merge was fast-forward only
- `go test ./...` and `go vet ./...` passed
- development builds derive their identity from the latest Git tag, commit, and
  dirty state; the latest tag in this clone is `v0.2.2`

| Workstream | State |
|---|---|
| **A - linearize the ring** | done; live-validated on Linux and a ConPTY host |
| **B phase 1 - durable host facts and stderr classification** | done; live-validated on Windows cmd.exe and a Duo RHEL host |
| **B identity foundation** | done; identity separated from shell capability in `9620df6` |
| **B phase 2 - passive screen fingerprinting** | done; unit-validated and live-validated on Windows cmd.exe |
| **B phase 3 - explicit active identity probe** | done; live-validated on POSIX, cmd.exe, Windows PowerShell 5.1, PowerShell 7, and Duo RHEL |
| **B MFA provenance warning** | done on `b-mfa-status`; unit/race-tested and live-validated on Duo and ordinary passwordless POSIX paths |
| **B closeout - unknown-target safety** | done; unit/race-tested and live-validated on unknown, POSIX, and cmd.exe in-band routes |
| **C - SFTP as probe and transport** | not started; premise empirically confirmed |
| **D - out-of-band activity log** | not started; designed in `windows-targets-plan.md` |

The linearization and phase-1 fact model landed in these commits:

```text
4bdd338  sshmux: re-probe a live channel whose facts were reset
b4b82c5  term: measure the linearization blast radius against real captures
06875b5  docs: record the durable-facts model and the linearization invariant
157323e  mcpserver: report what a host is instead of inviting a re-probe
c4656ff  sshmux: record durable host facts and identify non-POSIX shells
a81c19c  framing, read_output: consume the linearizer
08ea05a  term: linearize absolute cursor movement into line breaks
```

The identity foundation landed in `9620df6`; passive screen identity landed in
`5499871`; the MFA provenance warning landed in `345be94`; and the unknown-target
closeout landed in `99a0ab9`. All are merged to `main`.

---

## Active work

Branch: none; workstream B is merged to `main` through `99a0ab9`.

Completed objective: make truly unknown remote command syntax explicit and prevent
every POSIX-framed visible fallback from running until the dialect is
authoritatively identified as POSIX.

### Status

The closeout implementation, automated checks, and live acceptance are
complete. Workstream B is done.

### Completed

- Added a random nonce-framed, labeled expansion grammar and pure
  cmd/PowerShell/POSIX classifier. The parser starts at the exact nonce and
  accepts only the first value for each label, bounding profile-noise effects.
- Added an explicit remote-command runner with independent 8 KiB stdout/stderr
  caps and a 60-second total deadline.
- Cached identified, unknown, timeout, and command-failure outcomes per
  ControlMaster socket. Cache reads happen before authorization/routing.
- Added per-socket single-flight so concurrent explicit requests open at most
  one SSH session. Waiting callers share the result.
- Added `probe_host{deep:true}` and scoped `deep:true,force:true`. Deep force
  clears only deep state; ordinary force clears only shell state.
- Added structured deep status, attempts, cache, evidence, and exit metadata.
  Identified results create authoritative `deep_probe` identity facts but never
  alter `ShellAxis`, persistent channels, host confidence, or `oob_tools`.
- Added anti-loop guidance: unknown/failed outcomes state that they are cached,
  explain the MFA cost, and require both `deep=true` and `force=true` to retry.
- Updated the server instructions and repository design guidance so models use
  `oob_tools`, not deep identity, as the capability decision surface.
- Added a reference-counted, 500 ms-debounced SSH-session attempt tracker. A
  pending attempt replaces the status bar with a modal 2FA warning naming the
  operation and target, and adds `[2FA?]` to the title while the bar is hidden.
  Background tasks use a filtered random startup marker so the warning clears
  after remote startup rather than after a long-running command exits.
- Added `remote_identity_status` (`unknown`, `advisory`, or `authoritative`) to
  remote `session_status` and `probe_host` results. Unknown identity carries an
  explicit command-syntax warning; authoritative platform-only evidence still
  states that it does not establish a shell dialect.
- Changed in-band availability from a platform denylist to a dialect allowlist:
  only authoritative `posix` enables `file_read`, `file_write`, or foreground
  `exec`. Unknown is reported as `unknown`; cmd, PowerShell, network, restricted,
  and no-shell identities are unavailable.
- Added the missing `requireTool("exec")` enforcement. Before this closeout,
  `exec` could bypass its reported availability and reach `RunSentinel` on an
  unknown target. Routes now retain their `ConnInfo` identity key when a tracked
  SSH connection loses its ControlMaster, preserving durable facts in-band.

### Changed files

`internal/sshmux/deep_probe.go`, `internal/sshmux/deep_probe_test.go`,
`internal/sshmux/facts.go`, `internal/sshmux/mux.go`,
`internal/mcpserver/tools.go`, `internal/mcpserver/tools_remote.go`,
`internal/mcpserver/capability_test.go`, `internal/proxy/aggregate.go`,
`internal/proxy/proxy_test.go`, `CLAUDE.md`, `windows-targets-plan.md`, and this
handoff. Follow-up live validation also hardened ANSI cleanup in
`internal/sshmux/dialect.go` and `dialect_test.go`. The MFA warning checkpoint
adds `internal/sshmux/activity.go`, task startup-marker handling,
`internal/mcpserver/statusline.go`, title integration, and focused tests.
The unknown-target closeout changes `internal/mcpserver/capability.go`,
`tools.go`, `tools_remote.go`, `screen_identity.go`, their focused tests,
`internal/proxy/aggregate.go`, README/CLAUDE guidance, and the planning docs.

### Verification

```text
make check                                                    PASS
go test -race ./internal/sshmux ./internal/mcpserver ./internal/proxy  PASS
git diff --check                                              PASS
```

The classifier fixtures include captured cmd.exe, Windows PowerShell 5.1, and
POSIX expansion shapes plus missing/incomplete/mixed responses, unset variables,
trailing profile noise, timeout, command failure, unknown-result caching,
deep-only force, and concurrent single-flight.

### Live validation completed 2026-08-14

The `test` session connected passwordlessly over real ControlMaster paths to
`mike@home.krzywonski.me:55522` and `mk31@localhost`.

- POSIX: the first deep call identified `posix/unix` from expansion grammar,
  returned exit 0 and source `deep_probe`; a repeat returned
  `deep_probe_cached:true`; `deep+force` made one uncached attempt. `ShellAxis`
  remained unknown and every `oob_tools` entry remained unchanged at `unknown`.
- Windows cmd.exe: before deep probing, passive status identified advisory
  `cmd/windows` with source `screen` while every tool remained `unknown`. The
  first deep call upgraded both identity sources to `deep_probe`; repeat and
  force behavior matched POSIX, without changing shell capability or tools.
- Independence: one subsequent ordinary shell probe classified the exact cmd
  stderr fingerprint, moved all POSIX OOB tools to sticky `unavailable`, and
  preserved the authoritative deep identity and its cache. A later deep call
  remained a cache hit and did not change that availability.
- Windows PowerShell 5.1: OpenSSH `DefaultShell` was temporarily changed to
  `powershell.exe`, `sshd` restarted, and the old master explicitly closed.
  The ordinary probe classified the PowerShell 5.1 stderr form; deep probing
  identified `powershell/windows` with source `deep_probe`, and the repeat was
  a cache hit with unchanged tool availability.
- PowerShell 7.6.4: the same fresh-master procedure using `pwsh.exe` produced
  the expected interactive prompt, ordinary PowerShell classification, deep
  expansion evidence, and cached repeat. Its redirected stderr exposed a
  cosmetic evidence bug: rendered ANSI markers appeared as `\x1b[...]` in the
  note. Evidence generation now strips both CSI bytes and that literal form,
  with regression tests; classification continues to use the original bytes.
- Restoration: the complete `HKLM\SOFTWARE\OpenSSH` key was exported before
  testing. After both shells, test-added shell values were removed, the export
  re-imported, `sshd` restarted, a fresh cmd.exe login verified, and the
  temporary export deleted. The test session was returned to local Bash.
- Duo-protected RHEL: passive `session_status`/screen reads caused no push. The
  interactive login caused one expected push; the first ordinary probe caused
  one expected push to open the persistent shell channel; its repeat returned
  immediately with `probe_attempts:1` and no push. The first explicit deep probe
  caused one expected push and identified `posix/unix`; its cached repeat
  returned in milliseconds with no push. `deep:true,force:true` caused exactly
  one expected push, replaced only the deep cache, and its following repeat was
  cached with no push. Ordinary shell attempts remained `1`, and all
  `oob_tools` values stayed available throughout. A final `file_stat` completed
  over the original persistent channel in 45 ms without a push, confirming that
  deep probing did not replace or drop it. No unexpected Duo notifications were
  observed.
- MFA warning UI: on the freshly installed build, an 8.9-second ordinary probe
  showed the modal `OOB shell probe` warning and its 23 ms cache hit did not.
  First and forced deep probes showed `deep identity probe` for 4.0 and 5.5
  seconds; 12-16 ms cache hits remained invisible. A dedicated background task
  showed `background command`; its startup marker restored the standard bar
  after Duo approval while the two-second task continued, and the marker was
  absent from captured output. The user confirmed each takeover and restoration
  visually.
- Non-MFA regression check: on the passwordless POSIX host, ordinary probing
  completed in 202 ms, first and forced deep probes in 66 and 75 ms, and
  background startup in 9 ms. Cache hits completed in 9-15 ms. Each path was
  followed by at least 700 ms idle time to expose a stale debounce callback.
  The user observed no takeover, title marker, flicker, or interruption. This
  confirms that the warning is conspicuous when Duo is pending and effectively
  invisible when authentication finishes within the 500 ms debounce.
- Unknown-target closeout: the installed build first connected to the same
  passwordless POSIX host with explicit `ControlMaster=no,ControlPath=none`,
  intentionally removing all authoritative identity. Status reported
  `remote_identity_status:unknown`; `file_read`, `file_write`, and `exec` were
  non-available with a command-syntax warning. Direct `file_read` and `exec`
  calls both refused, and screen generation/content remained identical after a
  700 ms stale-timer check, proving no sentinel bytes were typed.
- Known POSIX in-band: after an ordinary probe established authoritative
  `posix`, OOB was disabled and the access prompt declined. Foreground `exec`
  completed visibly with exit 0 and `via:in_band`, proving safe fallback was not
  over-blocked.
- Recognized non-POSIX in-band: cmd.exe first appeared as `advisory` from the
  screen, then its 81 ms ordinary probe upgraded identity to authoritative and
  made every POSIX tool unavailable. After OOB was disabled and access prompts
  declined, direct `file_read` and `exec` refused with cmd.exe-specific
  guidance. The Windows screen remained byte-for-byte unchanged at generation
  52 after another 700 ms wait. Durable identity survived the route downgrade.

### Exact next step

For workstream C, first run the separate SFTP subsystem test over the existing
Duo-protected master; whether it causes another push determines the default
SFTP open-order policy. Then create `c-sftp-axis` from updated `main` and
implement the narrow subsystem/fact-axis checkpoint described below.

### Blockers or uncertainties

- Workstream C's default SFTP open order still depends on whether a subsystem
  request over the existing Duo-protected master causes another push.

---

## Next work: workstream C

Start the next implementation branch from updated `main`; `c-sftp-axis` is a
suitable name. `windows-targets-plan.md` contains the current C sequence.

Do not collapse SFTP into `ShellAxis`. Add `SftpAxis` beside it, publish SFTP
platform evidence through `IdentitySourceSFTP`, and merge tool capability only
in `mcpserver.availability`. The first narrow checkpoint should prove subsystem
startup, bounded failure/cache behavior, `realpath(".")` platform
classification, and the Duo open-cost policy before routing file operations.

---

## Decisions already made

| Decision | Outcome |
|---|---|
| Order of work | A first, then B; C depends on B's fact model; D is independent |
| Linearize globally or per call site | **Global**, bounded by property tests rather than a flag |
| May a passive screen hint suppress availability? | **No.** Advisory evidence annotates status only |
| May deep identity imply shell capability? | **No.** Identity and `sh -s` capability are independent facts |
| SFTP | **Adopted** as workstream C; open order remains a policy switch pending a Duo test |
| Install location | **`/usr/local/bin` only.** Multiple installed copies caused version drift |
| Version stamping | `git describe` for Make builds; unstamped builds use embedded VCS revision/dirty metadata |

---

## Findings that constrain the design

These were measured directly and corrected earlier assumptions.

- **ConPTY ends lines with absolute cursor moves, not newlines.** Deleting those
  sequences fused unrelated lines and made prompt trimming eat real output. The
  linearizer now reconstructs that line structure.
- **Exit status does not identify a Windows shell.** cmd.exe returned `1`, not
  the commonly cited `9009`. A POSIX host missing `/bin/sh` returned `127` with
  `not found`, which is a usable positive POSIX signal.
- **The useful evidence from a failed `sh -s` invocation is on stderr.** stdout
  was empty in the observed cmd.exe and PowerShell cases.
- **The SSH server banner is unavailable on the ControlMaster slave path.** A
  mux slave does not exchange protocol versions.
- **An active identity probe cannot run on the persistent channel's stdin.**
  cmd.exe fails while attempting to execute `sh -s` and exits before reading
  stdin. A deep probe requires a separate remote command and may cost an MFA
  push.
- **The proposed literal polyglot needs a real output protocol.** Windows
  PowerShell 5.1 emits `echo AISHDIALECT %OS% %COMSPEC% $SHELL` as multiple
  lines and supplies no explicit PowerShell marker. Use a random marker and
  labeled expansion fields, then classify expansion behavior from bounded
  stdout/stderr.
- **SFTP works over the existing master without new authentication.** On
  Windows OpenSSH, `realpath(".")` returned `/C:/Users/...`, an unambiguous
  platform signature for workstream C.
- **On the tested per-session-MFA Duo host, every channel open is one push.**
  One persistent channel then supports unlimited foreground operations until it
  is dropped. `session_status` remains channel-free.

---

## Operational traps

- **`git log` pages on a PTY.** Set `GIT_PAGER=cat` and wrap captures in
  `timeout`; otherwise `less` can wait indefinitely.
- **`pkill -f <pattern>` can match its own wrapper command line.** Use exact
  process IDs.
- **DrvFs produced stale reads.** The active repository is now on the Linux
  filesystem, but keep this in mind when building from any `/mnt/c` checkout.
- **PowerShell mangles nested Bash quoting easily.** Run build and Git commands
  directly in WSL.
- **Duo push accounting matters.** `session_status` and `version_info` do not
  open a channel. The first ordinary probe can open one. Background `exec` uses
  a dedicated channel by design. A future deep probe also opens a separate
  session and must never be implicit.

---

## Open threads

1. **Unexplained repeated Duo pushes.** Two pushes, login plus the persistent
   channel, are understood. The leading untested hypothesis for later pushes is
   that an operation exceeding `timeout_ms` kills and drops the channel, so the
   next operation opens a replacement. Test with `sleep 8` under
   `timeout_ms: 2000`, followed by a trivial `file_stat`, while comparing the
   channel process start time.
2. **PSK reconnect notices can spam the terminal.** `connauth.go` notifies on
   every recognized reconnect. Notify once per client key per session instead.
3. **Console confirmation prompts can be easy to miss.** During the first
   background-task test, the one-time unknown-target write confirmation sat
   unanswered before any SSH session was opened. The MFA status takeover then
   appeared correctly after the user answered. Consider a separate modal
   `INPUT REQUIRED` status/title state for console prompts.
4. **README platform wording remains conservative.** Windows targets are
   detected and refused quickly for POSIX OOB operations, but are not useful for
   native file operations until workstream C lands.
---

## Verification and deployment

```sh
cd /home/mike/aish
make check
make version
make build && sudo make install

aish sessions
aish version
```

`version_info` through MCP reports proxy and session versions together. If they
disagree, stale installed binaries are the likely cause.

The MFA warning build was installed as `v0.2.2-22-gd115a47-dirty`; the artifact
and `/usr/local/bin/aish` had identical SHA-256 hashes. The existing long-lived
proxy still reported `v0.2.2-18-g5561c90` during testing and will pick up the
installed build when its AI client restarts.

The B-closeout build was installed as `v0.2.2-23-g345be94-dirty`; the artifact
and `/usr/local/bin/aish` both had SHA-256
`023ce9ae65beefe52343e66aea2b2a1d29023734072c9ad25b00872013ba53fd`.

Do not assume any previous live session still exists. `aish sessions` is
authoritative.
