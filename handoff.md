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
- the release source reports `0.4.0`, but no `v0.4.0` Git tag exists in this
  clone; `make version` therefore derives from `v0.2.2`

| Workstream | State |
|---|---|
| **A - linearize the ring** | done; live-validated on Linux and a ConPTY host |
| **B phase 1 - durable host facts and stderr classification** | done; live-validated on Windows cmd.exe and a Duo RHEL host |
| **B phase 2 - passive screen fingerprinting** | not started; design reviewed |
| **B phase 3 - explicit active identity probe** | not started; design reviewed |
| **C - SFTP as probe and transport** | not started; premise empirically confirmed |
| **D - out-of-band activity log** | not started; designed in `windows-targets-plan.md` |

The linearization and phase-1 fact model landed in these commits:

```text
4bdd338  sshmux: re-probe a live channel whose facts were reset
b4b82c5  term: measure the linearization blast radius against real captures
06875b5  docs: record the durable-facts model and the linearization invariant
157323e  mcpserver: report what a host is instead of inviting a re-probe
c4656ff  sshmux: record durable host facts and identify non-POSIX shells
a81c19c  framing, read_output: consume the linearizer; bump to 0.4.0
08ea05a  term: linearize absolute cursor movement into line breaks
```

---

## Next work: finish B

Start the next implementation branch from the updated `main`. A suitable name
is `windows-target-identity`.

The critical design correction is to separate **target identity** from
**transport capability** before adding either detector:

- `ShellAxis` answers whether the persistent `sh -s` transport works.
- Authoritative identity facts answer dialect and platform, with their source.
- A deep probe may identify cmd.exe or PowerShell but must not mark `ShellAxis`
  up or down.
- Screen evidence is recomputed from the current snapshot, never persisted in
  `Mux`, and never changes `oob_tools`, host tracking, or retry policy.
- Model-facing status must distinguish advisory `screen` evidence from
  authoritative `shell_probe` or `deep_probe` evidence.

Recommended order:

1. Add characterization tests around the completed phase-1 fact behavior.
2. Move dialect/platform identity out of `ShellAxis` into independent facts.
3. Add the pure passive-screen classifier and status-only integration.
4. Add a bounded, nonce-framed active identity probe behind
   `probe_host{deep:true}`.
5. Cache and single-flight deep probes per ControlMaster socket so concurrent
   clients cannot produce duplicate MFA pushes.
6. Define scoped reset semantics: `force` for the ordinary shell probe and
   `deep+force` for the deep-probe cache.
7. Update proxy instructions and docs so models plan against `oob_tools`; a
   screen-derived dialect is only a hint.
8. Unit-test evidence precedence and live-validate POSIX, cmd.exe, Windows
   PowerShell, PowerShell 7, and one Duo-protected host.

The full critical review and implementation plan should be folded into
`windows-targets-plan.md` as B is implemented; its original phase-2/3 outline is
not detailed enough about source precedence, deep/force behavior, or MFA cost.

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
4. **The `v0.4.0` release tag is absent.** The source and package files report
   `0.4.0`, but `make version` currently reports a development version based on
   `v0.2.2`. Decide whether to tag the merged `main` before the next release.
5. **README platform wording remains conservative.** Windows targets are
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
