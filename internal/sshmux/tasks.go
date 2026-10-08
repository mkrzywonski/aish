package sshmux

import (
	"fmt"
	"io"
	"os/exec"
	"sync"

	"ai-ssh/internal/term"
)

// Task is one background command. A local task runs as a child of aish with
// its combined output buffered in a ring so callers can poll incrementally.
// A remote task (Remote != nil) runs detached on the remote host and is
// polled through the persistent channel (see chantask.go); Out is nil.
type Task struct {
	ID     string
	Out    *term.Ring
	Remote *ChannelTask
	mu     sync.Mutex
	exit   *int
	done   bool
}

func (t *Task) Status() (running bool, exit *int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.done, t.exit
}

const taskBufSize = 2 << 20 // 2 MiB per task

// Table tracks background tasks for a session.
type Table struct {
	mu      sync.Mutex
	m       map[string]*Task
	next    int
	remotes int
}

func NewTable() *Table { return &Table{m: map[string]*Task{}} }

// NewChannelTask registers a remote task bound to ci and allocates its
// identity (task ID and remote directory) BEFORE anything is sent, so a
// launch whose acknowledgment is lost can still be reported and polled.
func (tb *Table) NewChannelTask(ci *ConnInfo, sessionID string) (*Task, error) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if tb.remotes >= MaxChannelTasks {
		return nil, fmt.Errorf("this session already has %d remote background tasks, the limit (their output is kept on the remote until the session ends); run further work in the foreground", MaxChannelTasks)
	}
	id := fmt.Sprintf("task-%d", tb.next+1)
	dir, err := newChannelTaskDir(sessionID, id)
	if err != nil {
		return nil, err
	}
	tb.next++
	tb.remotes++
	t := &Task{ID: id, Remote: &ChannelTask{CI: *ci, Dir: dir}}
	tb.m[id] = t
	return t, nil
}

// Remove forgets a task whose launch definitely failed before anything ran.
func (tb *Table) Remove(id string) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if t, ok := tb.m[id]; ok {
		if t.Remote != nil {
			tb.remotes--
		}
		delete(tb.m, id)
	}
}

// Start launches cmd with combined output captured; returns the task id.
func (tb *Table) Start(cmd *exec.Cmd) (*Task, error) {
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
	var out io.Writer = t.Out
	cmd.Stdout = out
	cmd.Stderr = out
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
