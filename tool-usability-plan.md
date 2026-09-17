# AISH tool usability improvement plan

Research baseline: commit `9293571`; observations in `tool-usability-observations.md`.
Status: implemented in this checkout, with matching applicable Windows changes
in `/home/mike/aish`. The sections below retain the design and acceptance criteria.

Validation completed:

- Full Go tests and `go vet` pass in both repositories.
- Generated search commands run against Go, ripgrep, GNU grep, and BusyBox.
- File-read tests cover local/remote page parity, UTF-8 and binary byte
  reconstruction, complete-line pagination, output budgets, producer failures,
  status-marker truncation, and actual MCP serialization.
- An isolated PTY session through the aggregate proxy passed schema discovery,
  line reads, single-file alternation search, and session-status checks. After
  closing that session, the proxy served refreshed cached schemas and `[]` for
  the empty session list.
- Windows portable executor, daemon/wire, and MCP response tests pass; the GUI
  cross-build uses `-H=windowsgui`. CI includes native Windows tests.

Validation limits: this environment cannot authenticate noninteractively to
`localhost` over SSH, so remote shell selection is covered by executed GNU and
BusyBox scripts rather than a live authenticated SSH session. No native Windows
GUI runtime is available here. Nothing has been deployed or committed.

## Findings and decisions

| Observation | Evidence in this checkout | Recommended action |
|---|---|---|
| Reading by line is awkward | `fileReadArgs` only accepts byte `offset`/`max_bytes`; `limit` is absent | Add explicit line pagination without changing the meaning of `offset` |
| Numbering produces oversized results | `fileRead` includes both `content` and `numbered_content`; default read is 256 KiB and explicit `max_bytes` is unbounded | Return one representation, use smaller defaults, enforce a serialized response budget |
| Alternation silently misses matches | Both grep fallbacks omit `-E`; generated commands reproduce the failure | Use extended regex in grep fallbacks and test executed commands |
| Empty collections are `null` | Five public array fields can receive nil slices | Normalize successful collections to `[]` |
| Multiple sessions require a target | Proxy intentionally rejects ambiguity, but mirrored schema and README promise an attached default | Preserve routing behavior and correct descriptions/documentation |

A second search bug was reproduced: every remote search command omits `-H`.
For one explicit file, grep/ripgrep omit the filename, and AISH's parsers discard
the resulting records. Fix this together with alternation.

The original deployed backend and literal JSON arguments are unavailable. The
grep fallback reproduces the reported symptom, but this does not prove which
backend Claude used. The report also mentions `visibility`, `output_path`, and
`mode_note` behavior absent from this checkout; verify the deployed version and
wrapper during acceptance testing rather than assuming those APIs exist here.

## 1. Repair search correctness

Primary files: `internal/mcpserver/search.go`, `search_test.go`, and, if needed,
`internal/sshmux/probe.go`.

- Add `-E` to both grep fallbacks and `-H` to all remote search backends.
- Preserve shell argument quoting and `-e`/`--` handling.
- Document the shared regex subset: alternation, grouping, anchors, character
  classes, and escaped punctuation. Do not promise full PCRE or identical syntax
  across Go regexp, ripgrep's default engine, and grep's POSIX extended regex.
- Return compact `backend` and `regex_dialect` metadata so failures can be
  understood without inferring the engine from `via: channel`.
- Ensure a missing/unreadable search root is an error locally as well as remotely;
  current local walkers swallow root errors. Successful no-match searches remain
  successful. Keep best-effort handling of inaccessible descendants explicit.
- Preserve the distinction between no matches, invalid patterns, and execution
  failures. When a transport cap prevents knowing the producer's exit status,
  report an incomplete result rather than asserting complete success.

Acceptance tests must execute generated commands and parse actual output. Cover
local Go, ripgrep, GNU grep with NUL framing, and the colon fallback; exercise
BusyBox in an available CI/container environment. Include one-file and directory
targets, Claude's alternation examples, escaped dots and literal pipes, grouping,
case-insensitive matching, invalid regex, missing roots, include filters, limits,
and quoted filenames. Keep the colon fallback's filename ambiguity documented.
Existing command-substring tests pass despite both reproduced bugs.

## 2. Make file reading convenient and bounded

Primary files: `internal/mcpserver/tools_remote.go`, a new focused file-read
helper/test file, and line-read capability support in `internal/sshmux/probe.go`.

### Input contract

- Keep `offset` as a zero-based byte offset. Existing calls retain their meaning.
- Add `start_line` (one-based) and `limit` (number of lines). Either selects line
  mode; `limit` alone starts at line 1, and `start_line` alone defaults to 200 lines.
- Reject an explicitly supplied `offset` combined with either line parameter,
  including `offset: 0`; use optional/pointer fields to distinguish omission.
  The error should show the valid alternative: `start_line: 241, limit: 160`.
- Preserve byte mode when neither line parameter is present. `max_bytes` limits
  source bytes in either mode. Validate negative values and arithmetic overflow;
  reject explicit nonpositive limits rather than silently ignoring them.
- Recommend an initial default source budget of 16 KiB, a hard source limit of
  256 KiB, and a 64 KiB serialized MCP result budget. These are byte budgets,
  not guarantees about any client's tokenizer or context limit. Verify the
  defaults against the reported 29 KB reading workflow before release.

Example line read:

```json
{"session":"alloy","path":"/opt/alloy/example.py","start_line":241,"limit":160,"line_numbers":true}
```

### Output contract

- Return exactly one file representation: raw `content` by default, or
  `numbered_content` when numbering is requested. Omit the unused field; preserve
  an empty string for the selected representation of an empty file.
