package main

// gui_writer.go: an io.Writer adapter for code (background.go) that writes
// raw, not-yet-line-split bytes -- unlike shell.go's mirrorLine, which
// already receives one complete line at a time from its own scanner loop.

import (
	"bytes"
	"sync"
)

type guiLogWriter struct {
	mu  sync.Mutex
	buf []byte
}

func (w *guiLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimRight(w.buf[:i], "\r"))
		AppendLog(line)
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

var guiLog = &guiLogWriter{}
