package main

// gui_settings.go: the Settings dialog. General/Connection/Environment are
// real pages under a SysTabControl32 (comctl32) tab strip -- created at
// runtime in wmInitDialog as an ordinary child window (the same pattern
// gui.go uses for the RICHEDIT50W log view), not baked into the DLGTEMPLATE
// itself, since a named (non-predefined-atom) control class inside a
// hand-built DLGTEMPLATE is unproven in this codebase and safer to avoid;
// creating it as a plain child window after the dialog exists is the
// already-working pattern. The Environment page's SysListView32 (added
// alongside the console's former Env menu, which this replaced -- see
// realmenu.go's git history) follows the same runtime-child-window
// approach for the identical reason. aishwin.exe.manifest's
// Common-Controls v6 dependency is what makes both control classes (and
// their modern visual styles) available at all. Adding a future *General*
// setting means one more entry in buildSettingsFields; the Connection
// page's controls (a mode choice plus three text fields) and the
// Environment page's list+buttons don't fit that same label+edit shape, so
// they're laid out directly instead of through a generic list.
//
// Runtime-created child windows (the tab strip, the env list) are placed
// via dluToPixels rather than the raw numbers passed to CreateWindowExW:
// DLGTEMPLATE items are positioned in dialog units, which the dialog
// manager scales to pixels using the dialog's font, while CreateWindowExW
// takes literal pixels -- found live that a list view given the SAME
// number as its surrounding DLGTEMPLATE fields rendered visibly narrower
// than them (roughly 2/3 width), because a dialog unit is not a pixel
// (very roughly 1.5-2x for "MS Shell Dlg" at typical DPI).
//
// The Environment page edits in place rather than through a popup dialog:
// Add inserts a blank row and starts the list view's own built-in label
// edit on the Key cell (LVN_BEGINLABELEDIT/LVN_ENDLABELEDIT); committing
// that (Enter, Tab, or any other focus loss -- only Escape cancels)
// upper-cases/trims the text and opens a small custom EDIT control
// overlaid exactly on the Value cell's rect (SysListView32 has no built-in
// support for editing a subitem, only the item's own main/column-0 text).
// That overlay is subclassed (a genuine WNDPROC swap, since it's a plain
// control with no built-in "end editing" concept of its own) to commit on
// Enter/losing focus and cancel on Escape. Nothing is written to the
// access state until the value step commits -- Escape at any point (key
// or value step) discards the whole edit, and refreshEnvList's rebuild
// from access.entries() alone is enough to visually undo an in-progress,
// uncommitted change.

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

const idSettingsEditBase = 200 // general-page field edit-box IDs start here
const idGeneralLabelBase = 220 // general-page field label IDs start here -- distinct from edit IDs so each can be shown/hidden independently

// settingsPageTop is the Y coordinate every tab page's controls start at,
// leaving room above for the tab strip itself (created at 10,10,280,24 by
// createSettingsTab). Shared by buildSettingsDialogTemplate (General/
// Connection/Environment page layout) and createEnvList (the Environment
// page's runtime-created list view) so the two can't drift apart. Both
// are dialog units -- see dluToPixels.
const settingsPageTop = int16(40)

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

	idEnvList      = 320 // the SysListView32 itself, created at runtime (not part of the DLGTEMPLATE)
	idEnvAddBtn    = 321
	idEnvEditBtn   = 322
	idEnvDeleteBtn = 323
)

// ---- SysTabControl32/SysListView32 bindings (comctl32) ----
// Kept local to this file rather than win32.go since nothing else in the
// app uses a common control -- see the file-level comment on why these are
// runtime-created child windows rather than DLGTEMPLATE items.

var comctl32 = syscall.NewLazyDLL("comctl32.dll")
var procInitCommonControlsEx = comctl32.NewProc("InitCommonControlsEx")

type initCommonControlsEx struct {
	dwSize uint32
	dwICC  uint32
}

const (
	iccTabClasses      = 0x0008
	iccListviewClasses = 0x0001
)

var commonControlsOnce sync.Once

