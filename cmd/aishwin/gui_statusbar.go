//go:build windows

package main

// gui_statusbar.go: the graphical status bar, replacing the old plain-text
// strip (gui.go's original STATIC control, one long SetWindowTextW string
// of pid/session/shell/version). Four items so far: a connected/
// not-connected LED on the left (hoverable for a tooltip, clickable to jump
// straight to Settings' Connection page), a session-name item right after it
// ("Session: <name>", hoverable for the name+ID, clickable to raise the
// rename dialog -- the one item whose width changes at runtime, see its own
// section below), a connected-client count after that (polled periodically
// from aishwnd, hoverable for a per-client tooltip, clickable to open the
// Clients dialog), and an Auto Scroll switch on the right (see gui.go's
// drainLogQueue/pollAutoScrollState for the log-view side of that feature).
// More items can be appended to statusItems later without changing the
// layout/hit-test/paint machinery below.
//
// The hover tooltip (showTooltip/hideTooltip, tooltipWndProc) is a
// hand-rolled popup window, not the real Windows tooltip common control
// (TOOLTIPS_CLASS/TTM_ADDTOOL): win32.go's own file-level comment documents
// a real, reproducible TTM_ADDTOOL bug a previous GUI library hit here (a
// comctl32 TOOLINFO struct-size mismatch from the DLL loading before the
// app manifest activates), noting this app "doesn't use tooltips at all,
// sidestepping that class of bug entirely" -- a small owned popup keeps
// that true while still giving "tooltip style" info on hover. It's also
// why the status bar itself is a plain custom RegisterClassExW window
// (like hwndMain), not a comctl32 control: msctls_statuswindow32 itself
// previously failed to even create on this host (see the removed comment
// this file's predecessor carried), and a fully custom class sidesteps
// that mystery entirely on top of being the only way to get a graphical,
// per-item hit-testable bar at all.

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"ai-ssh/internal/aishwinwire"
)

const statusBarClassName = "AishwinStatusBar"
const tooltipClassName = "AishwinTooltip"

const (
	statusItemSize = 22 // fixed item height in pixels, and the LED item's width
	statusItemPad  = 6  // margin at each bar edge, and gap between items
	ledDiameter    = 12

	// boxInsetPad is the gap between a boxed text item's sunken border
	// (drawInsetBox) and its own text, on each side -- applies only to the
	// session-name and client-count items (the bar's two plain "text
	// field" items), not the LED or the Auto Scroll switch.
	boxInsetPad = 4
)

// drawInsetBox paints a 1px sunken border (dark top/left, light
// bottom/right) around r, giving a boxed text item the same "separate
// control" look a native status bar's parts have. Built from plain
// FillRect calls rather than DrawEdge/EDGE_SUNKEN so it stays independent
// of any comctl32 theming quirk -- consistent with this file's general
// avoidance of common controls (see the file header).
func drawInsetBox(hdc syscall.Handle, r rect) {
	shadow, _, _ := procGetSysColor.Call(colorBtnShadow)
	highlight, _, _ := procGetSysColor.Call(colorBtnHighlight)
	shadowBrush, _, _ := procCreateSolidBrush.Call(shadow)
	highlightBrush, _, _ := procCreateSolidBrush.Call(highlight)

	top := rect{left: r.left, top: r.top, right: r.right, bottom: r.top + 1}
	left := rect{left: r.left, top: r.top, right: r.left + 1, bottom: r.bottom}
	bottom := rect{left: r.left, top: r.bottom - 1, right: r.right, bottom: r.bottom}
	right := rect{left: r.right - 1, top: r.top, right: r.right, bottom: r.bottom}

	procFillRect.Call(uintptr(hdc), uintptr(unsafe.Pointer(&top)), shadowBrush)
	procFillRect.Call(uintptr(hdc), uintptr(unsafe.Pointer(&left)), shadowBrush)
	procFillRect.Call(uintptr(hdc), uintptr(unsafe.Pointer(&bottom)), highlightBrush)
	procFillRect.Call(uintptr(hdc), uintptr(unsafe.Pointer(&right)), highlightBrush)

	procDeleteObject.Call(shadowBrush)
	procDeleteObject.Call(highlightBrush)
}

