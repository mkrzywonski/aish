package session

import (
	"fmt"
	"strings"
	"sync"
)

// StatusBar reserves the terminal's bottom physical row for a caller-supplied
// line. It works by giving the shell one fewer row (Session.reserveRow, so the
// PTY is sized rows-1) AND confining it to a matching scroll region (1..rows-1)
// — the two together keep full-screen apps (vim/htop) rendering correctly: they
// run in rows-1 rows, matching the region, so nothing they draw reaches or
// scrolls the reserved row. The bar is painted only at a prompt (a full-screen
// app clears the reserved row itself and owns the cursor); at the prompt it
// reappears. Not byte-transparent: a deliberate standing exception, painted only
// via Session.WriteOut (under outMu).
type StatusBar struct {
	sess    *Session
	content func() string
	isAlt   func() bool

	mu        sync.Mutex
	shellRows int // rows the shell has (physical rows minus the reserved one)
	cols      int
	regionSet bool
	enabled   bool
	wasAlt    bool // last Tick saw the alt screen (a full-screen app was up)
	last      string
	ticks     int
}

func NewStatusBar(s *Session, content func() string, isAlt func() bool) *StatusBar {
	s.SetReserveRow(true) // hand the shell one fewer row; we own the physical bottom
	return &StatusBar{sess: s, content: content, isAlt: isAlt}
}

// SetSize records the shell's row/col count (from Session.OnResize — already the
// reserved rows-1) and forces the scroll region to be re-asserted at next Tick.
func (b *StatusBar) SetSize(rows, cols uint16) {
	b.mu.Lock()
	b.shellRows, b.cols = int(rows), int(cols)
	b.regionSet = false
	b.enabled = b.shellRows >= 2 && b.cols >= 8
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
	alt := b.isAlt()
	// A full-screen app resets the scroll margins to full height on exit (rmcup /
	// ESC[r), which would let the shell scroll into our reserved row. Force a
	// re-assert when we leave the alt screen so the region is restored the moment
	// the app quits.
	if b.wasAlt && !alt {
		b.regionSet = false
	}
	b.wasAlt = alt
	// Keep the shell's scroll region matched to its (already-shrunk) PTY size, so
	// the reserved row survives and full-screen apps render right. Re-asserted on
	// resize and on alt-screen exit; also re-asserted periodically (off the alt
	// screen) to recover from any other clobber (reset/tput) that widened the
	// margins without our knowing. Must track the PTY even while an app is up, or a
	// resize mid-app would desync region and size — so the initial set still fires
	// under alt, but the periodic recovery does not (it would fight the app's own
	// margins).
	if !b.regionSet || (!alt && b.ticks%4 == 0) {
		b.sess.WriteOut([]byte(fmt.Sprintf("\x1b7\x1b[1;%dr\x1b8", b.shellRows)))
		b.regionSet = true
		b.last = ""
	}
	// Don't paint over a full-screen app: it owns the alt screen (and clears the
	// reserved row itself), and painting there would fight its cursor. The bar
	// reappears at the next prompt.
	if alt {
		return
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

// paint writes the bar across the full width of the physical bottom row
// (shellRows+1) without moving the shell's cursor. The content is padded with
// spaces to fill every column, all under reverse video, so the bar reads as one
// continuous strip. Autowrap is disabled around the paint (ESC[?7l / ESC[?7h) so
// filling the final column can't trigger a wrap/scroll and any cell-width miscount
// clips at the right edge instead of spilling onto the shell's rows; the extra
// safety space guarantees the row fills even on a one-cell undercount. DECSC/DECRC
// keep the shell's cursor put.
func (b *StatusBar) paint(line string) {
	line = truncateCells(line, b.cols)
	pad := b.cols - displayCells(line)
	if pad < 0 {
		pad = 0
	}
	b.sess.WriteOut([]byte(fmt.Sprintf(
		"\x1b7\x1b[?7l\x1b[%d;1H\x1b[7m%s%s\x1b[0m\x1b[?7h\x1b8",
		b.shellRows+1, line, strings.Repeat(" ", pad+1))))
}

// Close resets the scroll region and clears the reserved row so the terminal is
// left clean on exit.
func (b *StatusBar) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.shellRows == 0 {
		return
	}
	b.sess.WriteOut([]byte(fmt.Sprintf("\x1b7\x1b[r\x1b[%d;1H\x1b[K\x1b8", b.shellRows+1)))
	b.sess.SetReserveRow(false)
	b.regionSet = false
}

// truncateCells trims s to at most n display columns, appending an ellipsis when
// it has to cut. Widths come from displayCells so the bar's wide glyphs are
// accounted for. Under-fill is padded by paint; over-fill is clipped by paint's
// disabled autowrap — so this only needs to be close.
func truncateCells(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if displayCells(s) <= n {
		return s
	}
	// Must cut; reserve one column for the ellipsis.
	limit := n - 1
	if limit < 0 {
		limit = 0
	}
	w := 0
	var out []rune
	for _, r := range s {
		rw := runeCells(r)
		if w+rw > limit {
			break
		}
		out = append(out, r)
		w += rw
	}
	return string(out) + "…"
}

// runeCells approximates the terminal columns a rune occupies. The bar uses a
// small fixed set of wide glyphs (the ⧉ session marker and the ⚠ drift warning);
// everything else is treated as one cell. paint disables autowrap, so an
// occasional miscount clips rather than wraps.
func runeCells(r rune) int {
	switch r {
	case '⧉', '⚠':
		return 2
	default:
		return 1
	}
}

// displayCells approximates the number of terminal columns s occupies.
func displayCells(s string) int {
	n := 0
	for _, r := range s {
		n += runeCells(r)
	}
	return n
}
