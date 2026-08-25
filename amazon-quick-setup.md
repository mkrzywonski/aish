# Installing the aish MCP Server in Amazon Quick

This guide sets up the **aish** MCP connection so Amazon Quick can drive your shared terminal sessions.

## Prerequisites

- **Windows** with WSL2 installed (Ubuntu distro)
- **aish** installed inside WSL at `/usr/local/bin/aish`
- **Amazon Quick** desktop app

> If you haven't installed aish yet, run this inside your WSL Ubuntu terminal:
> ```bash
> curl -fsSL https://raw.githubusercontent.com/mkrzywonski/aish/main/install.sh | bash
> sudo cp ~/.local/bin/aish /usr/local/bin/aish
> ```

---

## Step 1: Generate a PSK (Pre-Shared Key)

The PSK gives the proxy a stable identity so aish sessions recognize reconnects without re-prompting for approval each time.

Open a **WSL terminal** and run:

```bash
openssl rand -hex 32
```

This outputs a 64-character hex string, e.g.:

```
a224ce1a25d790662846c199e3f50ced1ba99719187f156429cf176b3870b868
```

**Copy this value** — you'll paste it into the Quick settings next.

---

## Step 2: Add the MCP Connection in Amazon Quick

1. Open **Amazon Quick**
2. Go to **Settings** → **Capabilities** → **Connections**
3. Click **+ Add MCP**
4. Fill in the fields:

| Field | Value |
|-------|-------|
| **Connection type** | `Local` |
| **ID** | `aish` |
| **Name** | `aish` |
| **Command** | `C:\Windows\System32\wsl.exe` |
| **Arguments** | `-d Ubuntu -- env AISH_PSK=<your-psk-here> aish mcp-proxy` |
| **Description** | `Allows AI and human to collaborate in a terminal session` |
| **Timeout** | `30` |

**Important**: In the **Arguments** field, replace `<your-psk-here>` with the hex string you generated in Step 1. The full arguments string should look like:

```
-d Ubuntu -- env AISH_PSK=a224ce1a25d790662846c199e3f50ced1ba99719187f156429cf176b3870b868 aish mcp-proxy
```

5. Click **Test connection** to verify it works
6. Click **+ Save changes**

---

## Step 3: Start an aish Session

In any WSL terminal, start a session:

```bash
aish
```

This opens an aish-wrapped shell. Anything you type works normally — but now Quick can also drive it via MCP tools.

---

## Step 4: Verify in Quick

In an Amazon Quick chat, ask:

> "Can you see my aish sessions?"

Quick should respond by listing your active session(s). You can then ask it to run commands, read files, SSH into hosts, etc.

---

## How It Works

```
┌─────────────────┐      stdio       ┌─────────────────┐    unix socket    ┌─────────────────┐
│   Amazon Quick   │ ───────────────► │  aish mcp-proxy │ ────────────────► │  aish session   │
│   (MCP client)   │                  │  (aggregator)   │                   │  (PTY + tools)  │
└─────────────────┘                  └─────────────────┘                   └─────────────────┘
                                                                                    │
                                                                            SSH ControlMaster
                                                                                    │
                                                                            ┌───────▼───────┐
                                                                            │  Remote host   │
                                                                            └───────────────┘
```

- **Quick** launches `wsl.exe` which starts the `aish mcp-proxy` inside WSL
- The **proxy** discovers all running aish sessions on the machine and forwards tool calls to them
- Each **session** is a terminal the human is using — Quick can run commands, read/write files, and follow SSH connections
- The **PSK** ensures the proxy keeps a consistent identity across restarts, so sessions don't re-prompt for approval

---

## Troubleshooting

### "No aish sessions found"
You need at least one running `aish` session in a WSL terminal. Start one with just `aish`.

### Connection times out
Make sure `aish` is at `/usr/local/bin/aish` (not just `~/.local/bin/aish`). WSL's non-login shell uses a PATH that starts with `/usr/local/bin`, so that's where the binary must live.

Verify: `wsl -d Ubuntu -- which aish` should return `/usr/local/bin/aish`.

### Session prompts for approval every time
The PSK isn't working. Check that:
- The `AISH_PSK` value in Quick's arguments matches what you generated
- The binary at `/usr/local/bin/aish` supports PSK auth (v0.4.0+)
- Confirm with: `wsl -d Ubuntu -- aish --version` (should show ≥ 0.4.0)

### After upgrading aish
Always sync the binary to `/usr/local/bin`:
```bash
sudo cp ~/.local/bin/aish /usr/local/bin/aish
```
Then toggle the MCP connection off/on in Quick settings (or restart Quick) to pick up the new version.

---

## Optional: Install the aish Skill

For better AI behavior when driving sessions, install the aish skill file:

```bash
# From WSL, copy into Quick's skills folder:
cp /mnt/c/Users/<username>/aish/SKILL.md \
   "/mnt/c/Users/<username>/.quickwork/profiles/<profile-id>/skills/aish/SKILL.md"
```

Or ask Quick: *"Install the SKILL.md from my aish repo"* — it knows how to do this.
