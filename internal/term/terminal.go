package term

import (
	"bytes"
	"regexp"
)

// Terminal is the single output tap for a session: every byte the PTY emits
// is fed to the scrollback ring and the screen emulator. (M3 adds the OSC
// 133/7 event parser here.)
type Terminal struct {
	Ring   *Ring
	Screen *Screen
	Parser *OSCParser
}

const DefaultRingSize = 4 << 20 // 4 MiB

func NewTerminal(rows, cols int) *Terminal {
	return &Terminal{
		Ring:   NewRing(DefaultRingSize),
		Screen: NewScreen(rows, cols),
		Parser: &OSCParser{},
	}
}

func (t *Terminal) Write(p []byte) (int, error) {
	base := t.Ring.End()
	t.Ring.Write(p)
	t.Screen.Write(p)
	t.Parser.Feed(base, p)
	return len(p), nil
}

// ansiRe matches CSI sequences, OSC sequences (BEL- or ST-terminated), string
// sequences (DCS/SOS/PM/APC), and any remaining ECMA-48 escape sequence, for
// stripping escape soup out of raw scrollback before handing it to an AI.
//
// The final alternative is the general ECMA-48 form — ESC, zero or more
// intermediate bytes (0x20-0x2F), one final byte (0x30-0x7E) — which covers
// ESC 7 / ESC 8 (DECSC/DECRC), ESC c (RIS), ESC = / ESC > (keypad modes) and
// charset designations like ESC ( B (emitted by every `tput sgr0`). Those all
// leaked through to the model before. It MUST stay last: its final-byte class
// includes '[' and ']', so at a given position Go's leftmost-first alternation
// has to try the CSI and OSC branches first. A truncated CSI (a read starting
// or ending mid-escape) is consequently consumed as a bare two-byte sequence
// rather than leaking its '[' as text.
var ansiRe = regexp.MustCompile(
	`\x1b\[[0-9;?<=>!]*[ -/]*[@-~]` + // CSI
		`|\x1b\][^\x07\x1b]*(\x07|\x1b\\)?` + // OSC
		`|\x1b[PX^_][^\x1b]*\x1b\\` + // DCS / SOS / PM / APC
		`|\x1b[ -/]*[0-~]`) // ECMA-48 catch-all — keep last

// StripEscapes removes ANSI escape sequences from b, plus stray BEL bytes
// (a read window can start mid-OSC, orphaning its terminator). Carriage
// returns are kept; callers see the byte stream otherwise unmodified.
func StripEscapes(b []byte) []byte {
	b = ansiRe.ReplaceAll(b, nil)
	return bytes.ReplaceAll(b, []byte{0x07}, nil)
}