func ensureCommonControlsInit() {
	commonControlsOnce.Do(func() {
		icc := initCommonControlsEx{dwICC: iccTabClasses | iccListviewClasses}
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

// lvColumnW mirrors LVCOLUMNW (commctrl.h) through the fields this file
// uses (mask/fmt/cx/pszText/cchTextMax/iSubItem) -- the same
// stop-at-what-you-use approach tcItemW takes, safe here because comctl32
// only reads/writes fields gated by bits actually set in mask, and this
// file never sets LVCF_IMAGE/LVCF_ORDER/etc.
type lvColumnW struct {
	mask       uint32
	fmt        int32
	cx         int32
	pszText    *uint16
	cchTextMax int32
	iSubItem   int32
}

// lvItemW mirrors LVITEMW (commctrl.h) through the fields this file uses
// (mask/iItem/iSubItem/state/stateMask/pszText/cchTextMax/iImage/lParam) --
// same rationale as lvColumnW: fields past lParam (iIndent, iGroupId, ...)
// are only touched by comctl32 when their own mask bits are set, which
// this file never does.
type lvItemW struct {
	mask       uint32
	iItem      int32
	iSubItem   int32
	state      uint32
	stateMask  uint32
	pszText    *uint16
	cchTextMax int32
	iImage     int32
	lParam     uintptr
}

const (
	lvmFirst                    = 0x1000
	lvmGetItemCount             = lvmFirst + 4
	lvmInsertItemW              = lvmFirst + 77
	lvmDeleteAllItems           = lvmFirst + 9
	lvmInsertColumnW            = lvmFirst + 97
	lvmSetItemTextW             = lvmFirst + 116
	lvmGetItemTextW             = lvmFirst + 115
	lvmGetNextItem              = lvmFirst + 12
	lvmSetExtendedListViewStyle = lvmFirst + 54
	lvmSetItemState             = lvmFirst + 43 // used by devctl.go's envselect test command
	lvmEditLabelW               = lvmFirst + 118
	lvmEnsureVisible            = lvmFirst + 19
	lvmGetSubItemRect           = lvmFirst + 56
	lvmGetEditControl           = lvmFirst + 24
	lvmCancelEditLabel          = lvmFirst + 179

	lvnFirst           = -100
	lvnBeginLabelEditW = lvnFirst - 75
	lvnEndLabelEditW   = lvnFirst - 76

	nmFirst  = 0
	nmDblClk = nmFirst - 3

	lvsReport        = 0x0001
	lvsSingleSel     = 0x0004
	lvsShowSelAlways = 0x0008
	lvsEditLabels    = 0x0200 // required for LVM_EDITLABEL to do anything at all

	lvsExFullRowSelect = 0x00000020

	lvcfFmt     = 0x0001
	lvcfWidth   = 0x0002
	lvcfText    = 0x0004
	lvcfSubItem = 0x0008

	lvifText  = 0x0001
	lvifState = 0x0008

	lvniSelected = 0x0002

	lvisFocused  = 0x0001
	lvisSelected = 0x0002

	lvirBounds = 0
	lvirLabel  = 2
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

// dluToPixels converts a DLGTEMPLATE-style x,y,cx,cy (dialog units) into
// this dialog's actual client-pixel coordinates, via MapDialogRect -- the
// same conversion the dialog manager applies to every DLGTEMPLATE item.
// See the file-level comment for why this matters for runtime-created
// child windows.
func dluToPixels(hwndDlg syscall.Handle, x, y, cx, cy int16) (px, py, pcx, pcy int32) {
	r := rect{left: int32(x), top: int32(y), right: int32(x) + int32(cx), bottom: int32(y) + int32(cy)}
	procMapDialogRect.Call(uintptr(hwndDlg), uintptr(unsafe.Pointer(&r)))
	return r.left, r.top, r.right - r.left, r.bottom - r.top
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

// currentSettingsFields/currentSettingsPage/settingsTabHwnd/envListHwnd are
// valid only while a Settings dialog is open (one at a time, like the
// other custom dialogs in this package).
var currentSettingsFields []settingsField
var currentSettingsPage int // 0 = General, 1 = Connection, 2 = Environment
var settingsTabHwnd syscall.Handle
var envListHwnd syscall.Handle
var currentSettingsDlgHwnd syscall.Handle // target for the deferred wmEnvKeyEditDone/wmEnvValueEditDone messages below

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

// resetEnvEditState abandons any in-progress Environment-tab inline edit
// (the key edit, or the value overlay) without writing anything to the
// access state. Called whenever the Settings dialog itself is closing
// (idOK/idCancelBtn/wmClose) so a stray edit in progress at that moment
// can't leave a dangling overlay window or stale package-level state
// behind for the next time the dialog opens.
func resetEnvEditState() {
	if keyEditHwnd != 0 {
		unsubclassKeyEdit()
		procSendMessageW.Call(uintptr(envListHwnd), lvmCancelEditLabel, 0, 0)
	}
	destroyEnvValueEdit()
	currentEnvEdit = envRowEdit{}
	envValueEditCommitting = false
}

func settingsDialogProc(hwndDlg syscall.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmInitDialog:
		currentSettingsDlgHwnd = hwndDlg
		for i, f := range currentSettingsFields {
			editHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), uintptr(idSettingsEditBase+i))
			procSetWindowTextW.Call(editHwnd, uintptr(unsafe.Pointer(utf16ptr(f.get()))))
		}
		createSettingsTab(hwndDlg)
		populateConnectionFields(hwndDlg)
		createEnvList(hwndDlg)
		refreshEnvList()
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
		switch {
		case hdr.hwndFrom == settingsTabHwnd && int32(hdr.code) == tcnSelChange:
			sel, _, _ := procSendMessageW.Call(uintptr(settingsTabHwnd), tcmGetCurSel, 0, 0)
			showSettingsPage(hwndDlg, int(sel))
			return 1
		case hdr.hwndFrom == envListHwnd && int32(hdr.code) == lvnBeginLabelEditW:
			// Grab and subclass the list view's own temporary label-edit
			// control the instant it exists, rather than trusting its
			// default Enter/Tab/focus-loss behavior (see
			// keyEditSubclassProc's doc comment for why that trust turned
			// out to be misplaced), and resize it to match the Key
			// column's own width -- CreateWindow-style pixel placement
			// isn't ours to control here, comctl32 sizes it to the
			// item's text bounds by default, which is narrower than the
			// column.
			h, _, _ := procSendMessageW.Call(uintptr(envListHwnd), lvmGetEditControl, 0, 0)
			if h != 0 {
				keyEditHwnd = syscall.Handle(h)
				orig, _, _ := procGetWindowLongPtrW.Call(uintptr(keyEditHwnd), uintptr(gwlpWndProc))
				keyEditOrigProc = orig
				procSetWindowLongPtrW.Call(uintptr(keyEditHwnd), uintptr(gwlpWndProc), keyEditSubclassPtr)

				// LVM_GETSUBITEMRECT's documented quirk (see
				// ListView_GetSubItemRect in commctrl.h): the subitem
				// index goes in rect.top, and which kind of rect to
				// compute goes in rect.left.
				r := rect{top: 0, left: lvirLabel}
				procSendMessageW.Call(uintptr(envListHwnd), lvmGetSubItemRect, uintptr(currentEnvEdit.row), uintptr(unsafe.Pointer(&r)))
				procMoveWindow.Call(uintptr(keyEditHwnd), uintptr(r.left), uintptr(r.top), uintptr(r.right-r.left), uintptr(r.bottom-r.top), 1)
			}
			setDlgMsgResult(hwndDlg, 0) // allow editing
			return 1
		case hdr.hwndFrom == envListHwnd && int32(hdr.code) == lvnEndLabelEditW:
			// keyEditSubclassProc always ends editing itself (via
			// LVM_CANCELEDITLABEL, after already applying whatever it
			// decided to the item's text) before this notification can
			// fire naturally, so there's nothing left to do here except
			// give the DLGPROC a well-defined reply.
			setDlgMsgResult(hwndDlg, 0)
			return 1
		case hdr.hwndFrom == envListHwnd && int32(hdr.code) == nmDblClk:
			if !currentEnvEdit.active {
				beginEditSelectedEnvRow() // NM_DBLCLK fires after the click already selected the row
			}
			return 1
		}
	case wmEnvKeyEditDone:
		finishKeyEdit()
		return 1
	case wmEnvValueEditDone:
		finishValueEdit()
		return 1
	case wmCommand:
		id := uint16(wParam & 0xFFFF)
		if uint16(wParam>>16) != bnClicked {
			// Every case below means a button/radio was clicked, never
			// an edit control's EN_CHANGE/EN_UPDATE/EN_SETFOCUS/etc --
			// necessary, not just tidy: comctl32 hardcodes the id of the
			// list view's own temporary Key-cell label-edit control to 1,
			// the same value this dialog already uses for idOK, so typing
			// into that box sends WM_COMMAND(id=1, EN_UPDATE) here. Found
			// live: typing a single character in the Environment tab's
			// Add flow was silently closing the entire Settings dialog,
			// because nothing checked the notification code before
			// treating id==1 as an OK click.
			return 0
		}
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
		case idEnvAddBtn:
			if !currentEnvEdit.active {
				beginAddEnvRow()
			}
			return 1
		case idEnvEditBtn:
			if !currentEnvEdit.active {
				beginEditSelectedEnvRow()
			}
			return 1
		case idEnvDeleteBtn:
			key, _, ok := selectedEnvRow()
			if !ok {
				AppendLog("aishwin: select a variable to delete first")
				return 1
			}
			access.unsetEnv(key)
			refreshEnvList()
			AppendLog(fmt.Sprintf("aishwin: removed %s", key))
			return 1
		case idOK:
			resetEnvEditState()
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
			envListHwnd = 0
			currentSettingsDlgHwnd = 0
			onSettingsDialogClose()
			return 1
		case idCancelBtn:
			resetEnvEditState()
			procEndDialog.Call(uintptr(hwndDlg), 0)
			settingsTabHwnd = 0
			envListHwnd = 0
			currentSettingsDlgHwnd = 0
			onSettingsDialogClose()
			return 1
		}
	case wmClose:
		resetEnvEditState()
		procEndDialog.Call(uintptr(hwndDlg), 0)
		settingsTabHwnd = 0
		envListHwnd = 0
		currentSettingsDlgHwnd = 0
		onSettingsDialogClose()
		return 1
	}
	return 0
}

// createSettingsTab builds the SysTabControl32 strip as an ordinary child
// window of the already-created dialog (mirroring gui.go's RICHEDIT50W
// creation) and inserts its three items. Positioned above settingsPageTop
// -- see buildSettingsDialogTemplate.
func createSettingsTab(hwndDlg syscall.Handle) {
	inst := getModuleHandle()
	px, py, pcx, pcy := dluToPixels(hwndDlg, 10, 10, 280, 24)
	h, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(utf16ptr("SysTabControl32"))),
		uintptr(unsafe.Pointer(utf16ptr(""))),
		uintptr(wsChild|wsVisible|wsTabStop),
		uintptr(px), uintptr(py), uintptr(pcx), uintptr(pcy),
		uintptr(hwndDlg), uintptr(idSettingsTab), uintptr(inst), 0,
	)
	settingsTabHwnd = syscall.Handle(h)
	insertTabItem(settingsTabHwnd, 0, "General")
	insertTabItem(settingsTabHwnd, 1, "Connection")
	insertTabItem(settingsTabHwnd, 2, "Environment")
}