// Alignment for one statusItem: left-aligned items are laid out left to
// right from the bar's left edge (the connected LED); right-aligned items
// are laid out right to left from the bar's right edge (the Auto Scroll
// switch) -- keeping the two groups independent lets items be added to
// either side later without the other group's positions shifting.
const (
	alignLeft = iota
	alignRight
)

// statusItem is one clickable/hoverable region of the status bar. width is
// its total footprint in pixels (including any label, for a switch-style
// item); height is always statusItemSize, vertically centered in the bar.
type statusItem struct {
	width   int32
	align   int
	draw    func(hdc syscall.Handle, r rect)
	tooltip func() string
	onClick func() // nil means the item isn't clickable

	// widthFunc, if non-nil, overrides width -- for an item whose footprint
	// can change after construction (currently only the session-name item,
	// whose label grows/shrinks with the name itself). Reads a cached value
	// rather than remeasuring text on every layout pass; see
	// updateSessionNameDisplay. Every other item leaves this nil and uses
	// its fixed width, unchanged from before this field existed.
	widthFunc func() int32
}

// itemWidth returns item's current width, preferring widthFunc when set.
func itemWidth(item statusItem) int32 {
	if item.widthFunc != nil {
		return item.widthFunc()
	}
	return item.width
}

var statusItems []statusItem

// statusConnected mirrors whether aishwin currently has a live wire link to
// aishwnd -- read by the LED item's draw/tooltip callbacks. Set via
// SetConnected, called from refreshStatus (runtime.go) whenever
// runtimeState's connected flag changes.
var statusConnected atomic.Bool

// buildStatusItems constructs the bar's item list. Called once from
// createChildWidgets; a plain function rather than a package-level literal
// since its onClick closures need ShowSettingsDialogPage/setAutoScroll,
// defined elsewhere, and Go initializes package vars before any function
// runs.
func buildStatusItems() []statusItem {
	return []statusItem{
		{
			width:   statusItemSize,
			align:   alignLeft,
			draw:    drawConnectedLED,
			tooltip: connectedLEDTooltip,
			onClick: func() {
				ShowSettingsDialogPage(1) // Connection page
			},
		},
		buildSessionItem(),
		buildClientsItem(),
		buildAutoScrollItem(),
	}
}

// connectedLEDTooltip reports how (wsl/ssh) and what aishwin is connected
// to, plus both sides' versions, rather than just that it is -- the
// mode/target come from rt's own connDescriptor (set once, at the moment a
// connection was actually started -- see StartConnection, connection.go),
// never re-read from Settings, so this can't drift out of sync with a
// setting changed after the fact or a --ssh/--wsl CLI override that never
// touched Settings at all.
func connectedLEDTooltip() string {
	snap := rt.snapshot()
	if !snap.connected {
		return "Not Connected"
	}
	var via string
	switch snap.connMode {
	case connModeSSH:
		via = "Connected via SSH to " + snap.connTarget
	default:
		via = "Connected via WSL"
		if snap.connTarget != "" {
			via += " (" + snap.connTarget + ")"
		}
	}
	aishwndVer := snap.aishwndVersion
	if aishwndVer == "" {
		aishwndVer = "unknown"
	}
	return fmt.Sprintf("%s — aishwin %s, aishwnd %s", via, version, aishwndVer)
}

// SetConnected updates the LED item's state and repaints it. Safe to call
// from any goroutine.
func SetConnected(connected bool) {
	statusConnected.Store(connected)
	RunOnUIThread(func() {
		if hwndStatus != 0 {
			procInvalidateRect.Call(uintptr(hwndStatus), 0, 1)
		}
	})
	if connected {
		// Otherwise the client-count item could show a stale "0 clients"
		// for up to startClientCountPoller's own 3-second tick right after
		// a fresh connect.
		go refreshClientCount()
	}
}

// statusItemRect returns item i's client-area rectangle within the status
// bar, given the bar's current client size. Computed on demand rather than
// cached at resize time -- nothing here is expensive enough to bother, and
// only the bar's WIDTH ever changes (window resize), which left-aligned
// items don't care about at all and right-aligned items handle by walking
// from the (possibly moved) right edge each time anyway.
func statusItemRect(i int, clientWidth, clientHeight int32) rect {
	item := statusItems[i]
	y := (clientHeight - statusItemSize) / 2
	if item.align == alignRight {
		x := clientWidth - statusItemPad
		for j := len(statusItems) - 1; j >= i; j-- {
			if statusItems[j].align != alignRight {
				continue
			}
			x -= itemWidth(statusItems[j])
			if j > i {
				x -= statusItemPad
			}
		}
		return rect{left: x, top: y, right: x + itemWidth(item), bottom: y + statusItemSize}
	}
	x := int32(statusItemPad)
	for j := 0; j < i; j++ {
		if statusItems[j].align != alignLeft {
			continue
		}
		x += itemWidth(statusItems[j]) + statusItemPad
	}
	return rect{left: x, top: y, right: x + itemWidth(item), bottom: y + statusItemSize}
}

