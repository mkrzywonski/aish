//go:build windows

package main

// screenshot.go: lets the AI see the actual GUI window (or, with
// permission, the whole screen) without asking the human to take a
// screenshot, via the capture_screen wire frame (handleCaptureScreen,
// dispatched from link.go's handleFrame) and its aishwnd-side MCP tool
// (internal/aishwnd/screenshot.go). Capture happens entirely in memory
// (CaptureWindow/CaptureFullScreen return PNG bytes, not a path) and the
// result travels back over the same persistent stdio connection every
// exec/file_* call already uses -- no disk artifact, no polling, and no
// separate PID-discovery step, since the wire round trip is already
// scoped to this one connected session.
//
// An earlier version used a file-trigger/file-result mechanism in
// C:\Users\Public specifically to avoid touching aishwnd/the wire
// protocol at all (the AI would create a trigger file and download the
// resulting PNG using the already-working exec/file_download tools).
// That shortcut caused real, accumulating problems once actually used
// live: a persistent per-PID .png file with nothing ever cleaning it up
// (found live, the human watching the actual screen), a hard race when
// more than one aishwin.exe instance polled the same shared path, and up
// to 500ms of trigger-polling latency. The wire-based version here
// removes all three at once.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"ai-ssh/internal/aishwinwire"
)

var errNoWindow = errors.New("aishwin: no window to capture (target window is zero-sized or unset)")

const (
	pwRenderFullContent = 0x00000002
	biRGB               = 0
	dibRGBColors        = 0
	srcCopy             = 0x00CC0020

	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79

	// maxStripWidth bounds every single BitBlt-from-screen +
	// GetDIBits call's width to a value confirmed live (bisecting with a
	// dev-build-only diagnostic, since removed) to stay under a hard
	// limit on this host's (RDP/virtualized) display adapter: capture
	// succeeded up to width 681 and failed at 682+, regardless of height
	// or total buffer size -- GetDeviceCaps' HORZRES/VERTRES meanwhile
	// correctly reported the full 1600x1200 desktop, ruling out a
	// simple resolution mismatch. A same-process PrintWindow-based
	// capture (CaptureWindow) was thought immune, but that was only
	// because it had been tested at narrow widths -- the same limit
	// applies to its GetDIBits read-back too (a 1616px-wide window
	// returned 0 from a single GetDIBits), so CaptureWindow now mosaics
	// through captureDCMosaic identically. 640
	// keeps a safety margin below the observed 681 edge rather than
	// hugging it exactly. Full-screen capture mosaics the desktop into
	// vertical strips this width or narrower, each captured with its own
	// BitBlt+GetDIBits call and stitched together in Go -- no single GDI
	// call ever sees a width wider than this limit.
	maxStripWidth = 640
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

// handleCaptureScreen answers a capture_screen wire request (link.go's
// handleFrame) with a base64-encoded PNG, or an error -- the sole
// producer/consumer contract with internal/aishwnd/screenshot.go's
// capture_screen MCP tool on the other end of this connection.
func handleCaptureScreen(wc *aishwinwire.Conn, f aishwinwire.Frame) {
	var req aishwinwire.CaptureScreenData
	if err := json.Unmarshal(f.Data, &req); err != nil {
		return
	}

	result := aishwinwire.CaptureScreenResultData{}
	fullScreen := req.Mode == "full" || req.Mode == "screen"

	switch {
	case fullScreen && !fullScreenCaptureAllowed():
		result.Error = "the user denied full-screen capture"
	default:
		var data []byte
		var err error
		if fullScreen {
			data, err = CaptureFullScreen()
		} else {
			data, err = CaptureWindow()
		}
		if err != nil {
			result.Error = err.Error()
		} else {
			result.PNG = base64.StdEncoding.EncodeToString(data)
			if fullScreen {
				AppendLogColor("Full-screen screenshot captured", colorRunning)
			} else {
				AppendLogColor("Screenshot captured", colorRunning)
			}
		}
	}

	data, err := json.Marshal(result)
	if err != nil {
		return
	}
	_ = wc.Send(aishwinwire.Frame{Type: "capture_screen_result", ID: f.ID, Data: data})
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

// CaptureWindow renders the relevant window for this process (targetWindow
// -- either hwndMain or a currently open modal dialog, including
// non-client chrome via PrintWindow) and returns it PNG-encoded.
//
// PrintWindow renders the whole window into one memory bitmap; the pixels
// are then read back in vertical strips at most maxStripWidth wide, exactly
// the way CaptureFullScreen mosaics the desktop. This host's GetDIBits
// width limit (see maxStripWidth) applies to THIS path too, not just
// screen-DC BitBlt: a single GetDIBits across the full width of a wide
// window (e.g. 1616px) returns 0 on this host's display adapter, which
// manifested as window capture failing outright at large window sizes
// while still succeeding when the window was narrow. PrintWindow itself has
// no such width limit, so the render happens once at full size and only the
// GetDIBits read-back is striped -- deterministic memory-to-memory work
// with neither the width limit nor the DWM race.
func CaptureWindow() ([]byte, error) {
	hwnd := targetWindow()

	var rc rect
	procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&rc)))
	width := int(rc.right - rc.left)
	height := int(rc.bottom - rc.top)
	if width <= 0 || height <= 0 {
		return nil, errNoWindow
	}

	hdcScreen, _, _ := procGetDC.Call(0)
	defer procReleaseDC.Call(0, hdcScreen)

	hdcMem, _, _ := procCreateCompatibleDC.Call(hdcScreen)
	defer procDeleteDC.Call(hdcMem)
	hBitmap, _, _ := procCreateCompatibleBmp.Call(hdcScreen, uintptr(width), uintptr(height))
	defer procDeleteObject.Call(hBitmap)

	// hBitmap stays selected into hdcMem for the whole read-back: it is
	// PrintWindow's render target AND the BitBlt source for every strip in
	// captureDCMosaic (a BitBlt source DC must have its bitmap selected).
	// Only each per-strip DESTINATION bitmap needs deselecting before its
	// own GetDIBits, which captureScreenStripOnce already handles. Defers
	// run LIFO, so this deselect runs before the bitmap/DC are deleted.
	oldObj, _, _ := procSelectObject.Call(hdcMem, hBitmap)
	defer procSelectObject.Call(hdcMem, oldObj)

	// Retry the render itself: PrintWindow with PW_RENDERFULLCONTENT can
	// involve DWM composition that isn't synchronously complete when
	// PrintWindow returns. A fresh call into the same bitmap gets a new
	// attempt; the strip read-back below is deterministic and never hits
	// the DWM race or the width limit.
	const maxRenderAttempts = 4
	rendered := false
	for attempt := 1; attempt <= maxRenderAttempts; attempt++ {
		if r, _, _ := procPrintWindow.Call(uintptr(hwnd), hdcMem, pwRenderFullContent); r != 0 {
			rendered = true
			break
		}
		if attempt < maxRenderAttempts {
			time.Sleep(75 * time.Millisecond)
		}
	}
	if !rendered {
		return nil, fmt.Errorf("PrintWindow failed for %dx%d window after %d attempts", width, height, maxRenderAttempts)
	}

	return captureDCMosaic(hdcMem, 0, 0, width, height)
}

