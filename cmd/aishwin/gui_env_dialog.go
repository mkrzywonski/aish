package main

// gui_env_dialog.go: a modal two-field (Name/Value) dialog for adding or
// editing one custom environment variable, replacing the Environment tab's
// former in-place list-view label editing. Modeled on AskText
// (gui_input_dialog.go): the same in-memory DLGTEMPLATE technique and the
// same single-GUI-thread, one-modal-at-a-time invariant (it is opened only
// from a Settings-dialog button or double-click handler, which mainWndProc
// already runs on the GUI thread, so DialogBoxIndirectParamW is called
// directly). It adds a second edit field, name validation, and an inline
// error line -- a standard, accessible pattern (labeled EDIT controls,
// tab/Enter/Escape handled by the dialog manager) rather than the fragile
// two-step label edit it replaces.

import (
	"bytes"
	"encoding/binary"
	"strings"
	"syscall"
	"unsafe"
)

const (
	idEnvNameEdit  = 201
	idEnvValueEdit = 202
	idEnvErrText   = 203
)

var envDialogProcPtr = syscall.NewCallback(envDialogProc)

// envDialogNameIn/ValueIn carry the pre-fill into the dialog; envDialogName/
// Value carry the result back out. Safe as package globals for the same
// reason AskText's are: exactly one modal call at a time on the single GUI
// thread (a Settings-button handler can't be re-entered while its own child
// dialog is still modal).
var (
	envDialogNameIn  string
	envDialogValueIn string
	envDialogName    string
	envDialogValue   string
)

// onEnvDialogOpen/onEnvDialogClose are hooks devctl.go (aishwindev build
// tag only) replaces to track the currently-open dialog's HWND so a dev
// command can fill the fields and click OK/Cancel programmatically; no-ops
// in an ordinary build.
var onEnvDialogOpen = func(hwnd syscall.Handle) {}
var onEnvDialogClose = func() {}

// AskEnvVar shows the modal Add/Edit Variable dialog pre-filled with
// nameDefault/valueDefault (both empty for a fresh Add). It returns the
// entered name (trimmed and uppercased, matching how env keys are stored)
// and value with ok=true, or ("", "", false) if the user cancels. An
// invalid name (empty, or containing '=') keeps the dialog open with an
// inline message instead of returning.
func AskEnvVar(title, nameDefault, valueDefault string) (name, value string, ok bool) {
	envDialogNameIn = nameDefault
	envDialogValueIn = valueDefault
	envDialogName = ""
	envDialogValue = ""

	tmpl := buildEnvDialogTemplate(title)
	inst := getModuleHandle()
	r, _, _ := procDialogBoxIndirectParamW.Call(
		uintptr(inst),
		uintptr(unsafe.Pointer(&tmpl[0])),
		uintptr(hwndMain),
		envDialogProcPtr,
		0,
	)
	if r != 1 {
		return "", "", false
	}
	return envDialogName, envDialogValue, true
}

func envDialogProc(hwndDlg syscall.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmInitDialog:
		nameHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), idEnvNameEdit)
		procSetWindowTextW.Call(nameHwnd, uintptr(unsafe.Pointer(utf16ptr(envDialogNameIn))))
		valueHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), idEnvValueEdit)
		procSetWindowTextW.Call(valueHwnd, uintptr(unsafe.Pointer(utf16ptr(envDialogValueIn))))
		procSetForegroundWin.Call(uintptr(hwndDlg))
		// Add (empty name) focuses Name; Edit (name already set) focuses
		// Value with its text selected, since that's the field most likely
		// being changed.
		if envDialogNameIn == "" {
			procSetFocus.Call(nameHwnd)
		} else {
			procSetFocus.Call(valueHwnd)
			procSendMessageW.Call(valueHwnd, emSetSel, 0, ^uintptr(0))
		}
		onEnvDialogOpen(hwndDlg)
		return 0 // focus was set here; a nonzero return would override it
	case wmCommand:
		id := uint16(wParam & 0xFFFF)
		switch id {
		case idOK:
			nameHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), idEnvNameEdit)
			name := strings.ToUpper(strings.TrimSpace(envDialogFieldText(nameHwnd)))
			valueHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), idEnvValueEdit)
			value := envDialogFieldText(valueHwnd)
			if msg := validateEnvName(name); msg != "" {
				errHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), idEnvErrText)
				procSetWindowTextW.Call(errHwnd, uintptr(unsafe.Pointer(utf16ptr(msg))))
				procSetFocus.Call(nameHwnd)
				return 1 // reject: keep the dialog open with the message shown
			}
			envDialogName = name
			envDialogValue = value
			procEndDialog.Call(uintptr(hwndDlg), 1)
			onEnvDialogClose()
			return 1
		case idCancelBtn:
			procEndDialog.Call(uintptr(hwndDlg), 0)
			onEnvDialogClose()
			return 1
		}
	case wmClose:
		procEndDialog.Call(uintptr(hwndDlg), 0)
		onEnvDialogClose()
		return 1
	}
	return 0
}

