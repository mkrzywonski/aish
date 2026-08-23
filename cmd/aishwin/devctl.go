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
//	clickjump               synthesize a BN_CLICKED on the jump-to-bottom
//	                        button, exactly as a real mouse click would
//	                        post it -- exercises the sticky-auto-scroll
//	                        indicator without needing a human to click it
//	connset:<host|port|user>:<value>  if the Settings dialog is open, set
//	                        that Connection-page field's text
//	tabselect:<index>       if the Settings dialog is open, select tab
//	                        <index> (0=General, 1=Connection, 2=Environment)
//	                        on the real SysTabControl32, mirroring a user click
//	envselect:<index>       if the Settings dialog's Environment tab is
//	                        open, select row <index> of the env var list --
//	                        needed before settingsclick:<idEnvEditBtn> or
//	                        settingsclick:<idEnvDeleteBtn>, mirroring a
//	                        user clicking a row before Edit/Delete
//	envtype:<value>         if the Environment tab's in-place Key or Value
//	                        edit is currently active, set its text
//	envcommit               if the Environment tab's in-place Key or Value
//	                        edit is currently active, commit it (mirrors
//	                        Enter/Tab/losing focus -- posted as a real
//	                        WM_KEYDOWN so the list view's own built-in
//	                        Key-cell label-edit handling runs exactly as it
//	                        would for a real keypress)
//	envcancel               if the Environment tab's in-place Key or Value
//	                        edit is currently active, cancel it (mirrors
//	                        Escape)
//	radioclick:<id>         simulate a real click (BM_CLICK) on the given
//	                        control id -- needed for BS_AUTORADIOBUTTON
//	                        controls, whose check/uncheck auto-behavior a
//	                        synthetic WM_COMMAND does not trigger
//	settingsclick:<id>      post a synthetic WM_COMMAND(id) to the open
//	                        Settings dialog -- fine for plain pushbuttons
//	                        (OK/Cancel/Connect), not for radio buttons
// currentEnvEditControl returns whichever control the Environment tab's
// in-place edit is currently focused on: the Value overlay if the value
// step is active, else the list view's own built-in Key-cell label-edit
// control (retrieved via LVM_GETEDITCONTROL) if the key step is active,
// else 0.
func currentEnvEditControl() syscall.Handle {
	if envValueEditHwnd != 0 {
		return envValueEditHwnd
	}
	if currentEnvEdit.active && envListHwnd != 0 {
		h, _, _ := procSendMessageW.Call(uintptr(envListHwnd), lvmGetEditControl, 0, 0)
		return syscall.Handle(h)
	}
	return 0
}

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

	case cmd == "clickjump":
		procPostMessageW.Call(uintptr(hwndMain), wmCommand, uintptr(idJumpBtn), uintptr(hwndJumpBtn))
		return "ok"

	case strings.HasPrefix(cmd, "connset:"):
		rest := strings.TrimPrefix(cmd, "connset:")
		field, value, found := strings.Cut(rest, ":")
		if !found {
			return `error: expected "connset:<host|port|user>:<value>"`
		}
		var id uintptr
		switch field {
		case "host":
			id = idConnHost
		case "port":
			id = idConnPort
		case "user":
			id = idConnUser
		default:
			return fmt.Sprintf("error: unknown connection field %q", field)
		}
		devSettingsDialogMu.Lock()
		hwnd := devSettingsDialogHwnd
		devSettingsDialogMu.Unlock()
		if hwnd == 0 {
			return "error: the Settings dialog is not currently open"
		}
		editHwnd, _, _ := procGetDlgItem.Call(uintptr(hwnd), id)
		procSetWindowTextW.Call(editHwnd, uintptr(unsafe.Pointer(utf16ptr(value))))
		return "ok"

	case strings.HasPrefix(cmd, "tabselect:"):
		idxStr := strings.TrimPrefix(cmd, "tabselect:")
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			return fmt.Sprintf("error: invalid tab index %q", idxStr)
		}
		devSettingsDialogMu.Lock()
		hwnd := devSettingsDialogHwnd
		devSettingsDialogMu.Unlock()
		if hwnd == 0 || settingsTabHwnd == 0 {
			return "error: the Settings dialog is not currently open"
		}
		procSendMessageW.Call(uintptr(settingsTabHwnd), tcmSetCurSel, uintptr(idx), 0)
		showSettingsPage(hwnd, idx)
		return "ok"

	case strings.HasPrefix(cmd, "envselect:"):
		idxStr := strings.TrimPrefix(cmd, "envselect:")
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			return fmt.Sprintf("error: invalid row index %q", idxStr)
		}
		if envListHwnd == 0 {
			return "error: the Settings dialog's Environment tab is not currently open"
		}
		item := lvItemW{mask: lvifState, state: lvisSelected | lvisFocused, stateMask: lvisSelected | lvisFocused}
		procSendMessageW.Call(uintptr(envListHwnd), lvmSetItemState, uintptr(idx), uintptr(unsafe.Pointer(&item)))
		return "ok"

	case strings.HasPrefix(cmd, "envtype:"):
		value := strings.TrimPrefix(cmd, "envtype:")
		hwnd := currentEnvEditControl()
		if hwnd == 0 {
			return "error: no environment-tab edit is currently active"
		}
		procSetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(utf16ptr(value))))
		return "ok"

	case cmd == "envcommit":
		hwnd := currentEnvEditControl()
		if hwnd == 0 {
			return "error: no environment-tab edit is currently active"
		}
		procPostMessageW.Call(uintptr(hwnd), wmKeyDown, uintptr(vkReturn), 0)
		return "ok"

	case cmd == "envcancel":
		hwnd := currentEnvEditControl()
		if hwnd == 0 {
			return "error: no environment-tab edit is currently active"
		}
		procPostMessageW.Call(uintptr(hwnd), wmKeyDown, uintptr(vkEscape), 0)
		return "ok"

	case strings.HasPrefix(cmd, "radioclick:"):
		// A synthetic WM_COMMAND to the dialog (unlike settingsclick's
		// plain-pushbutton case) does NOT trigger BS_AUTORADIOBUTTON's own
		// check/uncheck handling -- that's implemented inside the button
		// control's own window procedure in response to a real click.
		// BM_CLICK, sent to the control itself, simulates that real click.
		idStr := strings.TrimPrefix(cmd, "radioclick:")
		id, err := strconv.Atoi(idStr)
		if err != nil {
			return fmt.Sprintf("error: invalid control id %q", idStr)
		}
		devSettingsDialogMu.Lock()
		hwnd := devSettingsDialogHwnd
		devSettingsDialogMu.Unlock()
		if hwnd == 0 {
			return "error: the Settings dialog is not currently open"
		}
		ctrlHwnd, _, _ := procGetDlgItem.Call(uintptr(hwnd), uintptr(id))
		procSendMessageW.Call(ctrlHwnd, bmClick, 0, 0)
		return "ok"

	case strings.HasPrefix(cmd, "settingsclick:"):
		idStr := strings.TrimPrefix(cmd, "settingsclick:")
		id, err := strconv.Atoi(idStr)
		if err != nil {
			return fmt.Sprintf("error: invalid control id %q", idStr)
		}
		devSettingsDialogMu.Lock()
		hwnd := devSettingsDialogHwnd
		devSettingsDialogMu.Unlock()
		if hwnd == 0 {
			return "error: the Settings dialog is not currently open"
		}
		procPostMessageW.Call(uintptr(hwnd), wmCommand, uintptr(id), 0)
		return "ok"

	default:
		return fmt.Sprintf("error: unrecognized dev command %q", cmd)
	}
}