// CaptureFullScreen renders the entire virtual screen (spanning all
// monitors, not just the primary one) and returns it PNG-encoded. Gated by
// fullScreenCaptureAllowed at the call site (handleCaptureScreen), not
// here, since this function is the mechanical capture step only.
//
// Captured as a mosaic of vertical strips, each at most maxStripWidth wide
// -- see that constant's doc comment for why a single BitBlt+GetDIBits
// call spanning the whole desktop width fails silently (succeeds, but
// returns all-zero pixels) on this host.
func CaptureFullScreen() ([]byte, error) {
	originX, _, _ := procGetSystemMetrics.Call(smXVirtualScreen)
	originY, _, _ := procGetSystemMetrics.Call(smYVirtualScreen)
	widthR, _, _ := procGetSystemMetrics.Call(smCXVirtualScreen)
	heightR, _, _ := procGetSystemMetrics.Call(smCYVirtualScreen)
	// originX/originY can be negative (a monitor positioned left of or
	// above the primary) -- int32->int->uintptr is a genuine runtime
	// conversion here (not a constant expression), so it sign-extends
	// correctly, unlike the CW_USEDEFAULT constant-folding pitfall
	// elsewhere in this codebase.
	ox, oy := int(int32(originX)), int(int32(originY))
	width, height := int(int32(widthR)), int(int32(heightR))
	if width <= 0 || height <= 0 {
		return nil, errNoWindow
	}

	hdcScreen, _, _ := procGetDC.Call(0)
	defer procReleaseDC.Call(0, hdcScreen)

	return captureDCMosaic(hdcScreen, ox, oy, width, height)
}

// captureDCMosaic reads a width x height region of srcDC (its top-left at
// (srcOriginX, srcOriginY) in srcDC's own coordinate space) into a
// PNG-encoded image, in vertical strips at most maxStripWidth wide so no
// single GetDIBits call exceeds this host's width limit (see maxStripWidth).
// Shared by CaptureFullScreen (srcDC is the screen DC, origin the virtual
// desktop's) and CaptureWindow (srcDC is a memory DC holding a
// PrintWindow'd window, origin 0,0).
func captureDCMosaic(srcDC uintptr, srcOriginX, srcOriginY, width, height int) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for stripX := 0; stripX < width; stripX += maxStripWidth {
		stripW := maxStripWidth
		if stripX+stripW > width {
			stripW = width - stripX
		}
		if err := captureScreenStrip(srcDC, img, srcOriginX+stripX, srcOriginY, stripX, stripW, height); err != nil {
			return nil, err
		}
	}
	return encodePNG(img)
}

