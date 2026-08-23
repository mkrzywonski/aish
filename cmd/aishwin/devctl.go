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

	devClientsDialogMu   sync.Mutex
	devClientsDialogHwnd syscall.Handle
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
	onClientsDialogOpen = func(hwnd syscall.Handle) {
		devClientsDialogMu.Lock()
		devClientsDialogHwnd = hwnd
		devClientsDialogMu.Unlock()
	}
	onClientsDialogClose = func() {
		devClientsDialogMu.Lock()
		devClientsDialogHwnd = 0
		devClientsDialogMu.Unlock()
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
//	clientsclick:<id>       post a synthetic WM_COMMAND(id) to the open
//	                        Clients dialog (idCancelBtn=Close,
//	                        idClientsRefreshBtn=Refresh,
//	                        idClientsRowBtnBase+N=row N's Disconnect)
//	statusbarhover:<index>  post a real WM_MOUSEMOVE to the status bar
//	                        (gui_statusbar.go) at item <index>'s center --
//	                        exercises the hover tooltip exactly as a real
//	                        mouse move would, through the same hit-test
//	                        code a real mouse uses
//	statusbarclick:<index>  post a real WM_LBUTTONUP to the status bar at
//	                        item <index>'s center -- exercises its onClick
//	appendlog:<text>        append text to the log view directly, as if it
//	                        were a real exec/file operation's own log line
//	                        -- lets a test trigger new output at an exact,
//	                        known moment
//	scrollupby:<n>          scroll the log view up by n lines (a real
//	                        EM_LINESCROLL, exercising the same code path a
//	                        mouse wheel does) -- for testing the Auto
//	                        Scroll switch (gui_statusbar.go) turning itself
//	                        off when the user scrolls away
//	scrollcheck             report the log view's current scroll position
//	                        and the Auto Scroll switch's state
//
// currentEnvEditControl returns whichever control the Environment tab's
// in-place edit is currently focused on: the Value overlay if the value
// step is active, else the list view's own built-in Key-cell label-edit
// control (retrieved via LVM_GETEDITCONTROL) if the key step is active,
// else 0.
// statusBarCheckString is a temporary diagnostic for verifying
// gui_statusbar.go without a working screenshot pipeline -- reports the
// LED's logical state, the tooltip popup's visibility/text, and (if the
// Settings dialog happens to be open) its current tab selection, all from
// live window state rather than pixels.
func statusBarCheckString() string {
	tipVisible, _, _ := procIsWindowVisible.Call(uintptr(hwndTooltip))
	devSettingsDialogMu.Lock()
	settingsHwnd := devSettingsDialogHwnd
	devSettingsDialogMu.Unlock()
	tabSel := -1
	if settingsHwnd != 0 && settingsTabHwnd != 0 {
		sel, _, _ := procSendMessageW.Call(uintptr(settingsTabHwnd), tcmGetCurSel, 0, 0)
		tabSel = int(int32(sel))
	}
	return fmt.Sprintf("connected=%v hotItem=%d tooltipVisible=%v tooltipText=%q settingsOpen=%v tabSel=%d autoScrollEnabled=%v",
		statusConnected.Load(), statusHotItem, tipVisible != 0, tooltipText, settingsHwnd != 0, tabSel, autoScrollEnabled)
}

// statusItemCenterLParam resolves idxStr to a status bar item index and
// packs its rect's center point into a WM_MOUSEMOVE/WM_LBUTTONUP-shaped
// lParam, using the same statusItemRect the real hit-test code uses so
// this never drifts from the real layout.
func statusItemCenterLParam(idxStr string) (idx int, lParam uintptr, err error) {
	idx, err = strconv.Atoi(idxStr)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid item index %q", idxStr)
	}
	if hwndStatus == 0 {
		return 0, 0, fmt.Errorf("the status bar does not exist yet")
	}
	var rc rect
	procGetClientRect.Call(uintptr(hwndStatus), uintptr(unsafe.Pointer(&rc)))
	r := statusItemRect(idx, rc.right-rc.left, rc.bottom-rc.top)
	cx := (r.left + r.right) / 2
	cy := (r.top + r.bottom) / 2
	return idx, uintptr(uint32(cx)) | uintptr(uint32(cy))<<16, nil
}

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

	case strings.HasPrefix(cmd, "clientsclick:"):
		idStr := strings.TrimPrefix(cmd, "clientsclick:")
		id, err := strconv.Atoi(idStr)
		if err != nil {
			return fmt.Sprintf("error: invalid control id %q", idStr)
		}
		devClientsDialogMu.Lock()
		hwnd := devClientsDialogHwnd
		devClientsDialogMu.Unlock()
		if hwnd == 0 {
			return "error: the Clients dialog is not currently open"
		}
		procPostMessageW.Call(uintptr(hwnd), wmCommand, uintptr(id), 0)
		return "ok"

	case strings.HasPrefix(cmd, "statusbarhover:"):
		idx, lParam, err := statusItemCenterLParam(strings.TrimPrefix(cmd, "statusbarhover:"))
		if err != nil {
			return "error: " + err.Error()
		}
		// SendMessageW (synchronous), unlike every other devctl command
		// here, deliberately: TrackMouseEvent's WM_MOUSELEAVE checks the
		// REAL cursor position, which a synthetic hover never actually
		// moves, so it fires almost immediately behind a merely-posted
		// WM_MOUSEMOVE -- a race that only exists for this fake-input test
		// path, not for a real mouse. Blocking here until the move is
		// fully handled lets statusbarcheck observe the hover state
		// deterministically, before that follow-up leave can land. Safe to
		// block on here (unlike statusbarclick) because hovering never
		// enters a nested modal message loop the way a click might.
		procSendMessageW.Call(uintptr(hwndStatus), wmMouseMove, 0, lParam)
		return fmt.Sprintf("ok (item %d)", idx)

	case strings.HasPrefix(cmd, "statusbarclick:"):
		idx, lParam, err := statusItemCenterLParam(strings.TrimPrefix(cmd, "statusbarclick:"))
		if err != nil {
			return "error: " + err.Error()
		}
		procPostMessageW.Call(uintptr(hwndStatus), wmLButtonUp, 0, lParam)
		return fmt.Sprintf("ok (item %d)", idx)

	case strings.HasPrefix(cmd, "statusbarhovercheck:"):
		// statusbarhover + statusbarcheck as one atomic round trip: the
		// separate two-command version above raced against Windows' own
		// internal WM_MOUSELEAVE detection, which had the whole wall-clock
		// gap between two separate devctl requests (each a real file
		// write/read over the wire) to fire before the check could
		// observe the hover. Doing both in one call leaves only a few
		// instructions of gap, nowhere near enough for that timer to fire.
		idx, lParam, err := statusItemCenterLParam(strings.TrimPrefix(cmd, "statusbarhovercheck:"))
		if err != nil {
			return "error: " + err.Error()
		}
		procSendMessageW.Call(uintptr(hwndStatus), wmMouseMove, 0, lParam)
		return fmt.Sprintf("item=%d %s", idx, statusBarCheckString())

	case cmd == "statusbarcheck":
		return statusBarCheckString()

	case strings.HasPrefix(cmd, "appendlog:"):
		// Temporary diagnostic for investigating a reported sticky-scroll
		// bug: appends a controlled line directly, distinct from a real
		// exec/file operation's own logging, so a test can trigger new
		// output at an exact, known moment.
		AppendLog(strings.TrimPrefix(cmd, "appendlog:"))
		return "ok"

	case strings.HasPrefix(cmd, "scrollupby:"):
		nStr := strings.TrimPrefix(cmd, "scrollupby:")
		n, err := strconv.Atoi(nStr)
		if err != nil {
			return fmt.Sprintf("error: invalid line count %q", nStr)
		}
		// This call's own "Dev command: scrollupby:..." log line (appended
		// by runDevCommand's caller, above, before this switch runs) is
		// posted async and drained on the GUI thread -- same as the
		// EM_LINESCROLL below, dispatched via SendMessageW from this
		// goroutine, also to the GUI thread. Nothing guarantees which runs
		// first: if the log line's own auto-scroll-to-bottom (Auto Scroll
		// is still on at this point) lands AFTER the deliberate scroll
		// below, it silently undoes it. A brief pause lets that log line
		// finish draining first, so the scroll below is the last thing to
		// happen -- purely a test-harness concern; a real mouse-wheel
		// scroll has no such race since it isn't correlated with any log
		// append at all.
		time.Sleep(100 * time.Millisecond)
		procSendMessageW.Call(uintptr(hwndEdit), emLineScroll, 0, uintptr(-int32(n)))
		return "ok"

	case cmd == "scrollcheck":
		first, _, _ := procSendMessageW.Call(uintptr(hwndEdit), emGetFirstVisibleLine, 0, 0)
		lineCount, _, _ := procSendMessageW.Call(uintptr(hwndEdit), emGetLineCount, 0, 0)
		return fmt.Sprintf("atBottom=%v firstVisibleLine=%d lineCount=%d autoScrollEnabled=%v",
			isScrolledToBottom(), int32(first), int32(lineCount), autoScrollEnabled)

	default:
		return fmt.Sprintf("error: unrecognized dev command %q", cmd)
	}
}
