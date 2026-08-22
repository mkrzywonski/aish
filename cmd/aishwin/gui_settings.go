package main

// gui_settings.go: an extensible Settings dialog. Adding a future setting
// means adding one more entry to buildSettingsFields -- the dialog
// template, field population, and OK-time apply/validation all follow
// from the list automatically, rather than each setting needing its own
// hand-built dialog.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const idSettingsEditBase = 200 // settings dialog field edit-box IDs start here; one dialog at a time so reuse across calls is fine

// settingsField is one labeled, editable row in the Settings dialog.
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

// currentSettingsFields is valid only while a Settings dialog is open (one
// at a time, like the other custom dialogs in this package).
var currentSettingsFields []settingsField

// ShowSettingsDialog displays the modal Settings window. Must be called
// from the GUI's own thread (a menu click handler, which mainWndProc
// already runs there).
func ShowSettingsDialog() {
	currentSettingsFields = buildSettingsFields()
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
		procSetForegroundWin.Call(uintptr(hwndDlg))
		onSettingsDialogOpen(hwndDlg)
		return 1
	case wmCommand:
		id := uint16(wParam & 0xFFFF)
		switch id {
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
			if len(errs) > 0 {
				AppendLog("aishwin: some settings were not applied: " + strings.Join(errs, "; "))
			} else {
				AppendLog("aishwin: settings updated")
			}
			procEndDialog.Call(uintptr(hwndDlg), 1)
			onSettingsDialogClose()
			return 1
		case idCancelBtn:
			procEndDialog.Call(uintptr(hwndDlg), 0)
			onSettingsDialogClose()
			return 1
		}
	case wmClose:
		procEndDialog.Call(uintptr(hwndDlg), 0)
		onSettingsDialogClose()
		return 1
	}
	return 0
}

// buildSettingsDialogTemplate assembles one label+edit row per field,
// followed by OK/Cancel -- the same in-memory DLGTEMPLATE technique as
// gui_dialog.go/gui_input_dialog.go's dialogs, just built from a list
// instead of a fixed layout so future settings don't need a new template.
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

	const rowHeight = int16(36)
	const topMargin = int16(10)
	const dialogWidth = int16(280)
	dialogHeight := topMargin + rowHeight*int16(len(fields)) + 34

	style := uint32(dsSetFont | dsModalFrame | dsCenter | wsPopup | wsCaption | wsSysMenu)
	w(style)
	w(uint32(0))
	w(uint16(len(fields)*2 + 2)) // label+edit per field, plus OK and Cancel
	w(int16(0)); w(int16(0)); w(dialogWidth); w(dialogHeight)
	w(uint16(0))
	w(uint16(0))
	writeStr(title)
	w(uint16(8))
	writeStr("MS Shell Dlg")

	y := topMargin
	for i, f := range fields {
		align4()
		w(uint32(wsChild | wsVisible))
		w(uint32(0))
		w(int16(10)); w(y); w(int16(240)); w(int16(12))
		w(uint16(idStaticText))
		w(uint16(0xFFFF)); w(uint16(0x0082)) // STATIC
		writeStr(f.label)
		w(uint16(0))

		align4()
		w(uint32(wsChild | wsVisible | wsBorder | wsTabStop | esAutoHScroll))
		w(uint32(0))
		w(int16(10)); w(y + 14); w(int16(240)); w(int16(20))
		w(uint16(idSettingsEditBase + i))
		w(uint16(0xFFFF)); w(uint16(0x0081)) // EDIT
		writeStr("")
		w(uint16(0))

		y += rowHeight
	}

	align4()
	w(uint32(wsChild | wsVisible | wsTabStop | bsDefPushButton))
	w(uint32(0))
	w(int16(60)); w(y + 4); w(int16(60)); w(int16(20))
	w(uint16(idOK))
	w(uint16(0xFFFF)); w(uint16(0x0080)) // BUTTON
	writeStr("OK")
	w(uint16(0))

	align4()
	w(uint32(wsChild | wsVisible | wsTabStop | bsPushButton))
	w(uint32(0))
	w(int16(150)); w(y + 4); w(int16(60)); w(int16(20))
	w(uint16(idCancelBtn))
	w(uint16(0xFFFF)); w(uint16(0x0080)) // BUTTON
	writeStr("Cancel")
	w(uint16(0))

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
