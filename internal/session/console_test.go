package session

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

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

func TestPromptRingsOnceAndPublishesAttention(t *testing.T) {
	var out bytes.Buffer
	s := &Session{Stdout: &out}
	states := make(chan bool, 2)
	s.SetPromptChanged(func() { states <- s.PromptActive() })

	type promptResult struct {
		choice byte
		ok     bool
	}
	result := make(chan promptResult, 1)
	go func() {
		choice, ok := s.Prompt("allow access?", "yn", time.Second)
		result <- promptResult{choice: choice, ok: ok}
	}()

	if active := <-states; !active {
		t.Fatal("prompt start callback reported inactive")
	}
	if !s.PromptActive() {
		t.Fatal("prompt was not active while waiting for input")
	}
	s.deliverCaptured([]byte{'y'})
	got := <-result
	if !got.ok || got.choice != 'y' {
		t.Fatalf("prompt result = %+v", got)
	}
	if active := <-states; active {
		t.Fatal("prompt end callback reported active")
	}
	if s.PromptActive() {
		t.Fatal("prompt remained active after answer")
	}
	if count := strings.Count(out.String(), "\a"); count != 1 {
		t.Fatalf("prompt emitted %d bells, want 1: %q", count, out.String())
	}
}

func TestPromptLineRingsAndClearsOnTimeout(t *testing.T) {
	var out bytes.Buffer
	s := &Session{Stdout: &out}
	states := make(chan bool, 2)
	s.SetPromptChanged(func() { states <- s.PromptActive() })
	done := make(chan bool, 1)
	go func() {
		_, ok := s.PromptLine("new name:", 20*time.Millisecond)
		done <- ok
	}()

	if active := <-states; !active {
		t.Fatal("line prompt start callback reported inactive")
	}
	if ok := <-done; ok {
		t.Fatal("timed-out line prompt succeeded")
	}
	if active := <-states; active {
		t.Fatal("line prompt end callback reported active")
	}
	if s.PromptActive() {
		t.Fatal("line prompt remained active after timeout")
	}
	if count := strings.Count(out.String(), "\a"); count != 1 {
		t.Fatalf("line prompt emitted %d bells, want 1: %q", count, out.String())
	}
}
