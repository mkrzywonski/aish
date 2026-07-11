# Remote-parity test scenarios

Manual/semi-manual scenarios to exercise the OOB file/exec toolset and surface
easily-triggered bugs. Run against a
real remote reached by SSH *through* an aish session — that exercises the actual
shim → ControlMaster → persistent `sh -s` channel path, not a shortcut.

Recommended test beds:
- **Debian/Ubuntu VM or LXC** — GNU userland: happy path, missing-tool, rg
  toggle, divergence, probe lifecycle, consent.
- **Alpine (BusyBox) with sshd** — the fallback branches (`stat -c` but no
  `find -printf`/`grep --null`/`head -z`).
- **FreeBSD or macOS** (if available) — BSD `stat -f`, `base64 -D` branches.
  These cannot be faithfully faked on Debian (real GNU stat has no `-f`).

Driving the tools without an AI harness (see CLAUDE.md):
`./aish client --session <id|name> <tool> '<json-args>'` and
`./aish client --session <id|name> session_status '{}'`. Pipe JSON through
`python3 -m json.tool` to read it.

---

## Mechanism notes (read first)

### 1. You usually can't `apt remove` the tool — rename its binary instead
`coreutils` (base64, stat, sha256sum, head, mv, chmod), `findutils` (find), and
`grep` are *essential* packages; apt refuses or cascades. To break a tool
reversibly, drop its binary off the OOB shell's PATH by renaming it:

```sh
# on the remote — break, then restore
sudo mv "$(command -v base64)" /root/base64.hidden
sudo mv /root/base64.hidden /usr/bin/base64
```

`ripgrep` is the one package to genuinely `apt install ripgrep` /
`apt remove ripgrep` — it is non-essential and toggles the `file_grep` backend.

### 2. The probe is cached for the channel's lifetime
Once a host is probed, breaking or installing a tool does **not** change
availability until the channel reopens. To re-detect: exit and re-`ssh` the
host, or wait out `ControlPersist=60s` after the last op, *then* `probe_host`.

> Design flag: consider `probe_host {"refresh": true}` to bust the cache and
> re-probe the live channel (no reconnect, no extra MFA push). It would collapse
> the break/re-enable loop from three steps to one and matches "reset button"
> semantics. Not yet implemented.

### 3. One Debian VM can't cover the non-GNU fallbacks
Debian is GNU, so it exercises the happy path plus "tool missing." The BusyBox
and BSD fallback branches need Alpine and BSD/macOS respectively (see test beds).

### Break/restore helper (drop on the remote)
```sh
# ~/break.sh  — usage: break.sh hide <tool> | break.sh show <tool>
set -e
STASH=/root/.aish-stash; mkdir -p "$STASH"
case "$1" in
  hide) p="$(command -v "$2")"; sudo mv "$p" "$STASH/$2"; echo "hid $p" ;;
  show) sudo mv "$STASH/$2" "/usr/bin/$2"; echo "restored /usr/bin/$2" ;;
  *) echo "usage: break.sh hide|show <tool>"; exit 2 ;;
esac
```
Remember to reconnect the ssh (or wait 60s) between a change and the next probe.

---

## A. Probe lifecycle (Phase 7 core)

| # | Setup / action | Expected |
|---|---|---|
| A1 | Local session, `session_status` before any ssh | `oob_via: local`; every `oob_tools` state `available` |
| A2 | `ssh vm`, then `session_status` (do **not** probe) | `oob_via: controlmaster`; every tool `unknown`; and **no new ssh connection** appears on the VM from this call (verify: `ss -tnp` / `journalctl -u ssh`) — `session_status` must never open a channel |
| A3 | `probe_host` | `probed: true`; tools resolve to `available`; `remote_capabilities` populated (os / pkg_mgr / hasher); `target_confidence: same` |
| A4 | `session_status` again | tools `available` from cache; still **no new** channel opened |
| A5 | Fresh `ssh vm` session; call `file_read` directly, no probe first | auto-probes and succeeds — proves `unknown` is not a tollgate |
| A6 | `ssh vm2` (second host); `session_status` | new host reads `unknown` again (per-host cache), not inherited from vm |

## B. Capability matrix & fallbacks
Reconnect ssh (or wait 60s) after each change, then `probe_host`.