// hitTestStatusItem returns the index of the item containing client point
// (x, y), or -1 if none.
func hitTestStatusItem(x, y, clientWidth, clientHeight int32) int {
	for i := range statusItems {
		r := statusItemRect(i, clientWidth, clientHeight)
		if x >= r.left && x < r.right && y >= r.top && y < r.bottom {
			return i
		}
	}
	return -1
}

// drawConnectedLED paints a filled circle, green when connected, gray
// otherwise, centered in r. NULL_PEN is selected in first so Ellipse fills
// without also stroking a visible outline in the default black pen.
func drawConnectedLED(hdc syscall.Handle, r rect) {
	color := uintptr(0x00A0A0A0) // gray -- not connected
	if statusConnected.Load() {
		color = 0x0000C000 // green (COLORREF 0x00BBGGRR)
	}
	cx := (r.left + r.right) / 2
	cy := (r.top + r.bottom) / 2
	half := int32(ledDiameter / 2)

	brush, _, _ := procCreateSolidBrush.Call(color)
	oldBrush, _, _ := procSelectObject.Call(uintptr(hdc), brush)
	nullPenH, _, _ := procGetStockObject.Call(nullPen)
	oldPen, _, _ := procSelectObject.Call(uintptr(hdc), nullPenH)

	procEllipse.Call(uintptr(hdc), uintptr(cx-half), uintptr(cy-half), uintptr(cx+half), uintptr(cy+half))

	procSelectObject.Call(uintptr(hdc), oldBrush)
	procSelectObject.Call(uintptr(hdc), oldPen)
	procDeleteObject.Call(brush)
}

// ---- session name ----

// sessionItemMu guards the session-name item's cached label/width, updated
// by updateSessionNameDisplay whenever runtimeState's name changes
// (refreshStatus, runtime.go) rather than measured fresh on every layout
// pass -- this item's width is the one that changes at runtime (see
// statusItem.widthFunc), so the cached value is what itemWidth actually
// reads. Named sessionItem*, not sessionName*, to stay clear of
// connection.go's own sessionNameMu/sessionName (a different mutex guarding
// the persisted --name-flag-or-rename value each reconnect's hello frame
// carries -- an unrelated concern this file has no need to touch).
var (
	sessionItemMu    sync.Mutex
	sessionItemText  = "Session: (unnamed)"
	sessionItemWidth int32
)

// buildSessionItem seeds the cache from whatever runtimeState already knows
// (usually nothing yet, this early) and returns an item whose width tracks
// sessionNameWidth rather than a value fixed at construction, unlike every
// other item here -- a session's name can be arbitrarily short or long,
// so there's no sane fixed sizing template the way the 2-digit client
// count has.
func buildSessionItem() statusItem {
	updateSessionNameDisplay(rt.snapshot().name)
	return statusItem{
		align: alignLeft,
		draw:  drawSessionName,
		widthFunc: func() int32 {
			sessionItemMu.Lock()
			defer sessionItemMu.Unlock()
			return sessionItemWidth
		},
		tooltip: sessionNameTooltip,
		onClick: promptRenameSession,
	}
}

func drawSessionName(hdc syscall.Handle, r rect) {
	drawInsetBox(hdc, r)
	sessionItemMu.Lock()
	text := sessionItemText
	sessionItemMu.Unlock()
	textPtr, textLen := utf16CountedString(text)
	textY := r.top + (r.bottom-r.top-textHeight(hdc))/2
	procTextOutW.Call(uintptr(hdc), uintptr(r.left+boxInsetPad), uintptr(textY), uintptr(unsafe.Pointer(textPtr)), uintptr(textLen))
}

