package main

import (
	"bytes"
	"testing"
)

// TestCRLFWriter locks in the fix for a real bug seen live: multi-line
// console output (the menu's help text) rendered as a cascading staircase
// on a real Windows console because Go's raw \n writes don't reset the
// cursor to column 0 there. Every line must come out ending in "\r\n", and
// a caller that already wrote "\r\n" itself must not end up with "\r\r\n".
func TestCRLFWriter(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"bare LF", "a\nb\nc\n", "a\r\nb\r\nc\r\n"},
		{"already CRLF", "a\r\nb\r\n", "a\r\nb\r\n"},
		{"mixed", "a\r\nb\nc\r\n", "a\r\nb\r\nc\r\n"},
		{"no trailing newline", "a\nb", "a\r\nb"},
		{"no newlines at all", "hello", "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := &crlfWriter{w: &buf}
			n, err := w.Write([]byte(tt.input))
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			if n != len(tt.input) {
				t.Errorf("n = %d, want %d (io.Writer contract: full write reports len(p))", n, len(tt.input))
			}
			if buf.String() != tt.want {
				t.Errorf("output = %q, want %q", buf.String(), tt.want)
			}
		})
	}
}
