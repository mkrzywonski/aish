package main

import (
	"bytes"
	"io"
	"os"
)

// stdout and stderr wrap the real streams to translate every line feed into
// "\r\n" before writing. Go's standard library does no such translation on
// Windows -- os.Stdout.Write is a raw syscall write, not the CRT's
// text-mode file translation -- and the native Windows console does not
// treat a bare '\n' as "return to column 0, then move down" the way a Unix
// terminal does. Without this, multi-line output rendered as a staircase,
// each line starting from wherever the previous one happened to end
// (confirmed live, back when this process's primary UI was a text console;
// that console is gone now -- see gui.go's AppendLog/log view -- but these
// writers remain the fallback for the few paths with no window yet to log
// into: --version's output, a StartGUI failure, and early GUI-init
// diagnostics).
var (
	stdout io.Writer = &crlfWriter{w: os.Stdout}
	stderr io.Writer = &crlfWriter{w: os.Stderr}
)

type crlfWriter struct {
	w io.Writer
}

func (c *crlfWriter) Write(p []byte) (int, error) {
	// Normalize any existing \r\n to \n first, then expand every \n to
	// \r\n, so the result is consistently \r\n regardless of what mix the
	// caller passed in.
	translated := bytes.ReplaceAll(p, []byte("\r\n"), []byte("\n"))
	translated = bytes.ReplaceAll(translated, []byte("\n"), []byte("\r\n"))
	if _, err := c.w.Write(translated); err != nil {
		return 0, err
	}
	// Report the length of p, not the translated length: io.Writer's
	// contract is that a full, successful write returns len(p).
	return len(p), nil
}
