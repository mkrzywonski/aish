package main

// gui.go: the main window -- a menu bar, a large read-only log view, and a
// status bar. Runs on its own OS thread (Win32 requires the thread that
// creates a window to be the one that pumps its messages); other goroutines
// reach it only through the thread-safe functions at the bottom
// (AppendLog, SetStatus, RunOnUIThread), never by touching a HWND directly.

import (
	"fmt"
	"sync"
	"syscall"
	"unsafe"
)

const mainClassName = "AishwinMainWindow"

const (
	wmAppLog    = wmApp + 1 // a log line was queued; drain it
	wmAppRunUI  = wmApp + 2 // a func() was queued via RunOnUIThread; drain and run it
	idEdit      = 1001
	idStatus    = 1002
	idMenuFirst = 2000 // menu action IDs start here, assigned sequentially

	statusBarHeight = 24 // fixed height in pixels for the bottom status strip
)

var (
	hwndMain   syscall.Handle
	hwndEdit   syscall.Handle
	hwndStatus syscall.Handle

	menuActionsMu sync.Mutex
	menuActions   = map[uint16]func(){}
	nextMenuID    uint16 = idMenuFirst

	logMu    sync.Mutex
	logQueue []string

	uiFuncMu    sync.Mutex
	uiFuncQueue []func()

	onClose func() // set by main; called once when the window is closing
)

// NewMenuBar creates an empty top-level menu bar.
func NewMenuBar() syscall.Handle {
	h, _, _ := procCreateMenu.Call()
	return syscall.Handle(h)
}

// NewSubmenu creates a popup menu, appends it to parent under label, and
// returns it so items can be added with AddMenuItem/AddMenuSeparator.
func NewSubmenu(parent syscall.Handle, label string) syscall.Handle {
	h, _, _ := procCreatePopupMenu.Call()
	procAppendMenuW.Call(uintptr(parent), mfPopup, h, uintptr(unsafe.Pointer(utf16ptr(label))))
	return syscall.Handle(h)
}

// AddMenuItem appends a clickable item to menu, registering fn as its
// click action.
func AddMenuItem(menu syscall.Handle, label string, fn func()) {
	id := AddMenuAction(fn)
	procAppendMenuW.Call(uintptr(menu), mfString, uintptr(id), uintptr(unsafe.Pointer(utf16ptr(label))))
}

func AddMenuSeparator(menu syscall.Handle) {
	procAppendMenuW.Call(uintptr(menu), mfSeparator, 0, 0)
}

// AddCheckableMenuItem appends a checkable item, initially checked per
// initial, and returns its assigned id so a later toggle can update the
// checkmark via SetMenuChecked.
func AddCheckableMenuItem(menu syscall.Handle, label string, initial bool, fn func()) uint16 {
	id := AddMenuAction(fn)
	flags := uintptr(mfString)
	if initial {
		flags |= mfChecked
	}
	procAppendMenuW.Call(uintptr(menu), flags, uintptr(id), uintptr(unsafe.Pointer(utf16ptr(label))))
	return id
}

// SetMenuChecked updates a checkable item's checkmark after its state
// changes.
func SetMenuChecked(menu syscall.Handle, id uint16, checked bool) {
	flag := uintptr(mfUnchecked)
	if checked {
		flag = uintptr(mfChecked)
	}
	procCheckMenuItem.Call(uintptr(menu), uintptr(id), flag)
}

