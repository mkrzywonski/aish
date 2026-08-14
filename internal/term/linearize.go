package term

import (
	"bytes"
	"os"
)

// Linearization: reconstructing logical line structure from a byte stream that
// moves the cursor instead of emitting newlines.
//
// Windows ConPTY — which is what a cmd.exe or PowerShell session looks like
// when reached over ssh — does not end a line with LF. It repositions the
// cursor absolutely (CUP, "ESC [ row ; col H") and carries on. StripEscapes
// deletes those sequences with no substitution, so two unrelated logical lines
// fuse into one, and downstream code that looks for a newline (framing's
// trailing-prompt trim, afterEcho) then makes a second wrong decision on top of
// the first. The result reaching the model is not garbled but silently
// TRUNCATED, which is the worst failure mode for an automated consumer.
//
// The fix is to translate vertical cursor movement back into newlines. The
// apparent obstacle is that a ring read can begin at an arbitrary offset with
// no known cursor row — but CUP is ABSOLUTE, which makes the linearizer
// self-synchronizing: the first absolute row-setting sequence in the window
// ESTABLISHES exact state, and every decision after it is exact. Relative moves
// (CUU/CUD/IND/RI) never need absolute state at all, since the delta is the
// whole answer. So an arbitrary starting offset, a ring wrap, or a client
// polling in small chunks costs AT MOST ONE heuristic decision per call, and
// that heuristic errs toward inserting a break rather than fusing.
//
// State therefore lives for the duration of one call. Nothing persists between
// calls, nothing is stored on Terminal, and no lock is involved.
//
// Guarantees, all covered by tests in linearize_test.go:
//
//   - Content preservation: removing every '\n' and '\r' from Linearize's
//     output yields exactly the same bytes as removing them from
//     StripEscapes' output. Linearization can only ADD line structure; it can
//     never drop content.
//   - No-op on ordinary streams: on input containing no vertical-movement
//     construct, Linearize is byte-identical to StripEscapes. Ordinary Linux
//     shell output — SGR colour, bare-CR progress bars, ESC[K, OSC 7/133 — is
//     provably untouched.
//   - Bounded growth: output stays under 2x input regardless of adversarial
//     escape alternation.

// defaultRowClamp bounds how many newlines a single jump may produce when the
// caller does not know the screen height.
const defaultRowClamp = 64

// linearizeDisabled is an undocumented escape hatch (AISH_NO_LINEARIZE=1) so a
// live session can fall back to plain stripping without a rebuild.
var linearizeDisabled = os.Getenv("AISH_NO_LINEARIZE") != ""

// Linearize strips ANSI escapes like StripEscapes, but translates VERTICAL
// cursor movement into newlines, so terminals that position absolutely instead
// of emitting newlines do not fuse unrelated lines together.
//
// rows is the screen height and only bounds how many newlines one jump may
// produce; rows <= 0 uses defaultRowClamp.
func Linearize(b []byte, rows int) []byte {
	if linearizeDisabled {
		return StripEscapes(b)
	}
	l := newLinearizer(len(b), rows)
	l.walk(b, nil)
	return l.out
}

// FirstBreak returns the index just past the first construct in b that ends a
// logical line: an LF, or an escape sequence that moves the cursor to a
// different row. ok is false when b contains no such construct.
//
// This is what framing's afterEcho needs. Scanning for a bare '\n' is wrong on
// a ConPTY stream, where the echoed command line is frequently terminated by a
// CUP rather than a newline — so a newline-only scan skips forward into the
// middle of the real output and silently eats its first lines.
//
// A break only counts once something has been written on the current line:
// leading cursor movement with nothing before it separates nothing.
func FirstBreak(b []byte) (idx int, ok bool) {
	// No capacity hint: walk stops at the first break, so the accumulated output
	// is only ever the leading fragment. afterEcho hands us up to a megabyte and
	// wants an index, not a copy of it.
	l := newLinearizer(0, 0)
	l.budget = 1 << 30 // never let the growth budget suppress a break here
	found := -1
	l.walk(b, func(end int) bool {
		found = end
		return true // stop
	})
	if found < 0 {
		return 0, false
	}
	return found, true
}

type linearizer struct {
	out      []byte
	row      int // 1-based current row; 0 means "not yet known"
	savedRow int // DECSC / SCOSC
	clamp    int // max newlines a single jump may emit
	budget   int // remaining newlines beyond one-per-move (growth guard)
	broke    bool
}

