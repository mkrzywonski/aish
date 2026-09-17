# Changelog

## 0.3.0 (unreleased)

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

Restart updated sessions and reconnect the MCP proxy/client to refresh schemas.
Remote line selection requires `head`, `tail`, and `wc`; byte reads keep their
existing prerequisites. This release does not change OOB consent or auth rules.
