# Windows targets: the blind spot

Implementation plan for two defects found by driving a real cmd.exe session over
ssh, plus the transport that makes non-POSIX hosts genuinely useful.

- **Repo**: aish; see `handoff.md` for the active branch
- **Verified on**: WSL → Windows OpenSSH 10.0p2 (cmd.exe), and a Duo-protected
  RHEL 9.8 host
- **Status**: A **done**; B phases 1-3 **implemented** (phase 1 live-validated,
  phases 2-3 unit-validated; phase 3 still needs the remote-shell validation
  matrix). C and D not started. See `handoff.md` for current state.

---

## Why this matters more than it looks

The shared terminal already works on Windows targets. `read_screen` renders
cmd.exe perfectly, and the out-of-band refusal is fast and clear. None of this is
about making Windows a first-class citizen.

It's about one asymmetry. When the AI asked for command output, it got back a
confident, well-formed, **wrong** answer — no error, no truncation flag, no
`dropped_bytes`. That poisons every downstream inference, because there's no
signal to distrust. An AI handles *"this failed, try something else"* perfectly
well. It cannot handle *"here is your output"* when it isn't.

The design lesson is already in the codebase: the loud, fast OOB refusal
(*"the oob channel to localhost closed immediately… use run_command instead"*)
was the best part of the experience — under a second, unambiguous, actionable.
Bias everything here that way: fail loudly rather than degrade quietly.

---

## What was measured

Running `echo AISH-START & ver & echo AISH-END` in a cmd.exe session reached over
ssh from WSL. Actual bytes from the session ring:

```
AISH-START \x1b[7;1HMicrosoft Windows [Version 10.0.22631.3155]\r\nAISH-END\x1b[10;1Htxstate\mk31@TAG232207 C:\Users\mk31>\x1b[?25h
```

ConPTY does not emit newlines for vertical movement — it positions absolutely
with CUP. `StripEscapes` deletes those with no substitution, so the lines fuse;
then `runIdle`'s "drop everything after the last newline" prompt heuristic eats
what's left. What `run_command` actually returned:

```
AISH-START Microsoft Windows [Version 10.0.22631.3155]
```

`AISH-END` is gone. Meanwhile `read_screen` was flawless, because midterm
interprets the CUP.

### Shell fingerprints, captured directly

Both shells run locally with `sh -s` — the exact remote command aish's channel
sends — so these strings are exact, not guessed.

| Target | stderr | Exit |
|---|---|---|
| cmd.exe | `'sh' is not recognized as an internal or external command,` | `1` |
| PowerShell 7.6 | `sh: The term 'sh' is not recognized as a name of a cmdlet, function, script file, or executable program.` | `1` |

Both share the prefix `is not recognized as` and diverge only in the suffix, so
the classifier must match the full distinguishing tail — `an internal or external
command` vs `a name of a cmdlet`.

---

## Signals that didn't survive

Recorded so nobody re-proposes them.

| Proposed signal | Why it fails |
|---|---|
| SSH server banner (`OpenSSH_for_Windows_9.5`) | Not available on the path we use. `openChannel` runs a ControlMaster *slave*, which speaks the mux protocol over a Unix socket and never does a protocol version exchange. The process that did exchange banners is the user's interactive ssh; adding `-v` there would write to the shared terminal — a byte-transparency violation. |
| Exit code `9009` as a cmd.exe signature | Measured: cmd returns **1**, not 9009. (9009 is what MSBuild-style wrappers report.) ssh also truncates remote status to one byte, so 9009 would arrive as 49. Exit status is worthless here — `1` is maximally ambiguous. |
| Dialect polyglot on the channel's stdin | sshd runs `cmd.exe /c "sh -s"`. cmd fails at the *exec* of `sh` and exits before reading a byte of stdin, so nothing we write is ever seen. Only works as a separate remote command — a second session on the master, i.e. potentially a second MFA push. |

**Net effect: stderr text is the only reliable signal**, which simplifies the
detection design considerably.

---

## Workstream A — Linearize the ring

*The correctness fix. Ships first. Independent of B and C.*

### Approach: a stateful linearizer, scoped to one call

The obvious objection to tracking cursor state is that a ring read can start at
an arbitrary offset with no known starting row. That objection dissolves:
**CUP is absolute, so the linearizer is self-synchronizing.** The first absolute
row-setting sequence in a window *establishes* exact state, and every decision
after it is exact. Relative moves never need absolute state — the delta is the
whole answer.

