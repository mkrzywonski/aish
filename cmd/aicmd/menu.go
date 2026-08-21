package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"ai-ssh/internal/aicmdwire"
)

// handleMenuLine parses and dispatches one line typed at the console when
// no approval prompt is pending (console.go routes it here). Any line, not
// a special keybind, since there's no raw terminal mode to capture a single
// keypress like aish's own Ctrl-] menu — typing a word and Enter is the
// whole interface.
func handleMenuLine(line string) {
	fields := strings.Fields(line)
	cmd := strings.ToLower(fields[0])
	args := fields[1:]

	switch cmd {
	case "help", "menu", "?":
		printMenuHelp()
	case "status":
		printStatus()
	case "version":
		printVersion()
	case "rename":
		if len(args) != 1 {
			fmt.Fprintln(stdout, "usage: rename <new-name>")
			return
		}
		menuRename(args[0])
	case "access":
		menuAccessToggle(args)
	case "block":
		menuBlockToggle(args)
	case "env":
		menuEnv(args)
	default:
		fmt.Fprintf(stdout, "aicmd: unknown command %q — type 'help' for the list\n", cmd)
	}
}

func printMenuHelp() {
	fmt.Fprint(stdout, `
aicmd console commands:
  help                  show this list
  status                show current session/toggle state
  version                show aicmd and aicmdd versions
  rename <name>          rename this session
  access on|off          enable/disable the AI's access entirely
  block on|off           block/allow new commands (already-running ones are unaffected)
  env set KEY=VALUE       set a persistent env var for future commands
  env unset KEY           remove a persistent env var
  env list                show current persistent env vars

Not yet available from this console: connected-client list, per-client
revoke, and recent-activity history — these need aicmdd-side additions not
built yet.
`)
}

func printStatus() {
	snap := rt.snapshot()
	fmt.Fprintln(stdout, "aicmd status:")
	fmt.Fprintf(stdout, "  connected to linux half: %v\n", snap.connected)
	if snap.sessionID != "" {
		fmt.Fprintf(stdout, "  session: %s\n", sessionLabel(aicmdwire.HelloAckData{SessionID: snap.sessionID, Name: snap.name}))
	}
	if snap.aicmddVersion != "" {
		fmt.Fprintf(stdout, "  aicmdd version: %s\n", snap.aicmddVersion)
	}
	fmt.Fprintf(stdout, "  shell: %s\n", execD.kind)
	fmt.Fprintf(stdout, "  AI access: %s\n", onOff(access.aiEnabled.Load()))
	fmt.Fprintf(stdout, "  new commands blocked: %s\n", onOff(access.newExecBlocked.Load()))
	if vars := access.listEnv(); len(vars) > 0 {
		fmt.Fprintln(stdout, "  env vars:")
		for _, v := range vars {
			fmt.Fprintln(stdout, "    " + v)
		}
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func printVersion() {
	snap := rt.snapshot()
	fmt.Fprintf(stdout, "aicmd %s\n", version)
	if snap.aicmddVersion != "" {
		fmt.Fprintf(stdout, "aicmdd %s\n", snap.aicmddVersion)
	} else {
		fmt.Fprintln(stdout, "aicmdd: not connected")
	}
}

func menuRename(name string) {
	snap := rt.snapshot()
	if !snap.connected || snap.wire == nil {
		fmt.Fprintln(stdout, "aicmd: not connected to the linux half")
		return
	}
	data, err := json.Marshal(aicmdwire.RenameData{Name: name})
	if err != nil {
		fmt.Fprintln(stdout, "aicmd: rename failed:", err)
		return
	}
	id := randHex(8)
	ch := snap.wire.Await(id)
	defer snap.wire.CancelAwait(id)
	if err := snap.wire.Send(aicmdwire.Frame{Type: "rename", ID: id, Data: data}); err != nil {
		fmt.Fprintln(stdout, "aicmd: rename failed:", err)
		return
	}
	select {
	case f := <-ch:
		var res aicmdwire.RenameResultData
		if err := json.Unmarshal(f.Data, &res); err != nil {
			fmt.Fprintln(stdout, "aicmd: rename failed: malformed response")
			return
		}
		if res.Error != "" {
			fmt.Fprintln(stdout, "aicmd: rename failed:", res.Error)
			return
		}
		rt.setName(name)
		fmt.Fprintf(stdout, "aicmd: renamed to %q\n", name)
	case <-time.After(10 * time.Second):
		fmt.Fprintln(stdout, "aicmd: rename timed out")
	}
}

func menuAccessToggle(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(stdout, "usage: access on|off")
		return
	}
	switch args[0] {
	case "on":
		access.aiEnabled.Store(true)
		fmt.Fprintln(stdout, "aicmd: AI access enabled")
	case "off":
		access.aiEnabled.Store(false)
		fmt.Fprintln(stdout, "aicmd: AI access disabled — exec and file operations will be refused until turned back on")
	default:
		fmt.Fprintln(stdout, "usage: access on|off")
	}
}

func menuBlockToggle(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(stdout, "usage: block on|off")
		return
	}
	switch args[0] {
	case "on":
		access.newExecBlocked.Store(true)
		fmt.Fprintln(stdout, "aicmd: new commands blocked — already-running commands are unaffected")
	case "off":
		access.newExecBlocked.Store(false)
		fmt.Fprintln(stdout, "aicmd: new commands allowed again")
	default:
		fmt.Fprintln(stdout, "usage: block on|off")
	}
}

