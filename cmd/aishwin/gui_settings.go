package main

// gui_settings.go: the Settings dialog. General/Connection are real pages
// under a SysTabControl32 (comctl32) tab strip -- created at runtime in
// wmInitDialog as an ordinary child window (the same pattern gui.go uses
// for the RICHEDIT50W log view), not baked into the DLGTEMPLATE itself,
// since a named (non-predefined-atom) control class inside a hand-built
// DLGTEMPLATE is unproven in this codebase and safer to avoid; creating it
// as a plain child window after the dialog exists is the already-working
// pattern. aishwin.exe.manifest's Common-Controls v6 dependency is what
// makes SysTabControl32 (and its modern visual style) available at all.
// Adding a future *General* setting means one more entry in
// buildSettingsFields; the Connection page's controls (a mode choice plus
// three text fields) don't fit that same label+edit shape, so they're laid
// out directly instead of through a generic list.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const idSettingsEditBase = 200  // general-page field edit-box IDs start here
const idGeneralLabelBase = 220  // general-page field label IDs start here -- distinct from edit IDs so each can be shown/hidden independently

const (
	idSettingsTab = 300 // the SysTabControl32 itself, created at runtime (not part of the DLGTEMPLATE)

	idLabelConnMode = 302
	idConnModeWSL   = 303
	idConnModeSSH   = 304

	idLabelHost = 305
	idConnHost  = 306
	idLabelPort = 307
	idConnPort  = 308
	idLabelUser = 309
	idConnUser  = 310

	idConnConnectBtn = 311
)

// ---- SysTabControl32 bindings (comctl32) ----
// Kept local to this file rather than win32.go since nothing else in the
// app uses a common control -- see the file-level comment on why this is a
// runtime-created child window rather than a DLGTEMPLATE item.

var comctl32 = syscall.NewLazyDLL("comctl32.dll")
var procInitCommonControlsEx = comctl32.NewProc("InitCommonControlsEx")

type initCommonControlsEx struct {
	dwSize uint32
	dwICC  uint32
}

const iccTabClasses = 0x0008

var commonControlsOnce sync.Once

func ensureCommonControlsInit() {
	commonControlsOnce.Do(func() {
		icc := initCommonControlsEx{dwICC: iccTabClasses}
		icc.dwSize = uint32(unsafe.Sizeof(icc))
		procInitCommonControlsEx.Call(uintptr(unsafe.Pointer(&icc)))
	})
}

// tcItemW mirrors TCITEMW (commctrl.h) field-for-field, the same
// computed-size-via-unsafe.Sizeof approach as charFormat2W/scrollInfoT in
// win32.go.
type tcItemW struct {
	mask        uint32
	dwState     uint32
	dwStateMask uint32
	pszText     *uint16
	cchTextMax  int32
	iImage      int32
	lParam      uintptr
}

const (
	tcifText       = 0x0001
	tcmFirst       = 0x1300
	tcmInsertItemW = tcmFirst + 62
	tcmGetCurSel   = tcmFirst + 11
	tcmSetCurSel   = tcmFirst + 12
	tcnFirst       = -550
	tcnSelChange   = tcnFirst - 1

	// bmClick simulates a real mouse click on a button-class control
	// (including BS_AUTORADIOBUTTON's own check/uncheck handling, which a
	// synthetic WM_COMMAND posted to the dialog does not trigger) -- used
	// by devctl.go's radioclick test command.
	bmClick = 0x00F5
)

// nmhdr mirrors NMHDR (commctrl.h): every WM_NOTIFY lParam starts with one
// of these, whether or not the real notification struct behind it carries
// more fields (TCN_SELCHANGE doesn't need any).
type nmhdr struct {
	hwndFrom syscall.Handle
	idFrom   uintptr
	code     uint32
}

func insertTabItem(hwndTab syscall.Handle, index int, text string) {
	item := tcItemW{mask: tcifText, pszText: utf16ptr(text)}
	procSendMessageW.Call(uintptr(hwndTab), tcmInsertItemW, uintptr(index), uintptr(unsafe.Pointer(&item)))
}