// captureScreenStrip BitBlts one vertical strip out of srcDC -- the real
// screen DC (CaptureFullScreen) or a memory DC holding a PrintWindow'd
// window (CaptureWindow) -- stripW wide, full height, source top-left at
// (srcX, srcY) in srcDC's coordinate space, and copies it into img at
// destination x=dstX, y=0.
//
// Retries the whole BitBlt-to-GetDIBits sequence, with a fresh bitmap each
// attempt: found live that when srcDC is the real screen DC a real Windows
// host can fail GetDIBits even across several whole-sequence retries, not
// just within one bitmap's inner retry window (the memory-DC window path
// never fails here, but shares the code harmlessly). Measured live at
// 4 attempts: roughly 1 real full-screen capture in 4 still failed
// outright (most reproducibly on the first strip captured right after the
// one-time full-screen consent dialog closes, suggesting the dialog's own
// teardown disrupts DWM timing worse than an ordinary PrintWindow call).
// 16 attempts (this capture_screen call has a 130-second wire-round-trip
// budget to spend, aishwnd's captureScreenTimeout) cleared that live test
// with zero failures across repeated full captures; kept well under the
// budget even at 3 strips x 16 attempts worst case.
func captureScreenStrip(hdcScreen uintptr, img *image.RGBA, srcX, srcY, dstX, stripW, height int) error {
	const maxAttempts = 16
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		buf, err := captureScreenStripOnce(hdcScreen, srcX, srcY, stripW, height)
		if err == nil {
			bgraToRGBA(img, buf, dstX, 0, stripW, height)
			return nil
		}
		lastErr = err
		if attempt < maxAttempts {
			time.Sleep(75 * time.Millisecond)
		}
	}
	return lastErr
}

// captureScreenStripOnce is one BitBlt + GetDIBits attempt for a single
// strip, given a fresh DC and bitmap -- captureScreenStrip retries this as
// a whole unit.
func captureScreenStripOnce(hdcScreen uintptr, srcX, srcY, stripW, height int) ([]byte, error) {
	hdcMem, _, _ := procCreateCompatibleDC.Call(hdcScreen)
	defer procDeleteDC.Call(hdcMem)
	hBitmap, _, _ := procCreateCompatibleBmp.Call(hdcScreen, uintptr(stripW), uintptr(height))
	defer procDeleteObject.Call(hBitmap)

	oldObj, _, _ := procSelectObject.Call(hdcMem, hBitmap)
	procBitBlt.Call(hdcMem, 0, 0, uintptr(stripW), uintptr(height), hdcScreen, uintptr(srcX), uintptr(srcY), srcCopy)
	procSelectObject.Call(hdcMem, oldObj) // must deselect before GetDIBits, see CaptureWindow's comment

	return captureDIBits(hdcMem, hBitmap, stripW, height)
}

// captureDIBits extracts hBitmap's raw top-down BGRA pixel bytes
// (width*height*4) via GetDIBits. hBitmap must already be deselected from
// any DC it was rendered into (see CaptureWindow's comment).
//
// Retries a few times on failure: found live that GetDIBits fails outright
// (returns 0) roughly half the time immediately after PrintWindow with
// PW_RENDERFULLCONTENT, even with the deselect fix above already applied
// -- PW_RENDERFULLCONTENT can involve DWM composition, and the actual
// render into the target bitmap isn't always synchronously complete by
// the time PrintWindow itself returns. A short retry loop lets that
// finish rather than failing the whole capture on a timing fluke.
func captureDIBits(hdcMem, hBitmap uintptr, width, height int) ([]byte, error) {
	var bi bitmapInfoHeader
	bi.size = uint32(unsafe.Sizeof(bi))
	bi.width = int32(width)
	bi.height = int32(-height) // negative: top-down DIB, matches image.RGBA row order
	bi.planes = 1
	bi.bitCount = 32
	bi.compression = biRGB

	buf := make([]byte, width*height*4)
	const maxAttempts = 8
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		r, _, _ := procGetDIBits.Call(hdcMem, hBitmap, 0, uintptr(height), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&bi)), dibRGBColors)
		if r != 0 {
			return buf, nil
		}
		if attempt < maxAttempts {
			time.Sleep(25 * time.Millisecond)
		}
	}
	return nil, fmt.Errorf("GetDIBits failed to copy %dx%d pixels after %d attempts", width, height, maxAttempts)
}

// bgraToRGBA copies one BGRA-packed capture buffer (as captureDIBits
// returns) into img at (dstX, dstY), converting BGRA -> RGBA -- GDI's
// 32bpp DIB format is BGRX/BGRA, image.RGBA expects RGBA.
func bgraToRGBA(img *image.RGBA, buf []byte, dstX, dstY, width, height int) {
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := (y*width + x) * 4
			b, g, r := buf[i], buf[i+1], buf[i+2]
			off := img.PixOffset(dstX+x, dstY+y)
			img.Pix[off] = r
			img.Pix[off+1] = g
			img.Pix[off+2] = b
			img.Pix[off+3] = 255
		}
	}
}

// encodePNG is the shared final step for both CaptureWindow and
// CaptureFullScreen -- entirely in memory, no temp file.
func encodePNG(img *image.RGBA) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