// StartGUI creates the main window and runs its message loop until the
// window closes. Must be called on the goroutine that will own the window
// (main should runtime.LockOSThread() first) -- it blocks until the user
// closes the window or Quit() is called. buildMenu, if non-nil, is called
// right after the window is created (hwndMain isn't valid before that) to
// build the menu bar via NewMenuBar/NewSubmenu/AddMenuItem; its return
// value is attached to the window.
func StartGUI(title string, buildMenu func() syscall.Handle, onCloseFn func()) error {
	onClose = onCloseFn

	inst := getModuleHandle()
	cls := utf16ptr(mainClassName)

	wndProcPtr := syscall.NewCallback(mainWndProc)

	var wc wndClassExW
	wc.size = uint32(unsafe.Sizeof(wc))
	wc.style = 0
	wc.wndProc = wndProcPtr
	wc.instance = inst
	wc.cursor = loadCursorArrow()
	wc.background = syscall.Handle(colorBtnFace + 1) // (HBRUSH)(COLOR_BTNFACE+1), the standard idiom for a stock system color as a class background brush
	wc.className = cls

	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return err
	}

	hwnd, _, err := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(utf16ptr(title))),
		uintptr(wsOverlappedWindow|wsClipChildren),
		uintptr(cwUseDefault), uintptr(cwUseDefault),
		900, 600,
		0, 0,
		uintptr(inst),
		0,
	)
	if hwnd == 0 {
		return err
	}
	hwndMain = syscall.Handle(hwnd)

	if buildMenu != nil {
		bar := buildMenu()
		procSetMenu.Call(uintptr(hwndMain), uintptr(bar))
	}

	createChildWidgets(hwndMain, inst)

	procShowWindow.Call(uintptr(hwndMain), swShow)
	procUpdateWindow.Call(uintptr(hwndMain))

	return messageLoop()
}

func createChildWidgets(parent syscall.Handle, inst syscall.Handle) {
	editHandle, _, editErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(utf16ptr("EDIT"))),
		uintptr(unsafe.Pointer(utf16ptr(""))),
		uintptr(wsChild|wsVisible|wsBorder|wsVScroll|esMultiline|esAutoVScroll|esReadOnly|esWantReturn),
		0, 0, 0, 0,
		uintptr(parent), uintptr(idEdit), uintptr(inst), 0,
	)
	hwndEdit = syscall.Handle(editHandle)
	if hwndEdit == 0 {
		fmt.Fprintf(stderr, "aishwin: CreateWindowExW(EDIT) failed: %v\n", editErr)
	}
	// Remove the legacy ~32KB text limit (0 means "no practical limit" on
	// modern EDIT controls when sent EM_SETLIMITTEXT).
	procSendMessageW.Call(uintptr(hwndEdit), emSetLimitText, 0, 0)

	// The status strip is a plain bordered STATIC control, not the native
	// msctls_statuswindow32 common control: that class reproducibly failed
	// CreateWindowExW ("Cannot find window class") on the real Windows host
	// even after InitCommonControlsEx reported success and a manifest
	// declaring Common Controls v6 was added -- while a different common
	// control (msctls_progress32) created fine under identical conditions,
	// ruling out a wholesale comctl32 registration failure. STATIC is a
	// core user32 class with no comctl32 dependency at all, so it sidesteps
	// the mystery entirely for what's functionally just a text strip.
	statusHandle, _, statusErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(utf16ptr("STATIC"))),
		uintptr(unsafe.Pointer(utf16ptr("starting..."))),
		uintptr(wsChild|wsVisible|wsBorder),
		0, 0, 0, 0,
		uintptr(parent), uintptr(idStatus), uintptr(inst), 0,
	)
	hwndStatus = syscall.Handle(statusHandle)
	if hwndStatus == 0 {
		fmt.Fprintf(stderr, "aishwin: CreateWindowExW(STATIC status) failed: %v\n", statusErr)
	}

	layoutChildren(parent)
}

// layoutChildren sizes the edit view to fill the client area above the
// status bar. Called on WM_SIZE and once after creation.
func layoutChildren(parent syscall.Handle) {
	var rc rect
	procGetClientRect.Call(uintptr(parent), uintptr(unsafe.Pointer(&rc)))

	width := rc.right - rc.left
	height := rc.bottom - rc.top

	procMoveWindow.Call(uintptr(hwndEdit), 0, 0, uintptr(width), uintptr(height-statusBarHeight), 1)
	procMoveWindow.Call(uintptr(hwndStatus), 0, uintptr(height-statusBarHeight), uintptr(width), uintptr(statusBarHeight), 1)
}