// createEnvList builds the Environment page's SysListView32 as a runtime
// child window (same rationale as createSettingsTab), in report mode with
// two columns (Key/Value) and full-row selection so clicking anywhere on a
// row selects it, not just the first column's cell. Column widths are
// derived from the control's actual (post-DLU-conversion) pixel width
// rather than hardcoded, so they always fill it exactly.
func createEnvList(hwndDlg syscall.Handle) {
	inst := getModuleHandle()
	px, py, pcx, pcy := dluToPixels(hwndDlg, 10, settingsPageTop, 270, 128)
	h, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(utf16ptr("SysListView32"))),
		uintptr(unsafe.Pointer(utf16ptr(""))),
		uintptr(wsChild|wsBorder|wsTabStop|lvsReport|lvsSingleSel|lvsShowSelAlways|lvsEditLabels),
		uintptr(px), uintptr(py), uintptr(pcx), uintptr(pcy),
		uintptr(hwndDlg), uintptr(idEnvList), uintptr(inst), 0,
	)
	envListHwnd = syscall.Handle(h)
	procSendMessageW.Call(uintptr(envListHwnd), lvmSetExtendedListViewStyle, 0, uintptr(lvsExFullRowSelect))

	var cr rect
	procGetClientRect.Call(uintptr(envListHwnd), uintptr(unsafe.Pointer(&cr)))
	width := cr.right - cr.left
	if width <= 0 {
		width = pcx // GetClientRect hasn't settled yet; fall back to the requested width
	}
	keyWidth := width * 35 / 100
	insertEnvColumn(0, "Key", keyWidth)
	insertEnvColumn(1, "Value", width-keyWidth)
}

