# Handoff — aish `linearize-ring`

Context for picking this work up in a fresh session, particularly one running
**natively in WSL** rather than on the Windows side.

Companion document: `windows-targets-plan.md` holds the design for all four
workstreams. This file holds *current state*, decisions already made, and the
operational traps that cost real time to find.

---

## Where the work stands

Branch `linearize-ring`, **10 commits ahead of `main`**, unpushed. Tag `v0.4.0`
sits 4 commits behind the tip.

```
69ed543  build: install to /usr/local/bin only
e8b570a  exec: refuse privilege escalation on invisible routes
4bdd338  sshmux: re-probe a live channel whose facts were reset
b4b82c5  term: measure the linearization blast radius against real captures
44e6b89  build: stamp dev builds from git describe
06875b5  docs: record the durable-facts model and the linearization invariant
157323e  mcpserver: report what a host is instead of inviting a re-probe
c4656ff  sshmux: record durable host facts and identify non-POSIX shells
a81c19c  framing, read_output: consume the linearizer; bump to 0.4.0
08ea05a  term: linearize absolute cursor movement into line breaks
```

| Workstream | State |
|---|---|
| **A** — linearize the ring | done, live-validated on Linux and on a ConPTY host |
| **B phase 1** — durable host facts + dialect detection | done, live-validated on Windows cmd.exe and a Duo RHEL host |
| **B phases 2–3** | not started (passive screen fingerprint; active polyglot) |
| **C** — SFTP as probe and transport | not started; premise empirically confirmed |
| **D** — out-of-band activity log | not started; designed in the plan |

Untracked and deliberately so: `windows-targets-plan.md`, `handoff.md`. Committing
or gitignoring them is an open decision.

---

## Switching this session to WSL

### 1. Re-register the MCP server

The current registration lives in the **Windows** `C:\Users\mk31\.claude.json`
and crosses the boundary:

```
command: wsl.exe
args   : -d Ubuntu -- aish mcp-proxy
env    : AISH_PSK, WSLENV
```

Running Claude Code *inside* WSL removes the boundary, so it becomes:

```sh
claude mcp add aish --scope user --env AISH_PSK=<hex> -- aish mcp-proxy
```

Two notes:

- **`WSLENV` is no longer needed.** It existed only to carry `AISH_PSK` across
  the Windows→WSL boundary. Inside WSL the env var is passed directly.
- **Reuse the same PSK** if you want existing session grants to keep working —
  grants are keyed by the PSK-derived public key. It is in the Windows
  `.claude.json` under `mcpServers.aish.env.AISH_PSK`. A fresh
  `aish generate-psk` also works; the only cost is one approval prompt per
  session, once.

### 2. Decide where the repo lives

It is currently at `/mnt/c/Users/mk31/mcp/aish` (DrvFs). That works, and the Go
toolchain builds fine there, but **DrvFs caused three separate stale-read
incidents** in one afternoon — see the traps below.

Moving it into the Linux filesystem (`~/src/aish`) would make Go builds
substantially faster and eliminate that entire class of confusion, since git and
go would then be reading and writing the same filesystem. It is a judgement call;
nothing in the code depends on the location.

### 3. What gets better automatically

Much of the friction in the prior session was the Windows→WSL boundary rather
than the work itself:

- PowerShell repeatedly mangled `$VAR` and quoting inside `bash -c '...'`,
  silently passing empty strings. The workaround was writing shell scripts to
  files and executing those. In WSL, just write bash.
- `git describe` reading stale refs after a Windows-side `git tag` / `git commit`.
- Two `aish` binaries drifting apart on different PATH precedences.

---

## Decisions already made (do not re-litigate)

| Decision | Outcome |
|---|---|
| Order of work | A first (silent corruption outranks everything), then B; C depends on B's fact model; D is independent |
| Linearize globally or per call site | **Global**, bounded by property tests rather than a flag |
| May the passive screen hint suppress availability? | **No** — probe evidence only; advisory sources annotate, never change state |
| SFTP | **Adopted** as workstream C; open order is a policy switch pending a Duo test |
| Install location | **`/usr/local/bin` only.** Two copies on different PATH precedences let a session and its proxy silently run different builds |
| Version stamping | `git describe` for dev builds; the `var version` constant is bumped per *release*, not per build |

---

