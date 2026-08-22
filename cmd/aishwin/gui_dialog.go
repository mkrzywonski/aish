package main

// gui_dialog.go: a modal Yes/No dialog with a timeout, replacing the
// console's askYN. Built from an in-memory DLGTEMPLATE (DialogBoxIndirectParam)
// rather than MessageBoxW, which has no timeout parameter -- the wire
// protocol's prompt/prompt_answer frames always carry a deadline (the AI
// side is blocked waiting), so an unanswered dialog must auto-deny rather
// than hang forever.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync"
	"syscall"
	"unsafe"
)

var (
	procDialogBoxIndirectParamW = user32.NewProc("DialogBoxIndirectParamW")
	procEndDialog               = user32.NewProc("EndDialog")
)

const (
	dsSetFont    = 0x00000040
	dsModalFrame = 0x00000080
	dsCenter     = 0x00000800
	wsPopup      = 0x80000000
	wsCaption    = 0x00C00000
	wsSysMenu    = 0x00080000

	bsDefPushButton = 0x0001
	bsPushButton    = 0x0000

	idYes        = 6
	idNoBtn      = 7
	idStaticText = 100

	dlgTimerID = 1
)

var dialogProcPtr = syscall.NewCallback(yesNoDialogProc)

// dialogTimeoutMs is stashed here for the duration of one modal call --
// AskYesNo holds dialogMu for its entire call, so there is never more than
// one dialog (and one live timeout value) at a time.
var dialogMu sync.Mutex
var dialogTimeoutMs uint32

// AskYesNo shows a modal Yes/No dialog and blocks until the user answers or
// timeoutSeconds elapses, in which case it returns false (deny), matching
// the wire protocol's fail-closed contract for an unanswered prompt.
func AskYesNo(question string, timeoutSeconds int) bool {
	// devBuild is a compile-time fact (aishwindev build tag), never a
	// runtime flag: an ordinary aishwin.exe can never accidentally skip
	// this gate. Auto-approval is still logged loudly to the visible log
	// view so a human watching a dev build always knows it happened.
	if devBuild {
		AppendLog(fmt.Sprintf("aishwin [dev build]: auto-approved prompt: %s", question))
		return true
	}

	dialogMu.Lock()
	defer dialogMu.Unlock()

	if timeoutSeconds <= 0 {
		timeoutSeconds = 120
	}
	dialogTimeoutMs = uint32(timeoutSeconds * 1000)

	tmpl := buildYesNoDialogTemplate("aishwin", question)

	result := make(chan bool, 1)
	RunOnUIThread(func() {
		inst := getModuleHandle()
		r, _, _ := procDialogBoxIndirectParamW.Call(
			uintptr(inst),
			uintptr(unsafe.Pointer(&tmpl[0])),
			uintptr(hwndMain),
			dialogProcPtr,
			0,
		)
		result <- r == 1
	})
	return <-result
}

func yesNoDialogProc(hwndDlg syscall.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmInitDialog:
		procSetTimer.Call(uintptr(hwndDlg), dlgTimerID, uintptr(dialogTimeoutMs), 0)
		procSetForegroundWin.Call(uintptr(hwndDlg))
		return 1
	case wmTimer:
		procKillTimer.Call(uintptr(hwndDlg), dlgTimerID)
		procEndDialog.Call(uintptr(hwndDlg), 0)
		return 1
	case wmCommand:
		id := uint16(wParam & 0xFFFF)
		switch id {
		case idYes:
			procKillTimer.Call(uintptr(hwndDlg), dlgTimerID)
			procEndDialog.Call(uintptr(hwndDlg), 1)
			return 1
		case idNoBtn:
			procKillTimer.Call(uintptr(hwndDlg), dlgTimerID)
			procEndDialog.Call(uintptr(hwndDlg), 0)
			return 1
		}
	case wmClose:
		procKillTimer.Call(uintptr(hwndDlg), dlgTimerID)
		procEndDialog.Call(uintptr(hwndDlg), 0)
		return 1
	}
	return 0
}

// buildYesNoDialogTemplate hand-assembles an in-memory DLGTEMPLATE followed
// by three DLGITEMTEMPLATE entries (a static question label, Yes button, No
// button). Each DLGITEMTEMPLATE must start on a 4-byte boundary; the
// preceding variable-length UTF-16 strings make manual padding necessary.
func buildYesNoDialogTemplate(title, question string) []byte {
	var buf bytes.Buffer
	w := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	align4 := func() {
		for buf.Len()%4 != 0 {
			buf.WriteByte(0)
		}
	}
	writeStr := func(s string) {
		u, _ := syscall.UTF16FromString(s) // includes a terminating NUL
		for _, c := range u {
			w(c)
		}
	}

	style := uint32(dsSetFont | dsModalFrame | dsCenter | wsPopup | wsCaption | wsSysMenu)
	w(style)
	w(uint32(0)) // dwExtendedStyle
	w(uint16(3)) // cdit: 3 controls
	w(int16(0)); w(int16(0)); w(int16(260)); w(int16(90))
	w(uint16(0)) // menu: none
	w(uint16(0)) // class: default dialog class
	writeStr(title)
	w(uint16(8)) // point size
	writeStr("MS Shell Dlg")

	align4()
	w(uint32(wsChild | wsVisible))
	w(uint32(0))
	w(int16(10)); w(int16(10)); w(int16(240)); w(int16(40))
	w(uint16(idStaticText))
	w(uint16(0xFFFF)); w(uint16(0x0082)) // STATIC
	writeStr(question)
	w(uint16(0))

	align4()
	w(uint32(wsChild | wsVisible | wsTabStop | bsDefPushButton))
	w(uint32(0))
	w(int16(60)); int16Write(&buf, 60); w(int16(60)); w(int16(20))
	w(uint16(idYes))
	w(uint16(0xFFFF)); w(uint16(0x0080)) // BUTTON
	writeStr("Yes")
	w(uint16(0))

	align4()
	w(uint32(wsChild | wsVisible | wsTabStop | bsPushButton))
	w(uint32(0))
	w(int16(150)); w(int16(60)); w(int16(60)); w(int16(20))
	w(uint16(idNoBtn))
	w(uint16(0xFFFF)); w(uint16(0x0080)) // BUTTON
	writeStr("No")
	w(uint16(0))

	return buf.Bytes()
}

func int16Write(buf *bytes.Buffer, v int16) {
	_ = binary.Write(buf, binary.LittleEndian, v)
}