func insertEnvColumn(index int, text string, width int32) {
	col := lvColumnW{
		mask:     lvcfFmt | lvcfWidth | lvcfText | lvcfSubItem,
		cx:       width,
		pszText:  utf16ptr(text),
		iSubItem: int32(index),
	}
	procSendMessageW.Call(uintptr(envListHwnd), lvmInsertColumnW, uintptr(index), uintptr(unsafe.Pointer(&col)))
}

// refreshEnvList clears and repopulates the Environment page's list from
// the current access state -- called on dialog open and after every
// committed add/edit/delete (and after a canceled in-place edit) so the
// list never shows anything but what's actually persisted.
func refreshEnvList() {
	if envListHwnd == 0 {
		return
	}
	procSendMessageW.Call(uintptr(envListHwnd), lvmDeleteAllItems, 0, 0)
	for i, e := range access.entries() {
		item := lvItemW{mask: lvifText, iItem: int32(i), pszText: utf16ptr(e.Key)}
		procSendMessageW.Call(uintptr(envListHwnd), lvmInsertItemW, 0, uintptr(unsafe.Pointer(&item)))
		setEnvCellText(i, 1, e.Value)
	}
}

func setEnvCellText(row, subItem int, text string) {
	item := lvItemW{mask: lvifText, iItem: int32(row), iSubItem: int32(subItem), pszText: utf16ptr(text)}
	procSendMessageW.Call(uintptr(envListHwnd), lvmSetItemTextW, uintptr(row), uintptr(unsafe.Pointer(&item)))
}

// selectedEnvIndex returns the currently selected row's index, or -1 if
// nothing is selected.
func selectedEnvIndex() int {
	if envListHwnd == 0 {
		return -1
	}
	idx, _, _ := procSendMessageW.Call(uintptr(envListHwnd), lvmGetNextItem, ^uintptr(0), uintptr(lvniSelected))
	return int(int32(idx))
}

// selectedEnvRow returns the currently selected row's key/value, and false
// if nothing is selected.
func selectedEnvRow() (key, value string, ok bool) {
	idx := selectedEnvIndex()
	if idx < 0 {
		return "", "", false
	}
	return envListItemText(idx, 0), envListItemText(idx, 1), true
}

func envListItemText(row, subItem int) string {
	buf := make([]uint16, 256)
	item := lvItemW{iSubItem: int32(subItem), pszText: &buf[0], cchTextMax: int32(len(buf))}
	n, _, _ := procSendMessageW.Call(uintptr(envListHwnd), lvmGetItemTextW, uintptr(row), uintptr(unsafe.Pointer(&item)))
	return syscall.UTF16ToString(buf[:n])
}

// envRowEdit tracks the two-step (key, then value) in-place edit flow for
// the Environment tab's list view: originalKey == "" means Add (a
// genuinely blank new row); a non-empty originalKey means Edit (an
// existing row, possibly having its key renamed). newKey is filled in
// once the key step commits, right before the value step begins.
type envRowEdit struct {
	active      bool
	row         int
	originalKey string
	newKey      string
}