// newLinearizer prepares state for one call. size is the input length, used
// only to size the output buffer and the growth budget; pass 0 when the caller
// wants state tracking without accumulating output.
func newLinearizer(size, rows int) *linearizer {
	clamp := rows
	if clamp <= 0 {
		clamp = defaultRowClamp
	}
	var out []byte
	if size > 0 {
		out = make([]byte, 0, size+size/8+8)
	}
	return &linearizer{
		out:    out,
		clamp:  clamp,
		budget: size/4 + 8,
	}
}

// walk tokenizes b with ansiRe and drives the state machine. When onBreak is
// non-nil it is called with the index just past each construct that ends a
// logical line; returning true stops the walk (FirstBreak's early exit).
func (l *linearizer) walk(b []byte, onBreak func(end int) bool) {
	last := 0
	for _, m := range ansiRe.FindAllIndex(b, -1) {
		if l.text(b[last:m[0]], last, onBreak) {
			return
		}
		wasDirty := l.dirty()
		l.broke = false
		l.escape(b[m[0]:m[1]])
		if onBreak != nil && l.broke && wasDirty && onBreak(m[1]) {
			return
		}
		last = m[1]
	}
	l.text(b[last:], last, onBreak)
}

// text copies literal bytes through, dropping BEL (as StripEscapes does) and
// tracking rows across line feeds. base is the offset of p within the original
// buffer, so onBreak can report absolute indices.
func (l *linearizer) text(p []byte, base int, onBreak func(end int) bool) (stop bool) {
	for i, c := range p {
		switch c {
		case 0x07: // BEL: dropped, matching StripEscapes
			continue
		case '\n', '\v', '\f':
			if l.row > 0 {
				l.row++
			}
			l.out = append(l.out, c)
			if c == '\n' && onBreak != nil && onBreak(base+i+1) {
				return true
			}
		default:
			l.out = append(l.out, c)
		}
	}
	return false
}

// dirty reports whether the output currently ends mid-line, i.e. whether a
// break here would actually separate two pieces of content.
func (l *linearizer) dirty() bool {
	return len(l.out) > 0 && l.out[len(l.out)-1] != '\n'
}

// nl emits n newlines, clamped to the screen height and drawing anything beyond
// the first from the growth budget. One newline per vertical move is always
// free, and moves are bounded by the number of escape sequences (each at least
// three bytes), so total output stays under 2x input even for pathological
// input like alternating "ESC[1;1H ESC[200;1H".
func (l *linearizer) nl(n int) {
	if n < 1 {
		n = 1
	}
	if n > l.clamp {
		n = l.clamp
	}
	if n > 1 {
		extra := n - 1
		if extra > l.budget {
			extra = l.budget
		}
		l.budget -= extra
		n = 1 + extra
	}
	for i := 0; i < n; i++ {
		l.out = append(l.out, '\n')
	}
	l.broke = true
}

// vertical handles an ABSOLUTE row move. col is the target column, or -1 when
// the sequence leaves the column alone (VPA).
func (l *linearizer) vertical(target, col int) {
	if target < 1 {
		target = 1
	}
	if l.row == 0 {
		// Pre-sync. The ONLY heuristic in the design, reachable at most once
		// per call: we don't know the current row, so we can't compute a delta.
		// Establish state from this absolute move, and break only if we're
		// mid-line and the cursor is going to column 1 — which is what "start a
		// new line" looks like. Biased toward inserting a break, never fusing.
		l.row = target
		if col == 1 && l.dirty() {
			l.nl(1)
		}
		return
	}
	d := target - l.row
	l.row = target
	switch {
	case d > 0:
		l.nl(d)
	case d == 0 && col == 1 && l.dirty():
		// In-row reposition: the faithful answer is a carriage return, not a
		// line break. This is the case a stateless "every CUP is a newline"
		// rule gets wrong, and it is the most common use of CUP in the wild
		// (PSReadLine redrawing its edit line, ConPTY repainting a row).
		l.out = append(l.out, '\r')
	}
	// Upward movement never inserts anything.
}

func (l *linearizer) down(n int) {
	if n < 1 {
		n = 1
	}
	if l.row > 0 {
		l.row += n
	}
	l.nl(n)
}