A mid-stream start, a ring wrap, or a client polling in 4 KB chunks each cost
**at most one** heuristic decision, and that heuristic errs toward inserting a
break, never toward fusing. Nothing persists between calls; no new mutable state
on `Terminal`.

### Why not derive output from the screen model

It was the tempting option and it loses decisively:

- midterm's grid has **no scrollback**, so it truncates past one screen
- it **hard-wraps** long lines with no way to tell them from real ones
- it carries **no ring offsets**, breaking the documented cursor invariant
- it can't render a *historical* range without replaying into a second emulator
- it wouldn't fix `read_output` at all

The Screen stays "what the user sees now"; the Ring stays "what was emitted, in
order".

### Two further defects with the same root cause

- **`afterEcho` is broken identically.** It scans raw ring bytes for the first
  `\n` after injection to find the window start. On ConPTY the command echo is
  frequently terminated by a CUP, not a newline — so it skips to a newline
  *inside the output* and silently eats the first output lines. A fix that only
  touches `StripEscapes` leaves this second corruption path live.
- **`ansiRe` has gaps.** Its final alternative misses `ESC 7`/`ESC 8`
  (DECSC/DECRC), `ESC c`, keypad modes, and `ESC ( B` — the last emitted by every
  `tput sgr0`. Those bytes reach the model verbatim today. The linearizer must
  recognise DECSC/DECRC anyway to track the cursor, so fix the regex in the same
  change.

### Sequence

1. Extend `ansiRe` to the ECMA-48 catch-all, keeping it **last** so the
   CSI/OSC/DCS branches still win. Independently reviewable, own tests.
   — `internal/term/terminal.go`
2. Add `Linearize(b, rows)`, `FirstBreak(b)`, and `Screen.Size()`. Full test
   suite, no callers switched yet. `StripEscapes` keeps its exact signature and
   becomes the reference the property test compares against.
   — `internal/term/linearize.go`, `screen.go`
3. Switch `framing.window()`, extract the trailing-prompt drop into a pure
   testable `dropTrailingPrompt`, add the output byte clamp (insertion can now
   push past `maxReturn`).
   — `internal/framing/framing.go`
4. Fix `afterEcho` via `FirstBreak`. **Validate hardest here** — it changes where
   a window *starts*, not just how it renders.
   — `internal/framing/framing.go`
5. Switch `read_output` (non-raw only) and update its tool description to note
   that lines may be reconstructed from cursor movement.
   — `internal/mcpserver/tools.go`

### How the blast radius is bounded

Not by a flag — by property tests. The regression argument is mechanical:

| Guard | Guarantees |
|---|---|
| **P2 — no-op property** | On any stream with no vertical-movement construct, `Linearize` is byte-identical to `StripEscapes`. Ordinary Linux output (SGR, bare `\r`, `ESC[K`, OSC 7/133) is provably untouched. |
| **P1 — content preservation** | Stripping all newlines from the output reproduces `StripEscapes` exactly. Linearization can only *add* line structure, never lose text — a hard ceiling on how bad a mistake can be. |
| **Growth budget** | Output bounded under 2× input regardless of adversarial escape alternation. |
| **Capture corpus** | Real byte streams in `testdata/` — `git log --color`, `docker pull` multi-line progress, zsh RPROMPT redraw, a vim session — to *measure* the Linux blast radius rather than reason about it. |
| **`AISH_NO_LINEARIZE=1`** | Undocumented kill switch to recover a live session without a rebuild. |

Primary regression case is the real captured sequence above. The assertion that
matters is end-to-end: after `dropTrailingPrompt`, the result must still contain
`AISH-END` and must not contain `TAG232207`. That is the test that would have
caught this in production.

---

## Workstream B — Know the target

*The orientation fix. Depends on nothing in A.*

Today a failed probe caches **nothing**: `ChannelRun` deletes the dead channel,
and capabilities live on the channel object. So `session_status` keeps reporting
every tool `unknown` with *"host not probed yet; call probe_host to initialize"*
— inviting an infinite re-probe loop of a host that can never succeed, each
attempt costing a channel open and, on an MFA host, a push.

### Data model