var currentEnvEdit envRowEdit

// beginAddEnvRow inserts a blank row and starts the list view's own
// built-in label edit on its Key cell (LVN_BEGINLABELEDIT, handled in
// settingsDialogProc, subclasses the resulting edit control -- see
// keyEditSubclassProc).
func beginAddEnvRow() {
	count, _, _ := procSendMessageW.Call(uintptr(envListHwnd), lvmGetItemCount, 0, 0)
	row := int(count)
	item := lvItemW{mask: lvifText, iItem: int32(row), pszText: utf16ptr("")}
	procSendMessageW.Call(uintptr(envListHwnd), lvmInsertItemW, 0, uintptr(unsafe.Pointer(&item)))
	setEnvCellText(row, 1, "")
	currentEnvEdit = envRowEdit{active: true, row: row, originalKey: ""}
	procSendMessageW.Call(uintptr(envListHwnd), lvmEnsureVisible, uintptr(row), 0)
	procSetFocus.Call(uintptr(envListHwnd))
	procSendMessageW.Call(uintptr(envListHwnd), lvmEditLabelW, uintptr(row), 0)
}

// beginEditSelectedEnvRow starts the list view's own built-in label edit
// on the currently selected row's Key cell.
func beginEditSelectedEnvRow() {
	idx := selectedEnvIndex()
	if idx < 0 {
		AppendLog("aishwin: select a variable to edit first")
		return
	}
	currentEnvEdit = envRowEdit{active: true, row: idx, originalKey: envListItemText(idx, 0)}
	procSendMessageW.Call(uintptr(envListHwnd), lvmEnsureVisible, uintptr(idx), 0)
	procSetFocus.Call(uintptr(envListHwnd))
	procSendMessageW.Call(uintptr(envListHwnd), lvmEditLabelW, uintptr(idx), 0)
}

// keyEditHwnd/keyEditOrigProc are valid only while the list view's
// built-in Key-cell label edit control exists.
var (
	keyEditHwnd        syscall.Handle
	keyEditOrigProc    uintptr
	keyEditSubclassPtr = syscall.NewCallback(keyEditSubclassProc)
)

// keyEditSubclassProc is a genuine WNDPROC swap on the list view's own
// temporary label-edit control (installed from LVN_BEGINLABELEDIT). This
// control is NOT a dialog child (it's a child of the list view), so
// IsDialogMessage's normal Tab-navigation/default-button handling doesn't
// apply to it the way it would to a real dialog control -- found live
// that Tab did nothing (no way to reach the Value cell) and that relying
// on the control's own default Enter/Escape handling for LVN_ENDLABELEDIT
// correlated with the whole Settings dialog closing after a single
// keystroke (a DLGPROC must report a WM_NOTIFY reply via
// SetWindowLongPtr(DWLP_MSGRESULT), not its own return value -- see
// win32.go's dwlpMsgResult -- so the control was reading an undefined
// accept/reject result). Subclassing hands Tab/Enter/Escape entirely to
// this file's own logic instead. The actual teardown (LVM_CANCELEDITLABEL,
// which destroys this very control) is deferred to wmEnvKeyEditDone rather
// than done synchronously here, since this handler is itself running AS
// that control's WM_KEYDOWN processing -- destroying a window from deep
// inside its own message handling is the kind of reentrancy Win32 code
// conventionally avoids even where it happens to work.
func keyEditSubclassProc(hwnd syscall.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmGetDlgCode:
		// Belt-and-braces alongside envValueEditSubclassProc's identical
		// case: comctl32's own internal label-edit subclass may already
		// claim this, but our own WM_KEYDOWN handling must never depend
		// on that being true.
		r, _, _ := procCallWindowProcW.Call(keyEditOrigProc, uintptr(hwnd), uintptr(message), wParam, lParam)
		return r | dlgcWantAllKeys
	case wmKeyDown:
		switch wParam {
		case vkReturn, vkTab:
			deferKeyEditEnd(hwnd, true)
			return 0
		case vkEscape:
			deferKeyEditEnd(hwnd, false)
			return 0
		}
	case wmChar:
		// TranslateMessage synthesizes a WM_CHAR follow-up for each of the
		// WM_KEYDOWNs handled above (13/9/27) -- swallow it too, or it
		// reaches comctl32's OWN internal label-edit subclass underneath
		// ours (via the CallWindowProcW fallthrough below) and ends
		// editing its own way, racing deferKeyEditEnd's already-scheduled
		// wmEnvKeyEditDone. Found live: the dialog's Environment tab
		// accepted a typed key but never advanced to editing the Value
		// cell, because comctl32's own end-of-edit won that race.
		switch wParam {
		case vkReturn, vkTab, vkEscape:
			return 0
		}
	}
	r, _, _ := procCallWindowProcW.Call(keyEditOrigProc, uintptr(hwnd), uintptr(message), wParam, lParam)
	return r
}

