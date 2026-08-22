package main

// screenshot.go: lets the AI see the actual GUI window (or, with
// permission, the whole screen) without asking the human to take a
// screenshot. A background goroutine polls for a trigger file at a
// cross-account-readable path (C:\Users\Public -- the same fix used
// earlier in this project for the mike/mk31 WSL-visibility split) and,
// when it appears, captures a PNG at a second path, writes a result file,
// and deletes the trigger. This needs zero changes to aicmdd/the wire
// protocol: the AI creates the trigger and downloads the PNG using the
// already-working exec/file_download tools.
//
// The trigger/output paths are scoped by this process's own PID rather
// than fixed: with a single shared path, every running aishwin.exe watches
// the same file, so whichever instance's watcher goroutine happens to
// notice it first "wins" -- observed live, capturing the wrong window
// entirely when a test instance ran alongside the production one. The
// AI already has to resolve a target instance's PID to safely address it
// for anything else (cleanup, task #22's status-bar PID), so requiring
// the same PID here removes the race instead of just narrowing it.

import (
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var errNoWindow = errors.New("aishwin: no window to capture (target window is zero-sized or unset)")

var (
	screenshotTriggerPath = fmt.Sprintf(`C:\Users\Public\aishwin-screenshot-request-%d`, os.Getpid())
	screenshotOutputPath  = fmt.Sprintf(`C:\Users\Public\aishwin-screenshot-%d.png`, os.Getpid())
	screenshotResultPath  = fmt.Sprintf(`C:\Users\Public\aishwin-screenshot-result-%d`, os.Getpid())
)

const (
	pwRenderFullContent = 0x00000002
	biRGB               = 0
	dibRGBColors        = 0
	srcCopy             = 0x00CC0020

	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
)

var (
	procGetDC                    = user32.NewProc("GetDC")
	procReleaseDC                = user32.NewProc("ReleaseDC")
	procPrintWindow              = user32.NewProc("PrintWindow")
	procCreateCompatibleDC       = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBmp      = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject             = gdi32.NewProc("SelectObject")
	procDeleteDC                 = gdi32.NewProc("DeleteDC")
	procDeleteObject             = gdi32.NewProc("DeleteObject")
	procGetDIBits                = gdi32.NewProc("GetDIBits")
	procGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	procBitBlt                   = gdi32.NewProc("BitBlt")
)

type bitmapInfoHeader struct {
	size          uint32
	width         int32
	height        int32
	planes        uint16
	bitCount      uint16
	compression   uint32
	sizeImage     uint32
	xPelsPerMeter int32
	yPelsPerMeter int32
	clrUsed       uint32
	clrImportant  uint32
}

// fullScreenGrantMu/fullScreenGranted implement a one-time-per-session
// permission grant for full-screen capture, separate from (and stricter
// than) the plain window capture: a full-screen shot can reveal whatever
// else the human has open, not just what the AI itself is doing in this
// window, so it warrants explicit consent rather than being always-on
// like the window capture. AskYesNo already auto-approves in dev builds
// (see gui_dialog.go), so this needs no separate devBuild bypass -- dev
// testing gets the grant for free, same as the connection-approval dialog.
var (
	fullScreenGrantMu sync.Mutex
	fullScreenGranted bool
)

func fullScreenCaptureAllowed() bool {
	fullScreenGrantMu.Lock()
	granted := fullScreenGranted
	fullScreenGrantMu.Unlock()
	if granted {
		return true
	}
	if !AskYesNo("The AI wants to capture your ENTIRE screen (not just the aishwin window). Allow for the rest of this session?", 0) {
		return false
	}
	fullScreenGrantMu.Lock()
	fullScreenGranted = true
	fullScreenGrantMu.Unlock()
	return true
}

// startScreenshotWatcher polls for the trigger file every 500ms for the
// life of the process. Safe to call once from main regardless of mode
// (smoke-test or real): it only touches hwndMain, which both modes set.
// The trigger file's content selects the capture mode: empty (the
// existing `type nul >` usage) captures just the relevant window;
// "full" or "screen" captures the whole screen instead, subject to the
// one-time permission grant above.
func startScreenshotWatcher() {
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			raw, err := os.ReadFile(screenshotTriggerPath)
			if err != nil {
				continue
			}
			_ = os.Remove(screenshotTriggerPath)
			mode := strings.TrimSpace(string(raw))

			if mode == "full" || mode == "screen" {
				if !fullScreenCaptureAllowed() {
					AppendLogColor("Full-screen screenshot denied by user", colorRunning)
					_ = os.WriteFile(screenshotResultPath, []byte("denied"), 0644)
					continue
				}
				if err := CaptureFullScreenToFile(screenshotOutputPath); err != nil {
					AppendLogColor(fmt.Sprintf("Screenshot failed: %v", err), colorRunning)
					fmt.Fprintf(stderr, "aishwin: screenshot failed: %v\n", err)
					_ = os.WriteFile(screenshotResultPath, []byte("error: "+err.Error()), 0644)
					continue
				}
				AppendLogColor("Full-screen screenshot captured", colorRunning)
				_ = os.WriteFile(screenshotResultPath, []byte("ok"), 0644)
				continue
			}

			if err := CaptureWindowToFile(screenshotOutputPath); err != nil {
				AppendLogColor(fmt.Sprintf("Screenshot failed: %v", err), colorRunning)
				fmt.Fprintf(stderr, "aishwin: screenshot failed: %v\n", err)
				_ = os.WriteFile(screenshotResultPath, []byte("error: "+err.Error()), 0644)
				continue
			}
			AppendLogColor("Screenshot captured", colorRunning)
			_ = os.WriteFile(screenshotResultPath, []byte("ok"), 0644)
		}
	}()
}

