package sshmux

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"ai-ssh/internal/term"
)

// Task is one background command (local or remote over the mux), with its
// combined output buffered in a ring so callers can poll incrementally.
type Task struct {
	ID   string
	Out  *term.Ring
	mu   sync.Mutex
	exit *int
	done bool
}

func (t *Task) Status() (running bool, exit *int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.done, t.exit
}

const taskBufSize = 2 << 20 // 2 MiB per task

// Table tracks background tasks for a session.
type Table struct {
	mu   sync.Mutex
	m    map[string]*Task
	next int
}

func NewTable() *Table { return &Table{m: map[string]*Task{}} }

// Start launches cmd with combined output captured; returns the task id.
func (tb *Table) Start(cmd *exec.Cmd) (*Task, error) {
	return tb.start(cmd, nil, nil)
}

// StartAfterMarker launches cmd and calls ready once marker has appeared in its
// output, proving the remote shell started after any SSH authentication. The
// marker is removed from captured task output. If startup fails before the
// marker, ready is still called when the process exits.
//
// The marker is written by a printf small enough to reach the pipe in one
// write, so it arrives contiguous even though stderr shares the stream.
func (tb *Table) StartAfterMarker(cmd *exec.Cmd, marker []byte, ready func()) (*Task, error) {
	return tb.start(cmd, marker, ready)
}

func (tb *Table) start(cmd *exec.Cmd, marker []byte, ready func()) (*Task, error) {
	tb.mu.Lock()
	tb.next++
	t := &Task{ID: fmt.Sprintf("task-%d", tb.next), Out: term.NewRing(taskBufSize)}
	tb.m[t.ID] = t
	tb.mu.Unlock()

	// Stdout and Stderr must be the SAME writer value. os/exec reuses one pipe
	// and one copying goroutine when they compare equal, and opens two of each
	// when they don't — so handing stderr a different writer would merge the two
	// streams at chunk boundaries in whatever order the goroutines happened to
	// run, instead of in the order the remote actually wrote them.
	var marked *markerWriter
	out := io.Writer(t.Out)
	if len(marker) > 0 {
		marked = &markerWriter{dst: t.Out, marker: append([]byte(nil), marker...), ready: ready}
		out = marked
	}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		if ready != nil {
			ready()
		}
		tb.mu.Lock()
		delete(tb.m, t.ID)
		tb.mu.Unlock()
		return nil, err
	}
	go func() {
		err := cmd.Wait()
		if marked != nil {
			marked.Close()
		}
		code := 0
		if err != nil {
			code = 1
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
		}
		t.mu.Lock()
		t.exit = &code
		t.done = true
		t.mu.Unlock()
	}()
	return t, nil
}

// BackgroundCommand adds a random startup acknowledgment before a POSIX
// background command. The marker is emitted only after the remote session has
// passed authentication and started its command shell.
func BackgroundCommand(command string) (wrapped string, marker []byte, err error) {
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", nil, err
	}
	token := "@AISHSTART@" + hex.EncodeToString(nonce) + "@"
	marker = []byte(token + "\n")
	wrapped = "printf '%s\\n' " + Quote(token) + "; " + command
	return wrapped, marker, nil
}

// markerWriter removes one startup marker while preserving arbitrary chunking
// around it. It retains only a marker-sized suffix between writes.
type markerWriter struct {
	mu     sync.Mutex
	dst    io.Writer
	marker []byte
	buf    []byte
	found  bool
	closed bool
	ready  func()
	once   sync.Once
}

func (w *markerWriter) signalReady() {
	if w.ready != nil {
		w.once.Do(w.ready)
	}
}

func (w *markerWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	n := len(p)
	if w.found {
		_, err := w.dst.Write(p)
		w.mu.Unlock()
		return n, err
	}
	w.buf = append(w.buf, p...)
	if i := bytes.Index(w.buf, w.marker); i >= 0 {
		before := append([]byte(nil), w.buf[:i]...)
		after := append([]byte(nil), w.buf[i+len(w.marker):]...)
		w.buf = nil
		w.found = true
		if len(before) > 0 {
			_, _ = w.dst.Write(before)
		}
		if len(after) > 0 {
			_, _ = w.dst.Write(after)
		}
		w.mu.Unlock()
		w.signalReady()
		return n, nil
	}
	keep := len(w.marker) - 1
	if keep < 0 {
		keep = 0
	}
	if flush := len(w.buf) - keep; flush > 0 {
		_, _ = w.dst.Write(w.buf[:flush])
		w.buf = append(w.buf[:0], w.buf[flush:]...)
	}
	w.mu.Unlock()
	return n, nil
}

func (w *markerWriter) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	if len(w.buf) > 0 {
		_, _ = w.dst.Write(w.buf)
		w.buf = nil
	}
	w.mu.Unlock()
	w.signalReady()
}

func (tb *Table) Get(id string) *Task {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return tb.m[id]
}