A `HostFacts` record on `Mux`, keyed by `ci.Sock` (already deterministic per ssh
target), holding a `ShellAxis` with `AxisUnknown | AxisUp | AxisDown`, matched
failure evidence, and an attempt count. Critically it **outlives the channel
that produced it**. Capabilities move here too, which also fixes a live bug where
positive caps die on a channel timeout and silently regress `target_confidence`
from `same` back to `unknown`.

Dialect and platform identity are independent facts beside `ShellAxis`, each
carrying its source, evidence, and observation time. This separation is
load-bearing for phases 2-3 and C: an active identity probe can identify cmd.exe
without proving whether `sh -s` works, while SFTP can identify Windows without
identifying cmd.exe versus PowerShell. A forced shell retry clears only the
shell axis and shell-derived identity; independently learned identity survives.

Its own mutex, separate from `chMu` — `session_status` reads facts on every call
and must never block behind `openChannel`'s `cmd.Start()`, which runs under
`chMu` today.

**Sticky versus soft** is the key rule: a *classified* failure (dialect
identified, or the shell answered but never returned our sentinel) is a host
property → sticky, never retried. An *unclassified* failure is a transport fact →
soft, retried once, sticky at two attempts. An unclassifiable host costs at most
two MFA pushes before requiring an explicit force.

### Detection tiers

| Tier | Cost | Signal | Phase |
|---|---|---|---|
| **0 — passive screen** | Free. No channel, no grant, no round trip. | Fingerprint the visible screen: `Microsoft Windows [Version`, `PS C:\…>`, `C:\…>`. One strong or two weak hits. **Advisory only** — annotates status and never changes availability. | 2 |
| **A — stderr fingerprint** | Nothing beyond the channel open already paid for. | Pipe `cmd.Stderr` (currently wired to the null device), keep the `out` buffer on failure, classify against the exact strings above. **The workhorse** — fully covers the observed case. | 1 |
| **B — active expansion probe** | A second session on the master; may cost an MFA push. | A random nonce plus labeled cmd, PowerShell, and POSIX variable forms behind `probe_host{deep:true}`. Never implicit. | 3 |

### What the model sees afterwards

`session_status` stays a pure cache reader that never opens a channel. It gains
`remote_dialect`, `remote_platform`, and — load-bearing —
`remote_dialect_source`, distinguishing authoritative `shell_probe` and
`deep_probe` results from an advisory `screen` guess. Platform has its own source
because SFTP may establish it independently. A passive guess must never stop the
AI trying; an authoritative shell-capability result must.

A classified non-POSIX host flips from `unknown` to `unavailable`, and its detail
text stops saying "call probe_host to initialize". This sits squarely inside the
existing model — a missing POSIX shell *is* a probe-time capability, and the
failed probe *is* the probe.

**The anti-loop test**: for a cmd.exe fact, the refusal message must name
cmd.exe, must contain `run_command`, must mention `force`, and must **not**
contain "call probe_host to initialize". Stating explicitly that aish is not
retrying, and why, is what actually stops a model looping.

### Phase 2 implementation

Passive classification is deliberately narrower than the original sketch:

- A PowerShell prompt on the cursor row (`PS C:\...>` or the UNC provider form)
  identifies PowerShell and Windows.
- A drive-path prompt on the cursor row plus a visible Windows version banner
  identifies cmd.exe and Windows.
- A drive-path prompt without the banner identifies only the Windows platform;
  the dialect remains unknown.
- Banner-only, non-current prompt text, stale banners followed by a POSIX prompt,
  invalid cursor positions, and alternate-screen content produce no dialect.

The result is computed from each `term.Snapshot`, never stored in `HostFacts`,
and fills only status identity fields left unknown by authoritative evidence.
`remote_identity_note` states that the result is advisory. Tests assert that
screen hints cannot change durable facts, authoritative `remoteDialect`, or any
`oob_tools` state.

### Phase 3 implementation

The original literal polyglot was not discriminating enough. Windows PowerShell
5.1 parses `echo` as `Write-Output` and emits the arguments on separate lines,
and the proposed command had no PowerShell-specific field. The implemented
command uses a random 64-bit hex nonce and labels every expansion form:

```text
echo AISH_DIALECT_<nonce> PCTOS=%OS% PCTCOMSPEC=%COMSPEC% PSOS=$env:OS PSCOMSPEC=$env:ComSpec SH=$SHELL
```

Classification starts only after the exact nonce and uses the first occurrence
of each label, so banners before the response and profile noise after it cannot
be mistaken for probe fields. It classifies expansion grammar rather than
specific environment values:

