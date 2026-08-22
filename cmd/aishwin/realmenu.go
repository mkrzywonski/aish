package main

// realmenu.go: the native Win32 menu bar for the real (non-smoke-test)
// window, replacing console.go/menu.go's typed text commands. Ported
// logic, not new behavior -- see the removed menu.go for the original
// console-command shape.

import (
	"encoding/json"
	"fmt"
	"strings"
	"syscall"
	"time"

	"ai-ssh/internal/aishwinwire"
)

func buildRealMenu() syscall.Handle {
	bar := NewMenuBar()

	session := NewSubmenu(bar, "Session")
	AddMenuItem(session, "Rename...", func() {
		current := rt.snapshot().name
		name, ok := AskText("Rename Session", "New session name:", current)
		if ok && name != "" {
			menuRename(name)
		}
	})
	AddMenuSeparator(session)
	AddMenuItem(session, "Quit", func() { Quit() })

	accessMenu := NewSubmenu(bar, "Access")
	var accessID, blockID uint16
	accessID = AddCheckableMenuItem(accessMenu, "AI access enabled", access.aiEnabled.Load(), func() {
		enabled := !access.aiEnabled.Load()
		access.aiEnabled.Store(enabled)
		SetMenuChecked(accessMenu, accessID, enabled)
		if enabled {
			AppendLog("aishwin: AI access enabled")
		} else {
			AppendLog("aishwin: AI access disabled — exec and file operations will be refused until turned back on")
		}
	})
	blockID = AddCheckableMenuItem(accessMenu, "Block new commands", access.newExecBlocked.Load(), func() {
		blocked := !access.newExecBlocked.Load()
		access.newExecBlocked.Store(blocked)
		SetMenuChecked(accessMenu, blockID, blocked)
		if blocked {
			AppendLog("aishwin: new commands blocked — already-running commands are unaffected")
		} else {
			AppendLog("aishwin: new commands allowed again")
		}
	})

	envMenu := NewSubmenu(bar, "Env")
	AddMenuItem(envMenu, "Set variable...", func() {
		kv, ok := AskText("Set Env Var", "KEY=VALUE:", "")
		if !ok || kv == "" {
			return
		}
		key, value, found := strings.Cut(kv, "=")
		if !found || key == "" {
			AppendLog("aishwin: invalid input, expected KEY=VALUE")
			return
		}
		access.setEnv(key, value)
		pushLiveEnv(key, value)
		AppendLog(fmt.Sprintf("aishwin: set %s (applies to new commands now; already-running ones are unaffected)", key))
	})
	AddMenuItem(envMenu, "List variables", func() {
		vars := access.listEnv()
		if len(vars) == 0 {
			AppendLog("aishwin: no persistent env vars set")
			return
		}
		for _, v := range vars {
			AppendLog(v)
		}
	})

	helpMenu := NewSubmenu(bar, "Help")
	AddMenuItem(helpMenu, "Status", func() {
		snap := rt.snapshot()
		ShowInfo("aishwin Status", fmt.Sprintf(
			"Connected to linux half: %v\nSession: %s\nShell: %s\nAI access: %s\nNew commands blocked: %s",
			snap.connected,
			sessionLabel(aishwinwire.HelloAckData{SessionID: snap.sessionID, Name: snap.name}),
			execD.kind, onOff(access.aiEnabled.Load()), onOff(access.newExecBlocked.Load()),
		))
	})
	AddMenuItem(helpMenu, "About", func() {
		snap := rt.snapshot()
		aicmddVersion := snap.aicmddVersion
		if aicmddVersion == "" {
			aicmddVersion = "not connected"
		}
		ShowInfo("About aishwin", fmt.Sprintf(
			"aishwin %s\n\nLets an AI assistant run commands and manage files on this computer, with everything it does shown in this window so you can see what happened.\n\nCompanion service: aicmdd %s",
			version, aicmddVersion,
		))
	})

	return bar
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func menuRename(name string) {
	snap := rt.snapshot()
	if !snap.connected || snap.wire == nil {
		AppendLog("aishwin: not connected to the linux half")
		return
	}
	data, err := json.Marshal(aishwinwire.RenameData{Name: name})
	if err != nil {
		AppendLog(fmt.Sprintf("aishwin: rename failed: %v", err))
		return
	}
	id := randHex(8)
	ch := snap.wire.Await(id)
	defer snap.wire.CancelAwait(id)
	if err := snap.wire.Send(aishwinwire.Frame{Type: "rename", ID: id, Data: data}); err != nil {
		AppendLog(fmt.Sprintf("aishwin: rename failed: %v", err))
		return
	}
	select {
	case f := <-ch:
		var res aishwinwire.RenameResultData
		if err := json.Unmarshal(f.Data, &res); err != nil {
			AppendLog("aishwin: rename failed: malformed response")
			return
		}
		if res.Error != "" {
			AppendLog(fmt.Sprintf("aishwin: rename failed: %s", res.Error))
			return
		}
		rt.setName(name)
		AppendLog(fmt.Sprintf("aishwin: renamed to %q", name))
	case <-time.After(10 * time.Second):
		AppendLog("aishwin: rename timed out")
	}
}

// pushLiveEnv applies a newly-set var to the currently-running persistent
// shell immediately, if there is one -- otherwise it only takes effect the
// next time the shell (re)starts. Best-effort: output isn't captured or
// checked, matching a human just typing `set X=Y` themselves.
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
