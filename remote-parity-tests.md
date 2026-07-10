# Remote-parity test scenarios

Manual/semi-manual scenarios to exercise the OOB file/exec toolset (Phases 0–7
of `remote-parity-plan.md`) and surface easily-triggered bugs. Run against a
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

---

## Notes / findings
_(record surprises here as you run the plan)_