| Login command grammar | Observed expansion pattern |
|---|---|
| cmd.exe | `%OS%` and `%COMSPEC%` expand; PowerShell and POSIX forms remain literal |
| PowerShell | percent forms remain literal; `$env:*` expands; `$SHELL` is consumed |
| POSIX shell | percent forms remain literal; `$env:OS` becomes `:OS`; `$env:ComSpec` becomes `:ComSpec`; `$SHELL` expands or becomes empty |

The command is an explicit diagnostic operation, not another capability axis:

- `probe_host{deep:true}` may open one ControlMaster slave session and trigger
  MFA. No automatic tool path runs it.
- stdout and stderr are each capped at 8 KiB and the entire operation has a
  60-second context deadline.
- identified, unknown, timeout, and command-failure outcomes are all cached per
  ControlMaster socket. Cache lookup happens before `route()`, so reading a
  result cannot ask for authorization or open a session.
- concurrent callers share one per-socket flight. A waiting caller may cancel
  its wait without starting another command.
- `deep:true, force:true` clears only the deep result and deep-derived identity.
  Ordinary `force:true` still clears only the persistent-shell probe and its
  identity. Neither reset crosses the other axis.
- an identified result records authoritative `deep_probe` dialect/platform
  facts but never changes `ShellAxis`, persistent channel state, host
  confidence, or `oob_tools`.
- failed and unknown results explicitly say they are cached and that only
  `deep:true, force:true` pays for another attempt. This is the phase-3
  anti-loop rule.

`session_status` remains channel-free and reports only cached
`deep_probe_status`, `deep_probe_attempts`, and failure/unknown note fields.
`probe_host` additionally reports cache status, expansion evidence, and an exit
status when one exists.

The expansion behavior has been captured locally from cmd.exe, Windows
PowerShell 5.1, and POSIX `sh`. Before calling phase 3 live-validated, run it over
real ControlMaster paths against POSIX, cmd.exe, Windows PowerShell, PowerShell
7, and the Duo-protected host; confirm one explicit call means at most one new
MFA event and every repeat is a cache hit.

### Sequence

1. Pipe stderr into a bounded buffer; store the exit code and a `reaped` channel
   instead of discarding `cmd.Wait()`; have `runProbe` return a `probeFailure`
   carrying stdout, stderr and exit rather than dropping them.
   — `internal/sshmux/channel.go`
2. New `Dialect` type and the ordered fingerprint table. Pure, table-driven.
   — `internal/sshmux/dialect.go`
3. New `HostFacts` / `ShellAxis` / `AxisState`, the `facts` map and its own mutex,
   plus `Facts` / `ForgetFacts` / `NoteShellUnusable` / `ShellError`.
   — `internal/sshmux/facts.go`
4. Sticky short-circuit at the top of `ChannelRun`; record success and failure;
   `CachedCapabilities` reads facts; delete `channel.caps`.
   — `internal/sshmux/channel.go`, `probe.go`
5. An `availability(facts)` merge point (where the SFTP axis later slots in),
   `dialectUnavailability`, new detail strings — and make the `in_band` branch
   dialect-aware.
   — `internal/mcpserver/capability.go`
6. New `session_status` fields; source `remote_capabilities` and `oob_user` from
   facts so they survive channel death.
   — `internal/mcpserver/tools.go`
7. `probe_host{force}`; structured non-error response on a sticky-down host;
   check facts before `route()` so a known-dead host doesn't trigger an OOB
   consent prompt just to be refused.
   — `internal/mcpserver/tools_remote.go`
8. Gate `RemoteTrackingApplicable` on dialect; build the inline `[p]` option in
   the divergence confirm conditionally.
   — `internal/mcpserver/hosttracking.go`, `tools_remote.go`
9. Update server instructions and CLAUDE.md — including the invariant "the OOB
   channel protocol assumes a POSIX shell", which gains "…and a host that fails
   that assumption is recorded as a durable fact, never re-probed silently".
   — `internal/proxy/aggregate.go`, `CLAUDE.md`
10. Add the nonce-framed expansion classifier and bounded active command runner;
    cache every outcome and single-flight by ControlMaster socket.
    — `internal/sshmux/deep_probe.go`, `facts.go`, `mux.go`
11. Add `probe_host{deep}` and scoped `deep+force`, return structured deep
    metadata, and expose cached status without changing tool availability.
    — `internal/mcpserver/tools_remote.go`, `tools.go`