// targetWindow returns the window to capture: the current foreground
// window if it belongs to this process, else hwndMain. A modal dialog
// (MessageBoxW's ShowInfo, or AskYesNo/AskText's own DLGTEMPLATE windows)
// is a SEPARATE top-level window merely owned by hwndMain -- PrintWindow
// only ever renders the specific window handle it's given, so capturing
// hwndMain while a dialog is open silently shows the main window looking
// perfectly normal, with the dialog invisible to the screenshot entirely
// (found live: triggering Help>Status via devctl and screenshotting
// showed no dialog at all, even though it was genuinely open on screen).
func targetWindow() syscall.Handle {
	fg, _, _ := procGetForegroundWindow.Call()
	if fg == 0 {
		return hwndMain
	}
	var pid uint32
	procGetWindowThreadProcessId.Call(fg, uintptr(unsafe.Pointer(&pid)))
	if int(pid) != os.Getpid() {
		return hwndMain // some other application is focused; nothing of ours to prefer
	}
	return syscall.Handle(fg)
}

// CaptureWindowToFile renders the relevant window for this process
// (targetWindow -- either hwndMain or a currently open modal dialog,
// including non-client chrome via PrintWindow) into a PNG at path.
func CaptureWindowToFile(path string) error {
	hwnd := targetWindow()

	var rc rect
	procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&rc)))
	width := int(rc.right - rc.left)
	height := int(rc.bottom - rc.top)
	if width <= 0 || height <= 0 {
		return errNoWindow
	}

	hdcScreen, _, _ := procGetDC.Call(0)
	defer procReleaseDC.Call(0, hdcScreen)

	hdcMem, _, _ := procCreateCompatibleDC.Call(hdcScreen)
	defer procDeleteDC.Call(hdcMem)

	hBitmap, _, _ := procCreateCompatibleBmp.Call(hdcScreen, uintptr(width), uintptr(height))
	defer procDeleteObject.Call(hBitmap)

	oldObj, _, _ := procSelectObject.Call(hdcMem, hBitmap)
	defer procSelectObject.Call(hdcMem, oldObj)

	procPrintWindow.Call(uintptr(hwnd), hdcMem, pwRenderFullContent)

	return captureBitmapToPNG(hdcMem, hBitmap, width, height, path)
}

// CaptureFullScreenToFile renders the entire virtual screen (spanning all
// monitors, not just the primary one) into a PNG at path. Gated by
// fullScreenCaptureAllowed at the call site (startScreenshotWatcher), not
// here, since this function is the mechanical capture step only.
func CaptureFullScreenToFile(path string) error {
	originX, _, _ := procGetSystemMetrics.Call(smXVirtualScreen)
	originY, _, _ := procGetSystemMetrics.Call(smYVirtualScreen)
	widthR, _, _ := procGetSystemMetrics.Call(smCXVirtualScreen)
	heightR, _, _ := procGetSystemMetrics.Call(smCYVirtualScreen)
	width, height := int(int32(widthR)), int(int32(heightR))
	if width <= 0 || height <= 0 {
		return errNoWindow
	}

	hdcScreen, _, _ := procGetDC.Call(0)
	defer procReleaseDC.Call(0, hdcScreen)

	hdcMem, _, _ := procCreateCompatibleDC.Call(hdcScreen)
	defer procDeleteDC.Call(hdcMem)

	hBitmap, _, _ := procCreateCompatibleBmp.Call(hdcScreen, uintptr(width), uintptr(height))
	defer procDeleteObject.Call(hBitmap)

	oldObj, _, _ := procSelectObject.Call(hdcMem, hBitmap)
	defer procSelectObject.Call(hdcMem, oldObj)

	// originX/originY can be negative (a monitor positioned left of or
	// above the primary) -- int32->int->uintptr is a genuine runtime
	// conversion here (not a constant expression), so it sign-extends
	// correctly, unlike the CW_USEDEFAULT constant-folding pitfall
	// elsewhere in this codebase.
	procBitBlt.Call(hdcMem, 0, 0, uintptr(width), uintptr(height), hdcScreen, uintptr(int(int32(originX))), uintptr(int(int32(originY))), srcCopy)

	return captureBitmapToPNG(hdcMem, hBitmap, width, height, path)
}

// captureBitmapToPNG extracts hBitmap's pixels (widthxheight, currently
// selected into hdcMem) and encodes them as a PNG at path. Shared by
// CaptureWindowToFile and CaptureFullScreenToFile, which differ only in
// how they fill the bitmap (PrintWindow vs BitBlt) and where its
// dimensions come from.
func captureBitmapToPNG(hdcMem, hBitmap uintptr, width, height int, path string) error {
	var bi bitmapInfoHeader
	bi.size = uint32(unsafe.Sizeof(bi))
	bi.width = int32(width)
	bi.height = int32(-height) // negative: top-down DIB, matches image.RGBA row order
	bi.planes = 1
	bi.bitCount = 32
	bi.compression = biRGB

	buf := make([]byte, width*height*4)
	procGetDIBits.Call(hdcMem, hBitmap, 0, uintptr(height), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&bi)), dibRGBColors)

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := (y*width + x) * 4
			b, g, r := buf[i], buf[i+1], buf[i+2]
			off := img.PixOffset(x, y)
			img.Pix[off] = r
			img.Pix[off+1] = g
			img.Pix[off+2] = b
			img.Pix[off+3] = 255
		}
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
