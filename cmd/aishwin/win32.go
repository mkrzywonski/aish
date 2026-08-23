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
	"unsafe"
)

var (
	user32    = syscall.NewLazyDLL("user32.dll")
	kernel32  = syscall.NewLazyDLL("kernel32.dll")
	gdi32     = syscall.NewLazyDLL("gdi32.dll")
	msftedit  = syscall.NewLazyDLL("msftedit.dll") // self-registers the RICHEDIT50W window class on load

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
	procGetScrollInfo     = user32.NewProc("GetScrollInfo")
	procCheckRadioButton  = user32.NewProc("CheckRadioButton")
	procIsDlgButtonChecked = user32.NewProc("IsDlgButtonChecked")
	procMapDialogRect     = user32.NewProc("MapDialogRect")
	procCallWindowProcW   = user32.NewProc("CallWindowProcW")

	// ---- graphical status bar (gui_statusbar.go): custom-drawn items and
	// their hand-rolled hover popup, both plain user32/gdi32 -- see that
	// file's header comment for why the popup isn't the real Windows
	// tooltip common control.
	procBeginPaint      = user32.NewProc("BeginPaint")
	procEndPaint        = user32.NewProc("EndPaint")
	procInvalidateRect  = user32.NewProc("InvalidateRect")
	procClientToScreen  = user32.NewProc("ClientToScreen")
	procSetWindowPos    = user32.NewProc("SetWindowPos")
	procGetSysColor     = user32.NewProc("GetSysColor")
	procTrackMouseEvent = user32.NewProc("TrackMouseEvent")
	procSetCursor       = user32.NewProc("SetCursor")
	procIsWindowVisible = user32.NewProc("IsWindowVisible")
	procFillRect        = user32.NewProc("FillRect")

	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")
	procOpenProcess         = kernel32.NewProc("OpenProcess")
	procGetExitCodeProcess  = kernel32.NewProc("GetExitCodeProcess")
	procCloseHandle         = kernel32.NewProc("CloseHandle")

	procGetStockObject        = gdi32.NewProc("GetStockObject")
	procCreateSolidBrush      = gdi32.NewProc("CreateSolidBrush")
	procEllipse               = gdi32.NewProc("Ellipse")
	procTextOutW              = gdi32.NewProc("TextOutW")
	procGetTextExtentPoint32W = gdi32.NewProc("GetTextExtentPoint32W")
	procSetBkColor            = gdi32.NewProc("SetBkColor")
	procSetTextColor          = gdi32.NewProc("SetTextColor")
	procSetBkMode             = gdi32.NewProc("SetBkMode")
	procRoundRect             = gdi32.NewProc("RoundRect")
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

// charFormat2W mirrors CHARFORMAT2W (richedit.h) field-for-field so its
// size (computed via unsafe.Sizeof, not hardcoded) matches what
// EM_SETCHARFORMAT expects exactly -- RichEdit uses cbSize to tell which
// CHARFORMAT version a caller is using, so a wrong size is rejected outright
// rather than partially accepted.
// scrollInfoT mirrors SCROLLINFO (winuser.h) field-for-field for
// GetScrollInfo -- used to tell whether the log view is currently scrolled
// to the bottom, without depending on where the caret/selection happens to
// be (a user can scroll the view with the mouse wheel or scrollbar without
// moving the caret at all).
type scrollInfoT struct {
	cbSize    uint32
	fMask     uint32
	nMin      int32
	nMax      int32
	nPage     uint32
	nPos      int32
	nTrackPos int32
}

type charFormat2W struct {
	cbSize          uint32
	dwMask          uint32
	dwEffects       uint32
	yHeight         int32
	yOffset         int32
	crTextColor     uint32
	bCharSet        byte
	bPitchAndFamily byte
	szFaceName      [32]uint16
	wWeight         uint16
	sSpacing        int16
	crBackColor     uint32
	lcid            uint32
	dwReserved      uint32
	sStyle          int16
	wKerning        uint16
	bUnderlineType  byte
	bAnimation      byte
	bRevAuthor      byte
	bReserved1      byte
}

// paintStruct mirrors PAINTSTRUCT (winuser.h) field-for-field for
// BeginPaint/EndPaint. Only hdc is actually used by this app's WM_PAINT
// handlers (gui_statusbar.go always redraws its whole small client area
// rather than clipping to rcPaint) -- the rest of the fields still have to
// be present and correctly sized so BeginPaint has valid memory to write
// its other output fields into.
type paintStruct struct {
	hdc         syscall.Handle
	fErase      int32
	rcPaint     rect
	fRestore    int32
	fIncUpdate  int32
	rgbReserved [32]byte
}

// trackMouseEvent mirrors TRACKMOUSEEVENT (winuser.h) field-for-field, used
// with TME_LEAVE to arm a one-shot WM_MOUSELEAVE for the status bar's hover
// tooltip (gui_statusbar.go) -- WM_MOUSEMOVE alone never fires once the
// cursor leaves the window entirely, so without this a tooltip shown while
// hovering an item could get stuck onscreen after a fast mouse-out.
type trackMouseEvent struct {
	cbSize      uint32
	dwFlags     uint32
	hwndTrack   syscall.Handle
	dwHoverTime uint32
}

// sizeT mirrors SIZE (windef.h), the output parameter GetTextExtentPoint32W
// fills in to size the hover tooltip popup to its text.
type sizeT struct {
	cx, cy int32
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
	swHide = 0

	sbVert   = 1
	sifRange = 0x0001
	sifPage  = 0x0002
	sifPos   = 0x0004

	bnClicked = 0

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
	wmNotify     = 0x004E

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
	emGetLineCount = 0x00BA
	emLineIndex    = 0x00BB
	emLineScroll   = 0x00B6
	emGetFirstVisibleLine = 0x00CE

	gwlpUserData = -21

	wmKeyDown   = 0x0100
	wmChar      = 0x0102
	wmKillFocus = 0x0008
	vkReturn    = 0x0D
	vkEscape    = 0x1B
	vkTab       = 0x09

	// wmEnvKeyEditDone/wmEnvValueEditDone are private, posted (never sent)
	// messages the Environment tab's edit-control subclasses use to defer
	// their own teardown (destroying/canceling the very control currently
	// running their WM_KEYDOWN handler) until after that handler returns
	// and the message loop picks the deferred message back up -- safer
	// than a synchronous self-destroy from inside one's own message
	// handling, which is the kind of reentrancy Win32 code conventionally
	// avoids even when it happens to work.
	wmEnvKeyEditDone   = wmApp + 1
	wmEnvValueEditDone = wmApp + 2

	// wmGetDlgCode/dlgcWantAllKeys: a subclassed control must claim
	// DLGC_WANTALLKEYS in reply to WM_GETDLGCODE, or the modal dialog's
	// IsDialogMessage loop intercepts VK_RETURN itself and invokes the
	// dialog's default button (OK) directly -- never dispatching the
	// keystroke as a normal message at all, so a subclass's own WM_KEYDOWN
	// handling never runs. Found live: Tab (plain focus loss, caught by
	// WM_KILLFOCUS regardless of cause) correctly saved the Value overlay's
	// text, but Enter silently discarded it and closed the whole Settings
	// dialog, because idOK's handler abandons any in-progress env-tab edit
	// before closing.
	wmGetDlgCode    = 0x0087
	dlgcWantAllKeys = 0x0004

	// dwlpMsgResult is DWLP_MSGRESULT, the dialog-extra-bytes offset a
	// DLGPROC must write a WM_NOTIFY reply through via SetWindowLongPtr --
	// unlike a plain WNDPROC, a dialog procedure's own return value only
	// means "did I handle this message", not the notification's actual
	// result; returning a meaningful value directly (as if this were a
	// normal WNDPROC) leaves comctl32 reading whatever was last in this
	// slot. Found live: skipping this for LVN_ENDLABELEDIT correlated with
	// the whole Settings dialog closing after a single keystroke.
	dwlpMsgResult = 0

	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpNoZOrder   = 0x0004
	swpNoActivate = 0x0010

	processQueryLimitedInformation = 0x1000
	stillActive                   = 259

	// RichEdit char-formatting (richedit.h): EM_SETCHARFORMAT sets the
	// format either of the current selection or, with an empty/collapsed
	// selection, of text about to be inserted there -- exactly the caret
	// position appendEditText already moves to before each append.
	emSetCharFormat = wmUser + 68
	scfSelection    = 0x0001
	cfmColor        = 0x40000000
	// cfeAutoColor reuses CFM_COLOR's own bit value in dwEffects (a real,
	// documented Win32 API quirk, not a typo) to mean "automatic/default
	// color" instead of crTextColor.
	cfeAutoColor = 0x40000000

	// ---- graphical status bar (gui_statusbar.go) ----
	wmPaint       = 0x000F
	wmMouseMove   = 0x0200
	wmLButtonUp   = 0x0202
	wmSetCursor   = 0x0020
	wmMouseLeave  = 0x02A3

	idcHand = 32649 // IDC_HAND

	nullPen        = 8  // GetStockObject index, NULL_PEN
	defaultGuiFont = 17 // GetStockObject index, DEFAULT_GUI_FONT

	colorInfoText = 23 // GetSysColor index, COLOR_INFOTEXT
	colorInfoBk   = 24 // GetSysColor index, COLOR_INFOBK

	// Sunken-border pair for a boxed status bar text item (gui_statusbar.go's
	// drawInsetBox): dark on the top/left edge, light on the bottom/right --
	// the classic two-tone "inset" look, e.g. WS_EX_CLIENTEDGE's own border.
	colorBtnShadow    = 16 // GetSysColor index, COLOR_BTNSHADOW
	colorBtnHighlight = 20 // GetSysColor index, COLOR_BTNHIGHLIGHT

	tmeLeave = 0x00000002 // TRACKMOUSEEVENT.dwFlags, TME_LEAVE

	wsExTopmost    = 0x00000008
	wsExToolWindow = 0x00000080

	swpShowWindow = 0x0040

	bkModeTransparent = 1 // SetBkMode mode, so text drawn directly on the status bar doesn't paint an opaque box behind each character
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

// gwlpWndProc is GWLP_WNDPROC (-4), computed the same way as cwUseDefault
// above and for the same reason: converting the untyped constant -4
// directly to uintptr is a compile error, since constant conversions use
// the constant's own value rather than runtime two's-complement sign
// extension.
var gwlpWndProcI32 int32 = -4
var gwlpWndProc = uintptr(gwlpWndProcI32)

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

func loadCursorHand() syscall.Handle {
	h, _, _ := procLoadCursorW.Call(0, uintptr(idcHand))
	return syscall.Handle(h)
}

func getModuleHandle() syscall.Handle {
	h, _, _ := procGetModuleHandleW.Call(0)
	return syscall.Handle(h)
}

// setDlgMsgResult stores a WM_NOTIFY reply value the correct way for a
// DLGPROC (see dwlpMsgResult) -- the caller must still return TRUE/1 from
// the dialog procedure afterward to indicate the message was handled.
func setDlgMsgResult(hwndDlg syscall.Handle, result uintptr) {
	procSetWindowLongPtrW.Call(uintptr(hwndDlg), uintptr(dwlpMsgResult), result)
}

// isScrolledToBottom reports whether the log view's vertical scrollbar is
// currently at (or past) its maximum position -- i.e. whether appending
// more text and following it would be invisible anyway because the user is
// already looking at the end. Checked via the control's real scrollbar
// info (GetScrollInfo) rather than caret/selection position: a user can
// scroll with the mouse wheel or drag the scrollbar thumb without moving
// the caret at all, so caret position alone can't tell "are they reading
// history" from "are they following the tail".
func isScrolledToBottom() bool {
	var si scrollInfoT
	si.cbSize = uint32(unsafe.Sizeof(si))
	si.fMask = sifRange | sifPage | sifPos
	r, _, _ := procGetScrollInfo.Call(uintptr(hwndEdit), sbVert, uintptr(unsafe.Pointer(&si)))
	if r == 0 || si.nPage == 0 {
		// No scrollbar info yet, or content shorter than the view (nPage
		// covers the whole range) -- nothing to scroll past, so "following"
		// is trivially true.
		return true
	}
	return si.nPos+int32(si.nPage) >= si.nMax
}

// setCaretTextColor sets the color newly appended text will be inserted
// with, on the RichEdit control at hwnd. auto=true resets to the control's
// normal default color (CFE_AUTOCOLOR); otherwise color is a COLORREF
// (0x00BBGGRR -- reversed from RGB).
func setCaretTextColor(hwnd syscall.Handle, auto bool, color uint32) {
	var cf charFormat2W
	cf.cbSize = uint32(unsafe.Sizeof(cf))
	cf.dwMask = cfmColor
	if auto {
		cf.dwEffects = cfeAutoColor
	} else {
		cf.crTextColor = color
	}
	procSendMessageW.Call(uintptr(hwnd), emSetCharFormat, scfSelection, uintptr(unsafe.Pointer(&cf)))
}

// processExited independently checks, via OpenProcess+GetExitCodeProcess,
// whether pid has actually exited -- a second opinion that doesn't depend
// on cmd.Wait()'s own bookkeeping. Go's Cmd.Wait() blocks not just on the
// process exiting but also on its stdout/stderr pipe-copying goroutines
// seeing EOF, which requires EVERY process holding the pipe's write-end
// handle to close it; a grandchild spawned by the shell (a compiler's own
// worker processes, say) that inherits the handle and outlives its parent
// can leave Wait() blocked indefinitely even though the command a caller
// actually cares about is long gone (background.go's Poll uses this as a
// fallback so exec_status doesn't report running:true forever in that
// case). ok is false if the process handle couldn't be opened at all
// (already exited and cleaned up, or never existed).
func processExited(pid int) (exited bool, code int) {
	h, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return true, -1 // no such process -- treat as exited
	}
	defer procCloseHandle.Call(h)

	var status uint32
	r, _, _ := procGetExitCodeProcess.Call(h, uintptr(unsafe.Pointer(&status)))
	if r == 0 {
		return false, 0 // couldn't query; don't claim it exited
	}
	if status == stillActive {
		return false, 0
	}
	return true, int(status)
}
