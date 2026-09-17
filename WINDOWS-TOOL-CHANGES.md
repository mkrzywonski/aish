# Windows tool usability changes (v0.5.4)

- `file_read` accepts `start_line` (one-based) and `limit` (line count). Existing
  `offset` still counts bytes; mixing line and byte positions is an error.
- Default source pages are 16 KiB, explicit `max_bytes` is bounded at 256 KiB,
  and serialized responses are bounded with pagination metadata. Continue with
  `next_line` or `next_offset` until `eof`. Limits are bytes, not token estimates.
- `line_numbers: true` returns only `numbered_content`. Omit it for raw `content`
  used in exact edits. Both preserve the selected empty representation for an
  empty file; version tokens still describe only complete-file reads.
- Native grep already supported alternation. Added regression coverage and
  backend/dialect metadata, fixed missing-root errors, and normalized successful
  empty search/session collections to `[]`. Empty directory listings were
  already arrays and now have explicit coverage.
- Proxy session descriptions explain explicit targeting with multiple sessions.

Update both `aishwin.exe` and the Linux/WSL `aishwnd`, reconnect the Windows
session, and refresh the MCP client's schemas. The daemon detects old peers
that cannot select lines and directs callers to update or use byte mode.
The Windows GUI keeps mirroring operations to the user as before.

The changed portable executor and daemon/wire tests run on Linux, and the GUI
cross-build uses `-H=windowsgui`. Native Windows tests are included in CI;
cross-compilation alone does not establish GUI/runtime behavior on Windows.