| # | On the remote | Expected |
|---|---|---|
| B1 | `apt install ripgrep` | `file_grep` works; `remote_capabilities.has_rg: true` (rg honors `.gitignore`) |
| B2 | `apt remove ripgrep` | `file_grep` still `available`, falls back to grep; results still correct |
| B3 | hide `base64` | `file_read`/`file_write`/`file_edit`/`file_patch`/`file_upload`/`file_download` → `unavailable` with `missing: base64...` and `install: apt-get install -y coreutils`; **`exec` stays available** |
| B4 | hide `find` | `directory_list` + `file_search` → `unavailable` (find hint); `file_read` still available |
| B5 | hide `grep` (and no rg) | `file_grep` → `unavailable` (ripgrep-or-grep hint) |
| B6 | hide `sha256sum` | tools stay available; `if_match` staleness can't be auto-derived — writes still work, reads return no `version` |

## C. Graceful failure

- **C1 — non-POSIX host / fast-fail.** Create a user whose login shell never
  runs `sh`:
  ```sh
  printf '#!/bin/sh\necho "=== restricted device ==="\nexec sleep 1\n' | sudo tee /usr/local/bin/fakeshell >/dev/null
  sudo chmod +x /usr/local/bin/fakeshell
  sudo useradd -m -s /usr/local/bin/fakeshell probeuser
  ```
  `ssh probeuser@vm` through aish, then `probe_host` (or any OOB op). Must
  **fail in seconds** with the "didn't present /bin/sh" `probeOpenError`, not
  hang ~60s+. Time it with `time`.
- **C2 — silent-grep guard.** Probe first (grep cached available), *then* hide
  `grep`, then run `file_grep`. It should surface a real error (via the
  `@AISHRC@` marker), **not** report "no matches." (Exact bug Phase 6 fixed.)

## D. Wrong-host protection (divergence policy)

- **D1 — jump box / mismatch.** From the aish session `ssh vm`, then *inside*
  that shell `ssh othervm` (a hop the shim doesn't multiplex). Interactive tty
  is on `othervm` but the OOB channel targets `vm`: a **read** returns a
  `warning` (mismatch); a **write** **refuses** (fail-closed) with reconnect
  guidance.
- **D2 — alias, not a mismatch.** `ssh web` where `web` is a `~/.ssh/config`
  alias for the VM. `target_confidence` must be `same` (compared against the
  *probed hostname*, not the alias). A `mismatch` here is a bug.
- **D3 — unknown → one-time confirm.** A write to a host whose OSC7 tty host
  can't be read prompts once, then remembers (`confirmedTargets`) for the
  session.

## E. Consent / auth

- **E1 — ungranted probe prompts.** Start aish **without** `--oob`. `ssh vm`,
  then `probe_host` → should trigger the y/n/a OOB consent prompt on the
  terminal (probing *is* opening the invisible channel). `a` grants the session;
  `n`/timeout downgrades to in-band and `probe_host` reports `via: in_band` with
  the fallback note.
- **E2 — one push per channel.** If the VM is behind any per-session challenge
  (even a scripted PAM prompt), confirm the channel opens **once** and
  subsequent file ops don't re-challenge — the reason for the shared `sh -s`.

## F. Client identity in the approval prompt

The connection-approval prompt shows a **self-declared** identity next to a
**kernel-verified** peer: `"<declared>" [verified: <comm> (pid N, uid U)]`. This
is disambiguation, not access control — the trust boundary is the `0700` runtime
dir (same-uid). The goal is to avoid good-faith mistakes; the verified line is a
tripwire for a mismatch.

- **F1 — debug CLI, explicit identity.** `./aish client --session <id>
  --identity "My Test Harness" session_status '{}'` → prompt reads
  `"My Test Harness" [verified: aish (pid N, uid 1000)]`. The `--identity`
  string wins over auto-derivation.
- **F2 — debug CLI, auto-derived.** Same call without `--identity` → prompt
  reads `"aish debug CLI (launched by <parent-comm>)"` (e.g. `bash`), never a
  bare `aish-client`.
- **F3 — via the proxy.** Connect a real MCP client (Claude/Gemini/etc.) through
  `aish mcp-proxy`. The prompt's declared name should be the upstream TUI plus
  the proxy's launcher, e.g. `"gemini (via aish proxy launched by
  antigravity)"`, and the verified peer should be the proxy process (`aish`),
  **not** the AI product — that's expected (peercred sees the local relay).