func unsubclassKeyEdit() {
	if keyEditHwnd == 0 {
		return
	}
	procSetWindowLongPtrW.Call(uintptr(keyEditHwnd), uintptr(gwlpWndProc), keyEditOrigProc)
	keyEditHwnd = 0
	keyEditOrigProc = 0
}

// pendingKeyEditCommit/pendingKeyEditText carry a decision from
// deferKeyEditEnd (running inside the key edit's own WM_KEYDOWN handling)
// to finishKeyEdit (running afresh once the posted wmEnvKeyEditDone is
// dequeued, safely after that handling has fully returned).
var (
	pendingKeyEditCommit bool
	pendingKeyEditText   string
)

// deferKeyEditEnd reads the Key edit's current text directly (rather than
// trusting the list view's own label-edit commit path) while the control
// still exists, then posts wmEnvKeyEditDone to actually end editing and
// act on it once this call stack unwinds.
func deferKeyEditEnd(hwnd syscall.Handle, commit bool) {
	if commit {
		buf := make([]uint16, 256)
		n, _, _ := procSendMessageW.Call(uintptr(hwnd), wmGetText, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
		pendingKeyEditText = strings.ToUpper(strings.TrimSpace(syscall.UTF16ToString(buf[:n])))
	}
	pendingKeyEditCommit = commit
	procPostMessageW.Call(uintptr(currentSettingsDlgHwnd), wmEnvKeyEditDone, 0, 0)
}

// finishKeyEdit performs the actual teardown deferKeyEditEnd scheduled:
// ends the native label edit on our terms, then either abandons the row
// (Escape, or an empty key) or applies the committed key and starts
// editing the Value cell.
func finishKeyEdit() {
	if !currentEnvEdit.active || keyEditHwnd == 0 {
		return // already handled, e.g. the dialog closed in the meantime
	}
	row := currentEnvEdit.row
	unsubclassKeyEdit()
	procSendMessageW.Call(uintptr(envListHwnd), lvmCancelEditLabel, 0, 0)
	if !pendingKeyEditCommit || pendingKeyEditText == "" {
		// Escape, or cleared entirely: discard -- refreshEnvList rebuilds
		// from the untouched access state, which never had this row (Add)
		// or still has the old key text (Edit).
		currentEnvEdit = envRowEdit{}
		refreshEnvList()
		return
	}
	setEnvCellText(row, 0, pendingKeyEditText)
	beginValueEdit(row, pendingKeyEditText)
}

// envValueEditHwnd/envValueEditOrigProc/envValueEditCommitting are valid
// only while the Value-cell overlay edit control (below) exists.
var (
	envValueEditHwnd        syscall.Handle
	envValueEditOrigProc    uintptr
	envValueEditSubclassPtr = syscall.NewCallback(envValueEditSubclassProc)
	envValueEditCommitting  bool
)

// beginValueEdit creates a plain EDIT control positioned exactly over the
// Value cell of row (via LVM_GETSUBITEMRECT), pre-filled with its current
// text (or empty for a brand new row), selects all of it for convenient
// overwrite-by-typing, and subclasses it so Enter/losing focus commits and
// Escape cancels -- SysListView32 has no built-in notion of editing a
// subitem, only the item's own column-0 text, so this control (and all of
// its commit/cancel behavior) is entirely our own.
func beginValueEdit(row int, key string) {
	currentEnvEdit.newKey = key
	envValueEditCommitting = false

	procSendMessageW.Call(uintptr(envListHwnd), lvmEnsureVisible, uintptr(row), 0)

	// LVM_GETSUBITEMRECT's documented quirk (see ListView_GetSubItemRect
	// in commctrl.h): the subitem index goes in rect.top, and which kind
	// of rect to compute (LVIR_BOUNDS, the whole cell) goes in rect.left
	// -- neither is a normal message parameter.
	r := rect{top: 1, left: lvirBounds}
	procSendMessageW.Call(uintptr(envListHwnd), lvmGetSubItemRect, uintptr(row), uintptr(unsafe.Pointer(&r)))

	existing := ""
	if currentEnvEdit.originalKey != "" {
		existing = envListItemText(row, 1)
	}

	inst := getModuleHandle()
	h, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(utf16ptr("EDIT"))),
		uintptr(unsafe.Pointer(utf16ptr(existing))),
		uintptr(wsChild|wsVisible|wsBorder|esAutoHScroll),
		uintptr(r.left), uintptr(r.top), uintptr(r.right-r.left), uintptr(r.bottom-r.top),
		uintptr(envListHwnd), 0, uintptr(inst), 0,
	)
	envValueEditHwnd = syscall.Handle(h)

	orig, _, _ := procGetWindowLongPtrW.Call(uintptr(envValueEditHwnd), uintptr(gwlpWndProc))
	envValueEditOrigProc = orig
	procSetWindowLongPtrW.Call(uintptr(envValueEditHwnd), uintptr(gwlpWndProc), envValueEditSubclassPtr)

	procSendMessageW.Call(uintptr(envValueEditHwnd), emSetSel, 0, ^uintptr(0))
	procSetFocus.Call(uintptr(envValueEditHwnd))
}