// sessionNameTooltip shows both the name and the session ID -- the ID isn't
// in the item's own label (it would make an already-variable-width item
// unreasonably wide), but it's the one thing that's stable and unique
// across a rename, so it belongs in the tooltip.
func sessionNameTooltip() string {
	snap := rt.snapshot()
	name := snap.name
	if name == "" {
		name = "(unnamed)"
	}
	id := snap.sessionID
	if id == "" {
		id = "not connected"
	}
	return fmt.Sprintf("Session: %s\nID: %s", name, id)
}

// updateSessionNameDisplay recomputes the session-name item's cached label
// and width from name and repaints the status bar if either changed.
// Called from refreshStatus (runtime.go) on every connect/disconnect/
// rename, mirroring refreshClientCount's cache-then-repaint shape -- but
// driven by an explicit call rather than a poll, since a name change is
// always known synchronously the moment it happens, unlike the client
// list. Safe to call before the window exists (buildSessionItem's own seed
// call is one such case): RunOnUIThread and the hwndStatus nil check both
// already handle that.
func updateSessionNameDisplay(name string) {
	text := "Session: (unnamed)"
	if name != "" {
		text = "Session: " + name
	}
	width := measureTextWidth(text) + 2*boxInsetPad

	sessionItemMu.Lock()
	changed := sessionItemText != text
	sessionItemText = text
	sessionItemWidth = width
	sessionItemMu.Unlock()

	if changed {
		RunOnUIThread(func() {
			if hwndStatus != 0 {
				procInvalidateRect.Call(uintptr(hwndStatus), 0, 1)
			}
		})
	}
}

// ---- connected client count ----

const clientsSizingTemplate = "88 clients connected" // 2-digit template, sized once at construction (see buildClientsItem)

// clientCountMu guards the last-known client list rendering, refreshed
// periodically off the GUI thread by refreshClientCount (fetchClients,
// gui_clients.go, is a wire round trip -- far too slow to run directly
// inside a WM_TIMER handler the way pollAutoScrollState's local-only check
// does). The status bar's draw/tooltip callbacks only ever read this.
var (
	clientCountMu   sync.Mutex
	clientCountText = "0 clients connected"
	clientTooltip   = "0 clients connected"
)

// buildClientsItem sizes itself once against a 2-digit template rather
// than remeasuring as the real count changes: the alternative -- resizing
// the item every time a client connects/disconnects -- would shift every
// item to its right (Auto Scroll included) each time, which is worse than
// the rare case of a 100+ client count clipping slightly.
func buildClientsItem() statusItem {
	width := measureTextWidth(clientsSizingTemplate) + 2*boxInsetPad
	return statusItem{
		width: width,
		align: alignLeft,
		draw:  drawClientCount,
		tooltip: func() string {
			clientCountMu.Lock()
			defer clientCountMu.Unlock()
			return clientTooltip
		},
		onClick: func() {
			ShowClientsDialog()
		},
	}
}

func drawClientCount(hdc syscall.Handle, r rect) {
	drawInsetBox(hdc, r)
	clientCountMu.Lock()
	text := clientCountText
	clientCountMu.Unlock()
	textPtr, textLen := utf16CountedString(text)
	textY := r.top + (r.bottom-r.top-textHeight(hdc))/2
	procTextOutW.Call(uintptr(hdc), uintptr(r.left+boxInsetPad), uintptr(textY), uintptr(unsafe.Pointer(textPtr)), uintptr(textLen))
}

// startClientCountPoller periodically refreshes the client-count item from
// aishwnd's live connection list -- there's no push notification for this
// (unlike SetConnected, which runtimeState already updates the instant the
// wire link's own state changes), so polling is the only option. Called
// once from main.go; fetchClients itself handles "not connected yet"
// gracefully (returns an error this just ignores, leaving the last-known
// text in place), so it's safe to start immediately at process startup.
func startClientCountPoller() {
	refreshClientCount()
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			refreshClientCount()
		}
	}()
}

// refreshClientCount fetches the current client list and updates the
// cached text/tooltip, repainting the status bar if anything changed. Safe
// to call from any goroutine (RunOnUIThread does the actual repaint).
func refreshClientCount() {
	clients, err := fetchClients()
	if err != nil {
		return // keep showing the last-known state rather than flashing errors
	}
	text, tooltip := formatClientCount(clients)

	clientCountMu.Lock()
	changed := clientCountText != text
	clientCountText = text
	clientTooltip = tooltip
	clientCountMu.Unlock()

	if changed {
		RunOnUIThread(func() {
			if hwndStatus != 0 {
				procInvalidateRect.Call(uintptr(hwndStatus), 0, 1)
			}
		})
	}
}

