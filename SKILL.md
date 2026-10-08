---
name: aish
description: Use aish MCP tools to inspect or edit files and run commands in a human's shared terminal, including remote hosts reached through SSH, or an aishwin Windows session. Apply when working through aish, choosing its tools, or recovering from its permission, pagination, or connection errors.
---

# Working through aish

Aish tools follow the selected session's current host. Your assistant-native
shell and file tools operate in your own environment, which may be a different
machine. For work on an aish remote host, use aish tools; a local edit does not
update the remote file. Tool names below omit client-specific MCP prefixes.

Use the running tools' schemas, status, and error details when they differ
from this guide. Report the discrepancy rather than inventing a workaround.

## Establish the target

1. Call `list_sessions` and select the intended session. Inspect its backend
   and supported tools. Pass `session` explicitly when multiple sessions exist.
2. Call `session_status` to establish the host, current mode, OOB identity,
   and tool availability. Refresh after SSH disconnects, reconnects, or host changes.
3. For a `shared_terminal` session with unknown OOB capabilities, call
   `probe_host` once when OOB work is needed. Status alone does not initialize
   the channel. Probing can open an SSH session and trigger MFA.

Name the selected session and host before substantial work. Do not assume a
reconnected session still targets the same host or has the same capabilities.

- `shared_terminal`: supports the shared PTY and visible commands, plus OOB
  operations when available.
- `direct_host` (aishwin): native Windows operations; use its advertised tools.
  Do not assume it supports `probe_host`, `exec`, or shared-terminal input.

When `target_confidence` is present, respect it: `same` verifies the host;
`unknown` is not verification even after a write has been authorized;
`mismatch` blocks mutations and must not be bypassed. Compare the interactive
host with the probed `remote_hostname`, not the SSH alias in `oob_host`.
Remote prompt integration (`Ctrl-]`, then `p`) can help establish confidence.
If status includes `mode_note`, use the screen, recent output, and `wait_idle`
to assess readiness; `mode: running` may describe SSH rather than a remote job.

## Choose the operation

| Need | Preferred tool |
|---|---|
| Locate files or matching text | `file_search`, `file_grep`, `directory_list`, when advertised |
| Read a file or relevant section | `file_read` |
| Replace one exact passage | `file_edit` |
| Apply multiple text changes | `file_patch` |
| Create a file or replace its complete contents | `file_write` |
| Quiet, noninteractive command | `exec`, when supported and authorized |
| Interactive command or the visible shell's identity/privileges | `run_command` |

Prefer direct edits at the remote destination when permissions allow. Do not
habitually download, edit locally, upload to `/tmp`, and ask the human to copy
files. Staging is appropriate when an actual transfer or privileged installation
requires it; explain that reason and complete the authorized installation path.

OOB operations are normally silent. Inspect `via`, `visibility`, warnings, and
availability rather than assuming a requested route was used. Without OOB
access some tools use the visible terminal; others refuse. `oob_log`, when
available, records invisible work.

## Read, edit, and verify

Read the relevant current text before editing. Use raw `content` for exact
matches; `numbered_content` includes display-only line numbers. If a match is
missing or ambiguous, reread and choose a unique passage. Use `replace_all`
only when every occurrence should change. For a full rewrite use `file_write`,
with `if_match` from a suitable whole-file read or stat when available.

A read page is not the whole file unless `eof` says so:

- Byte mode: `offset` is zero-based bytes. Continue using returned `next_offset`.
- Line mode: use `start_line` and `limit`, for example `start_line: 241,
  limit: 160`. Continue using `next_line` when provided.
- Never combine byte offset with line parameters. If a long line exceeds the
  page budget, use byte mode as instructed by the error. SFTP reads use byte mode.
- Default source page size is 16 KiB, requested maximum 256 KiB; the serialized
  result budget can shorten the page. Truncation does not mean the file is unusable.
- `file_edit` and `file_patch` currently limit the entire input and resulting
  file to 1 MiB. A 32 KiB file is not too large for those tools.