func menuEnv(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(stdout, "usage: env set KEY=VALUE | env unset KEY | env list")
		return
	}
	switch args[0] {
	case "set":
		if len(args) != 2 || !strings.Contains(args[1], "=") {
			fmt.Fprintln(stdout, "usage: env set KEY=VALUE")
			return
		}
		key, value, _ := strings.Cut(args[1], "=")
		if key == "" {
			fmt.Fprintln(stdout, "usage: env set KEY=VALUE")
			return
		}
		access.setEnv(key, value)
		pushLiveEnv(key, value)
		fmt.Fprintf(stdout, "aicmd: set %s (applies to new commands now; already-running ones are unaffected)\n", key)
	case "unset":
		if len(args) != 2 {
			fmt.Fprintln(stdout, "usage: env unset KEY")
			return
		}
		if access.unsetEnv(args[1]) {
			fmt.Fprintf(stdout, "aicmd: unset %s (takes effect the next time the persistent shell restarts)\n", args[1])
		} else {
			fmt.Fprintf(stdout, "aicmd: %s was not set\n", args[1])
		}
	case "list":
		vars := access.listEnv()
		if len(vars) == 0 {
			fmt.Fprintln(stdout, "aicmd: no persistent env vars set")
			return
		}
		for _, v := range vars {
			fmt.Fprintln(stdout, v)
		}
	default:
		fmt.Fprintln(stdout, "usage: env set KEY=VALUE | env unset KEY | env list")
	}
}

// pushLiveEnv applies a newly-set var to the currently-running persistent
// shell immediately, if there is one — otherwise it only takes effect the
// next time the shell (re)starts. Best-effort: output isn't captured or
// checked, matching a human just typing `set X=Y` themselves; a failure
// here just means the var takes effect on the next restart instead.
func pushLiveEnv(key, value string) {
	shell := execD.liveShell()
	if shell == nil {
		return
	}
	var setCmd string
	switch shell.kind {
	case shellPowerShell:
		setCmd = fmt.Sprintf(`$env:%s = "%s"`, key, strings.ReplaceAll(value, `"`, "`\""))
	default:
		setCmd = fmt.Sprintf("set %s=%s", key, value)
	}
	go func() { _, _, _, _ = shell.Run(setCmd, 10*time.Second) }()
}