---

## Workstream C — SFTP as probe and transport

*The capability win. Depends on B's fact model.*

SFTP is an ssh *subsystem*: no shell needed, runs over the existing
ControlMaster, returns typed attributes rather than parsed command output. On a
host whose login shell is cmd.exe it is the only route to file operations that
exists. On a POSIX host it is arguably the *better* route.

### Platform detection, free, in the handshake

No filesystem archaeology needed. `SSH_FXP_REALPATH` on `"."` — the first call
any client makes — answers the platform question structurally:

| Platform | `realpath(".")` returns |
|---|---|
| Linux / BSD | `/home/mike` |
| macOS | `/Users/mk31` |
| **Windows OpenSSH** | `/C:/Users/mk31` — the leading-slash-drive-letter form is unmistakable |

This is *positive* identification rather than inference from failure, and costs
nothing beyond a call already being made. Free even earlier: the `VERSION` packet
carries the server's extension list (`posix-rename@`, `statvfs@`, `hardlink@`,
`limits@`…), and the Win32 port's set differs from POSIX OpenSSH's — `statvfs` is
inherently POSIX. **Verify the exact Windows list empirically.**

Marker files become a confirmation tier, distinguishing *which* POSIX. Prefer the
readable ones:

- `/proc/version` — one stat proves Linux; a short read gives kernel and distro
- `/System/Library/CoreServices/SystemVersion.plist` — macOS, with version
- `/COPYRIGHT` — FreeBSD

### Is the subsystem available, without paying for a channel?

SSH has no subsystem capability negotiation. You request one; it opens or returns
a channel failure. There is nothing to query in advance. But it resolves anyway:

| Host | Known in advance? | Resolution |
|---|---|---|
| POSIX, shell channel up | **free** | Add a line to `probeScript`: grep `/etc/ssh/sshd_config` for `Subsystem.*sftp`, or look for `sftp-server` under `/usr/lib/openssh`, `/usr/libexec`, `/usr/libexec/openssh`. Zero extra channels. |
| Non-POSIX, no shell channel | **impossible** | Also unnecessary — the shell probe already failed, so SFTP is the only remaining option. Nothing left to optimise. |

The prior is strong enough to act on regardless: `Subsystem sftp` ships enabled
in stock Windows OpenSSH (confirmed: `sftp-server.exe`, 10.0p2) and in every
mainstream Linux distribution. Policy is *assume, attempt, cache the negative* —
exactly B's sticky-fact machinery. Being wrong costs one channel open, once.

### The open question, and why it isn't blocking

Whether opening the sftp subsystem costs an MFA push is environment-specific and
untested. If the host runs `login_duo` as a `ForceCommand`, OpenSSH applies that
to subsystem requests too, so sftp may be MFA'd *or* outright broken. If it's
`pam_duo` at authentication time, it shouldn't fire at all on an
already-authenticated master — the observed failure mode identifies which flavour
is in play.

So the open order is built as a **single policy switch, not a structural
assumption**. Test on a Duo host once it works, then flip.

| If SFTP is free | If SFTP costs a push |
|---|---|
| Open it **first**. More likely to succeed than `sh -s`, more informative when it does, and it performs the actual file work. Shell channel then opens lazily, only for `exec` / `grep` / `search`. | Shell channel stays primary; SFTP opens lazily only when the shell axis is down. POSIX hosts never pay for it; non-POSIX hosts pay one extra push and gain the entire file suite. |

### Implementation

`pkg/sftp` normally wants an `*ssh.Client` from `x/crypto/ssh` — which would
abandon ControlMaster reuse and the whole MFA-saving design. The escape hatch is
`sftp.NewClientPipe(rd, wr)`, wired to the pipes of
`ssh -S <sock> -oControlMaster=no … -s sftp`. Pure Go, one new dependency, master
reuse preserved.

### Sequence

1. Add sftp-server detection to the existing probe script — free on every host
   with a working shell channel. — `internal/sshmux/probe.go`
2. An `SftpAxis` beside `ShellAxis` in `HostFacts`, carrying the realpath result,
   path style (posix/windows), and the advertised extension set.
   — `internal/sshmux/facts.go`
3. The SFTP client: subsystem open over the mux slave via `NewClientPipe`,
   handshake, `realpath(".")`, classify path style, cache into facts. Dead-client
   handling mirrors the shell channel — never reopened silently.
   — `internal/sshmux/sftp.go`