// envValueEditSubclassProc is a genuine WNDPROC swap (GWLP_WNDPROC),
// necessary because the Value overlay is a plain EDIT control with no
// built-in "end editing" concept -- unlike the list view's own Key-cell
// label edit, which at least knows an edit is in progress. Every message
// not explicitly handled here is forwarded to the control's original
// window procedure via CallWindowProcW. The actual teardown (destroying
// this very control) is deferred to wmEnvValueEditDone rather than done
// synchronously here, for the same reentrancy reason as the Key edit's
// keyEditSubclassProc.
func envValueEditSubclassProc(hwnd syscall.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmGetDlgCode:
		// Without this, IsDialogMessage intercepts Enter itself (see
		// dlgcWantAllKeys's doc comment) and our WM_KEYDOWN case below
		// never runs at all.
		r, _, _ := procCallWindowProcW.Call(envValueEditOrigProc, uintptr(hwnd), uintptr(message), wParam, lParam)
		return r | dlgcWantAllKeys
	case wmKeyDown:
		switch wParam {
		case vkReturn, vkTab:
			deferValueEditEnd(hwnd, true)
			return 0
		case vkEscape:
			deferValueEditEnd(hwnd, false)
			return 0
		}
	case wmChar:
		// A plain EDIT control beeps on an unhandled WM_CHAR like Enter/
		// Escape/Tab; since WM_KEYDOWN above already acted on them,
		// swallow the synthesized follow-up so it doesn't also beep.
		switch wParam {
		case vkReturn, vkTab, vkEscape:
			return 0
		}
	case wmKillFocus:
		// Losing focus for ANY reason (Tab, a click elsewhere, clicking
		// OK) commits, same as a real input field.
		deferValueEditEnd(hwnd, true)
	}
	r, _, _ := procCallWindowProcW.Call(envValueEditOrigProc, uintptr(hwnd), uintptr(message), wParam, lParam)
	return r
}

func destroyEnvValueEdit() {
	if envValueEditHwnd == 0 {
		return
	}
	procDestroyWindow.Call(uintptr(envValueEditHwnd))
	envValueEditHwnd = 0
	envValueEditOrigProc = 0
}

// pendingValueEditCommit/pendingValueEditText carry a decision from
// deferValueEditEnd (running inside the overlay's own message handling)
// to finishValueEdit (running afresh once the posted wmEnvValueEditDone is
// dequeued).
var (
	pendingValueEditCommit bool
	pendingValueEditText   string
)

// deferValueEditEnd reads the overlay's current text (if committing) while
// it still exists, then posts wmEnvValueEditDone to actually tear it down
// and act on the decision once this call stack unwinds.
// envValueEditCommitting is set here (not just in finishValueEdit) because
// WM_KILLFOCUS can otherwise fire again during that later, deferred
// teardown and re-enter this function.
func deferValueEditEnd(hwnd syscall.Handle, commit bool) {
	if envValueEditHwnd == 0 || envValueEditCommitting {
		return
	}
	envValueEditCommitting = true
	if commit {
		buf := make([]uint16, 512)
		n, _, _ := procSendMessageW.Call(uintptr(hwnd), wmGetText, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
		pendingValueEditText = syscall.UTF16ToString(buf[:n])
	}
	pendingValueEditCommit = commit
	procPostMessageW.Call(uintptr(currentSettingsDlgHwnd), wmEnvValueEditDone, 0, 0)
}

// finishValueEdit performs the actual teardown deferValueEditEnd
// scheduled, then, if committing, finalizes the whole two-step edit: a
// rename (the key changed from what an Edit started with) removes the old
// key first, then the new/updated key=value is written for real,
// live-pushed to any running shell, and the list is rebuilt to show it in
// its correct sorted position. Escape (or the dialog closing first)
// leaves the access state untouched.
func finishValueEdit() {
	if envValueEditHwnd == 0 {
		return // already handled, e.g. the dialog closed in the meantime
	}
	edit := currentEnvEdit
	destroyEnvValueEdit()
	currentEnvEdit = envRowEdit{}
	envValueEditCommitting = false

	if !pendingValueEditCommit {
		refreshEnvList()
		return
	}
	if edit.originalKey != "" && edit.originalKey != edit.newKey {
		access.unsetEnv(edit.originalKey)
	}
	access.setEnv(edit.newKey, pendingValueEditText)
	pushLiveEnv(edit.newKey, pendingValueEditText)
	refreshEnvList()
	if edit.originalKey == "" {
		AppendLog(fmt.Sprintf("aishwin: set %s (applies to new commands now; already-running ones are unaffected)", edit.newKey))
	} else {
		AppendLog(fmt.Sprintf("aishwin: updated %s", edit.newKey))
	}
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
// pages'; the tab strip itself and OK/Cancel are always visible.
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
	envIDs := []uintptr{idEnvAddBtn, idEnvEditBtn, idEnvDeleteBtn}

	var showIDs, hideIDs []uintptr
	switch page {
	case 1:
		showIDs = connectionIDs
		hideIDs = append(append([]uintptr{}, generalIDs...), envIDs...)
	case 2:
		showIDs = envIDs
		hideIDs = append(append([]uintptr{}, generalIDs...), connectionIDs...)
	default:
		showIDs = generalIDs
		hideIDs = append(append([]uintptr{}, connectionIDs...), envIDs...)
	}
	for _, id := range hideIDs {
		h, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), id)
		procShowWindow.Call(h, swHide)
	}
	for _, id := range showIDs {
		h, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), id)
		procShowWindow.Call(h, swShow)
	}

	// envListHwnd is a runtime-created child, not a DLGTEMPLATE item, so it
	// isn't reachable via GetDlgItem/the show/hide loops above.
	if envListHwnd != 0 {
		visibility := uintptr(swHide)
		if page == 2 {
			visibility = swShow
		}
		procShowWindow.Call(uintptr(envListHwnd), visibility)
	}
}

