---
name: aish
description: Drive a human's shared terminal session (aish) or a native Windows shell (aishwin) over MCP: choose the right session and backend, initialize a newly-SSH'd host, decide between visible and out-of-band operations, and avoid the wrong-host, wrong-user and stalled-mode traps. Use whenever aish MCP tools are present, or the user refers to a shared terminal, an aish session, or a host they SSH'd into there.
---

# Driving aish

Aish shares one terminal between a human and you. The human types in it, you
drive it through MCP tools, and both of you see the same screen. When the human
runs `ssh somewhere` in that terminal, your tools follow onto that host —
nothing is installed on the remote.

Your own native tools (Bash, Read, Edit, shell, filesystem) stay on YOUR
machine. That is a different computer from the session. When the user talks
about "the terminal", "the session", "this host", or a machine they SSH'd into,
they mean the aish session, so use aish tools.

## First moves

1. **`list_sessions`** — ids, names, backend, and the tools each one actually
   implements. Never assume the tool schema you loaded applies to every session.
2. **`session_status`** — where that session is right now. Re-run it after any
   SSH transition; the host changes under you.
3. **`probe_host`** — once, on a host you have not probed. It turns `oob_tools`
   from `unknown` into a real plan.

Every tool takes a `session` argument (id or name). With more than one session
live, pass it explicitly rather than relying on a default.

## Two backends, two tool sets

`list_sessions` reports a backend per session, and they do not implement the
same tools.

- **`shared_terminal`** — an aish PTY the human types into. Full set:
  `run_command`, `send_input`/`send_keys`, `read_screen`/`read_output`,
  `wait_idle`, `probe_host`, `oob_log`, `exec`, the `file_*` suite,
  `directory_list`/`directory_create`, `task_status`.
- **`direct_host`** — a native Windows shell in its own window (aishwin). No
  shared PTY. Adds `capture_screen` (a screenshot) and `read_console`
  (scrollback); has no `probe_host`, `send_input`, `oob_log`, or `exec`.

Plan against the `tools` list, not against habit.

## Visible or invisible

This is the distinction the whole tool exists for, so make it deliberately.

- **`run_command`** types into the shared terminal. The human sees the command
  and its output; it lands in scrollback. Use it for anything the human should
  witness, anything needing their shell's identity or privileges, and anything
  interactive.
- **`exec` and `file_*`** run out of band over a multiplexed SSH channel.
  Nothing appears on screen. Use them for the quiet mechanical work — reading,
  editing, searching — where narrating in the terminal would be noise.

Every result tells you which happened: `visibility` is `visible`, `silent`, or
`unknown`. Out-of-band work requires the user's authorization; without it,
tools either fall back to visible in-band operation or refuse with guidance.
`oob_log` is the record of what happened invisibly — read it when another client
shares the session, or when the user asks what you did off-screen.

## Five things that waste agents' time

**`oob_tools: unknown` does not mean broken.** It means the host has not been
probed. `session_status` never opens a channel (so it can never trigger an MFA
push), which is exactly why it cannot know. Call `probe_host` once and the
states resolve. Do not work around a tool reading `unknown` — probe, then plan.

**`unavailable` is different, and it is final.** If probe evidence says a tool
is unavailable, re-probing will not change it. Use `run_command` instead.

**`mode` stalls at `running` for the whole of an SSH session.** Prompt marking
(OSC 133) comes from the local shell and cannot see past `ssh`, so `mode` stays
`running` however idle the remote prompt is — correctly, since `ssh` really is
the running foreground process. `shell_integration: true` describes the LOCAL
shell. Whenever `session_status` includes `mode_note`, `mode` has stopped
tracking the shell you are talking to: use `last_output_ms_ago` or `wait_idle`.

**Out-of-band tools run as a different user than the human's shell may be.**
`oob_user` is the SSH login user, and it does NOT change when the human types
`su` or `sudo -i`. Check it before ownership- or privilege-sensitive work; if
their shell has switched users, say so and prefer `run_command`.

**`target_confidence` compares `interactive_host` with `remote_hostname` —
never `oob_host`.** `oob_host` is the connection target as configured: an alias,
an FQDN, an IP, a `ProxyJump` expression. It has no obligation to equal any
hostname, and comparing it against `interactive_host` will invent a
disagreement that is not there.