- **F4 — cross-session forwarding.** From session A, call a tool with
  `session: "<B>"` before B has granted A. B's prompt should read
  `"aish session <A-name-or-id> (cross-session forwarding)" [verified: aish ...]`.
- **F5 — reconnect doesn't re-prompt.** After approving a client, drop and
  reopen its connection (proxy reconnect, or a second `aish client` call with a
  *reused* identity). It should authenticate via the nonce challenge with **no**
  second prompt. A *new* identity (fresh `aish client` process) prompts again.
- **F6 — the mismatch tripwire.** Approve nothing blindly: eyeball that the
  declared name and the `verified:` comm are consistent with what you launched.
  A declared `"claude"` next to `[verified: python3 (pid …)]` or an unfamiliar
  comm is the "wait, what?" signal — deny and investigate. (To see a deliberate
  mismatch, connect with a hand-rolled client that sends a misleading
  `client_description`; the verified peer still reports the real process.)
- **F7 — no peer creds → graceful.** The verified line is omitted (not faked)
  when creds are unavailable; the prompt then shows only the declared identity.
  (Only reachable off-Linux or on a non-Unix transport — informational.)

## G. Prerequisite-command breakage matrix

Systematic version of B/C: break **each** command an OOB primitive depends on
and confirm the dependent tools fail *gracefully* — clear, actionable, and fast,
never a hang, a silent wrong answer, or a cryptic trace. Break a command by
renaming its binary (`break.sh hide/show <tool>`), as root, since they live in
root-owned `/usr/bin`.

### Two channel states to test each break in
The probe is cached per channel, so *when* you break the tool decides which path
runs — test both:

- **Fresh probe (gated path).** Break, then force a fresh channel + probe:
  reconnect ssh, or drop the channel — a foreground `exec {"command":"exit"}`
  kills the `sh -s` and the next op reopens. A **probe-gated** tool then reports
  `unavailable` in `oob_tools` with `missing:` + `install:`, and `requireTool`
  refuses it *before running*. The clean path.
- **Stale probe (runtime path).** Probe while healthy (tool cached `available`),
  *then* break, then run the op on the still-open channel. The op attempts and
  must fail with a **clear runtime error** — surfaced by the atomic-write
  `) 2>&1` capture or the `@AISHRC@` exit marker — not a silent "no matches" or a
  false success. This is where regressions hide.

Restore (`break.sh show`) + reopen after each row and confirm recovery.

