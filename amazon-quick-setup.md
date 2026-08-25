# Installing the aish MCP Server in Amazon Quick

This guide sets up the **aish** MCP connection so Amazon Quick can drive your shared terminal sessions.

## Prerequisites

- **Windows** with WSL2 installed (Ubuntu distro)
- **Amazon Quick** desktop app

> If you haven't installed aish yet, open a **WSL Ubuntu terminal** and run:
> ```bash
> curl -fsSL https://raw.githubusercontent.com/mkrzywonski/aish/main/install.sh | bash
> ```
> This installs `aish` to `/usr/local/bin/aish` automatically.

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
Make sure `aish` is at `/usr/local/bin/aish`. WSL's non-login shell (which is what Quick launches) uses a PATH that starts with `/usr/local/bin`, so that's where the binary must live. The default `install.sh` puts it there.

Verify: `wsl -d Ubuntu -- which aish` should return `/usr/local/bin/aish`.

### Session prompts for approval every time
The PSK isn't working. Check that:
- The `AISH_PSK` value in Quick's arguments matches what you generated
- The binary at `/usr/local/bin/aish` supports PSK auth (v0.4.0+)
- Confirm with: `wsl -d Ubuntu -- aish --version` (should show ≥ 0.4.0)

### After upgrading aish
Re-run the installer to upgrade:
```bash
curl -fsSL https://raw.githubusercontent.com/mkrzywonski/aish/main/install.sh | bash
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

---

## Optional: Installing aishwin (Windows native session)

**aishwin** gives Quick direct access to a native Windows shell — no WSL terminal needed. It runs as a small GUI window (system tray) and exposes tools like `run_command`, `file_read`/`file_write`, `directory_list`, and `capture_screen` on the Windows host itself.

### Step 1: Build or download aishwin.exe

**Option A — Download prebuilt:**

Grab `aishwin.exe` from the [latest release](https://github.com/mkrzywonski/aish/releases) and place it somewhere permanent (e.g. `C:\Users\<username>\aish\aishwin.exe`).

**Option B — Build from source** (requires Go on Windows):

```powershell
cd C:\Users\<username>\aish
go build -ldflags "-H=windowsgui" -o aishwin.exe ./cmd/aishwin
```

> ⚠️ The `-H=windowsgui` flag is required — without it the binary holds the launching terminal open until it exits.

### Step 2: Run aishwin

Double-click `aishwin.exe` or run it from PowerShell:

```powershell
.\aishwin.exe
```

It starts in the background (no console window) and registers itself as an aish session that the MCP proxy can discover.

### Step 3: Verify

Once aishwin is running, the proxy picks it up automatically. In Quick, ask:

> "List my aish sessions"

You should see both your WSL terminal session(s) and the Windows `direct_host` session. The Windows session shows a different tool set — it has `capture_screen` and `read_console` instead of `send_input` and `probe_host`.

### Notes

- **aishwin sessions show as `direct_host` backend** in `list_sessions`, while WSL terminal sessions show as `shared_terminal`
- **`capture_screen`** takes a screenshot of the Windows desktop — useful for GUI automation or showing Quick what you see
- **`run_command`** in a direct_host session runs in a native Windows shell (cmd.exe/PowerShell), not WSL
- **Auto-start**: To have aishwin launch at login, place a shortcut in `shell:startup`:
  ```powershell
  $ws = New-Object -ComObject WScript.Shell
  $s = $ws.CreateShortcut("$env:APPDATA\Microsoft\Windows\Start Menu\Programs\Startup\aishwin.lnk")
  $s.TargetPath = "C:\Users\$env:USERNAME\aish\aishwin.exe"
  $s.Save()
  ```