// formatClientCount builds the item's label ("0 clients connected", "1
// client connected", ...) and its tooltip -- the same text when there are
// none, otherwise one formatClientLine (gui_clients.go, already used by
// the Clients dialog itself) per line, so hovering shows exactly what
// opening the dialog would.
func formatClientCount(clients []aishwinwire.ClientData) (text, tooltip string) {
	n := len(clients)
	label := "clients"
	if n == 1 {
		label = "client"
	}
	text = fmt.Sprintf("%d %s connected", n, label)
	if n == 0 {
		return text, text
	}
	lines := make([]string, len(clients))
	for i, c := range clients {
		lines[i] = formatClientLine(c)
	}
	return text, text + "\n" + strings.Join(lines, "\n")
}

// ---- Auto Scroll switch ----

const (
	switchTrackWidth    = 34
	switchTrackHeight   = 16
	switchLabelGap      = 6 // between the "Auto Scroll" label and the track
	autoScrollLabelText = "Auto Scroll"
)

// autoScrollEnabled is GUI-thread-only state (touched only from the log
// view's own append/paint/timer handling and the switch's click handler,
// all of which run via the message loop), unlike statusConnected above,
// which a background goroutine (SetConnected) also writes -- see
// gui.go's drainLogQueue/pollAutoScrollState for how this drives (and
// detects changes to) the log view's actual scroll behavior.
var autoScrollEnabled = true

// setAutoScroll changes the switch's state, jumping the log view to the
// bottom when turning it back on (the whole point of clicking it), and
// repainting the switch either way. A no-op if already in the requested
// state, so pollAutoScrollState's every-400ms check and a real click can't
// fight each other into a redundant paint. Must be called on the GUI
// thread (both current callers -- the switch's own onClick and
// pollAutoScrollState -- already are).
func setAutoScroll(enabled bool) {
	if autoScrollEnabled == enabled {
		return
	}
	autoScrollEnabled = enabled
	if enabled {
		scrollToBottom()
	}
	if hwndStatus != 0 {
		procInvalidateRect.Call(uintptr(hwndStatus), 0, 1)
	}
}

// buildAutoScrollItem measures the label once (against the same font
// drawItem's WM_PAINT selects, so the measured and rendered widths match)
// to size the item exactly, rather than guessing a fixed width that could
// clip the text or leave awkward empty space.
func buildAutoScrollItem() statusItem {
	labelW := measureTextWidth(autoScrollLabelText)
	totalWidth := labelW + switchLabelGap + switchTrackWidth

	draw := func(hdc syscall.Handle, r rect) {
		textPtr, textLen := utf16CountedString(autoScrollLabelText)
		textY := r.top + (r.bottom-r.top-textHeight(hdc))/2
		procTextOutW.Call(uintptr(hdc), uintptr(r.left), uintptr(textY), uintptr(unsafe.Pointer(textPtr)), uintptr(textLen))

		trackLeft := r.left + labelW + switchLabelGap
		trackTop := r.top + (r.bottom-r.top-switchTrackHeight)/2
		drawSwitchTrack(hdc, rect{left: trackLeft, top: trackTop, right: trackLeft + switchTrackWidth, bottom: trackTop + switchTrackHeight})
	}

	return statusItem{
		width: totalWidth,
		align: alignRight,
		draw:  draw,
		tooltip: func() string {
			if autoScrollEnabled {
				return "Auto Scroll: On"
			}
			return "Auto Scroll: Off — click to resume"
		},
		// Deliberately always turns ON rather than toggling: turning OFF
		// only ever happens by scrolling away (pollAutoScrollState), never
		// by clicking the switch itself.
		onClick: func() {
			setAutoScroll(true)
		},
	}
}

