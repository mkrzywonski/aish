//go:build windows

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
	AddMenuItem(session, "Rename...", promptRenameSession)
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

// promptRenameSession asks for a new session name and applies it via
// menuRename -- the shared implementation behind both the Session menu's
// "Rename..." item and the status bar's session-name item (gui_statusbar.go),
// so the two entry points can't drift out of sync.
func promptRenameSession() {
	current := rt.snapshot().name
	name, ok := AskText("Rename Session", "New session name:", current)
	if ok && name != "" {
		menuRename(name)
	}
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

// pushLiveEnv applies a newly-set var to every currently-running persistent
// shell immediately, if any -- otherwise it only takes effect the next
// time each shell (re)starts. More than one kind can be live at once (cmd
// and powershell each keep their own independent persistent process -- see
// execDispatcher, exec.go), so this pushes into all of them, each with its
// own correct set-var syntax, rather than a single fixed shell.
// Best-effort: output isn't captured or checked, matching a human just
// typing the equivalent command themselves.
func pushLiveEnv(key, value string) {
	for _, shell := range execD.liveShells() {
		setCmd := envSetCommand(shell.kind, key, value)
		go func(s *shellSession, cmd string) { _, _, _, _ = s.Run(cmd, 10*time.Second) }(shell, setCmd)
	}
}

func envSetCommand(kind shellKind, key, value string) string {
	switch kind {
	case shellPowerShell:
		return fmt.Sprintf(`$env:%s = "%s"`, key, strings.ReplaceAll(value, `"`, "`\""))
	default:
		return fmt.Sprintf("set %s=%s", key, value)
	}
}
