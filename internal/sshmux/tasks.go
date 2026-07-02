package sshmux

import (
	"fmt"
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
	tb.mu.Lock()
	tb.next++
	t := &Task{ID: fmt.Sprintf("task-%d", tb.next), Out: term.NewRing(taskBufSize)}
	tb.m[t.ID] = t
	tb.mu.Unlock()

	cmd.Stdout = t.Out
	cmd.Stderr = t.Out
	if err := cmd.Start(); err != nil {
		tb.mu.Lock()
		delete(tb.m, t.ID)
		tb.mu.Unlock()
		return nil, err
	}
	go func() {
		err := cmd.Wait()
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

func (tb *Table) Get(id string) *Task {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return tb.m[id]
}