- Number against actual source line positions. Line mode supports numbering from
  any `start_line`; reject numbering at arbitrary nonzero byte offsets with
  guidance to use line mode, instead of silently ignoring the request.
- Preserve exact raw bytes/newlines when returning raw content. Do not insert
  truncation notices into content that may be copied into an edit.
- Add pagination metadata: `bytes_read`, `next_offset` for byte continuation,
  source line range and `next_line` for line pages, plus `truncated` and a reason
  when a line/byte/response budget limits the result. Define `eof` as actual
  source EOF, established using lookahead, not merely the requested page's end.
- Line mode returns complete lines, including an unterminated final line at EOF.
  Stop before a line that would exceed the budget. If the first requested line
  cannot fit, return an actionable error identifying its source byte offset so
  the caller can use existing byte mode. Do not create a non-progressing cursor.
- Byte mode retains base64 for non-UTF-8 data. Line mode rejects non-UTF-8 content
  with byte-mode guidance. Include boundary cases where a byte cap cuts a UTF-8
  character so valid text is not misclassified solely because of lookahead.
- Emit the existing whole-file SHA-256 token only when the complete file was
  read and returned. Never label a page hash as a whole-file version token.

### Implementation and transport

- Separate route-specific acquisition from shared pagination, rendering, and
  budgeting. Keep routing, consent, target checks, and safe editing unchanged.
- Make new line mode available on authorized local/remote OOB routes. Preserve
  existing visible byte-mode fallback; do not add new visible sentinel wrappers.
- On remote hosts, find the requested line's byte offset with a bounded-output
  prefix scan (`head -n ... | wc -c`), then select the page with existing
  `tail`/`head`/`base64` commands and one-line/one-byte lookahead. Scan the prefix
  on the remote host rather than transferring it. Probe required line operations
  and `wc`; lack of line-mode prerequisites must not disable existing byte reads.
- Capture read/selection failures explicitly: a successful final `base64` process
  must not conceal a failed file read. Treat pagination as a live-file read, not
  a multi-call snapshot; concurrent file changes can invalidate line positions.
- Measure the rendered, JSON-escaped MCP result, including base64 and numbered
  text. The pinned SDK mirrors structured output into a text content block;
  account for both and preserve client compatibility. Trim/re-render pages until
  they fit, with room for metadata/proxy notices. Do not remove the text block
  merely to reduce wire size without client compatibility evidence.
- Validate the existing in-band base64/framing limit with the new defaults so
  internal capture does not truncate the encoded transfer before decoding.

Tests: line ranges beyond line 1, limit-only calls, legacy byte offsets, invalid
mixed parameters, empty files, EOF boundaries, CRLF, missing final newline,
UTF-8, binary data, long lines, many short lines, JSON escaping, negative/huge
limits, no-progress prevention, read failures, and whole-file version semantics.
Exercise local and remote shell extraction against the same fixtures. Include an
MCP round-trip test that measures the actual serialized result and confirms only
one file representation appears within each structured/text representation.

## 3. Normalize collections and explain session selection accurately

- Normalize these successful public results at the handler boundary: `matches`,
  `paths`, `entries`, `sessions`, and `other_sessions`. Keep fields present as
  arrays. Test serialized JSON and MCP results; checking length alone cannot
  distinguish nil from an empty slice.
- Preserve the aggregate proxy's rule: omit `session` only when one session is
  live; otherwise select an ID or name explicitly. Do not infer a target from
  recent activity or the last tool call.
- Adapt the proxy-advertised `session` schema description for both fresh and
  cached tool schemas. Copy schemas before changing them so a direct session
  socket can retain its accurate attached-session description.
- Correct README claims about proxy attachment, `--session`, and `AISH_SESSION`;
  distinguish the debug client from the aggregate proxy and document
  `list_sessions`. Warn on stderr when legacy `mcp-proxy --session` arguments
  are supplied, since the aggregate proxy ignores them.
- Cover zero/one/multiple sessions, explicit IDs and names, renames, and schema
  adaptation from both fresh and cached sources.

An optional pinned default is a separate future feature. If added, pin an
immutable session ID and fail when it disappears; never silently select another
session or follow a reassigned name. It is unnecessary for this repair.

## Delivery order and release checks

1. Search correctness and executed-backend regression tests.
2. File-read pagination, single-representation output, and response budgeting.
3. Collection normalization, session descriptions, and documentation updates.
4. End-to-end verification through the aggregate MCP proxy with an isolated
   local session and an authorized SSH test session; replay the recorded workflow.

Each change gets focused tests, then `go test ./...` and `go vet ./...` before
release. Ensure backend tests cannot all silently skip in CI. Use representative
GNU and BusyBox fixtures; record any untested platform rather than implying full
BSD/BusyBox validation from a GNU-only run.

Recommend a 0.3.0 release for the combined API changes. Preserving byte-offset
meaning avoids silent input breakage, but omitting raw `content` on numbered
reads and lowering default page sizes are intentional observable changes. Update
output schemas and release notes, tell programmatic callers how to request raw
content, and require consumers to follow pagination metadata.

Restart the updated session process and reconnect/restart the proxy with that
session live so tool schemas are fetched and cached again. Test the refreshed
cache with zero sessions as well. Mixed old/new session schemas are not made
compatible merely by restarting the proxy; upgrade targeted sessions together.
A broader schema-cache redesign, universal regex engine, new fixed-string mode,
and guessed/default session routing are outside this plan.

Completion means the recorded line-reading workflow has direct pagination and
bounded responses; alternation and single-file search work across tested
backends; successful empty collections are arrays; and the advertised session
selection rules match what the proxy actually does.
