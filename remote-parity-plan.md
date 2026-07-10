# Plan: native-tool parity for aish remote hosts (v2)

> Status: implemented on branch `remote-parity` (phases 0–5). Probe + host
> awareness, three-way divergence enforcement, atomic conditional writes with
> version tokens, `file_patch`, `file_grep`/`file_search`, and line-numbered
> `file_read` are landed and tested. The optional Claude skill (phase 5) is not
> built.


Goal: when the shared session is inside SSH, give the AI tools that behave like
its native local file/exec tools — search, atomic edits, patch, staleness
safety, host awareness — running on the remote's own coreutils over the
persistent OOB channel. No deployed binary; no file shuttling; nothing
installed on the remote.

Builds on `cf52832` (native host file primitives). Answers `file-tools.txt`
#3 (deploy/probe status), #4 (host awareness), #5 (atomicity/races), #6
(use-what's-there). v2 incorporates Codex's plan review (`codex-review.txt`).

## Guiding constraints (unchanged invariants)

- Run commands on the remote; only results travel back. Never copy the tree.
- Never install tooling on the remote. Use what's there; degrade or error
  precisely when it's missing.
- New native-style primitives are OOB-only: they refuse the visible `in_band`
  route (like `file_edit`/`file_stat` today).
- All routing goes through `route()`; results keep `via` + `host`.
- `session_status` must never open a channel (MFA risk): it reports cached
  capabilities or `unknown`, never probes on demand.

---

## Phase 0 — Capability probe + host awareness (foundation)

The OOB channel is one long-lived `sh -s` per host per session; probe once on
open and cache on the channel.

**Labeled (not positional) probe** — missing commands must not drift parsing:
```sh
printf 'uname=%s\n'        "$(uname -sm 2>/dev/null)"
printf 'user=%s\n'         "$(id -un 2>/dev/null)"
printf 'hostname=%s\n'     "$(hostname 2>/dev/null)"
printf 'pwd=%s\n'          "$(pwd 2>/dev/null)"
printf 'rg=%s\n'           "$(command -v rg 2>/dev/null)"
printf 'sha256sum=%s\n'    "$(command -v sha256sum 2>/dev/null)"
printf 'shasum=%s\n'       "$(command -v shasum 2>/dev/null)"
printf 'mktemp=%s\n'       "$(command -v mktemp 2>/dev/null)"
printf 'grep_version=%s\n' "$(grep --version 2>/dev/null | head -1)"
```

**`Capabilities`** (cached on the channel): `OS`, `Arch`, `Hostname`, `User`,
`Cwd`, `HasRg`, `Hasher` (`sha256sum|shasum|none`), `GrepFlavor`
(`gnu|busybox|bsd|unknown`), `HasMktemp`. Empty/non-POSIX `uname` → channel
`unsupported`; native primitives return a clean "unsupported on this host"
error (Windows/appliance boundary).

**Host awareness in `session_status`** (from cached caps + OSC7):
- `interactive_host` — where the tty is (OSC7 / tracker).
- `oob_host` — where the OOB channel terminates (`ci.Host`).
- `target_confidence` — `same | unknown | mismatch`.
- Remote `os`/`arch`/`user`/`hostname` when cached; else omitted/`unknown`.
- Never opens the channel. A real op (or an explicit future
  `remote_capabilities` tool) does the first open.

**Tests:** probe parser against GNU/BusyBox/BSD sample outputs; missing-command
lines yield empty values, not drift.

---

## Phase 1 — Host-divergence safety (protects tools that already ship)

The jump-box case (tty on host B, OOB channel on host A) can make the AI write
to the wrong host. `file_write`/`file_edit` already exist and already carry
this risk, so this lands early.

**Three-way policy keyed on `target_confidence`** (agreed with Codex):
- `same` (MATCHED): allow.
- `mismatch` (detected divergence): **fail closed** for mutating tools; **warn**
  for read-only tools.
- `unknown` (UNCERTAIN — OSC7 absent, nested ssh the shim didn't see): read-only
  proceed; mutating is gated by an **interactive confirmation fallback** (not
  "allow uncertain writes" — the safety boundary stays explicit): a one-time
  console confirm — a blanket refuse would break OOB writes on the many hosts
  that never emit OSC7. The confirm is
  **scoped narrowly** (per channel/session/host-token, NOT a global
  "trust-forever" switch), with an explicit message:
  `"Cannot verify the interactive shell is still on HOST. Proceed with OOB write to HOST? y/n"`.

This is an **initial policy, not a permanent UX commitment** — later we can raise
confidence via prompt integration, user-configured trusted hosts, or better
cwd/host reporting, and revisit the confirm.

`mismatch` = OSC7 host present AND ≠ OOB `ci.Host`. `unknown` = can't
determine. `same` = they agree.

**Tests:** simulated OSC7≠OOB host → mutating tool refused, read-only warned;
OSC7 absent → mutating warned-but-proceeds.

---

## Phase 2 — Version tokens + atomic conditional writes (the "hashes" ask)

Approximate the two native guarantees: atomic replace, and "can't silently
clobber a file changed since you read it" (optimistic concurrency / ETag —
native fingerprints, it doesn't lock).

**Version tokens** — `file_read`/`file_stat` return:
```json
{ "version": "sha256:abc123…", "version_kind": "sha256" }
```
- SHA-256 when the remote has a hasher; else `mtime-size:<mtime>:<size>` with
  `version_kind: "mtime-size"` — clearly labeled weak, never called a hash.

**`if_match`** on `file_write`/`file_edit`/`file_patch`:
- Optional at the API level; descriptions strongly encourage passing the
  `version` from the last `file_read`/`file_stat`.
- `file_edit`/`file_patch` use it internally/automatically for their
  read→modify→write (they already read first), closing aish's own TOCTOU.
- If the host has no hasher and no `if_match` token is derivable, `if_match`
  is rejected with a clear message rather than silently ignored.

**Atomic replacement path** — rewrite the OOB write (`sshmux.WriteScript` /
`writeOOBFile`): base64 → temp file *in the target's own directory* → preserve
mode → conditional swap in ONE remote command:
```sh
if [ "$(sha256sum < PATH | cut -c1-64)" = "$EXPECTED" ]; then
  mv -f "$TMP" PATH          # POSIX rename: atomic within one filesystem
else
  rm -f "$TMP"; echo STALE; exit 1
fi
```
Temp idiom (`mktemp` vs `$PATH.aish.$$`) and mode-preservation (`chmod
--reference`, GNU-only → `stat`+`chmod` fallback on BSD) chosen from the probe.

**Explicit semantics decisions (per review #4):**
- Regular-file replace → atomic temp+rename.
- `append=true` → keep direct append; **documented non-atomic** (temp+rename
  can't append without a full read-rewrite).
- **Symlink target → reject writes initially** with a clear error (rename-over
  would replace the link, not follow it as native does). Follow-target
  behavior is a deliberate later addition.
- Preserve **mode** at minimum. Owner/group/ACL/xattr preservation is later
  and documented as not-preserved for now.
- Cross-fs `mv` degrades to copy+unlink (non-atomic) — mitigated by keeping
  temp adjacent to the target; documented. NFS caching can weaken the
  staleness check; documented.

**Wire into existing tools:** `file_edit` and `file_write` adopt the
conditional atomic path. This hardens shipped code (today's `file_edit` has a
read→replace→write TOCTOU and a truncate-then-fill non-atomic write).

**Tests:** stale `if_match` → rejected, file untouched; concurrent writer →
STALE; mode preserved across replace; symlink write → rejected; append still
works (non-atomic); binary base64 round-trips; mtime-size fallback path.

---

## Phase 3 — `file_patch` (Codex-style edit parity)

Unified-diff patching, applied in Go so no remote `patch`/python/helper.

- Tool: `file_patch`; args `path`, `patch` (unified diff), optional `if_match`.
- One file per call initially.
- Read remote file over OOB → apply hunks in Go → write back via the Phase 2
  conditional atomic path.
- Mismatch → useful error with nearby context (which hunk, expected vs found).
- Description divides it from `file_edit`: `file_edit` = single exact-match
  replacement; `file_patch` = multi-hunk unified diff.

**Tests:** clean apply; context-mismatch error; multi-hunk; `if_match` stale →
rejected; CRLF/trailing-newline handling.

---

## Phase 4 — Search primitives

Two tools mirroring the native ones. Descriptions state **best-effort,
backend-dependent** — no promise of exact Grep/Glob equivalence.

### `file_grep` (≈ Grep) — content search
- Args: `path` (absolute), `pattern`, `include?`, `max_results?`,
  `ignore_case?`.
- Result: `[{path, line, text}]`, `truncated`, `via`, `host`.
- Backend from probe: `rg` (fast, `.gitignore`-aware, native-like) → GNU
  `grep -rInZ` (NUL-framed filenames) → BusyBox/BSD `grep -rIn`. No grep/rg →
  actionable error ("use exec").
- **Bounds:** regular files only on the `find`/grep fallback; caps on both
  bytes and result count; channel timeout; avoid descending `/proc`/network
  mounts where practical, else document best-effort.

### `file_search` (≈ Glob) — find files by name
- Reuse `directory_list`'s `find -printf` + NUL parsing without
  `-maxdepth 1`, adding `-name`/`-path`/`-type`.
- BusyBox `find` lacks `-printf` → `find -type f` (names only) per probe.

**Both:** OOB-only, absolute `path`, `max_results` + `truncated`, timeout-bound.

**Local route:** Go `regexp`/`filepath.Walk` (no hard local `rg`/`grep`
dependency); dialect documented as backend-dependent.

**Tests:** grep hit/miss, include filter, truncation, binary skip, NUL-in-name;
find by name/type; busybox fallbacks.

---

## Phase 5 — Polish

- **Line numbers without contaminating edits:** keep raw `content` as primary;
  add optional `numbered_content` (or `lines: [{line, text}]`) so models never
  feed line numbers back into `file_edit` old_text.
- **Tool description pass:** anchor each remote tool to its native counterpart;
  state OOB-only, best-effort, and the `if_match` encouragement.
- **(Optional) Claude skill:** `SKILL.md` teaching the remote workflow (native
  tools stay local past an SSH boundary; check `host`/`target_confidence`).
  Claude-only; MCP instructions + descriptions remain the cross-client
  foundation (Codex has no skills system).

---

## Sequencing

0. Capability probe + cached-cap exposure in `session_status`.
1. Host-divergence safety (protects already-shipped mutating tools).
2. Version tokens → conditional atomic replacement → wire into
   `file_edit`/`file_write` (hardens shipped code).
3. **Then patch OR search, by priority** (see open decision below).
4. Polish + optional skill.

Rationale: probe is foundational to everything; divergence + atomic/version
harden code that already ships, so they precede new surface area. Patch and
search are independent of each other.

## Decisions settled (Claude + Codex converged)

- **Patch before search** (after Phase 2). Claude leaned search-first; Codex
  leaned patch-first for edit parity. Settled on patch-first — it reuses the
  Phase 2 hash/atomic foundation directly, and search follows.
- **Divergence policy** is the three-way model in Phase 1 (MATCHED allow /
  MISMATCH fail-closed / UNCERTAIN scoped one-time confirm), an initial policy,
  not a permanent UX commitment.

## Decisions settled by the review

- Local regex: **no hard local `rg`/`grep` dependency** — Go locally, remote
  backend from probe, dialect documented. (Reverses plan v1.)
- `if_match`: optional at the API; strongly encouraged in descriptions;
  internal/automatic for `file_edit`/`file_patch`.
- Hasher fallback: `mtime+size` only as a **labeled weak** version token; if
  even that is unavailable, reject `if_match`.
- Staleness token is named `version`/`version_kind`, never "hash".
- Atomic write: regular-file replace atomic; append non-atomic (documented);
  symlink writes rejected initially; mode preserved, owner/ACL/xattr later.