// drawSwitchTrack paints a pill-shaped track (green/on, gray/off) with a
// white thumb at the corresponding end, in r.
func drawSwitchTrack(hdc syscall.Handle, r rect) {
	trackColor := uintptr(0x00A0A0A0) // gray -- off, matches the LED's own "not connected" gray
	if autoScrollEnabled {
		trackColor = 0x0000C000 // green -- on, matches the LED's own "connected" green
	}
	nullPenH, _, _ := procGetStockObject.Call(nullPen)

	trackBrush, _, _ := procCreateSolidBrush.Call(trackColor)
	oldBrush, _, _ := procSelectObject.Call(uintptr(hdc), trackBrush)
	oldPen, _, _ := procSelectObject.Call(uintptr(hdc), nullPenH)
	corner := r.bottom - r.top // fully rounded ends, i.e. a pill
	procRoundRect.Call(uintptr(hdc), uintptr(r.left), uintptr(r.top), uintptr(r.right), uintptr(r.bottom), uintptr(corner), uintptr(corner))
	procSelectObject.Call(uintptr(hdc), oldBrush)
	procDeleteObject.Call(trackBrush)

	const thumbInset = 2
	thumbDiameter := (r.bottom - r.top) - thumbInset*2
	thumbTop := r.top + thumbInset
	thumbLeft := r.left + thumbInset
	if autoScrollEnabled {
		thumbLeft = r.right - thumbInset - thumbDiameter
	}
	thumbBrush, _, _ := procCreateSolidBrush.Call(0x00FFFFFF) // white
	procSelectObject.Call(uintptr(hdc), thumbBrush)
	procEllipse.Call(uintptr(hdc), uintptr(thumbLeft), uintptr(thumbTop), uintptr(thumbLeft+thumbDiameter), uintptr(thumbTop+thumbDiameter))
	procSelectObject.Call(uintptr(hdc), oldPen)
	procDeleteObject.Call(thumbBrush)
}

// selectStatusFont selects DEFAULT_GUI_FONT into hdc (the status bar's
// custom window class carries no font of its own the way a dialog does)
// and returns the previously selected font, for the caller to restore.
func selectStatusFont(hdc syscall.Handle) uintptr {
	font, _, _ := procGetStockObject.Call(defaultGuiFont)
	old, _, _ := procSelectObject.Call(uintptr(hdc), font)
	return old
}

// textHeight returns hdc's currently selected font's line height, used to
// vertically center a single line of text within an item's rect.
func textHeight(hdc syscall.Handle) int32 {
	textPtr, textLen := utf16CountedString("Ag") // ascender+descender sample, not the real label -- only the height is used
	var sz sizeT
	procGetTextExtentPoint32W.Call(uintptr(hdc), uintptr(unsafe.Pointer(textPtr)), uintptr(textLen), uintptr(unsafe.Pointer(&sz)))
	return sz.cy
}

// measureTextWidth measures s as it will actually be drawn (DEFAULT_GUI_FONT,
// the same font WM_PAINT selects), using the screen DC -- safe for pure text
// metrics without a specific window, and needed here since this runs once
// at item-construction time, before the status bar window exists.
func measureTextWidth(s string) int32 {
	hdc, _, _ := procGetDC.Call(0)
	defer procReleaseDC.Call(0, hdc)
	oldFont := selectStatusFont(syscall.Handle(hdc))
	defer procSelectObject.Call(hdc, oldFont)
	return measureTextWidthHDC(hdc, s)
}

// measureTextWidthHDC is measureTextWidth given an already-open hdc with
// the desired font already selected -- for measuring several lines (the
// tooltip popup's multi-line sizing) without repeating GetDC/SelectObject
// per line.
func measureTextWidthHDC(hdc uintptr, s string) int32 {
	textPtr, textLen := utf16CountedString(s)
	var sz sizeT
	procGetTextExtentPoint32W.Call(hdc, uintptr(unsafe.Pointer(textPtr)), uintptr(textLen), uintptr(unsafe.Pointer(&sz)))
	return sz.cx
}

var statusBarWndProcPtr = syscall.NewCallback(statusBarWndProc)

// statusHotItem/statusTracking are only ever touched on the GUI thread
// (every message here arrives via the normal message loop), so they need
// no locking, unlike the atomic/mutex-guarded state other packages here
// share with background goroutines.
var (
	statusHotItem  = -1
	statusTracking bool
)