If a tool hits a limit, report the exact operation and limit. For larger files,
use bounded reads/searches and an appropriate host-side transformation through
an authorized command route. Do not substitute a partially read page for the
complete file in `file_write`.

After a change, inspect the affected section or diff and run a relevant syntax
or functional check on the target host when warranted. A successful exact-text
replacement does not prove the resulting program is correct. If damage is
suspected, inspect the file and available history before adding more edits or
claiming that aish corrupted it.

## Identity and permissions

OOB runs as `session_status.oob_user`, normally the SSH login user. A human's
`su` or `sudo -i` in the visible terminal does not change that identity.
Use the visible route when the task needs that shell's identity or privileges.
Never send authentication passwords or private keys through tools. When the
terminal is collecting a secret (`echo_off`), leave input to the human.
OOB privilege escalation is refused: use visible `run_command` for `sudo`,
`su`, and similar operations, without trying to bypass the restriction.

Atomic edits and non-append writes create a temporary file beside the target,
then rename it. They need write and execute access to the parent directory;
a writable existing file alone is insufficient. On a `.aishtmp` permission
error, inspect the OOB identity/groups, parent directory permissions, and file
permissions before choosing a remedy. Do not keep retrying the same denied write
or change permissions/ownership beyond the user's authorized scope.

For POSIX hosts:

- `file_write` accepts an octal string `mode`, such as `"0664"` for group-shared
  writable files or `"0660"` without access for others. Choose according to the
  project's policy; do not make every file group-writable automatically.
- Without `mode`, normal atomic writes preserve existing permission bits but
  default new files to `0644`. A group-friendly umask alone does not override
  that explicit default. `file_edit`/`file_patch` preserve existing modes.
- Atomic replacement creates a new inode owned by the writing account. It does
  not promise original owner, group, ACL, or extended-attribute preservation.
  A setgid parent directory can provide shared-group inheritance.
- `mode` does not set owner/group or grant access to the parent directory.
  Verify important permissions with `file_stat`; a write success alone is not
  proof that all requested metadata was applied.

Windows `mode` handling is limited to its supported read-only behavior; it is
not a POSIX group-permission or Windows ACL management interface.

## Connections, retries, and MFA

Ordinary remote OOB file operations and foreground `exec` reuse a persistent
shell. Foreground means the call waits for completion, not that it runs in the
visible terminal. Background `exec` opens a separate channel per task; SFTP
opens a separate retained channel. New channels may cause MFA on strict hosts.
Use background execution for genuinely asynchronous work, not by default.
Run anything that finishes within a few minutes in the foreground with a
generous `timeout_ms`: a remote out-of-band foreground call that hits its timeout (default
30s) closes the shared channel without confirming the command stopped, and the
next call's reopen may cause MFA. Check before re-running such a command.

On unavailability, read the reason. A missing host capability will not improve
with repeated probing; a transient lost channel may recover on the next call.
Follow retry guidance without looping `probe_host`, forcing probes, or opening
additional SSH connections speculatively.

After a lost channel, refresh status. A retry can reopen the OOB shell, but a
failed mutation may already have completed. Read the destination or check the
command's effects before replaying it, especially appends or non-idempotent
commands. If the interactive SSH session ended, do not continue remote work
against a route that now points locally. Ask the human to reconnect if needed.

## Command output and reporting

Command output has its own inline budget (normally 16 KiB), separate from
`file_read` pagination. For visible commands, page available scrollback using
`read_output` cursors. For oversized `exec` output, use the returned `output_path`
with `file_read` or `file_grep`; retrieve it before another command can replace
the spill file. Check warnings: saving full output can fail, and buffers are finite.
For background tasks, poll `task_status` using the returned task ID.

If behavior contradicts a tool's contract, preserve the exact call, result,
expected behavior, and `version_info`, with secrets removed. Distinguish a
reproducible tool fault from permissions, stale state, or an incorrect edit.
Draft an issue when useful; publish to a tracker only when the user authorizes it.
