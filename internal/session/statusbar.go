package session

import (
	"fmt"
	"sync"
	"unicode/utf8"
)

// StatusBar reserves the terminal's bottom row via a DECSTBM scroll region and
// paints a caller-supplied line there. At a shell prompt the shell's output
// scrolls within rows 1..rows-1, so the reserved row survives without shrinking
// the PTY. A full-screen (alt-screen) app uses the whole screen and covers the
// bar — and gets every row — so the bar is "gone" inside vim/htop; on exit the
// region is re-asserted and the bar repainted. Not byte-transparent: a
// deliberate standing exception, painted only through Session.WriteOut (under
// outMu, so it never interleaves with shell output or a console prompt).
type StatusBar struct {
	sess    *Session
	content func() string
	isAlt   func() bool

	mu        sync.Mutex
	rows      int
	cols      int
	regionSet bool
	enabled   bool
	last      string
	ticks     int
}

func NewStatusBar(s *Session, content func() string, isAlt func() bool) *StatusBar {
	return &StatusBar{sess: s, content: content, isAlt: isAlt}
}

// SetSize records the terminal dimensions (wire it to Session.OnResize) and
// forces the scroll region to be re-asserted at the next Tick.
func (b *StatusBar) SetSize(rows, cols uint16) {
	b.mu.Lock()
	b.rows, b.cols = int(rows), int(cols)
	b.regionSet = false
	b.enabled = b.rows >= 3 && b.cols >= 8
	b.mu.Unlock()
}

// Tick reconciles the reserved row with the current state. Cheap and idempotent
// at a steady prompt; call it on a modest interval.
func (b *StatusBar) Tick() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.enabled {
		return
	}
	b.ticks++
	if b.isAlt() {
		// A full-screen app owns the whole grid: release our region and stop
		// painting so it has every row and the bar is out of the way.
		if b.regionSet {
			b.sess.WriteOut([]byte("\x1b7\x1b[r\x1b8"))
			b.regionSet = false
		}
		return
	}
	if !b.regionSet {
		// Reserve rows 1..rows-1 for the shell, keeping the last row for the bar.
		// DECSTBM homes the cursor, so save/restore around it.
		b.sess.WriteOut([]byte(fmt.Sprintf("\x1b7\x1b[1;%dr\x1b8", b.rows-1)))
		b.regionSet = true
		b.last = ""
	}
	line := b.content()
	// Repaint on change, and periodically to recover from a clobber (e.g. a
	// `clear`, which wipes the row without changing the content).
	if line == b.last && b.ticks%4 != 0 {
		return
	}
	b.last = line
	b.paint(line)
}

// paint writes the bar on the last row without disturbing the shell's cursor.
func (b *StatusBar) paint(line string) {
	line = truncateCells(line, b.cols-1)
	// save cursor; go to last row col 1; clear it; reverse-video the text; reset;
	// restore cursor.
	b.sess.WriteOut([]byte(fmt.Sprintf("\x1b7\x1b[%d;1H\x1b[K\x1b[7m%s\x1b[0m\x1b8", b.rows, line)))
}

// Close resets the scroll region and clears the reserved row so the terminal is
// left clean on exit.
func (b *StatusBar) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rows == 0 {
		return
	}
	b.sess.WriteOut([]byte(fmt.Sprintf("\x1b7\x1b[r\x1b[%d;1H\x1b[K\x1b8", b.rows)))
	b.regionSet = false
}

// truncateCells trims s to at most n display columns, approximating one cell per
// rune with a 2-cell safety margin (a couple of the glyphs are double-width).
// Under-fill is harmless; over-fill would wrap the bar onto the shell's row.
func truncateCells(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n-2 {
		return s
	}
	r := []rune(s)
	end := n - 3
	if end < 0 {
		end = 0
	}
	return string(r[:end]) + "…"
}
