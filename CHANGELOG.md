# Changelog

## 0.5.5

- Fix native Windows sessions where approving an AI client in aishwin never
  took effect: `aishwnd` deadlocked between the approval prompt and the
  console's client-count poll, so every approval timed out with "no response
  to the authorization prompt". Prompts in `aishwin` no longer stall other
  traffic while the dialog is open.
- Remote `exec background=true` now runs over the persistent out-of-band
  channel instead of opening a new SSH session per task, so on hosts with
  per-session MFA (e.g. Duo push) a background task no longer costs an extra
  prompt once the channel is open. The command runs detached under POSIX `sh`
  on the host, and `task_status` polls it over the same channel.
- **Output change:** `task_status` now returns a `state` (`running`, `done`,
  and for remote tasks `uncertain`, `starting`, `start_failed`, `draining`,
  `capture_failed`, `lost`, `expired`), plus `via`/`host`, `dropped_bytes`,
  `output_limited` and `warning`. A remote launch whose acknowledgment is lost
  returns state `uncertain` with its task ID rather than an error; poll it
  before running the command again. New `exec_background` entry in `oob_tools`.
- Remote task output (first 16 MiB) is kept in `/tmp/aish-task-*` on the host
  and is not yet cleaned up: those files, and any still-running job, remain
  after the session ends. At most 16 remote background tasks per session.
- `exec` and its results now state what can cost an MFA prompt. A remote
  out-of-band foreground call that reaches `timeout_ms` closes the shared
  channel without confirming the command stopped; it no longer silently
  reopens the channel to save oversized partial output.
- `make` builds `aish` and `aishwnd`, and `make install` installs both,
  refusing a pair whose version stamps differ or that cannot run here.

Upgrade `aish`, `aishwnd` and `aishwin` together, then restart sessions,
`aishwin` windows and the MCP proxy/client to refresh schemas. Remote
background tasks need `head -c`, `tail -c` and `base64` on the host. This
release does not change OOB consent or auth rules.

## 0.5.4

- Fix regex alternation with grep fallbacks and missing single-file matches on
  remote hosts. Report the search backend/dialect and incomplete results.
- Add OOB `file_read` line pagination through `start_line` and `limit`. Existing
  `offset` still means bytes; mixing byte and line positions is an error.
- Bound read responses, with a 16 KiB default source page and continuation
  metadata. Explicit `max_bytes` must be 1–262144. Larger files require paging.
- **Output change:** `line_numbers: true` returns only `numbered_content`.
  Callers needing raw text must omit numbering. Numbering at a nonzero byte
  offset is rejected; use `start_line` instead. Empty files still include the
  selected representation as an empty string.
- Return empty arrays for successful empty collection results. Missing search
  roots return errors rather than empty results.
- Correct proxy session-selection descriptions; multiple sessions still require
  an explicit target. Legacy proxy `--session` arguments now warn on stderr.
- Apply bounded line/byte pagination and empty-result fixes to native Windows
  sessions as well. Upgrade both `aishwin` and `aishwnd` together.
- Preserve current remote identity checks, SFTP byte-read fallback, and dynamic
  proxy tool discovery when integrating the usability fixes.

Restart updated sessions and reconnect the MCP proxy/client to refresh schemas.
Remote line selection requires `head`, `tail`, and `wc`; byte reads keep their
existing prerequisites. SFTP fallback supports byte pages only. This release
does not change OOB consent or auth rules.
