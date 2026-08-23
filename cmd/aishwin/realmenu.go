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
	AddMenuItem(session, "Clients...", func() { ShowClientsDialog() })
	AddMenuSeparator(session)
	AddMenuItem(session, "Quit", func() { Quit() })

	settingsMenu := NewSubmenu(bar, "Settings")
	AddMenuItem(settingsMenu, "Preferences...", func() { ShowSettingsDialog() })

	helpMenu := NewSubmenu(bar, "Help")
	AddMenuItem(helpMenu, "About", func() {
		snap := rt.snapshot()
		aishwndVersion := snap.aishwndVersion
		if aishwndVersion == "" {
			aishwndVersion = "not connected"
		}
		ShowInfo("About aishwin", fmt.Sprintf("aishwin %s\naishwnd %s", version, aishwndVersion))
	})

	return bar
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
		SetSessionName(name) // so the NEXT reconnect (auto-retry or Settings > Connect) keeps this name instead of reverting to the --name flag's original value
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