## Not being on the wrong machine

Every routed result carries `target_confidence`. It answers one question: do
invisible commands land on the machine the human is watching?

- **`same`** — verified. Proceed.
- **`unknown`** — the remote reports no hostname, so aish cannot tell. Reads
  proceed with a note; the first write asks the human once. **After that one
  approval, later writes stop prompting but still carry the note — do not read
  that silence as verification.** Before anything destructive, say which host
  you believe you are on and check with the user.
- **`mismatch`** — the hosts genuinely differ. Writes fail closed. Do not try
  to route around it.

The fix is one keystroke for the human: `Ctrl-]` then `p` installs the aish
prompt on the remote, after which `target_confidence` becomes `same`. Offer it.

## Rules that are not negotiable

- **Never send passwords, keys, or other secrets** through any tool. If
  `echo_off` is true the terminal is collecting secret input — stop and let the
  human type.
- **`sudo`, `su`, `doas`, `pkexec`, `runuser` must go through `run_command`.**
  They are refused out of band by design: a privileged command has to be one the
  human saw, and they type their own password. Do not look for a way around it.
- **Name the target session and host in chat** before the first substantial or
  destructive operation, so the human can stop you if you are aimed wrong.
- **Do not exceed the request.** This is someone's live machine, often a
  production one, and the terminal is shared with a person who is watching.

## Oversized output

Results carry at most 16 KiB of output inline. Nothing is lost.

- `run_command` output is in the terminal scrollback: page the rest with
  `read_output` using the cursors in the result.
- `exec` has no scrollback, so oversized output is trimmed from the middle and
  the full text is written to a file the result names in `output_path`. Read it
  with `file_read`, or search it with `file_grep` without reading it. Only one
  exists at a time — collect it before running another command.

## When aish itself is wrong

Aish is under active development and its authors want the failures you hit.
Report one only when you can state a **specific, reproducible** fault:

- a tool's own error told you what to do, you did it, and it still failed;
- `oob_tools` reported a tool `available` and the call failed anyway;
- documented behaviour did not happen;
- a message was actively misleading about which host, user, or route was
  involved.

Being confused is not yet a report. Re-read `list_sessions` and
`session_status` first — most surprises are one of the five traps above.

When it does qualify: **draft the report and ask the human before filing it.**
Filing publishes to a public tracker under their identity, so it is theirs to
approve. If they agree and `gh` is available:

```sh
gh issue create --repo mkrzywonski/aish --title "<one line>" --body "<report>"
```

Include what you expected, what happened, the exact tool call and result, and
the output of `version_info`. Search existing issues first. If `gh` is not
available or they would rather not file, write it down for them instead.

---

## Installing this file on your platform

If the user has pointed you at this file and asked you to install it, they want
it available in future sessions without pointing at it again.

Everything above this heading is the content. This section is instructions to
you and should not be copied into what you install.

1. Work out where THIS platform keeps persistent instructions or reusable
   skills. You know your own harness; aish does not, which is why this asks you
   rather than guessing.
2. Confirm the location with the user before writing anything.
3. Write the content, then say what you wrote and where, so they can find or
   remove it later.

Conventions worth knowing:

| Platform | Location |
|---|---|
| Claude Code | `~/.claude/skills/aish/SKILL.md` — this file works as-is, frontmatter included |
| Codex | `~/.codex/AGENTS.md` (global) or `AGENTS.md` in a project |
| Anything else | Kiro, Amazon Quick, Antigravity, IDE extensions: use that platform's own mechanism |

Two things to get right:

- **Prefer a location that loads automatically or on relevance.** A file nobody
  reads is worse than no file, because it looks like the problem is solved.
- **If the file is shared with the user's own instructions**, put this content
  in a clearly delimited block so it can be updated or removed cleanly.

Prefer a per-user or global scope: aish sessions are not tied to one project.

This file ships in the aish repository and changes as aish does, so re-copy it
after upgrading. If anything here contradicts a tool's own description or its
error message, **trust the tool** — that text is generated from the running
build — and mention the discrepancy to the user.
