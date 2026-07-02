package term

import (
	"io"
	"sync"
	"sync/atomic"
)

// TitleMarker sits on the user-visible output path and rewrites terminal
// window-title sequences (OSC 0/1/2) to carry a "shared with AI" marker, so
// every aish session is identifiable in the tab/title bar — including titles
// set by remote hosts over ssh. This is the one sanctioned exception to
// byte-transparency: only title payloads are altered, never screen content.
//
// It also remembers the last title seen so the marker can be re-asserted
// immediately when an MCP client connects or disconnects (Refresh), rather
// than waiting for the next prompt to redraw the title.
type TitleMarker struct {
	mu        sync.Mutex
	out       io.Writer
	connected atomic.Bool

	// stream state machine
	state     int
	hold      []byte // bytes withheld while a potential title prefix is ambiguous
	titleBuf  []byte
	lastTitle []byte
	pending   bool // refresh requested while mid-sequence
}

const (
	tmGround     = iota
	tmEsc        // saw ESC
	tmOsc        // saw ESC ]
	tmOscDigit   // saw ESC ] [012]
	tmInTitle    // inside a title payload (marker already injected)
	tmInTitleEsc // inside title, saw ESC (possible ST)
)

const markerShared = "⧉ "
const markerActive = "⧉⚡ "

func NewTitleMarker(out io.Writer) *TitleMarker {
	return &TitleMarker{out: out, state: tmGround}
}

func (t *TitleMarker) marker() string {
	if t.connected.Load() {
		return markerActive
	}
	return markerShared
}

// SetConnected updates the AI-attached state and re-asserts the title.
func (t *TitleMarker) SetConnected(yes bool) {
	t.connected.Store(yes)
	t.Refresh()
}

// Refresh re-emits the last known title with the current marker. If the
// stream is mid-escape-sequence, the refresh is deferred until it returns
// to ground state so we never corrupt a sequence in flight.
func (t *TitleMarker) Refresh() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != tmGround {
		t.pending = true
		return
	}
	t.emitTitleLocked()
}

func (t *TitleMarker) emitTitleLocked() {
	title := t.lastTitle
	if len(title) == 0 {
		title = []byte("aish")
	}
	seq := append([]byte("\x1b]0;"+t.marker()), title...)
	seq = append(seq, 0x07)
	t.out.Write(seq)
	t.pending = false
}

// Write transforms p and forwards it. Bytes forming an ambiguous title
// prefix at a chunk boundary are withheld until resolved by the next chunk.
func (t *TitleMarker) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var outBuf []byte
	flushHold := func(extra ...byte) {
		outBuf = append(outBuf, t.hold...)
		t.hold = t.hold[:0]
		outBuf = append(outBuf, extra...)
	}

	for _, b := range p {
		switch t.state {
		case tmGround:
			if b == 0x1b {
				t.state = tmEsc
				t.hold = append(t.hold, b)
			} else {
				outBuf = append(outBuf, b)
			}
		case tmEsc:
			if b == ']' {
				t.state = tmOsc
				t.hold = append(t.hold, b)
			} else if b == 0x1b {
				// ESC ESC: flush the first, stay in esc on the second.
				outBuf = append(outBuf, 0x1b)
				t.hold[0] = 0x1b
			} else {
				t.state = tmGround
				flushHold(b)
			}
		case tmOsc:
			if b == '0' || b == '1' || b == '2' {
				t.state = tmOscDigit
				t.hold = append(t.hold, b)
			} else {
				t.state = tmGround
				flushHold(b)
			}
		case tmOscDigit:
			if b == ';' {
				// Title sequence confirmed: emit prefix + marker, capture payload.
				flushHold(b)
				outBuf = append(outBuf, []byte(t.marker())...)
				t.titleBuf = t.titleBuf[:0]
				t.state = tmInTitle
			} else {
				// e.g. OSC 133/12/10: not a title, pass through untouched.
				t.state = tmGround
				flushHold(b)
			}
		case tmInTitle:
			if b == 0x07 {
				t.finishTitle()
				outBuf = append(outBuf, b)
			} else if b == 0x1b {
				t.state = tmInTitleEsc
			} else {
				t.titleBuf = append(t.titleBuf, b)
				outBuf = append(outBuf, b)
			}
		case tmInTitleEsc:
			if b == '\\' {
				t.finishTitle()
				outBuf = append(outBuf, 0x1b, '\\')
			} else {
				// Malformed; forward what we swallowed and keep going.
				t.state = tmInTitle
				t.titleBuf = append(t.titleBuf, 0x1b, b)
				outBuf = append(outBuf, 0x1b, b)
			}
		}
	}

	if len(outBuf) > 0 {
		if _, err := t.out.Write(outBuf); err != nil {
			return len(p), err
		}
	}
	if t.pending && t.state == tmGround {
		t.emitTitleLocked()
	}
	return len(p), nil
}

func (t *TitleMarker) finishTitle() {
	t.lastTitle = append(t.lastTitle[:0], t.titleBuf...)
	t.state = tmGround
}