func statusBarWndProc(hwnd syscall.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmPaint:
		var ps paintStruct
		hdc, _, _ := procBeginPaint.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&ps)))
		oldFont := selectStatusFont(syscall.Handle(hdc))
		procSetBkMode.Call(hdc, bkModeTransparent)
		var rc rect
		procGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&rc)))
		for i, item := range statusItems {
			item.draw(syscall.Handle(hdc), statusItemRect(i, rc.right-rc.left, rc.bottom-rc.top))
		}
		procSelectObject.Call(hdc, oldFont)
		procEndPaint.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&ps)))
		return 0

	case wmMouseMove:
		if !statusTracking {
			armMouseLeaveTracking(hwnd)
		}
		x, y := lparamToXY(lParam)
		var rc rect
		procGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&rc)))
		hot := hitTestStatusItem(x, y, rc.right-rc.left, rc.bottom-rc.top)
		if hot != statusHotItem {
			statusHotItem = hot
			if hot >= 0 {
				// Anchor to the item's own rect, not the exact cursor
				// position -- the status bar sits at the bottom of the
				// window (often near the bottom of the screen too), so
				// showTooltip positions itself ABOVE this point; anchoring
				// below (as a first cut of this did) let the taskbar clip
				// it off-screen.
				r := statusItemRect(hot, rc.right-rc.left, rc.bottom-rc.top)
				topLeft := point{x: r.left, y: r.top}
				procClientToScreen.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&topLeft)))
				showTooltip(statusItems[hot].tooltip(), topLeft.x, topLeft.y)
			} else {
				hideTooltip()
			}
		}
		return 0

	case wmMouseLeave:
		statusTracking = false
		statusHotItem = -1
		hideTooltip()
		return 0

	case wmLButtonUp:
		x, y := lparamToXY(lParam)
		var rc rect
		procGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&rc)))
		if i := hitTestStatusItem(x, y, rc.right-rc.left, rc.bottom-rc.top); i >= 0 && statusItems[i].onClick != nil {
			statusItems[i].onClick()
		}
		return 0

	case wmSetCursor:
		if statusHotItem >= 0 && statusItems[statusHotItem].onClick != nil {
			procSetCursor.Call(uintptr(loadCursorHand()))
			return 1
		}
	}
	r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(message), wParam, lParam)
	return r
}

// armMouseLeaveTracking requests one WM_MOUSELEAVE for hwnd. Without this,
// WM_MOUSEMOVE alone never fires once the cursor leaves the window
// entirely, so a tooltip shown while hovering an item could get stuck
// onscreen after a fast mouse-out. Re-armed on every WM_MOUSEMOVE that
// finds tracking already lapsed (WM_MOUSELEAVE and TrackMouseEvent's own
// completion both clear it) -- the standard pattern for this API.
func armMouseLeaveTracking(hwnd syscall.Handle) {
	var tme trackMouseEvent
	tme.cbSize = uint32(unsafe.Sizeof(tme))
	tme.dwFlags = tmeLeave
	tme.hwndTrack = hwnd
	procTrackMouseEvent.Call(uintptr(unsafe.Pointer(&tme)))
	statusTracking = true
}

// lparamToXY extracts a mouse message's client-coordinate point from
// lParam (the GET_X_LPARAM/GET_Y_LPARAM macros' shape: signed low/high
// words -- coordinates can be negative just off a window's edge).
func lparamToXY(lParam uintptr) (x, y int32) {
	x = int32(int16(uint16(lParam & 0xFFFF)))
	y = int32(int16(uint16((lParam >> 16) & 0xFFFF)))
	return x, y
}

// ---- hand-rolled hover tooltip popup (see file header for why this isn't
// the real Windows tooltip common control) ----

var (
	hwndTooltip       syscall.Handle
	tooltipText       string
	tooltipWndProcPtr = syscall.NewCallback(tooltipWndProc)
)

// createTooltipWindow registers the tooltip's window class and creates it,
// hidden, owned by hwndMain (so it's destroyed/hidden together with the
// main window rather than lingering as an independent top-level window).
// Called once from createChildWidgets.
func createTooltipWindow(inst syscall.Handle) {
	cls := utf16ptr(tooltipClassName)
	bg, _, _ := procGetSysColor.Call(colorInfoBk)
	brush, _, _ := procCreateSolidBrush.Call(bg)

	var wc wndClassExW
	wc.size = uint32(unsafe.Sizeof(wc))
	wc.wndProc = tooltipWndProcPtr
	wc.instance = inst
	wc.cursor = loadCursorArrow()
	wc.background = syscall.Handle(brush)
	wc.className = cls
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	h, _, _ := procCreateWindowExW.Call(
		uintptr(wsExTopmost|wsExToolWindow),
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(utf16ptr(""))),
		uintptr(wsPopup|wsBorder),
		0, 0, 10, 10,
		uintptr(hwndMain), 0, uintptr(inst), 0,
	)
	hwndTooltip = syscall.Handle(h)
	if hwndTooltip != 0 {
		font, _, _ := procGetStockObject.Call(defaultGuiFont)
		procSendMessageW.Call(uintptr(hwndTooltip), wmSetFont, font, 0)
	}
}