// settingsField is one labeled, editable row on the General page.
type settingsField struct {
	label string
	get   func() string
	set   func(string) error // parses and applies value; a non-nil error keeps the old value and is reported to the user
}

func buildSettingsFields() []settingsField {
	return []settingsField{
		{
			label: "Scrollback buffer size (lines):",
			get:   func() string { return strconv.Itoa(settings.ScrollbackLines()) },
			set: func(v string) error {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil || n <= 0 {
					return fmt.Errorf("must be a positive whole number")
				}
				settings.SetScrollbackLines(n)
				return nil
			},
		},
	}
}

var settingsDialogProcPtr = syscall.NewCallback(settingsDialogProc)

// currentSettingsFields/currentSettingsPage/settingsTabHwnd are valid only
// while a Settings dialog is open (one at a time, like the other custom
// dialogs in this package).
var currentSettingsFields []settingsField
var currentSettingsPage int // 0 = General, 1 = Connection
var settingsTabHwnd syscall.Handle

// ShowSettingsDialog displays the modal Settings window. Must be called
// from the GUI's own thread (a menu click handler, which mainWndProc
// already runs there).
func ShowSettingsDialog() {
	ensureCommonControlsInit()
	currentSettingsFields = buildSettingsFields()
	currentSettingsPage = 0
	tmpl := buildSettingsDialogTemplate("Settings", currentSettingsFields)
	inst := getModuleHandle()
	procDialogBoxIndirectParamW.Call(
		uintptr(inst),
		uintptr(unsafe.Pointer(&tmpl[0])),
		uintptr(hwndMain),
		settingsDialogProcPtr,
		0,
	)
}

func settingsDialogProc(hwndDlg syscall.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmInitDialog:
		for i, f := range currentSettingsFields {
			editHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), uintptr(idSettingsEditBase+i))
			procSetWindowTextW.Call(editHwnd, uintptr(unsafe.Pointer(utf16ptr(f.get()))))
		}
		createSettingsTab(hwndDlg)
		populateConnectionFields(hwndDlg)
		showSettingsPage(hwndDlg, 0)
		procSetForegroundWin.Call(uintptr(hwndDlg))
		onSettingsDialogOpen(hwndDlg)
		return 1
	case wmNotify:
		// go vet's unsafeptr check flags this uintptr->pointer conversion,
		// but lParam here is a genuine NMHDR* the OS supplies for the
		// lifetime of this call, not a Go value that ever round-tripped
		// through uintptr storage -- the standard, unavoidable shape of
		// WM_NOTIFY handling in a raw Win32 callback.
		hdr := (*nmhdr)(unsafe.Pointer(lParam))
		if hdr.hwndFrom == settingsTabHwnd && int32(hdr.code) == tcnSelChange {
			sel, _, _ := procSendMessageW.Call(uintptr(settingsTabHwnd), tcmGetCurSel, 0, 0)
			showSettingsPage(hwndDlg, int(sel))
			return 1
		}
	case wmCommand:
		id := uint16(wParam & 0xFFFF)
		switch id {
		case idConnModeWSL, idConnModeSSH:
			updateConnectionFieldsEnabled(hwndDlg)
			return 1
		case idConnConnectBtn:
			if err := applyConnectionFields(hwndDlg); err != nil {
				AppendLog("aishwin: Connection " + err.Error())
				return 1
			}
			AppendLog("aishwin: connecting using the Connection settings just entered...")
			StartConnection(resolveSpawnFromSettings())
			return 1
		case idOK:
			var errs []string
			for i, f := range currentSettingsFields {
				editHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), uintptr(idSettingsEditBase+i))
				buf := make([]uint16, 256)
				n, _, _ := procSendMessageW.Call(editHwnd, wmGetText, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
				value := syscall.UTF16ToString(buf[:n])
				if err := f.set(value); err != nil {
					errs = append(errs, fmt.Sprintf("%s %v", f.label, err))
				}
			}
			if err := applyConnectionFields(hwndDlg); err != nil {
				errs = append(errs, "Connection "+err.Error())
			}
			if len(errs) > 0 {
				AppendLog("aishwin: some settings were not applied: " + strings.Join(errs, "; "))
			} else {
				AppendLog("aishwin: settings updated (connection changes apply on next connect, or click Connect now)")
			}
			procEndDialog.Call(uintptr(hwndDlg), 1)
			settingsTabHwnd = 0
			onSettingsDialogClose()
			return 1
		case idCancelBtn:
			procEndDialog.Call(uintptr(hwndDlg), 0)
			settingsTabHwnd = 0
			onSettingsDialogClose()
			return 1
		}
	case wmClose:
		procEndDialog.Call(uintptr(hwndDlg), 0)
		settingsTabHwnd = 0
		onSettingsDialogClose()
		return 1
	}
	return 0
}

