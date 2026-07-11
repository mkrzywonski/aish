package session

import "testing"

// feed returns a capture channel preloaded with bs, as the input pump delivers
// an escape sequence (one back-to-back burst).
func feed(bs ...byte) chan byte {
	ch := make(chan byte, 256)
	for _, b := range bs {
		ch <- b
	}
	return ch
}

func TestIsLoneEscape(t *testing.T) {
	// Nothing follows the ESC → a real Escape keypress (prompt should cancel).
	if !isLoneEscape(feed()) {
		t.Error("a lone ESC should be treated as a cancel")
	}

	// ESC-prefixed sequences (the bytes AFTER the ESC) must NOT cancel, and must
	// be fully drained so they aren't read as prompt input. These are exactly the
	// bytes a mouse move / focus change / arrow key emits.
	cases := []struct {
		name string
		seq  []byte
	}{
		{"focus-in", []byte{'[', 'I'}},
		{"focus-out", []byte{'[', 'O'}},
		{"arrow-up", []byte{'[', 'A'}},
		{"csi-params", []byte("[1;2R")},
		{"sgr-mouse", []byte("[<0;10;10M")},
		{"sgr-mouse-release", []byte("[<0;10;10m")},
		{"x10-mouse", []byte{'[', 'M', 0x20, 0x30, 0x30}},
		{"ss3-f1", []byte{'O', 'P'}},
	}
	for _, tc := range cases {
		ch := feed(tc.seq...)
		if isLoneEscape(ch) {
			t.Errorf("%s: an escape sequence must not be a cancel", tc.name)
		}
		if n := len(ch); n != 0 {
			t.Errorf("%s: sequence not fully drained, %d byte(s) left", tc.name, n)
		}
	}
}
