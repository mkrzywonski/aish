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
	idJumpBtn   = 1003
	idMenuFirst = 2000 // menu action IDs start here, assigned sequentially

	statusBarHeight = 24 // fixed height in pixels for the bottom status strip
	jumpBtnWidth    = 200

	// mainScrollTimerID polls (via WM_TIMER on hwndMain) whether the user
	// has scrolled back to the bottom on their own -- distinct from
	// gui_dialog.go's dlgTimerID, which is scoped to a separate, temporary
	// dialog HWND, so there's no collision even though both start at small
	// integers.
	mainScrollTimerID    = 2
	scrollPollIntervalMs = 400
)

var (
	hwndMain    syscall.Handle
	hwndEdit    syscall.Handle
	hwndStatus  syscall.Handle
	hwndJumpBtn syscall.Handle

	menuActionsMu sync.Mutex
	menuActions   = map[uint16]func(){}
	nextMenuID    uint16 = idMenuFirst

	logMu    sync.Mutex
	logQueue []logEntry

	uiFuncMu    sync.Mutex
	uiFuncQueue []func()

	jumpMu          sync.Mutex
	pendingNewLines int

	onClose func() // set by main; called once when the window is closing

	// onMenuItemAdded is a hook devctl.go (aishwindev build tag only)
	// replaces to build a label->id registry for unattended AI-driven
	// testing; a no-op in an ordinary build.
	onMenuItemAdded = func(label string, id uint16) {}
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
	onMenuItemAdded(label, id)
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
	onMenuItemAdded(label, id)
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

	// Polls whether the user has scrolled the log back to the bottom on
	// their own (mouse wheel/scrollbar drag don't move the caret, so there's
	// no cheap window message to hook instead) so the jump-to-bottom
	// indicator can hide itself without waiting for the next appended line.
	procSetTimer.Call(uintptr(hwndMain), mainScrollTimerID, scrollPollIntervalMs, 0)

	return messageLoop()
}