// createSettingsTab builds the SysTabControl32 strip as an ordinary child
// window of the already-created dialog (mirroring gui.go's RICHEDIT50W
// creation) and inserts its two items. Positioned above pageTop -- see
// buildSettingsDialogTemplate.
func createSettingsTab(hwndDlg syscall.Handle) {
	inst := getModuleHandle()
	h, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(utf16ptr("SysTabControl32"))),
		uintptr(unsafe.Pointer(utf16ptr(""))),
		uintptr(wsChild|wsVisible|wsTabStop),
		10, 10, 280, 24,
		uintptr(hwndDlg), uintptr(idSettingsTab), uintptr(inst), 0,
	)
	settingsTabHwnd = syscall.Handle(h)
	insertTabItem(settingsTabHwnd, 0, "General")
	insertTabItem(settingsTabHwnd, 1, "Connection")
}

// populateConnectionFields fills the Connection page's controls from the
// currently persisted settings, called once when the dialog opens.
func populateConnectionFields(hwndDlg syscall.Handle) {
	hostHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), idConnHost)
	procSetWindowTextW.Call(hostHwnd, uintptr(unsafe.Pointer(utf16ptr(settings.SSHHost()))))

	portHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), idConnPort)
	procSetWindowTextW.Call(portHwnd, uintptr(unsafe.Pointer(utf16ptr(strconv.Itoa(settings.SSHPort())))))

	userHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), idConnUser)
	procSetWindowTextW.Call(userHwnd, uintptr(unsafe.Pointer(utf16ptr(settings.SSHUser()))))

	checkedID := idConnModeWSL
	if settings.ConnectionMode() == connModeSSH {
		checkedID = idConnModeSSH
	}
	procCheckRadioButton.Call(uintptr(hwndDlg), idConnModeWSL, idConnModeSSH, uintptr(checkedID))
	updateConnectionFieldsEnabled(hwndDlg)
}

// updateConnectionFieldsEnabled greys out the host/port/username fields
// when WSL is selected -- they're meaningless in that mode, and disabling
// them (rather than merely leaving them editable-but-ignored) tells the
// user that directly instead of relying on them reading the mode radio
// correctly themselves.
func updateConnectionFieldsEnabled(hwndDlg syscall.Handle) {
	sshChecked, _, _ := procIsDlgButtonChecked.Call(uintptr(hwndDlg), idConnModeSSH)
	enable := uintptr(0)
	if sshChecked != 0 {
		enable = 1
	}
	for _, id := range []uintptr{idConnHost, idConnPort, idConnUser} {
		h, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), id)
		procEnableWindow.Call(h, enable)
	}
}