## Findings that were expensive to establish

Preserved because each one corrected a confident wrong assumption.

- **ConPTY ends lines with absolute cursor moves, not newlines.** Deleting those
  sequences fused unrelated lines *and* made the prompt-trim heuristic eat real
  output. The failure was silent truncation, not visible garbling.
- **Exit status is useless for identifying a Windows shell.** Measured over the
  wire: cmd.exe returns **1**, not the widely repeated 9009. A POSIX host missing
  `/bin/sh` returns 127 with "not found", which *is* a usable positive signal.
- **All the dialect evidence is on stderr; stdout comes back empty.** The channel
  was routing stderr to the null device, destroying 100% of it.
- **The SSH server banner is unavailable** on the ControlMaster slave path — a
  mux slave never exchanges protocol versions. Do not plan around it.
- **A polyglot probe cannot run on the channel's stdin**: cmd.exe fails at the
  *exec* of `sh` and exits before reading a byte.
- **SFTP works over the existing master with no new auth**, and `realpath(".")`
  returns `/C:/Users/...` on Windows OpenSSH — an unambiguous platform signature.
  This is workstream C's foundation and it is measured, not assumed.
- **On a per-session-MFA (Duo) host, every channel open is exactly one push.**
  Confirmed by process census: one login push, one channel push, then unlimited
  operations for free. `session_status` is deliberately channel-free and safe.

---

## Operational traps

- **`git log` pages on a PTY.** Capturing terminal output under `script(1)`
  hangs forever because `less` waits for input. Export `GIT_PAGER=cat` and wrap
  every capture in `timeout`.
- **`pkill -f <pattern>` matches its own wrapper command line** and kills the
  shell running it. CLAUDE.md warns about this; it still happened. Use exact
  pids.
- **DrvFs stale reads.** A file written from the Windows side is not always
  immediately visible to WSL. It produced a wrong version stamp twice and a stale
  `git describe` once. Verify versions in a command *separate* from the build.
- **Duo push accounting.** `session_status` and `version_info` never open a
  channel. `probe_host` opens one on first use (one push) and is free thereafter.
  Background `exec` takes a **dedicated** channel — one push each, by design.

---

## Open threads

1. **Unexplained repeated Duo pushes** on the RHEL host, seen while working with
   Amazon Quick. Two pushes (login + channel) are explained and expected;
   *repeated* pushes are not. Leading hypothesis: an operation that outruns its
   `timeout_ms` causes `ch.run` to `kill()` and drop the channel, so the next
   operation reopens and costs another push. **Untested.** The test is `exec`
   with `sleep 8` under `timeout_ms: 2000`, then a trivial `file_stat`.
   Diagnostic without spending pushes: compare the channel process start time
   (`ps -eo pid,lstart`) against the last census — if it moved, that timestamp
   *is* the push.
2. **PSK reconnect notice spams the terminal.** `connauth.go:156` calls
   `Sess.Notify` on every PSK-recognized reconnect. A host that restarts the
   proxy per tool call (Amazon Quick appears to) gets a line in the shared
   terminal on every call, defeating the point of a silent persisted grant.
   Notify once per client key per session instead.
3. **`--oob` suppresses the only MFA warning.** Without it, `route()` prompts
   before the first invisible operation — a natural place to warn that a push may
   follow. With `--oob` the push arrives unannounced. A `Notify` on first channel
   open per host would fix it.
4. **Tag placement.** `v0.4.0` points at a commit only reachable from this
   branch. A squash or rebase merge would orphan it; a fast-forward or merge
   commit keeps it. Consider re-tagging after merging.
5. **README platform table** still says Windows targets are "not supported
   (refused fast)". Understated now, and C would change the story again.

---

## Verifying the environment

```sh
cd /mnt/c/Users/mk31/mcp/aish     # or wherever the repo ends up
make check                        # go vet + full suite
make version                      # what a build would stamp
make build && sudo make install   # deploy to /usr/local/bin

aish sessions                     # live sessions
aish version                      # must match what the proxy reports
```

`version_info` through MCP shows proxy and session versions together — if they
disagree, one of them is a stale binary and that has caused confusion before.

A live aish session from the prior work may still exist (`alloy-server`, an ssh
into a Duo-protected RHEL host). It may have been closed; `aish sessions` is
authoritative.