// validateEnvName returns an empty string if name is a usable Windows
// environment-variable name, or a human-readable reason if not. The caller
// has already trimmed and uppercased it; this rejects an empty name and any
// '=' (which delimits name from value in the KEY=VALUE form and so can't
// appear in a name).
func validateEnvName(name string) string {
	if name == "" {
		return "Name can't be empty."
	}
	if strings.ContainsRune(name, '=') {
		return "Name can't contain '='."
	}
	return ""
}

func envDialogFieldText(hwnd uintptr) string {
	buf := make([]uint16, 1024)
	n, _, _ := procSendMessageW.Call(hwnd, wmGetText, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	return syscall.UTF16ToString(buf[:n])
}

// buildEnvDialogTemplate assembles two labeled edit fields, an (initially
// empty) inline error line, and OK/Cancel buttons -- structurally the same
// in-memory DLGTEMPLATE technique as buildTextDialogTemplate
// (gui_input_dialog.go), just with the extra Value row and error line.
func buildEnvDialogTemplate(title string) []byte {
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
	// control emits one DLGITEMTEMPLATE: style, exStyle, x, y, cx, cy, id,
	// class atom (0xFFFF + which), title text, and a zero creation-data
	// word -- the exact shape buildTextDialogTemplate uses per control.
	control := func(style uint32, x, y, cx, cy int16, id uint16, class uint16, text string) {
		align4()
		w(style)
		w(uint32(0))
		w(x); w(y); w(cx); w(cy)
		w(id)
		w(uint16(0xFFFF)); w(class)
		writeStr(text)
		w(uint16(0))
	}

	const (
		classStatic = 0x0082
		classEdit   = 0x0081
		classButton = 0x0080
	)

	style := uint32(dsSetFont | dsModalFrame | dsCenter | wsPopup | wsCaption | wsSysMenu)
	w(style)
	w(uint32(0))
	w(uint16(7)) // name label, name edit, value label, value edit, error, OK, Cancel
	w(int16(0)); w(int16(0)); w(int16(260)); w(int16(124))
	w(uint16(0))
	w(uint16(0))
	writeStr(title)
	w(uint16(8))
	writeStr("MS Shell Dlg")

	labelStyle := uint32(wsChild | wsVisible)
	editStyle := uint32(wsChild | wsVisible | wsBorder | wsTabStop | esAutoHScroll)

	control(labelStyle, 10, 12, 40, 12, idStaticText, classStatic, "Name:")
	control(editStyle, 55, 10, 195, 14, idEnvNameEdit, classEdit, "")
	control(labelStyle, 10, 34, 40, 12, idStaticText, classStatic, "Value:")
	control(editStyle, 55, 32, 195, 14, idEnvValueEdit, classEdit, "")
	control(labelStyle, 10, 54, 240, 18, idEnvErrText, classStatic, "")
	control(wsChild|wsVisible|wsTabStop|bsDefPushButton, 60, 96, 60, 18, idOK, classButton, "OK")
	control(wsChild|wsVisible|wsTabStop|bsPushButton, 140, 96, 60, 18, idCancelBtn, classButton, "Cancel")

	return buf.Bytes()
}