4. Route `file_read`, `file_write`, `file_stat`, `directory_list`, `file_upload`,
   `file_download` through the SFTP axis when the shell axis is down.
   **`resultVia` needs a route argument** — it currently hardcodes
   `controlmaster → "channel"`, and an SFTP-backed op wants `via: "sftp"`.
   — `internal/mcpserver/tools_remote.go`
5. Extend `availability(facts)` to merge both axes, so a Windows host reports
   file tools available and `exec`/`grep`/`search` unavailable — the honest split.
   — `internal/mcpserver/capability.go`
6. Path translation for Windows targets: `/C:/Users/…` on the wire versus
   `C:\Users\…` as the user writes it. Normalise at the boundary and state the
   convention in the tool descriptions.
   — `internal/mcpserver/tools_remote.go`
7. The open-order policy switch, defaulting to shell-first until the Duo result
   is known. — `internal/sshmux/facts.go`

### The larger prize (deliberately not in this pass)

SFTP has **no flavour variance**. No `stat -c` vs `-f`, no `base64 -d` vs `-D`,
no `find -printf` fallback, no shell quoting, and real atomic rename via
`posix-rename@openssh.com`. Attributes arrive typed instead of parsed.

So on POSIX hosts it could retire most of `probe.go`'s behavioural capability
apparatus for file operations, leaving the shell channel to do what only it can.
That's a simplification of the current model rather than an addition — but it
changes behaviour of hosts that currently *work*, so bundling it with a change
affecting only hosts that currently *don't* would risk the working fleet for a
refactor. Revisit once C is proven.

---

## Workstream D — an out-of-band activity log

*The transparency fix. Independent of A, B and C.*

aish's premise is that both parties see everything in the shared terminal.
Out-of-band operations are the deliberate exception, and the consent gate
(`--oob`, the y/n/a prompt) governs *whether* invisible work happens. But once
granted there is no record of *what* happened, and `read_screen`/`read_output`
cannot help by definition — those operations never touch the terminal.

That is the one real hole in the transparency model.

The motivating case is real: an AI client attempted `sudo` through the OOB
channel. On a host with `NOPASSWD` that would have executed silently with
nothing to tell the human. The escalation guard now refuses it, but the general
shape of the problem — invisible work leaving no trace — remains.

A second use case is concurrent AI clients. One person driving two assistants
against a single session (which is supported today, and being done in practice)
has no way for either to see what the other did out of band.

### What each entry holds

Timestamp, **which client**, tool, route (`channel` / `local` / `sftp` /
`in_band`), host, the identifying argument (path or command), and the outcome
(exit code, bytes, error).

Client attribution is what turns a merged blur into something useful —
"quick wrote /etc/x" versus "claude read /var/log/y". The session already knows
it: grants bind to the MCP `clientInfo` name, and `peercred.go` has the
kernel-verified pid.

**Never log file contents.** Paths, byte counts and version tokens only. An
in-memory record of every file the AI touched is useful; an in-memory copy of
their *contents* is a secret store nobody asked for. Exec command lines are
logged in full, since that is the point.

### Shape

Monotonic sequence numbers with cursor-based reads, mirroring `read_output`, so
"what has the other client been doing" is an incremental poll rather than a
re-dump — and so it matches an idiom the tool API already uses. Memory-only and
bounded (a few hundred entries), consistent with grants.

Cross-session viewing falls out for free: every tool already takes `session`, so
`oob_log{session: "alloy-server"}` routes through the existing forwarding
middleware.

### Where to instrument — one place, not twelve

The obvious approach is a `c.logOOB(...)` call in each routed handler. Reject
it: that is ~12 call sites to keep in sync, and an audit feature that can be
*forgotten at a call site* is worth much less than one that cannot.

Use a **receiving middleware**, alongside `connAuthMiddleware` and
`crossSession`. It sees tool name, arguments, result, timing and client identity
in one place, and a tool added later cannot bypass it. It does not know the
route directly — that is decided inside the handler — but it does not need to:
every routed result already carries `via`, so visibility can be read off the
result the middleware is already holding.

Log every call, tag each with its `via`, and let the *views* filter. The human
view shows invisible operations by default, since visible ones are already on
screen.

### Surfaces

- **Tool** `oob_log` — cursor-based, cross-session capable.
- **Menu** `Ctrl-]` → `l` — prints recent entries through the console
  (`session/console.go`), never through the PTY.
