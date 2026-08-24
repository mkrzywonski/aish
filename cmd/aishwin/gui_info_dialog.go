//go:build windows

package main

// gui_info_dialog.go: a modal informational dialog (one OK button),
// replacing ShowInfo's earlier use of a plain MessageBoxW. MessageBoxW's
// window isn't reachable through this app's own DLGTEMPLATE/
// DialogBoxIndirectParamW machinery, so devctl.go (aishwindev build tag)
// had no way to track or dismiss it -- found live when triggering
// Help>About via devctl left a real dialog open on screen with no way to
// close it except a human clicking OK or killing the whole process. Built
// from the same in-memory DLGTEMPLATE technique as AskYesNo/AskText
// (gui_dialog.go/gui_input_dialog.go), it gets the identical open/close
// hook pair those already have, for the same reason.

import (
	"bytes"
	"encoding/binary"
	"syscall"
	"unsafe"
)

var infoDialogProcPtr = syscall.NewCallback(infoDialogProc)

// onInfoDialogOpen/onInfoDialogClose are hooks devctl.go (aishwindev build
// tag only) replaces to track the currently-open info dialog's HWND, so a
// dev command can click its OK button programmatically; a no-op in an
// ordinary build.
var onInfoDialogOpen = func(hwnd syscall.Handle) {}
var onInfoDialogClose = func() {}

// ShowInfo displays a modal, owned informational dialog with an OK button.
// Unlike AskYesNo/AskText, this has no wire deadline to respect -- it's
// only ever triggered by a human clicking a menu item -- so blocking here
// until it's dismissed is fine. Must be called from the GUI's own thread
// (a menu click handler, which mainWndProc already runs there).
func ShowInfo(title, text string) {
	tmpl := buildInfoDialogTemplate(title, text)
	inst := getModuleHandle()
	procDialogBoxIndirectParamW.Call(
		uintptr(inst),
		uintptr(unsafe.Pointer(&tmpl[0])),
		uintptr(hwndMain),
		infoDialogProcPtr,
		0,
	)
}

func infoDialogProc(hwndDlg syscall.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmInitDialog:
		procSetForegroundWin.Call(uintptr(hwndDlg))
		onInfoDialogOpen(hwndDlg)
		return 1
	case wmCommand:
		id := uint16(wParam & 0xFFFF)
		if id == idOK {
			procEndDialog.Call(uintptr(hwndDlg), 1)
			onInfoDialogClose()
			return 1
		}
	case wmClose:
		procEndDialog.Call(uintptr(hwndDlg), 0)
		onInfoDialogClose()
		return 1
	}
	return 0
}

// buildInfoDialogTemplate assembles a static text label and a single OK
// button -- structurally the same in-memory DLGTEMPLATE technique as
// buildYesNoDialogTemplate/buildTextDialogTemplate.
func buildInfoDialogTemplate(title, text string) []byte {
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
	w(uint16(2)) // label, OK
	w(int16(0))
	w(int16(0))
	w(int16(260))
	w(int16(100))
	w(uint16(0))
	w(uint16(0))
	writeStr(title)
	w(uint16(8))
	writeStr("MS Shell Dlg")

	align4()
	w(uint32(wsChild | wsVisible))
	w(uint32(0))
	w(int16(10))
	w(int16(10))
	w(int16(240))
	w(int16(40))
	w(uint16(idStaticText))
	w(uint16(0xFFFF))
	w(uint16(0x0082)) // STATIC
	writeStr(text)
	w(uint16(0))

	align4()
	w(uint32(wsChild | wsVisible | wsTabStop | bsDefPushButton))
	w(uint32(0))
	w(int16(100))
	w(int16(65))
	w(int16(60))
	w(int16(20))
	w(uint16(idOK))
	w(uint16(0xFFFF))
	w(uint16(0x0080)) // BUTTON
	writeStr("OK")
	w(uint16(0))

	return buf.Bytes()
}