// showTooltip sizes the popup to fit text (which may be multiple lines,
// separated by \n -- the client-count item's tooltip lists one line per
// connected client) and shows it ABOVE (screenX, screenY) -- the anchor
// point is the hovered item's top-left corner, in screen coordinates, so
// the popup's bottom edge ends up a small gap above the status bar
// itself. The status bar sits at the bottom of the window, which is often
// near the bottom of the screen (with the taskbar right there too), so
// positioning below the anchor (this function's first cut) could clip the
// popup off-screen entirely; above always has the whole window and status
// bar itself as headroom. Shown without stealing focus (SWP_NOACTIVATE --
// a tooltip that could steal keyboard focus from whatever the user was
// doing would be a real, surprising bug, not just a cosmetic one).
func showTooltip(text string, screenX, screenY int32) {
	if hwndTooltip == 0 || text == "" {
		return
	}
	tooltipText = text
	lines := strings.Split(text, "\n")

	hdc, _, _ := procGetDC.Call(uintptr(hwndTooltip))
	oldFont := selectStatusFont(syscall.Handle(hdc))
	lineHeight := textHeight(syscall.Handle(hdc))
	var maxWidth int32
	for _, line := range lines {
		if w := measureTextWidthHDC(hdc, line); w > maxWidth {
			maxWidth = w
		}
	}
	procSelectObject.Call(hdc, oldFont)
	procReleaseDC.Call(uintptr(hwndTooltip), hdc)

	const padX, padY, lineGap = 8, 4, 2
	w := maxWidth + padX*2
	h := lineHeight*int32(len(lines)) + lineGap*int32(len(lines)-1) + padY*2

	const gap = 6
	x := screenX
	y := screenY - h - gap

	procSetWindowPos.Call(uintptr(hwndTooltip), 0, uintptr(x), uintptr(y), uintptr(w), uintptr(h), uintptr(swpNoActivate|swpShowWindow))
	procInvalidateRect.Call(uintptr(hwndTooltip), 0, 1)
}

func hideTooltip() {
	if hwndTooltip == 0 {
		return
	}
	procShowWindow.Call(uintptr(hwndTooltip), swHide)
}

func tooltipWndProc(hwnd syscall.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmPaint:
		var ps paintStruct
		hdc, _, _ := procBeginPaint.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&ps)))
		oldFont := selectStatusFont(syscall.Handle(hdc))
		textColor, _, _ := procGetSysColor.Call(colorInfoText)
		bkColor, _, _ := procGetSysColor.Call(colorInfoBk)
		procSetTextColor.Call(hdc, textColor)
		procSetBkColor.Call(hdc, bkColor)
		const padX, padY, lineGap = 8, 4, 2
		lineHeight := textHeight(syscall.Handle(hdc))
		y := int32(padY)
		for _, line := range strings.Split(tooltipText, "\n") {
			textPtr, textLen := utf16CountedString(line)
			procTextOutW.Call(hdc, padX, uintptr(y), uintptr(unsafe.Pointer(textPtr)), uintptr(textLen))
			y += lineHeight + lineGap
		}
		procSelectObject.Call(hdc, oldFont)
		procEndPaint.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&ps)))
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(message), wParam, lParam)
	return r
}

// utf16CountedString encodes s as UTF-16 and returns a pointer to its first
// character plus its length EXCLUDING the implicit NUL terminator --
// GetTextExtentPoint32W/TextOutW both take an explicit character count
// rather than expecting a NUL-terminated string the way most of this
// codebase's *W calls do via utf16ptr.
func utf16CountedString(s string) (*uint16, int32) {
	u, err := syscall.UTF16FromString(s)
	if err != nil || len(u) == 0 {
		return nil, 0
	}
	return &u[0], int32(len(u) - 1)
}