### Dependency reference (what needs what)
| Command | Probe key | Gated tools (refused pre-run) | Also used at runtime by |
|---|---|---|---|
| `sh` | — (channel) | all (channel won't open) | the persistent `sh -s` itself |
| `printf` | sentinel + probe | all (probe) | channel sentinel, probe lines |
| `uname` | `uname=` | all non-`exec` (→ Unsupported) | probe only |
| `base64` | `base64` | file_read, file_download | write/edit/patch/upload decode |
| `base64 -d`/`-D` | `base64d`/`base64D` | file_write, file_upload, file_edit, file_patch | write heredoc decode |
| `stat` | `statc`/`statf` | file_stat | directory_list, write mode-preserve, mtime-size CAS |
| `find` | `find` | file_search, directory_list | search/list producers |
| `grep` (no `rg`) | `grep`/`rg` | file_grep | grep backend |
| `rg` | `rg` | — (backend only) | preferred grep backend |
| `sha256sum`/`shasum` | `sha256sum`/`shasum` | **none** | sha256 `if_match` CAS on write |
| `mktemp` | `mktemp` | none | atomic-write temp (has fallback) |
| `mv` | — | none | atomic-write rename |
| `chmod` | — | none | atomic-write mode set |
| `head` | `headz` (behavioral) | none | grep/search cap, dir_list `-z`, probe pkg line |
| `cut` | — | none | sha256 CAS truncation |
| `dirname` | — | none | atomic-write temp path (has fallback) |

### Per-command expectations
| # | Break | Fresh-probe (gated) expectation | Stale-probe (runtime) expectation |
|---|---|---|---|
| G1 | `sh` gone | channel open fails fast: *"closed immediately … lacks /bin/sh; use run_command"* | live channel survives; reopen fails as above — **verified 2026-07-10** |
| G2 | `sh` → `cat` | probe fast-fails (~8s, two-phase): *"did not present a POSIX shell … non-Unix target"* | same on reopen — **verified 2026-07-10** |
| G3 | `base64` | file_read/write/edit/patch/upload/download → `unavailable`, `missing: base64`, `install: apt-get install -y coreutils`; **`exec` stays available** | op runs base64 in live channel → `base64: not found` surfaced as a runtime error, not a hang or an empty success |
| G4 | `stat` | file_stat → `unavailable` (needs stat); **directory_list may stay available** on GNU via `find -printf` + `head -z` — verify which branch; writes still succeed (mode-preserve → `chmod 644`) | file_stat runtime error; a write's mode-preserve silently falls back to 644 (still lands) |
| G5 | `find` | file_search + directory_list → `unavailable` (needs find/findutils); file_read still available | runtime error via `@AISHRC@`, not an empty "no results" |
| G6 | `grep` (rg absent) | file_grep → `unavailable` (ripgrep-or-grep hint) | **C2 regression guard:** real error via `@AISHRC@`, **never** "no matches" |
| G7 | `rg` removed | file_grep stays `available`, backend falls back to `grep`; results still correct | n/a |
| G8 | `uname` | fresh probe → `Unsupported: true` → every tool except `exec` `unavailable` (*"a POSIX shell (host not supported)"*); confirm `exec` still works | stale: no effect (cached) — documents Unsupported is probe-time only |
| G9 | `mktemp` | no tool flips; **write still succeeds** via `$_p.aishtmp.$$` fallback — verify the file lands with the right mode | same |
| G10 | `dirname` | write still succeeds (mktemp arg breaks → same fixed-name fallback) | same |
| G11 | `mv` | no tool flips; a write **fails** with mv's error via `) 2>&1` (perm/not-found) — clear error, temp cleaned up, **no partial file at target** | same |
| G12 | `chmod` | write **lands** but mode may be wrong (chmod is `2>/dev/null`) — flag whether a silent mode miss matters | same |
| G13 | `head` | probe pkg detection breaks (`pkg_mgr` empty → install hints blank — note the degraded hint); file_grep/file_search pipe through `\| head -c` — verify they error clearly, not truncate/empty as success | same |
| G14 | `sha256sum`+`shasum` | **suspected non-graceful:** no tool flips and `file_read` still returns a Go-computed `sha256` version, but a later `file_write`/`file_edit` carrying that `if_match` runs the remote CAS `sha256sum < f`, gets an empty `_cur`, and exits **92 → reported as "file changed (stale)"** though the file is unchanged | same |
| G15 | `cut` | same false-stale risk as G14 (CAS `cut -c1-64` fails) — confirm and fold into the same fix | same |

### Suspected gaps to confirm (found by code read, not yet observed)
- **G14 / G15** — a broken hasher or `cut` turns a healthy conditional write into
  a misleading "file changed" (`exit 92`): `file_read` advertises a `sha256`
  version the write path then can't verify. Candidate fix: when `Hasher=="none"`,
  don't emit a sha256 `if_match` (use `mtime-size`), or report "cannot verify
  version on this host" distinctly from "stale."
- **G12 / G13** — `chmod` and the `head -c` cap are the silent-ish paths; eyeball
  for wrong-but-successful results.

---

## Notes / findings
_(record surprises here as you run the plan)_

- **G14 CONFIRMED BUG (2026-07-10, debian VM).** Hid `sha256sum`+`shasum`, then
  `file_edit` on an unchanged file → `"the file changed since it was read
  (if_match mismatch); re-read it and retry"`. The file was untouched; the real
  cause (no remote hasher for the auto-derived sha256 CAS) is swallowed, and the
  advised "re-read and retry" would loop forever (each retry re-hashes fine in Go,
  the remote CAS keeps failing). `file_read` still advertises a Go-computed
  `sha256` version the write path can't verify. Restoring the hashers made the
  same edit succeed. Fix candidates: (a) casBlock detects a missing hasher and
  exits a distinct code → "can't verify version on this host" (not "stale");
  (b) auto-if_match uses `mtime-size` when `Hasher=="none"`. G15 (`cut`) is the
  same class.

- **Live run 2026-07-10 (debian VM, GNU). All graceful except G14:**
  - G3 `base64` — both states graceful. Stale/runtime: `file_read` fails
    `exit 127: base64: not found`. Fresh/gated: reopen+probe → read/write family
    `unavailable` (`missing: base64`, `install: apt-get install -y coreutils`)
    while `exec`/`grep`/`search`/`stat`/`directory_list` stay available; the call
    is refused pre-run with the install hint.
  - G6 `grep` (rg absent) — silent-grep guard holds: `file_grep` fails
    `(exit 127): grep: not found` via `@AISHRC@`, not "no matches".
  - G8 `uname` — probe → `Unsupported: true`; all tools but `exec` `unavailable`;
    `exec` still works. Minor finding: a missing benign `uname` over-disables —
    the shell is POSIX and base64/stat/find/grep still work, but OS=="" flips
    Unsupported and turns off every file tool. Consider decoupling Unsupported
    from `uname` presence (the real non-POSIX signal is the missing sentinel).
  - G9 `mktemp` — graceful fallback: `file_write` succeeds via `$_p.aishtmp.$$`,
    correct mode `0644`, no stray temp.
  - G1/G2 `sh` — verified earlier. G15 `cut` — not run separately; same root
    cause as G14.

- **Non-POSIX device (C1) live 2026-07-10.** Stood up a `netdev` user on the VM
  whose login shell is a fake network-device CLI (`fakeshell`: interactive
  `fakesw>` prompt; `-c "<cmd>"` prints `% Invalid input` and exits 1).
  `ssh netdev@vm` through aish → the human sees the FakeOS CLI; **all OOB tools**
  (`probe_host`, `file_read`, …) fast-fail with *"closed immediately (…may not
  allow a shell session or lacks /bin/sh); use run_command instead"*, and
  **`run_command` works** (`framing: idle`, `show version` → `FakeOS 0.1…`).
  Nuance: a device that **rejects-and-disconnects** lands in "closed immediately"
  (ssh returns fast → dead channel), whereas one that **holds the connection but
  never sends the sentinel** (the `sh→cat` stand-in) gets the *"non-Unix target
  (Windows, a network device, …)"* two-phase message. Both graceful; the exact
  wording depends on whether the far end disconnects or lingers.