- Possibly a status-bar counter (`oob: 12`) as an ambient hint.

A per-operation `Notify` would be the wrong shape. The PSK "recognized client"
notice already spams the terminal when a host restarts the proxy frequently; a
per-op notice would be worse. **Pull, don't push.**

### Sequence

1. `oobLog` type: bounded ring, monotonic sequence, `Entries(after int64)`.
   — `internal/mcpserver/ooblog.go`
2. Logging middleware registered after `connAuthMiddleware` (so the client is
   known) and after `crossSession` (so forwarded calls are recorded at the
   session that executes them, not the one that relayed them).
   — `internal/mcpserver/server.go`
3. `oob_log` tool with cursor semantics. — `internal/mcpserver/tools.go`
4. `Ctrl-]` → `l` menu entry rendering recent invisible operations.
   — `cmd/aish/main.go`

### Caveats to state plainly in the docs

It records **what was asked and what came back, not ground truth on the host**.
A buggy or bypassed path could act without logging. That is the same status as
the sudo guardrail and the tool annotations — call it an audit trail, never
imply tamper-evidence.

### Bonus

It doubles as a **coordination** channel: an assistant that can see another
wrote a file thirty seconds ago can avoid clobbering it.

## Bugs found en route

Independently real; several shippable on their own.

| | Defect | Consequence |
|---|---|---|
| live | `in_band` reports `file_read`, `file_write`, `exec` as available on any remote | Those fallbacks run through `RunSentinel`, which is pure POSIX. On cmd.exe it's garbage. Only `run_command` genuinely works — the availability map is a wrong answer, not a degraded one. |
| live | `probe_host` documents itself as the "explicit reset button" | It calls `EnsureProbed`, which short-circuits on cache — so it resets nothing. The `force` flag makes the docstring true. |
| live | Positive capabilities die with the channel on timeout | `target_confidence` silently regresses `same` → `unknown`, re-arming the one-time write confirmation the user already answered. |
| live | `CachedCapabilities` takes `chMu`; `openChannel`'s `cmd.Start()` runs under it | `session_status` can already stall behind a channel open today. |
| ux | `[p]` is dialect-blind | Offered on any remote, emits bash/zsh unconditionally. Observed twice: `'__aish_osc7' is not recognized` in cmd, then a `ParserError` in pwsh. It also can never help — on a non-POSIX host `target_confidence` is structurally pinned at `unknown`, because it derives from a probe that cannot run. |

---

## Decisions (settled)

| Decision | Outcome |
|---|---|
| Order of work | **A first.** Silent corruption outranks everything. A is independent; C depends on B. |
| Linearize globally or per call site | **Global.** Divergence between `read_output` and `run_command` over the same range would be its own bug report. Bounded by property tests, not a flag. |
| May the passive screen hint suppress availability? | **No** — suppress on probe evidence only; the screen hint drives the note. Advisory evidence doesn't change state. |
| SFTP | **Adopted** as workstream C. Open order built as a policy switch, pending the Duo push test. |

---

## Scope boundary

**No native Windows port.** Every defect here lives in aish's *interpretation* of
a byte stream and its *self-description* — both Linux code, both testable on
Linux with no Windows involved. The WSL → ssh → Windows path already delivers a
working shared terminal; it's the reading of that stream that needs work.

**No PowerShell OSC 133 integration yet.** It's the change that *looks* like
making Windows first-class, and it would buy exit codes and clean C→D framing —
but it doesn't fix what actually breaks the AI's reasoning. Line fusion survives
it, because `StripEscapes` runs on the osc133 path too. Polish it after the
stream can be trusted.

---

## Build and test loop

Go isn't on PATH on the Windows side, but the WSL toolchain works directly
against the mounted repo — verified, full suite green:

```sh
wsl -d Ubuntu -- bash -lc 'cd /mnt/c/Users/mk31/mcp/aish && go test ./...'
wsl -d Ubuntu -- bash -lc 'cd /mnt/c/Users/mk31/mcp/aish && go vet ./internal/term/'
```

`internal/term` and `internal/framing` currently have **no test files** — which is
exactly where workstream A adds them. All of A is pure Go, unit-testable without a
PTY. Step 4 (`afterEcho`) is the one wanting live-session validation, per the
`script` + FIFO harness in CLAUDE.md.
