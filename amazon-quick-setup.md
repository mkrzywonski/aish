# Connecting AISH to Amazon Quick Desktop (Windows + WSL2)

Quick's **Add MCP** dialog launches a local command and speaks MCP to it over
stdio. That is exactly what `aish mcp-proxy` is, so Quick can run the proxy
inside WSL directly. No Docker, no ports, no HTTP bridge.

## Prerequisites

- AISH installed inside your WSL distro (`aish` on `PATH`, e.g. `/usr/local/bin/aish`).
- A WSL2 distro name you know — check with `wsl --list --verbose`.
- Quick Desktop running as the **same Windows user** whose WSL account owns the
  aish sessions (session sockets are per-user).

## What to enter in the Add MCP dialog

| Field | Value |
|---|---|
| **Connection type** | `Local` — "Run a command on your machine" |
| **Name** | `aish` |
| **Command** | `C:\Windows\System32\wsl.exe` |
| **Arguments** | `-d Ubuntu -- env AISH_PSK=<your-key> aish mcp-proxy` |
| **Description** | *(optional)* Shared terminal session in WSL — run commands, read the screen, edit files on the session's current host |
| **Environment variables** | *(leave empty — the PSK is passed inline via `env`)* |
| **Timeout (seconds)** | `30` (the default; the proxy starts almost instantly) |

Replace `Ubuntu` with your distro name if it differs.

### Generating a PSK

Run this once inside WSL to generate a key:

```sh
aish generate-psk
```

Copy the hex string into the Arguments field as shown above. This allows the
proxy to reconnect after Quick restarts it without re-prompting you for
approval. See the README's "Pre-shared key (PSK) authentication" section for
details.

### Why `env` in the arguments?

Quick sets environment variables on the Windows side, but `wsl.exe` does not
automatically forward arbitrary variables into the Linux child. Passing the PSK
inline with `env AISH_PSK=<key> aish mcp-proxy` is the most reliable way to
ensure it reaches the proxy. An alternative is the `WSLENV` mechanism:

| Field | Value |
|---|---|
| **Arguments** | `-d Ubuntu -- aish mcp-proxy` |
| **Environment variables** | `AISH_PSK=<your-key>` and `WSLENV=AISH_PSK` |

`WSLENV` tells `wsl.exe` to translate the listed variables into the Linux
environment. Either approach works; the inline `env` method is simpler.

### Field notes

- **Command** — the full path is used deliberately. `System32` is always on the
  PATH for GUI apps, so bare `wsl.exe` normally works too, but the absolute path
  removes any doubt about how Quick resolves the executable.
- **Arguments** — the `--` separator is required. It tells `wsl.exe` that
  everything after it is the Linux command line, not more WSL flags.

### Pinning a default session

The proxy attaches to one session as its default target. To choose which:

```
-d Ubuntu -- env AISH_PSK=<key> aish mcp-proxy --session myproject
```

That is only the default, not a boundary — every AISH tool still accepts a
`session` argument, and `list_sessions` shows the others.

### The "Paste JSON" button

The form fields are the reliable path. If you use Paste JSON instead, the
conventional MCP shape is `{"mcpServers": {"aish": {"command": ..., "args": [...]}}}`,
but Quick's exact accepted schema is not documented in the field help — if a
paste is rejected, fall back to filling the fields in by hand.

## Using it

1. Open a WSL terminal and start a session:

   ```sh
   aish                    # wraps your $SHELL
   aish --name myproject   # ... with a name Quick can target
   aish --oob              # ... authorizing hidden out-of-band file/exec ops
   ```

2. Use Quick. Ask it to start with `list_sessions`, then `session_status`.

Start the session **before** asking Quick to use the tools. With no live session
the proxy still starts, but it can only offer `list_sessions` (or a stale cached
tool list) until a session exists and the client reconnects.

## First-time approval

The first time Quick connects to a new aish session, you will be prompted in
the terminal:

```
"mcp (via aish proxy ...)" wants to control this session — allow? [y/n]
```

Type `y`. If you configured a PSK, this approval is persisted — subsequent
Quick restarts reconnect silently. Without a PSK, you will be prompted on
every proxy restart.

## Verifying outside Quick

To confirm the command works before trusting the GUI, run the same thing Quick
will run. From a WSL terminal:

```sh
aish sessions           # should list your live session
aish client read_screen # poke the session without an AI client
```

From Windows PowerShell, confirming the exact Quick invocation resolves:

```powershell
wsl.exe -d Ubuntu -- aish sessions
```

If that lists your session, Quick will see it too.

## Troubleshooting

| Symptom | Cause |
|---|---|
| Quick reports the server started but no session tools appear | No live `aish` session. Start one, then reconnect the MCP server in Quick. |
| `aish: command not found` | `aish` isn't on the PATH of a non-login shell. Use an absolute path in Arguments: `-d Ubuntu -- /usr/local/bin/aish mcp-proxy`. |
| Wrong distro is used | Another distro is your WSL default (Docker Desktop registers `docker-desktop`). Always pass `-d <distro>` explicitly. |
| Sessions visible in the terminal but not to Quick | The proxy is running as a different Linux user. Force it with `-d Ubuntu -u <user> -- aish mcp-proxy`. |
| Connection drops when the terminal closes | Sessions live with the `aish` process. Closing the wrapped terminal ends the session. |
| Repeated approval prompts despite PSK | The proxy may be resolving to an older `aish` binary (e.g. `/usr/local/bin/aish`) that lacks PSK support. Check with `wsl.exe -d Ubuntu -- which aish` and ensure it points to the current version. |
| PSK set but prompt still appears | Quick may spawn a secondary probe process without env vars. Use the inline `env` approach in Arguments rather than the Environment variables field. |

## Why not Docker

AISH has no network listener — its MCP server is a per-session Unix socket
(`internal/mcpserver/server.go`), and `aish mcp-proxy` is a stdio-to-socket
bridge meant to be spawned by the client (`internal/proxy/aggregate.go`). There
is no port to publish and no HTTP endpoint to health-check.

Containerizing it would also defeat the point: the shared shell would be the
container's isolated userspace, not the WSL terminal you actually work in, and
it could not follow your `ssh` hops to real hosts.