func createChildWidgets(parent syscall.Handle, inst syscall.Handle) {
	// RICHEDIT50W (msftedit.dll), not plain EDIT: only a RichEdit control
	// supports per-run text color (EM_SETCHARFORMAT), needed to show file
	// operations in a different color from shell/exec output. Load() is
	// called explicitly since nothing else in this process ever calls a
	// msftedit proc directly -- the whole point of loading it is its
	// side effect of registering the RICHEDIT50W window class.
	msftedit.Load()
	editHandle, _, editErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(utf16ptr("RICHEDIT50W"))),
		uintptr(unsafe.Pointer(utf16ptr(""))),
		uintptr(wsChild|wsVisible|wsBorder|wsVScroll|esMultiline|esAutoVScroll|esReadOnly|esWantReturn),
		0, 0, 0, 0,
		uintptr(parent), uintptr(idEdit), uintptr(inst), 0,
	)
	hwndEdit = syscall.Handle(editHandle)
	if hwndEdit == 0 {
		fmt.Fprintf(stderr, "aishwin: CreateWindowExW(RICHEDIT50W) failed: %v\n", editErr)
	}
	// Remove the legacy ~32KB text limit (0 means "no practical limit" on
	// modern EDIT/RichEdit controls when sent EM_SETLIMITTEXT).
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

	// Created without WS_VISIBLE: hidden until sticky auto-scroll actually
	// has something to report (drainLogQueue's showJumpIndicator), and
	// overlaps the right end of the status STATIC above it in z-order --
	// harmless, since the two are never meaningfully informative at the
	// same pixels at the same time (the indicator only appears while
	// there's unseen output, which is exactly when knowing "N new lines"
	// matters more than the connection-status text underneath it).
	jumpHandle, _, jumpErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(utf16ptr("BUTTON"))),
		uintptr(unsafe.Pointer(utf16ptr(""))),
		uintptr(wsChild|wsTabStop),
		0, 0, 0, 0,
		uintptr(parent), uintptr(idJumpBtn), uintptr(inst), 0,
	)
	hwndJumpBtn = syscall.Handle(jumpHandle)
	if hwndJumpBtn == 0 {
		fmt.Fprintf(stderr, "aishwin: CreateWindowExW(BUTTON jump) failed: %v\n", jumpErr)
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

	btnWidth := int32(jumpBtnWidth)
	if btnWidth > width {
		btnWidth = width
	}
	procMoveWindow.Call(uintptr(hwndJumpBtn), uintptr(width-btnWidth), uintptr(height-statusBarHeight), uintptr(btnWidth), uintptr(statusBarHeight), 1)
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
		// A child control notification instead: HIWORD(wParam) is the
		// notification code, LOWORD(wParam) the control id.
		id := uint16(wParam & 0xFFFF)
		notify := uint16((wParam >> 16) & 0xFFFF)
		if id == idJumpBtn && notify == bnClicked {
			jumpToBottomClicked()
			return 0
		}
	case wmTimer:
		if wParam == mainScrollTimerID {
			checkAutoHideJumpIndicator()
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

// colorFileOp is the COLORREF (0x00BBGGRR) used for file-operation log
// lines (AppendLogColor), visually distinguishing them from shell/exec
// output -- there is no shared interactive tty in aishwin the way there is
// in aish, so the log view is the only record of what happened at all,
// in-band or out-of-band alike; color is what separates "a file operation
// touched this path" from "the shell printed this line" at a glance.
const colorFileOp = 0x00FF0000 // blue
const colorRunning = 0x000000FF // red -- "a command just started" announcement

// logEntry is one queued log line and how to render it: useColor false
// means "reset to the control's automatic default color" (never silently
// inherit color left over from a previous colored line at the same caret
// position), true means render in color.
type logEntry struct {
	text     string
	color    uint32
	useColor bool
}

// AppendLog queues line for the log view and wakes the UI thread to render
// it, in the control's normal default color. Safe to call from any
// goroutine.
func AppendLog(line string) {
	appendLogEntry(logEntry{text: line})
}

// AppendLogColor is AppendLog, rendered in color (a COLORREF, 0x00BBGGRR)
// instead of the default.
func AppendLogColor(line string, color uint32) {
	appendLogEntry(logEntry{text: line, color: color, useColor: true})
}

func appendLogEntry(e logEntry) {
	logMu.Lock()
	logQueue = append(logQueue, e)
	logMu.Unlock()
	if hwndMain != 0 {
		procPostMessageW.Call(uintptr(hwndMain), wmAppLog, 0, 0)
	}
}

// drainLogQueue appends every queued line, then follows the view to the
// bottom ONLY if it was already there before this batch landed -- sticky
// auto-scroll. Checked once per batch, before any insertion (inserting text
// itself doesn't move the scroll position; only an explicit scroll command
// does, so the pre-insertion check stays valid through the whole loop): a
// user who has scrolled up to read earlier output should never have the
// view yanked back down just because new output arrived, but the common
// case (already following the tail) should keep working exactly as before.
func drainLogQueue() {
	logMu.Lock()
	pending := logQueue
	logQueue = nil
	logMu.Unlock()
	if len(pending) == 0 || hwndEdit == 0 {
		return
	}
	wasAtBottom := isScrolledToBottom()
	for _, e := range pending {
		setCaretTextColor(hwndEdit, !e.useColor, e.color)
		appendEditText(e.text + "\r\n")
	}
	trimScrollback()
	if wasAtBottom {
		scrollToBottom()
		hideJumpIndicator()
	} else {
		showJumpIndicator(len(pending))
	}
}

func appendEditText(text string) {
	// Move the caret to the end, then insert there -- the standard
	// EDIT/RichEdit append idiom. The color for this insertion was already
	// set via setCaretTextColor before this call. Scrolling the VIEW is a
	// separate concern handled by scrollToBottom, not this function --
	// EM_SETSEL only positions where the next insertion lands.
	const maxUint = ^uintptr(0)
	procSendMessageW.Call(uintptr(hwndEdit), emSetSel, maxUint, maxUint)
	procSendMessageW.Call(uintptr(hwndEdit), emReplaceSel, 0, uintptr(unsafe.Pointer(utf16ptr(text))))
}

// scrollToBottom scrolls the log view all the way down via EM_LINESCROLL, a
// pure scrollbar-position message, rather than EM_SETSEL(end)+EM_SCROLLCARET
// (the "standard" caret-follow idiom this function used originally).
// Confirmed live, through the dev-build debug channel (GetScrollInfo before
// and after): EM_SCROLLCARET reliably left the view's scroll position
// completely unmoved (pinned at the top) in this build, focus or no focus,
// even though it's documented to "scroll the caret into view".
//
// EM_LINESCROLL itself turned out to have its own trap: a single call with
// a deliberately oversized count (the control's own EM_GETLINECOUNT, on the
// theory that it could never undershoot the true remaining distance)
// reliably moved GetScrollInfo's reported position to what LOOKED like the
// clamped bottom -- but a screenshot showed the log view genuinely blank,
// scrolled PAST all real content into empty space RichEdit apparently
// allows past the last line for an oversized request, a real rendering
// effect, not just a GetScrollInfo bookkeeping quirk. Scrolling in small,
// bounded steps instead and stopping via EM_GETFIRSTVISIBLELINE (a direct,
// real line index, unlike GetScrollInfo's apparently-proportional units)
// side-steps trusting any single call's magnitude: each step is small
// enough to never overshoot past real content, and stopping the moment
// EM_GETFIRSTVISIBLELINE stops advancing means "no further real progress
// is possible" regardless of what unit any given message uses internally.
func scrollToBottom() {
	const step = 3
	const maxSteps = 2000 // generous relative to any realistic scrollback size
	prevFirst := int32(-1)
	for i := 0; i < maxSteps; i++ {
		first, _, _ := procSendMessageW.Call(uintptr(hwndEdit), emGetFirstVisibleLine, 0, 0)
		if int32(first) == prevFirst {
			return
		}
		prevFirst = int32(first)
		procSendMessageW.Call(uintptr(hwndEdit), emLineScroll, 0, step)
	}
}

// showJumpIndicator reveals the "N new lines below" button, accumulating
// the count across multiple batches that land while the user stays
// scrolled away.
func showJumpIndicator(newLines int) {
	jumpMu.Lock()
	pendingNewLines += newLines
	count := pendingNewLines
	jumpMu.Unlock()
	label := fmt.Sprintf("%d new ↓ click to jump down", count)
	procSetWindowTextW.Call(uintptr(hwndJumpBtn), uintptr(unsafe.Pointer(utf16ptr(label))))
	procShowWindow.Call(uintptr(hwndJumpBtn), swShow)
}

func hideJumpIndicator() {
	jumpMu.Lock()
	pendingNewLines = 0
	jumpMu.Unlock()
	procShowWindow.Call(uintptr(hwndJumpBtn), swHide)
}

// jumpToBottomClicked handles the button itself being clicked.
func jumpToBottomClicked() {
	scrollToBottom()
	hideJumpIndicator()
}

// checkAutoHideJumpIndicator runs off mainScrollTimerID: it's the only way
// to notice the user scrolled back to the bottom by hand (mouse wheel and
// scrollbar dragging are handled entirely inside the RichEdit control's own
// window procedure, never reaching ours as a message) without waiting for
// the next appended line to trigger the same check in drainLogQueue.
func checkAutoHideJumpIndicator() {
	jumpMu.Lock()
	showing := pendingNewLines > 0
	jumpMu.Unlock()
	if showing && isScrolledToBottom() {
		hideJumpIndicator()
	}
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

// TriggerMenuAction invokes the action registered under id, as if its menu
// item had been clicked, returning false if id is unknown. Exists for
// devctl.go (aishwindev build tag) to drive menu items by label without a
// real click; harmless in an ordinary build since nothing outside a dev
// build ever calls it (there is no way to obtain an id without the
// onMenuItemAdded registry, which stays a no-op there).
func TriggerMenuAction(id uint16) bool {
	menuActionsMu.Lock()
	fn := menuActions[id]
	menuActionsMu.Unlock()
	if fn == nil {
		return false
	}
	RunOnUIThread(fn)
	return true
}

// Quit closes the main window, ending the message loop.
func Quit() {
	RunOnUIThread(func() {
		procSendMessageW.Call(uintptr(hwndMain), wmClose, 0, 0)
	})
}
