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
| **B phase 2 - passive screen fingerprinting** | done; pure/status-only and unit-validated |
| **B phase 3 - explicit active identity probe** | implemented and unit-validated; remote-shell matrix pending |
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
`5499871`. Both are merged to `main`.

---

## Active work

Branch: none after the `b-deep-probe` checkpoint is merged

Implementation branch point: `5499871` (`main`)

Completed objective: add an explicit, bounded, cached active identity probe
behind `probe_host{deep:true}` without coupling identity to shell capability or
opening more than one SSH session per explicit attempt.

### Status

Checkpoint complete. B is code-complete; phase 3 still needs live validation on
the target matrix below before it should be called fully live-validated.

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

### Changed files

`internal/sshmux/deep_probe.go`, `internal/sshmux/deep_probe_test.go`,
`internal/sshmux/facts.go`, `internal/sshmux/mux.go`,
`internal/mcpserver/tools.go`, `internal/mcpserver/tools_remote.go`,
`internal/mcpserver/capability_test.go`, `internal/proxy/aggregate.go`,
`internal/proxy/proxy_test.go`, `CLAUDE.md`, `windows-targets-plan.md`, and this
handoff.

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

### Live validation still required

Run `probe_host{deep:true}` over real ControlMaster paths against:

1. a POSIX host
2. Windows OpenSSH with cmd.exe
3. Windows PowerShell 5.1
4. PowerShell 7
5. the Duo-protected RHEL host

For each target, verify the first explicit call returns the correct dialect and
source, a repeat is a cache hit with no new session/MFA event, and
`deep:true,force:true` opens exactly one new attempt. Reconfirm that shell state
and every `oob_tools` value are byte-for-byte unchanged by deep probing.

### Exact next step

Create a fresh branch from updated `main` for workstream C. Before choosing its
SFTP open order, run the Duo subsystem test described below; that result decides
whether SFTP opens first or only after the shell axis is down.

### Blockers or uncertainties

- Phase 3 has fixture coverage based on locally captured cmd.exe, Windows
  PowerShell 5.1, and POSIX behavior, but no live remote matrix in this
  checkpoint.
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
| Version stamping | `git describe` for development builds; the source constant changes per release |

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
3. **`--oob` suppresses the natural MFA warning point.** Without it, `route()`
   prompts before the first invisible operation. With it, an MFA push can arrive
   unannounced. Consider one notification before the first channel open per
   host.
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

Do not assume any previous live session still exists. `aish sessions` is
authoritative.
