package main

// screenshot.go: lets the AI see the actual GUI window without asking the
// human to take a screenshot. A background goroutine polls for a trigger
// file at a cross-account-readable path (C:\Users\Public -- the same fix
// used earlier in this project for the mike/mk31 WSL-visibility split)
// and, when it appears, captures hwndMain to a PNG at a second path and
// deletes the trigger. This needs zero changes to aicmdd/the wire
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
	"time"
	"unsafe"
)

var errNoWindow = errors.New("aishwin: no window to capture (hwndMain is zero-sized or unset)")

var (
	screenshotTriggerPath = fmt.Sprintf(`C:\Users\Public\aishwin-screenshot-request-%d`, os.Getpid())
	screenshotOutputPath  = fmt.Sprintf(`C:\Users\Public\aishwin-screenshot-%d.png`, os.Getpid())
)

const (
	pwRenderFullContent = 0x00000002
	biRGB               = 0
	dibRGBColors        = 0
)

var (
	procGetDC                = user32.NewProc("GetDC")
	procReleaseDC             = user32.NewProc("ReleaseDC")
	procPrintWindow           = user32.NewProc("PrintWindow")
	procCreateCompatibleDC    = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBmp   = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject          = gdi32.NewProc("SelectObject")
	procDeleteDC              = gdi32.NewProc("DeleteDC")
	procDeleteObject          = gdi32.NewProc("DeleteObject")
	procGetDIBits             = gdi32.NewProc("GetDIBits")
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

// startScreenshotWatcher polls for the trigger file every 500ms for the
// life of the process. Safe to call once from main regardless of mode
// (smoke-test or real): it only touches hwndMain, which both modes set.
func startScreenshotWatcher() {
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if _, err := os.Stat(screenshotTriggerPath); err != nil {
				continue
			}
			_ = os.Remove(screenshotTriggerPath)
			if err := CaptureWindowToFile(screenshotOutputPath); err != nil {
				fmt.Fprintf(stderr, "aishwin: screenshot failed: %v\n", err)
			}
		}
	}()
}

// CaptureWindowToFile renders hwndMain (including non-client chrome, via
// PrintWindow) into a PNG at path.
func CaptureWindowToFile(path string) error {
	var rc rect
	procGetWindowRect.Call(uintptr(hwndMain), uintptr(unsafe.Pointer(&rc)))
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

	procPrintWindow.Call(uintptr(hwndMain), hdcMem, pwRenderFullContent)

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
