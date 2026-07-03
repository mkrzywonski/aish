# OOB Behavior on MFA-Protected Hosts

> **Status update:** the short-term recommendation below is implemented,
> inverted to default-deny: OOB is now disabled unless the session is
> started with `aish --oob`. Without it, `exec`/`file_*` run in-band
> (visible in the shared terminal), upload/download and background exec
> refuse, and the ssh shim doesn't inject ControlMaster at all — so no
> extra channels exist to trigger MFA. The medium/long-term items
> (consent-based OOB health probing, persistent single-channel OOB) remain
> open.

## Problem

`aish` was designed to let an AI work through a local shared terminal and follow the user into SSH sessions without installing AI tooling on every remote host.

That goal still holds, but on some locked-down hosts the current out-of-band (OOB) implementation has an undesirable side effect: OOB operations can trigger repeated Duo / 2FA prompts.

This is especially problematic because:

- repeated OOB operations can look like MFA spraying
- the user may receive unexpected pushes during normal work
- it undermines the promise that the AI can work quietly alongside an existing SSH session

## Current Behavior

Today, `aish` uses SSH multiplexing for remote OOB work:

- the interactive SSH session inside `aish` becomes a `ControlMaster`
- remote `exec` and `file_*` operations reuse that control socket
- each OOB operation opens a new SSH session/channel over the existing master

This works well on hosts where additional multiplexed channels do not require fresh MFA.

## Observed Behavior in Testing

We validated behavior against a remote session with Duo-protected SSH access.

### What worked without new MFA

These tools stayed inside the already-open interactive terminal session and are therefore low-risk for extra Duo prompts:

- `run_command`
- `send_input`
- `send_keys`
- `read_screen`
- `read_output`
- `wait_idle`
- `session_status`
- `set_session_name`

### What was associated with Duo pushes

These tools use the OOB remote path and are the likely source of extra Duo prompts:

- `exec`
- `exec_status` indirectly, when tracking a remote background task
- `file_read`
- `file_write`
- `file_upload`
- `file_download`

### Validation details

The control socket existed and passed a control-plane health check:

```sh
ssh -S <socket> -O check <host>
```

That reported:

```text
Master running
```

However, a real reused-channel probe:

```sh
ssh -S <socket> -oControlMaster=no -oBatchMode=yes -p 22 -l <user> <host> -- true
```

triggered Duo pushes.

Repeated executions of that same benign probe each generated another push.

## Conclusion

On this host, SSH multiplexing is technically functional, but it is not sufficient to avoid fresh MFA for additional OOB session/channel opens.

In other words:

- the control socket is reusable
- new OOB SSH session/channel opens still trigger Duo
- `run_command` is safer than OOB tools on such hosts

This means the current OOB model is not appropriate by default for some MFA-protected environments.

## Important Clarification

This does **not** imply that `aish` must install software on remote hosts.

The original goal remains valid:

- `aish` should run locally
- AI access should remain centralized on the local workstation
- remote hosts should not require local AI agent installation, separate updates, or separate login flows

Any future OOB improvement should preserve that model.

## Immediate Product Need

`aish` should support a way to disable OOB behavior for hosts where additional SSH channels trigger MFA.

Possible forms:

- `--no-oob`
- a per-session toggle
- a host policy / allowlist / denylist

In this mode:

- remote file and exec features would either be disabled or forced in-band
- the AI would continue using the shared visible terminal path
- surprise Duo prompts would be avoided

## Better Long-Term Direction

The preferred long-term outcome is still:

- one extra authorization event at most
- then reuse of an already-authorized OOB path

But that likely requires changing the architecture.

### What the current design does

- one interactive SSH master connection
- one new SSH session/channel per OOB operation

### What a better design would do

- open one dedicated OOB session/channel once
- authorize it once if required
- keep that session alive
- route all later remote `exec` and `file_*` operations through that same already-open channel

## Persistent OOB Channel Idea

This does **not** require installing software on the remote host.

Possible approaches:

- keep one long-lived remote `/bin/sh` or `bash` process
- run a small shell-based RPC loop over one SSH exec session
- stream a helper into memory/stdin and execute it without installation
- use a persistent subsystem-style channel if practical

Key property:

- no remote package install
- no remote update burden
- no separate remote AI login workflow

Only one persistent extra SSH-backed channel would be added.

## Open Question

The critical unknown is whether this environment will allow:

- one additional SSH session/channel to trigger Duo once
- then continued activity over that same already-open channel without more pushes

We confirmed that **new** channel opens trigger Duo.
We have **not** yet proven whether a single persistent extra channel would stay quiet after its initial authorization.

## Recommended Direction

### Short term

Add a way to disable OOB behavior on sensitive hosts.

This is the safest operational improvement and reduces the risk of surprise MFA prompts.

### Medium term

Improve OOB health detection so `aish` does not assume that `ssh -O check` means OOB reuse is safe.

A stronger test would be an actual benign remote command probe over the mux path.

### Long term

Explore a persistent single-channel OOB design that:

- remains fully local in deployment model
- installs nothing on remote hosts
- minimizes MFA prompts by avoiding repeated SSH channel/session creation

## Practical Implication for Current Use

For locked-down hosts with this MFA behavior:

- prefer `run_command`
- avoid `exec` and remote `file_*` unless extra Duo prompts are acceptable
- consider disabling OOB entirely for those sessions

For less restrictive hosts, the current OOB model may remain acceptable.
