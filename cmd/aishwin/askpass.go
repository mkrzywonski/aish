package main

// askpass.go: aishwin.exe's hidden SSH_ASKPASS helper mode.
//
// spawnSSH's ssh child talks the aishwinwire protocol over piped
// stdin/stdout, so its stdin is never a real console -- when the remote
// host needs a password, a passphrase, or a host-key yes/no confirmation,
// ssh can't fall back to reading the tty directly. spawnSSH instead points
// SSH_ASKPASS at this same exe and sets AISHWIN_ASKPASS=1 in ssh's
// environment (inherited by whatever child ssh execs to ask); main()
// checks for that variable before any flag parsing -- ssh's prompt text is
// passed as a plain positional argument and would otherwise collide with
// flag.Parse -- and hands off to runAskPass, which is this process
// invocation's entire job: pop one modal dialog, print the answer to
// stdout, and exit. No GUI has been started (no hwndMain, no log window,
// no menu) since this instance never reaches StartGUI.
//
// The same dialog serves passwords/passphrases and host-key confirmations:
// ssh routes both through SSH_ASKPASS identically, passing the full prompt
// text (a multi-line fingerprint message, for host-key confirmation) as
// the single argument. The input box stays password-masked in both cases
// -- harmless for typing "yes", and it means a real password or passphrase
// is never displayed in the clear.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

const esPassword = 0x0020

var (
	askPassDialogProcPtr = syscall.NewCallback(askPassDialogProc)
	askPassResult        string
)

// runAskPass is invoked from main() instead of the normal GUI startup path
// when AISHWIN_ASKPASS=1 is set. It returns the process exit code: 0 with
// the answer on stdout if the user submitted the dialog, 1 (nothing on
// stdout) if they canceled -- ssh treats a nonzero exit / empty answer as a
// failed or aborted auth attempt rather than looping on an empty password.
func runAskPass(prompt string) int {
	if prompt == "" {
		prompt = "Password:"
	}
	answer, ok := askPasswordDialog("aishwin: SSH login", prompt)
	if !ok {
		return 1
	}
	// Raw os.Stdout, not the package's crlf-translating stdout: this pipe
	// is read back by ssh as the literal answer, never displayed on a
	// console, so it must carry exactly what was typed plus one newline.
	fmt.Fprintln(os.Stdout, answer)
	return 0
}

func askPasswordDialog(title, prompt string) (string, bool) {
	askPassResult = ""

	tmpl := buildPasswordDialogTemplate(title, prompt)
	inst := getModuleHandle()
	r, _, _ := procDialogBoxIndirectParamW.Call(
		uintptr(inst),
		uintptr(unsafe.Pointer(&tmpl[0])),
		0, // no owner -- this process instance never creates hwndMain
		askPassDialogProcPtr,
		0,
	)
	if r != 1 {
		return "", false
	}
	return askPassResult, true
}

func askPassDialogProc(hwndDlg syscall.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmInitDialog:
		editHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), idInputEdit)
		procSetForegroundWin.Call(uintptr(hwndDlg))
		procSetFocus.Call(editHwnd)
		return 0 // we set focus ourselves; returning nonzero would override it
	case wmCommand:
		id := uint16(wParam & 0xFFFF)
		switch id {
		case idOK:
			editHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), idInputEdit)
			buf := make([]uint16, 512)
			n, _, _ := procSendMessageW.Call(editHwnd, wmGetText, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
			askPassResult = syscall.UTF16ToString(buf[:n])
			procEndDialog.Call(uintptr(hwndDlg), 1)
			return 1
		case idCancelBtn:
			procEndDialog.Call(uintptr(hwndDlg), 0)
			return 1
		}
	case wmClose:
		procEndDialog.Call(uintptr(hwndDlg), 0)
		return 1
	}
	return 0
}

// buildPasswordDialogTemplate is structurally the same in-memory DLGTEMPLATE
// technique as buildTextDialogTemplate (gui_input_dialog.go), with a taller
// label to fit a multi-line host-key fingerprint message, and with
// esPassword set on the edit control. \n is normalized to \r\n first: the
// STATIC control only honors explicit line breaks in that form, and ssh
// assembles its host-key message with bare \n.
//
// The label/edit/button y-coordinates and the overall dialog height must
// stay consistent by construction -- found live (a real screenshot) that an
// earlier version's OK/Cancel row sat past the declared dialog height
// entirely (buttons clipped off the bottom of the window), while the label
// was far taller than any single-line password prompt ever needs (a wall
// of blank space above the edit box). Both were just independently wrong
// numbers, not a genuine layout constraint -- this version defines one
// bottom Y and derives the dialog height from it instead of hand-picking
// both separately.
func buildPasswordDialogTemplate(title, prompt string) []byte {
	prompt = strings.ReplaceAll(prompt, "\r\n", "\n")
	prompt = strings.ReplaceAll(prompt, "\n", "\r\n")

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

	const (
		dialogWidth = int16(300)
		labelTop    = int16(10)
		labelHeight = int16(70) // enough for a several-line host-key message; a one-line password prompt just leaves it mostly blank
		editTop     = labelTop + labelHeight + 10
		editHeight  = int16(20)
		btnTop      = editTop + editHeight + 10
		btnHeight   = int16(20)
		dialogHeight = btnTop + btnHeight + 15 // bottom margin below the buttons
	)

	style := uint32(dsSetFont | dsModalFrame | dsCenter | wsPopup | wsCaption | wsSysMenu)
	w(style)
	w(uint32(0))
	w(uint16(4)) // label, edit, OK, Cancel
	w(int16(0)); w(int16(0)); w(dialogWidth); w(dialogHeight)
	w(uint16(0))
	w(uint16(0))
	writeStr(title)
	w(uint16(8))
	writeStr("MS Shell Dlg")

	align4()
	w(uint32(wsChild | wsVisible))
	w(uint32(0))
	w(int16(10)); w(labelTop); w(int16(280)); w(labelHeight)
	w(uint16(idStaticText))
	w(uint16(0xFFFF)); w(uint16(0x0082)) // STATIC
	writeStr(prompt)
	w(uint16(0))

	align4()
	w(uint32(wsChild | wsVisible | wsBorder | wsTabStop | esAutoHScroll | esPassword))
	w(uint32(0))
	w(int16(10)); w(editTop); w(int16(280)); w(editHeight)
	w(uint16(idInputEdit))
	w(uint16(0xFFFF)); w(uint16(0x0081)) // EDIT
	writeStr("")
	w(uint16(0))

	align4()
	w(uint32(wsChild | wsVisible | wsTabStop | bsDefPushButton))
	w(uint32(0))
	w(int16(70)); w(btnTop); w(int16(60)); w(btnHeight)
	w(uint16(idOK))
	w(uint16(0xFFFF)); w(uint16(0x0080)) // BUTTON
	writeStr("OK")
	w(uint16(0))

	align4()
	w(uint32(wsChild | wsVisible | wsTabStop | bsPushButton))
	w(uint32(0))
	w(int16(160)); w(btnTop); w(int16(60)); w(btnHeight)
	w(uint16(idCancelBtn))
	w(uint16(0xFFFF)); w(uint16(0x0080)) // BUTTON
	writeStr("Cancel")
	w(uint16(0))

	return buf.Bytes()
}
