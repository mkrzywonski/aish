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
// terminal does. Without this, every fmt.Println/Printf/Print call in this
// package renders as text that keeps starting from wherever the previous
// line happened to end, compounding into a staircase across the whole
// console session (confirmed live: aicmd's own console commands, which
// print several short lines in a row, drifted badly; the earlier
// single-line startup messages only looked fine because each one happened
// to be long enough to hit the terminal's own auto-wrap, which resets the
// column as a side effect).
//
// Shell output mirrored from the real cmd.exe/PowerShell process
// (shell.go's io.TeeReader) deliberately bypasses this: that text already
// arrives as proper \r\n from the real shell, so translating it again would
// double the \r.
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
