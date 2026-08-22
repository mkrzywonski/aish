//go:build aishwindev

package main

// devctl.go: a dev-build-only remote control channel, mirroring
// screenshot.go's file-trigger pattern, that lets the AI drive the real
// GUI unattended instead of needing a human to click through every
// verification. Two capabilities: invoke a menu action by its (leaf)
// label, and answer a currently-open text-input dialog (set its text,
// click OK or Cancel). The Yes/No approval dialog itself is never driven
// this way -- AskYesNo auto-approves outright in dev builds (see
// gui_dialog.go), so there is never an open approval dialog for this
// channel to answer, and this file adds no path that could answer one.
//
// Menu items are looked up by their leaf label only (not a full "Session >
// Rename..." path) -- a deliberate simplification since this is a testing
// convenience, not a general UI automation framework, and the current menu
// has no duplicate leaf labels.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Scoped by this process's own PID, not a fixed shared path: with more
// than one dev instance running, a shared path means whichever instance's
// watcher goroutine notices the trigger first "wins" -- the same race
// screenshot.go had (see its own doc comment) and fixed the same way,
// found here live when an older-build "master" instance kept intercepting
// commands meant for a freshly built test instance.
var (
	devctlTriggerPath = fmt.Sprintf(`C:\Users\Public\aishwin-devctl-request-%d`, os.Getpid())
	devctlResultPath  = fmt.Sprintf(`C:\Users\Public\aishwin-devctl-result-%d`, os.Getpid())
)

var (
	devMenuActionsMu sync.Mutex
	devMenuLabelToID = map[string]uint16{}

	devTextDialogMu   sync.Mutex
	devTextDialogHwnd syscall.Handle

	devSettingsDialogMu   sync.Mutex
	devSettingsDialogHwnd syscall.Handle
)

func init() {
	onMenuItemAdded = func(label string, id uint16) {
		devMenuActionsMu.Lock()
		devMenuLabelToID[label] = id
		devMenuActionsMu.Unlock()
	}
	onTextDialogOpen = func(hwnd syscall.Handle) {
		devTextDialogMu.Lock()
		devTextDialogHwnd = hwnd
		devTextDialogMu.Unlock()
	}
	onTextDialogClose = func() {
		devTextDialogMu.Lock()
		devTextDialogHwnd = 0
		devTextDialogMu.Unlock()
	}
	onSettingsDialogOpen = func(hwnd syscall.Handle) {
		devSettingsDialogMu.Lock()
		devSettingsDialogHwnd = hwnd
		devSettingsDialogMu.Unlock()
	}
	onSettingsDialogClose = func() {
		devSettingsDialogMu.Lock()
		devSettingsDialogHwnd = 0
		devSettingsDialogMu.Unlock()
	}
}

// startDevControlWatcher polls for a trigger file every 300ms for the life
// of the process, mirroring screenshot.go's startScreenshotWatcher. Only
// called (from main.go) in an aishwindev build.
func startDevControlWatcher() {
	AppendLog("aishwin [dev build]: dev control channel active (unattended AI-driven testing mode)")
	go func() {
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			raw, err := os.ReadFile(devctlTriggerPath)
			if err != nil {
				continue
			}
			_ = os.Remove(devctlTriggerPath)
			result := runDevCommand(strings.TrimSpace(string(raw)))
			_ = os.WriteFile(devctlResultPath, []byte(result), 0644)
		}
	}()
}

// runDevCommand dispatches one trigger-file command line:
//
//	menu:<leaf label>       invoke that menu item's registered action
//	text:<value>            if a text-input dialog is open, set its edit
//	                        box to value and click OK
//	cancel                  if a text-input dialog is open, click Cancel
//	setting:<index>:<value> if the Settings dialog is open, set field
//	                        <index>'s edit box to value
//	settingsok              if the Settings dialog is open, click OK
//	settingscancel          if the Settings dialog is open, click Cancel
func runDevCommand(cmd string) string {
	AppendLogColor("Dev command: "+cmd, colorRunning)
	switch {
	case strings.HasPrefix(cmd, "menu:"):
		label := strings.TrimPrefix(cmd, "menu:")
		devMenuActionsMu.Lock()
		id, ok := devMenuLabelToID[label]
		devMenuActionsMu.Unlock()
		if !ok {
			return fmt.Sprintf("error: no menu item labeled %q", label)
		}
		if !TriggerMenuAction(id) {
			return fmt.Sprintf("error: menu item %q has no action", label)
		}
		return "ok"

	case strings.HasPrefix(cmd, "text:"):
		value := strings.TrimPrefix(cmd, "text:")
		devTextDialogMu.Lock()
		hwnd := devTextDialogHwnd
		devTextDialogMu.Unlock()
		if hwnd == 0 {
			return "error: no text-input dialog is currently open"
		}
		editHwnd, _, _ := procGetDlgItem.Call(uintptr(hwnd), idInputEdit)
		procSetWindowTextW.Call(editHwnd, uintptr(unsafe.Pointer(utf16ptr(value))))
		procPostMessageW.Call(uintptr(hwnd), wmCommand, uintptr(idOK), 0)
		return "ok"

	case cmd == "cancel":
		devTextDialogMu.Lock()
		hwnd := devTextDialogHwnd
		devTextDialogMu.Unlock()
		if hwnd == 0 {
			return "error: no text-input dialog is currently open"
		}
		procPostMessageW.Call(uintptr(hwnd), wmCommand, uintptr(idCancelBtn), 0)
		return "ok"

	case strings.HasPrefix(cmd, "setting:"):
		rest := strings.TrimPrefix(cmd, "setting:")
		indexStr, value, found := strings.Cut(rest, ":")
		if !found {
			return `error: expected "setting:<index>:<value>"`
		}
		index, err := strconv.Atoi(indexStr)
		if err != nil {
			return fmt.Sprintf("error: invalid field index %q", indexStr)
		}
		devSettingsDialogMu.Lock()
		hwnd := devSettingsDialogHwnd
		devSettingsDialogMu.Unlock()
		if hwnd == 0 {
			return "error: the Settings dialog is not currently open"
		}
		editHwnd, _, _ := procGetDlgItem.Call(uintptr(hwnd), uintptr(idSettingsEditBase+index))
		if editHwnd == 0 {
			return fmt.Sprintf("error: no settings field at index %d", index)
		}
		procSetWindowTextW.Call(editHwnd, uintptr(unsafe.Pointer(utf16ptr(value))))
		return "ok"

	case cmd == "settingsok":
		devSettingsDialogMu.Lock()
		hwnd := devSettingsDialogHwnd
		devSettingsDialogMu.Unlock()
		if hwnd == 0 {
			return "error: the Settings dialog is not currently open"
		}
		procPostMessageW.Call(uintptr(hwnd), wmCommand, uintptr(idOK), 0)
		return "ok"

	case cmd == "settingscancel":
		devSettingsDialogMu.Lock()
		hwnd := devSettingsDialogHwnd
		devSettingsDialogMu.Unlock()
		if hwnd == 0 {
			return "error: the Settings dialog is not currently open"
		}
		procPostMessageW.Call(uintptr(hwnd), wmCommand, uintptr(idCancelBtn), 0)
		return "ok"

	default:
		return fmt.Sprintf("error: unrecognized dev command %q", cmd)
	}
}