// applyConnectionFields reads the Connection page's current controls,
// validates the port, and saves everything to settings (each setter
// persists to the registry itself). Shared by idOK and the Connect button
// so both apply identically.
func applyConnectionFields(hwndDlg syscall.Handle) error {
	getText := func(id uintptr) string {
		editHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), id)
		buf := make([]uint16, 256)
		n, _, _ := procSendMessageW.Call(editHwnd, wmGetText, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
		return strings.TrimSpace(syscall.UTF16ToString(buf[:n]))
	}

	host := getText(idConnHost)
	portStr := getText(idConnPort)
	user := getText(idConnUser)

	port := defaultSSHPort
	if portStr != "" {
		n, err := strconv.Atoi(portStr)
		if err != nil || n <= 0 || n > 65535 {
			return fmt.Errorf("port must be a number from 1-65535")
		}
		port = n
	}

	sshChecked, _, _ := procIsDlgButtonChecked.Call(uintptr(hwndDlg), idConnModeSSH)
	mode := connModeWSL
	if sshChecked != 0 {
		mode = connModeSSH
	}
	if mode == connModeSSH && host == "" {
		return fmt.Errorf("host/IP is required when SSH is selected")
	}

	settings.SetConnectionMode(mode)
	settings.SetSSHHost(host)
	settings.SetSSHPort(port)
	settings.SetSSHUser(user)
	return nil
}

// showSettingsPage shows the given page's controls and hides the other
// page's; the tab strip itself and OK/Cancel are always visible.
func showSettingsPage(hwndDlg syscall.Handle, page int) {
	currentSettingsPage = page

	generalIDs := make([]uintptr, 0, len(currentSettingsFields)*2)
	for i := range currentSettingsFields {
		generalIDs = append(generalIDs, uintptr(idGeneralLabelBase+i), uintptr(idSettingsEditBase+i))
	}
	connectionIDs := []uintptr{
		idLabelConnMode, idConnModeWSL, idConnModeSSH,
		idLabelHost, idConnHost, idLabelPort, idConnPort, idLabelUser, idConnUser,
		idConnConnectBtn,
	}

	showIDs, hideIDs := generalIDs, connectionIDs
	if page == 1 {
		showIDs, hideIDs = connectionIDs, generalIDs
	}
	for _, id := range hideIDs {
		h, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), id)
		procShowWindow.Call(h, swHide)
	}
	for _, id := range showIDs {
		h, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), id)
		procShowWindow.Call(h, swShow)
	}
}

