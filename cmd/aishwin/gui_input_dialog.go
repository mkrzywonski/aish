package main

// gui_input_dialog.go: a modal single-line text-input dialog (OK/Cancel),
// used by the native menu's Rename/Set-env-var actions. Unlike AskYesNo
// (invoked from other goroutines via RunOnUIThread), AskText is only ever
// called from a menu click handler, which mainWndProc already runs on the
// GUI's own thread -- so it calls DialogBoxIndirectParamW directly.

import (
	"bytes"
	"encoding/binary"
	"syscall"
	"unsafe"
)

const (
	idOK        = 1 // matches the real IDOK -- the dialog manager auto-invokes it on Enter for free
	idCancelBtn = 2 // matches the real IDCANCEL -- ditto for Escape
	idInputEdit = 101

	esAutoHScroll = 0x0080
	wmGetText     = 0x000D
)

var textDialogProcPtr = syscall.NewCallback(textDialogProc)

// textDialogDefault/textDialogResult are safe as package globals because
// AskText only ever runs on the single GUI thread, one call at a time
// (a menu action can't be re-entered while its own dialog is still modal).
var textDialogDefault string
var textDialogResult string

// onTextDialogOpen/onTextDialogClose are hooks devctl.go (aishwindev build
// tag only) replaces to track the currently-open text dialog's HWND, so a
// dev command can set its text and click OK/Cancel programmatically; a
// no-op in an ordinary build.
var onTextDialogOpen = func(hwnd syscall.Handle) {}
var onTextDialogClose = func() {}

// AskText shows a modal text-input dialog and returns the entered text and
// true, or ("", false) if the user cancels.
func AskText(title, prompt, defaultValue string) (string, bool) {
	textDialogDefault = defaultValue
	textDialogResult = ""

	tmpl := buildTextDialogTemplate(title, prompt)
	inst := getModuleHandle()
	r, _, _ := procDialogBoxIndirectParamW.Call(
		uintptr(inst),
		uintptr(unsafe.Pointer(&tmpl[0])),
		uintptr(hwndMain),
		textDialogProcPtr,
		0,
	)
	if r != 1 {
		return "", false
	}
	return textDialogResult, true
}

func textDialogProc(hwndDlg syscall.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmInitDialog:
		editHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), idInputEdit)
		procSetWindowTextW.Call(editHwnd, uintptr(unsafe.Pointer(utf16ptr(textDialogDefault))))
		procSetForegroundWin.Call(uintptr(hwndDlg))
		procSetFocus.Call(editHwnd)
		onTextDialogOpen(hwndDlg)
		return 0 // we set focus ourselves; returning nonzero would override it
	case wmCommand:
		id := uint16(wParam & 0xFFFF)
		switch id {
		case idOK:
			editHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), idInputEdit)
			buf := make([]uint16, 512)
			n, _, _ := procSendMessageW.Call(editHwnd, wmGetText, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
			textDialogResult = syscall.UTF16ToString(buf[:n])
			procEndDialog.Call(uintptr(hwndDlg), 1)
			onTextDialogClose()
			return 1
		case idCancelBtn:
			procEndDialog.Call(uintptr(hwndDlg), 0)
			onTextDialogClose()
			return 1
		}
	case wmClose:
		procEndDialog.Call(uintptr(hwndDlg), 0)
		onTextDialogClose()
		return 1
	}
	return 0
}

// buildTextDialogTemplate assembles a label, an edit box, and OK/Cancel
// buttons -- structurally the same in-memory DLGTEMPLATE technique as
// buildYesNoDialogTemplate (gui_dialog.go).
func buildTextDialogTemplate(title, prompt string) []byte {
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

	style := uint32(dsSetFont | dsModalFrame | dsCenter | wsPopup | wsCaption | wsSysMenu)
	w(style)
	w(uint32(0))
	w(uint16(4)) // label, edit, OK, Cancel
	w(int16(0)); w(int16(0)); w(int16(260)); w(int16(110))
	w(uint16(0))
	w(uint16(0))
	writeStr(title)
	w(uint16(8))
	writeStr("MS Shell Dlg")

	align4()
	w(uint32(wsChild | wsVisible))
	w(uint32(0))
	w(int16(10)); w(int16(10)); w(int16(240)); w(int16(20))
	w(uint16(idStaticText))
	w(uint16(0xFFFF)); w(uint16(0x0082)) // STATIC
	writeStr(prompt)
	w(uint16(0))

	align4()
	w(uint32(wsChild | wsVisible | wsBorder | wsTabStop | esAutoHScroll))
	w(uint32(0))
	w(int16(10)); w(int16(35)); w(int16(240)); w(int16(20))
	w(uint16(idInputEdit))
	w(uint16(0xFFFF)); w(uint16(0x0081)) // EDIT
	writeStr("")
	w(uint16(0))

	align4()
	w(uint32(wsChild | wsVisible | wsTabStop | bsDefPushButton))
	w(uint32(0))
	w(int16(60)); w(int16(70)); w(int16(60)); w(int16(20))
	w(uint16(idOK))
	w(uint16(0xFFFF)); w(uint16(0x0080)) // BUTTON
	writeStr("OK")
	w(uint16(0))

	align4()
	w(uint32(wsChild | wsVisible | wsTabStop | bsPushButton))
	w(uint32(0))
	w(int16(150)); w(int16(70)); w(int16(60)); w(int16(20))
	w(uint16(idCancelBtn))
	w(uint16(0xFFFF)); w(uint16(0x0080)) // BUTTON
	writeStr("Cancel")
	w(uint16(0))

	return buf.Bytes()
}