func messageLoop() error {
	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return nil // WM_QUIT, or GetMessage error
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func mainWndProc(hwnd syscall.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmSize:
		if hwnd == hwndMain {
			layoutChildren(hwnd)
		}
	case wmClose:
		if onClose != nil {
			onClose()
		}
		procDestroyWindow.Call(uintptr(hwnd))
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	case wmCommand:
		// LOWORD(wParam) is the menu item ID when lParam == 0 (a menu
		// command, as opposed to a child control notification).
		if lParam == 0 {
			id := uint16(wParam & 0xFFFF)
			menuActionsMu.Lock()
			fn := menuActions[id]
			menuActionsMu.Unlock()
			if fn != nil {
				fn()
			}
			return 0
		}
	case wmAppLog:
		drainLogQueue()
		return 0
	case wmAppRunUI:
		drainUIFuncQueue()
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(message), wParam, lParam)
	return r
}

// ---- thread-safe entry points for other goroutines ----

// AppendLog queues line for the log view and wakes the UI thread to render
// it. Safe to call from any goroutine.
func AppendLog(line string) {
	logMu.Lock()
	logQueue = append(logQueue, line)
	logMu.Unlock()
	if hwndMain != 0 {
		procPostMessageW.Call(uintptr(hwndMain), wmAppLog, 0, 0)
	}
}

func drainLogQueue() {
	logMu.Lock()
	pending := logQueue
	logQueue = nil
	logMu.Unlock()
	if len(pending) == 0 || hwndEdit == 0 {
		return
	}
	for _, line := range pending {
		appendEditText(line + "\r\n")
	}
}

func appendEditText(text string) {
	// Move the caret to the end, then insert there, then scroll it into
	// view -- the standard EDIT-control append idiom.
	const maxUint = ^uintptr(0)
	procSendMessageW.Call(uintptr(hwndEdit), emSetSel, maxUint, maxUint)
	procSendMessageW.Call(uintptr(hwndEdit), emReplaceSel, 0, uintptr(unsafe.Pointer(utf16ptr(text))))
	procSendMessageW.Call(uintptr(hwndEdit), emScrollCaret, 0, 0)
}

// SetStatus sets the status bar's text. Safe to call from any goroutine.
func SetStatus(text string) {
	RunOnUIThread(func() {
		procSetWindowTextW.Call(uintptr(hwndStatus), uintptr(unsafe.Pointer(utf16ptr(text))))
	})
}

// RunOnUIThread queues fn to run on the GUI's owning thread and wakes it.
// Safe to call from any goroutine, including the UI thread itself (fn just
// queues normally and runs on the next message-loop iteration).
func RunOnUIThread(fn func()) {
	uiFuncMu.Lock()
	uiFuncQueue = append(uiFuncQueue, fn)
	uiFuncMu.Unlock()
	if hwndMain != 0 {
		procPostMessageW.Call(uintptr(hwndMain), wmAppRunUI, 0, 0)
	}
}

func drainUIFuncQueue() {
	uiFuncMu.Lock()
	pending := uiFuncQueue
	uiFuncQueue = nil
	uiFuncMu.Unlock()
	for _, fn := range pending {
		fn()
	}
}

// AddMenuAction registers fn under a freshly allocated menu item ID and
// returns it, for use with AppendMenuW.
func AddMenuAction(fn func()) uint16 {
	menuActionsMu.Lock()
	defer menuActionsMu.Unlock()
	id := nextMenuID
	nextMenuID++
	menuActions[id] = fn
	return id
}

// Quit closes the main window, ending the message loop.
func Quit() {
	RunOnUIThread(func() {
		procSendMessageW.Call(uintptr(hwndMain), wmClose, 0, 0)
	})
}