// buildSettingsDialogTemplate assembles the whole Settings window: the
// General page (one label+edit row per field) and Connection page (mode
// radios, host/port/user) both built into the same template with the
// Connection page's controls initially not WS_VISIBLE (showSettingsPage
// corrects this again in wmInitDialog, but starting hidden avoids a
// one-frame flash of both pages overlapping before that runs), then
// Connect (Connection page only)/OK/Cancel at a fixed position below both.
// The tab strip itself is NOT part of this template -- see
// createSettingsTab.
func buildSettingsDialogTemplate(title string, fields []settingsField) []byte {
	var buf bytes.Buffer
	w := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	align4 := func() {
		for buf.Len()%4 != 0 {
			buf.WriteByte(0)
		}
	}
	writeStr := func(s string) {
		u, _ := syscall.UTF16FromString(s)
		for _, c := range u {
			w(c)
		}
	}
	item := func(style uint32, x, y, cx, cy int16, id uint16, class uint16, text string) {
		align4()
		w(style)
		w(uint32(0))
		w(x); w(y); w(cx); w(cy)
		w(id)
		w(uint16(0xFFFF)); w(class)
		writeStr(text)
		w(uint16(0))
	}

	const dialogWidth = int16(300)
	const dialogHeight = int16(240)
	const pageTop = int16(40)

	itemCount := uint16(len(fields) * 2)
	itemCount += 10 // connection page: label+2radios+3*(label+edit)+connect btn
	itemCount += 2  // OK/Cancel

	style := uint32(dsSetFont | dsModalFrame | dsCenter | wsPopup | wsCaption | wsSysMenu)
	w(style)
	w(uint32(0))
	w(itemCount)
	w(int16(0)); w(int16(0)); w(dialogWidth); w(dialogHeight)
	w(uint16(0))
	w(uint16(0))
	writeStr(title)
	w(uint16(8))
	writeStr("MS Shell Dlg")

	// General page.
	y := pageTop
	for i, f := range fields {
		item(wsChild|wsVisible, 10, y, 270, 12, uint16(idGeneralLabelBase+i), 0x0082, f.label)
		item(wsChild|wsVisible|wsBorder|wsTabStop|esAutoHScroll, 10, y+14, 270, 20, uint16(idSettingsEditBase+i), 0x0081, "")
		y += 36
	}

	// Connection page (created hidden -- no wsVisible -- corrected by
	// showSettingsPage as soon as the dialog opens).
	item(wsChild|wsGroup, 10, pageTop, 270, 12, idLabelConnMode, 0x0082, "Connection mode:")
	item(wsChild|wsTabStop|wsGroup|bsAutoRadioButton, 10, pageTop+14, 110, 20, idConnModeWSL, 0x0080, "WSL")
	item(wsChild|wsTabStop|bsAutoRadioButton, 130, pageTop+14, 110, 20, idConnModeSSH, 0x0080, "SSH")

	item(wsChild, 10, pageTop+42, 270, 12, idLabelHost, 0x0082, "Host / IP:")
	item(wsChild|wsBorder|wsTabStop|esAutoHScroll, 10, pageTop+56, 270, 20, idConnHost, 0x0081, "")

	item(wsChild, 10, pageTop+84, 120, 12, idLabelPort, 0x0082, "Port:")
	item(wsChild|wsBorder|wsTabStop|esAutoHScroll, 10, pageTop+98, 120, 20, idConnPort, 0x0081, "")

	item(wsChild, 150, pageTop+84, 130, 12, idLabelUser, 0x0082, "Username:")
	item(wsChild|wsBorder|wsTabStop|esAutoHScroll, 150, pageTop+98, 130, 20, idConnUser, 0x0081, "")

	item(wsChild|wsTabStop|bsPushButton, 10, pageTop+128, 120, 20, idConnConnectBtn, 0x0080, "Connect")

	// OK/Cancel: fixed position below both pages, always visible.
	const bottomY = pageTop + 168
	item(wsChild|wsVisible|wsTabStop|bsDefPushButton, 70, bottomY, 70, 20, idOK, 0x0080, "OK")
	item(wsChild|wsVisible|wsTabStop|bsPushButton, 160, bottomY, 70, 20, idCancelBtn, 0x0080, "Cancel")

	return buf.Bytes()
}

// onSettingsDialogOpen/onSettingsDialogClose are hooks devctl.go
// (aishwindev build tag only) replaces to track the currently-open
// Settings dialog's HWND for automated testing; a no-op in an ordinary
// build.
var onSettingsDialogOpen = func(hwnd syscall.Handle) {}
var onSettingsDialogClose = func() {}

// trimScrollback deletes the oldest lines from the log view once it
// exceeds the configured scrollback size. Called after every log-queue
// drain (gui.go), before drainLogQueue's own scrollToBottom (which -- now
// that scrolling is EM_LINESCROLL-based, not caret-based -- doesn't care
// what this function leaves the selection at, so this no longer needs to
// know or restore "were we following the bottom" itself).
func trimScrollback() {
	if hwndEdit == 0 {
		return
	}
	limit := settings.ScrollbackLines()
	if limit <= 0 {
		return
	}
	lineCount, _, _ := procSendMessageW.Call(uintptr(hwndEdit), emGetLineCount, 0, 0)
	excess := int(lineCount) - limit
	if excess <= 0 {
		return
	}
	cutIndex, _, _ := procSendMessageW.Call(uintptr(hwndEdit), emLineIndex, uintptr(excess), 0)

	procSendMessageW.Call(uintptr(hwndEdit), emSetSel, 0, cutIndex)
	setCaretTextColor(hwndEdit, true, 0)
	procSendMessageW.Call(uintptr(hwndEdit), emReplaceSel, 0, uintptr(unsafe.Pointer(utf16ptr(""))))
}
