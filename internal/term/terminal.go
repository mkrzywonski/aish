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

// ansiRe matches CSI sequences, OSC sequences (BEL- or ST-terminated), and
// other two-byte ESC sequences, for stripping escape soup out of raw
// scrollback before handing it to an AI.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?<=>!]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)?|\x1b[PX^_][^\x1b]*\x1b\\|\x1b[@-Z\\-_]`)

// StripEscapes removes ANSI escape sequences from b, plus stray BEL bytes
// (a read window can start mid-OSC, orphaning its terminator). Carriage
// returns are kept; callers see the byte stream otherwise unmodified.
func StripEscapes(b []byte) []byte {
	b = ansiRe.ReplaceAll(b, nil)
	return bytes.ReplaceAll(b, []byte{0x07}, nil)
}