// buildSettingsDialogTemplate assembles the whole Settings window: the
// General page (one label+edit row per field), Connection page (mode
// radios, host/port/user), and Environment page (Add/Edit/Delete buttons;
// the list view itself is a runtime child -- see createEnvList) all built
// into the same template with the Connection/Environment pages' controls
// initially not WS_VISIBLE (showSettingsPage corrects this again in
// wmInitDialog, but starting hidden avoids a one-frame flash of pages
// overlapping before that runs), then OK/Cancel at a fixed position below
// all three. The tab strip and list view are NOT part of this template --
// see createSettingsTab/createEnvList.
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
		w(x)
		w(y)
		w(cx)
		w(cy)
		w(id)
		w(uint16(0xFFFF))
		w(class)
		writeStr(text)
		w(uint16(0))
	}

	const dialogWidth = int16(300)
	const dialogHeight = int16(240)

	itemCount := uint16(len(fields) * 2)
	itemCount += 10 // connection page: label+2radios+3*(label+edit)+connect btn
	itemCount += 3  // environment page: add/edit/delete buttons
	itemCount += 2  // OK/Cancel

	style := uint32(dsSetFont | dsModalFrame | dsCenter | wsPopup | wsCaption | wsSysMenu)
	w(style)
	w(uint32(0))
	w(itemCount)
	w(int16(0))
	w(int16(0))
	w(dialogWidth)
	w(dialogHeight)
	w(uint16(0))
	w(uint16(0))
	writeStr(title)
	w(uint16(8))
	writeStr("MS Shell Dlg")

	// General page.
	y := settingsPageTop
	for i, f := range fields {
		item(wsChild|wsVisible, 10, y, 270, 12, uint16(idGeneralLabelBase+i), 0x0082, f.label)
		item(wsChild|wsVisible|wsBorder|wsTabStop|esAutoHScroll, 10, y+14, 270, 20, uint16(idSettingsEditBase+i), 0x0081, "")
		y += 36
	}

	// Connection page (created hidden -- no wsVisible -- corrected by
	// showSettingsPage as soon as the dialog opens).
	item(wsChild|wsGroup, 10, settingsPageTop, 270, 12, idLabelConnMode, 0x0082, "Connection mode:")
	item(wsChild|wsTabStop|wsGroup|bsAutoRadioButton, 10, settingsPageTop+14, 110, 20, idConnModeWSL, 0x0080, "WSL")
	item(wsChild|wsTabStop|bsAutoRadioButton, 130, settingsPageTop+14, 110, 20, idConnModeSSH, 0x0080, "SSH")

	item(wsChild, 10, settingsPageTop+42, 270, 12, idLabelHost, 0x0082, "Host / IP:")
	item(wsChild|wsBorder|wsTabStop|esAutoHScroll, 10, settingsPageTop+56, 270, 20, idConnHost, 0x0081, "")

	item(wsChild, 10, settingsPageTop+84, 120, 12, idLabelPort, 0x0082, "Port:")
	item(wsChild|wsBorder|wsTabStop|esAutoHScroll, 10, settingsPageTop+98, 120, 20, idConnPort, 0x0081, "")

	item(wsChild, 150, settingsPageTop+84, 130, 12, idLabelUser, 0x0082, "Username:")
	item(wsChild|wsBorder|wsTabStop|esAutoHScroll, 150, settingsPageTop+98, 130, 20, idConnUser, 0x0081, "")

	item(wsChild|wsTabStop|bsPushButton, 10, settingsPageTop+128, 120, 20, idConnConnectBtn, 0x0080, "Connect")

	// Environment page (created hidden, same rationale as Connection
	// above). The list view itself sits above these buttons but is a
	// runtime child window -- see createEnvList -- not a template item.
	item(wsChild|wsTabStop|bsPushButton, 10, settingsPageTop+136, 84, 20, idEnvAddBtn, 0x0080, "Add")
	item(wsChild|wsTabStop|bsPushButton, 100, settingsPageTop+136, 84, 20, idEnvEditBtn, 0x0080, "Edit")
	item(wsChild|wsTabStop|bsPushButton, 190, settingsPageTop+136, 84, 20, idEnvDeleteBtn, 0x0080, "Delete")

	// OK/Cancel: fixed position below all three pages, always visible.
	const bottomY = settingsPageTop + 168
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