- **Real Windows host live 2026-07-10.** `ssh Mike@<win>` (Windows 10 19045,
  OpenSSH server, cmd.exe default shell) through aish. All OOB tools
  (`probe_host`, `file_read`, …) fast-fail *"closed immediately (…lacks /bin/sh);
  use run_command instead"* (cmd.exe has no `sh`), and **`run_command` works**
  (`framing: idle`, `ver & echo %USERNAME%` → `Version 10.0.19045…, user=mike`).
  Confirms the non-Unix-target degradation on a genuine Windows box, not just the
  fake device. CRLF (`\r\n`) output handled fine.

- **Real macOS host live 2026-07-11 (Darwin arm64, Apple Silicon Mac mini).**
  First real exercise of the BSD/non-GNU fallbacks. Probe: `stat_c:false stat_f:true`,
  `find_printf:false`, `head_z:false`, base64 both flags, `has_rg:false`,
  confidence `same` (macOS zsh emits OSC 7). **Works:** `file_stat` (BSD `stat -f`),
  `file_read` (base64+sha256), `directory_list` on a real dir (`/Users/mike` — the
  `find -exec stat -f` portable path is correct), `exec`. **Two real bugs found
  (see [[aish-oob-fixes-todo]] Bug A/B):**
  - **A:** `find PATH -mindepth 1` lacks `-H`, so a symlinked dir root (`/etc`→
    `/private/etc`) → `find` treats it as a leaf → `directory_list`/`file_search`
    silently return empty. Add `-H`.
  - **B:** `searchRemote` hard-fails on any nonzero `find` exit; a permission-denied
    on one traversed subdir (`/private/etc/cups/certs`) discarded the valid
    `/private/etc/hosts` hit. Don't treat find's exit like grep's.
