package main

// Raw Win32 API bindings for the GUI (gui.go, gui_dialog.go). Hand-declared
// via syscall.NewLazyDLL/NewProc rather than a third-party GUI framework:
// golang.org/x/sys/windows only wraps MessageBox among what's needed here
// (confirmed by inspection) — nothing for window classes, the message loop,
// menus, or common controls. github.com/lxn/walk was tried first and hit a
// real, reproducible "TTM_ADDTOOL failed" bug in its automatic per-widget
// tooltip registration (root cause: comctl32.dll loads before the app
// manifest activates, so a compile-time-computed TOOLINFO.cbSize doesn't
// match what the loaded DLL expects — a known, never-fixed issue in a
// project unmaintained since 2021). This app doesn't use tooltips at all,
// sidestepping that class of bug entirely, and stays free of any
// third-party GUI dependency and its risk of similar surprises.
//
// This binary must stay CGO-free (it's meant to cross-compile from Linux
// via GOOS=windows normally; this session builds it natively on Windows
// only for live testing) — syscall.NewLazyDLL/NewProc are pure Go.

import (
	"syscall"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")

	procRegisterClassExW  = user32.NewProc("RegisterClassExW")
	procCreateWindowExW   = user32.NewProc("CreateWindowExW")
	procDefWindowProcW    = user32.NewProc("DefWindowProcW")
	procGetMessageW       = user32.NewProc("GetMessageW")
	procTranslateMessage  = user32.NewProc("TranslateMessage")
	procDispatchMessageW  = user32.NewProc("DispatchMessageW")
	procPostQuitMessage   = user32.NewProc("PostQuitMessage")
	procLoadCursorW       = user32.NewProc("LoadCursorW")
	procLoadIconW         = user32.NewProc("LoadIconW")
	procShowWindow        = user32.NewProc("ShowWindow")
	procUpdateWindow      = user32.NewProc("UpdateWindow")
	procPostMessageW      = user32.NewProc("PostMessageW")
	procSendMessageW      = user32.NewProc("SendMessageW")
	procSetWindowTextW    = user32.NewProc("SetWindowTextW")
	procGetClientRect     = user32.NewProc("GetClientRect")
	procMoveWindow        = user32.NewProc("MoveWindow")
	procCreateMenu        = user32.NewProc("CreateMenu")
	procCreatePopupMenu   = user32.NewProc("CreatePopupMenu")
	procAppendMenuW       = user32.NewProc("AppendMenuW")
	procSetMenu           = user32.NewProc("SetMenu")
	procDrawMenuBar       = user32.NewProc("DrawMenuBar")
	procEnableWindow      = user32.NewProc("EnableWindow")
	procDestroyWindow     = user32.NewProc("DestroyWindow")
	procSetFocus          = user32.NewProc("SetFocus")
	procSetTimer          = user32.NewProc("SetTimer")
	procKillTimer         = user32.NewProc("KillTimer")
	procGetWindowLongPtrW = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW = user32.NewProc("SetWindowLongPtrW")
	procCheckMenuItem     = user32.NewProc("CheckMenuItem")
	procSetForegroundWin  = user32.NewProc("SetForegroundWindow")
	procGetWindowRect     = user32.NewProc("GetWindowRect")
	procIsDialogMessageW  = user32.NewProc("IsDialogMessageW")
	procGetDlgItem        = user32.NewProc("GetDlgItem")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")

	procGetStockObject = gdi32.NewProc("GetStockObject")
)

// ---- structs (field layout must exactly match the real Win32 structs) ----

type wndClassExW struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   syscall.Handle
	icon       syscall.Handle
	cursor     syscall.Handle
	background syscall.Handle
	menuName   *uint16
	className  *uint16
	iconSm     syscall.Handle
}

type msg struct {
	hwnd    syscall.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type point struct {
	x, y int32
}

type rect struct {
	left, top, right, bottom int32
}

// ---- constants ----

const (
	wsOverlappedWindow = 0x00CF0000
	wsChild            = 0x40000000
	wsVisible          = 0x10000000
	wsVScroll          = 0x00200000
	wsBorder           = 0x00800000
	wsTabStop          = 0x00010000
	wsClipChildren     = 0x02000000

	esLeft      = 0x0000
	esMultiline = 0x0004
	esAutoVScroll = 0x0040
	esReadOnly  = 0x0800
	esWantReturn = 0x1000

	swShow = 5

	colorWindow  = 5
	colorBtnFace = 15

	idcArrow = 32512

	wmDestroy    = 0x0002
	wmSize       = 0x0005
	wmClose      = 0x0010
	wmCommand    = 0x0111
	wmSysCommand = 0x0112
	wmTimer      = 0x0113
	wmApp        = 0x8000
	wmSetFont    = 0x0030
	wmInitDialog = 0x0110
	wmGetMinMaxInfo = 0x0024

	mfString   = 0x00000000
	mfPopup    = 0x00000010
	mfSeparator = 0x00000800
	mfChecked  = 0x00000008
	mfUnchecked = 0x00000000

	smcxScreen = 0
	smcyScreen = 1

	wmUser = 0x0400

	emSetSel      = 0x00B1
	emReplaceSel  = 0x00C2
	emScrollCaret = 0x00B7
	emGetLine     = 0x00C4
	emLineFromChar = 0x00C9
	emSetLimitText = 0x00C5

	gwlpUserData = -21

	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpNoZOrder   = 0x0004
	swpNoActivate = 0x0010
)

// cwUseDefault is CW_USEDEFAULT (0x80000000 as a 32-bit signed int, i.e.
// -2147483648). Computed as a properly sign-extended uintptr at init time
// rather than declared as a Go constant: converting the untyped constant
// -2147483648 directly to uintptr is a compile error ("constant overflows
// uintptr") since uintptr conversions of untyped constants use the
// constant's own (unbounded, unsigned-looking-here) value rather than
// runtime two's-complement sign extension.
var cwUseDefaultI32 int32 = -2147483648
var cwUseDefault = uintptr(cwUseDefaultI32)

// ---- thin wrappers ----

func utf16ptr(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		// Only fails on an embedded NUL byte, which none of this app's
		// strings should ever contain; fall back to empty rather than panic.
		p, _ = syscall.UTF16PtrFromString("")
	}
	return p
}

func loadCursorArrow() syscall.Handle {
	h, _, _ := procLoadCursorW.Call(0, uintptr(idcArrow))
	return syscall.Handle(h)
}

func getModuleHandle() syscall.Handle {
	h, _, _ := procGetModuleHandleW.Call(0)
	return syscall.Handle(h)
}