func (l *linearizer) up(n int) {
	if n < 1 {
		n = 1
	}
	if l.row > 0 {
		l.row -= n
		if l.row < 1 {
			l.row = 1
		}
	}
}

// escape applies one escape sequence's vertical effect. seq is exactly one
// match of ansiRe.
func (l *linearizer) escape(seq []byte) {
	if len(seq) < 2 {
		return
	}
	if seq[1] == '[' {
		// A bare "ESC [" is a truncated CSI — the extended ansiRe consumes it
		// via the ECMA-48 catch-all so its '[' doesn't leak as text. There is no
		// parameter string and no vertical effect; just strip it.
		if len(seq) >= 3 {
			l.csi(seq)
		}
		return
	}
	// ECMA-48 two-byte forms. Sequences carrying intermediates (charset
	// designation and friends) have no vertical effect.
	if len(seq) != 2 {
		return
	}
	switch seq[1] {
	case 'D': // IND — index, down one
		l.down(1)
	case 'E': // NEL — next line
		l.down(1)
	case 'M': // RI — reverse index, up one
		l.up(1)
	case '7': // DECSC
		l.savedRow = l.row
	case '8': // DECRC
		l.row = l.savedRow
	case 'c': // RIS — full reset, row unknowable
		l.row = 0
	}
}

func (l *linearizer) csi(seq []byte) {
	body := seq[2 : len(seq)-1]
	final := seq[len(seq)-1]

	// Private/experimental parameter strings (?, <, =, >). None move the cursor
	// vertically, but the alternate-screen switches make the row meaningless,
	// so desync rather than compute a bogus delta across the transition.
	if len(body) > 0 && (body[0] == '?' || body[0] == '<' || body[0] == '=' || body[0] == '>') {
		if final == 'h' || final == 'l' {
			switch string(body) {
			case "?1049", "?1047", "?47":
				l.row = 0
			}
		}
		return
	}

	ps := csiParams(body)
	switch final {
	case 'H', 'f': // CUP / HVP — absolute row and column
		l.vertical(csiParam(ps, 0, 1), csiParam(ps, 1, 1))
	case 'd': // VPA — absolute row, column untouched
		l.vertical(csiParam(ps, 0, 1), -1)
	case 'A': // CUU
		l.up(csiParam(ps, 0, 1))
	case 'F': // CPL — up n, column 1
		l.up(csiParam(ps, 0, 1))
	case 'B': // CUD
		l.down(csiParam(ps, 0, 1))
	case 'e': // VPR — down n
		l.down(csiParam(ps, 0, 1))
	case 'E': // CNL — down n, column 1
		l.down(csiParam(ps, 0, 1))
	case 'r': // DECSTBM — setting the scroll region homes to its top margin
		l.row = csiParam(ps, 0, 1)
	case 's': // SCOSC
		l.savedRow = l.row
	case 'u': // SCORC
		l.row = l.savedRow
	}
	// Everything else — SGR (m), EL (K), ED (J), CUF/CUB (C/D), CHA (G), ICH,
	// DCH, cursor visibility — has no vertical effect and inserts nothing.
}

// csiParams splits a CSI parameter string into numbers. Trailing intermediate
// bytes are dropped, and any parameter that is not purely numeric (an empty
// slot, or an oddity like "!") yields 0 so the caller's default applies.
func csiParams(body []byte) []int {
	end := len(body)
	for end > 0 && body[end-1] >= 0x20 && body[end-1] <= 0x2f {
		end--
	}
	body = body[:end]
	if len(body) == 0 {
		return nil
	}
	parts := bytes.Split(body, []byte{';'})
	out := make([]int, len(parts))
	for i, p := range parts {
		n, okDigits := 0, len(p) > 0
		for _, c := range p {
			if c < '0' || c > '9' {
				okDigits = false
				break
			}
			if n < 1<<20 { // saturate; absurd values are clamped later anyway
				n = n*10 + int(c-'0')
			}
		}
		if okDigits {
			out[i] = n
		}
	}
	return out
}

// csiParam returns parameter i, substituting def for a missing or zero value
// (ECMA-48 treats an omitted or 0 parameter as the default).
func csiParam(ps []int, i, def int) int {
	if i >= len(ps) || ps[i] == 0 {
		return def
	}
	return ps[i]
}
