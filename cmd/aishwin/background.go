package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// backgroundTask tracks one detached command spawned by exec{background:
// true}. Unlike the persistent foreground shell, each background command
// gets its own one-shot process (`cmd.exe /c <command>` or
// `powershell.exe -Command <command>`) — Go's exec.Cmd reports the real
// exit code directly via ProcessState, so none of shell.go's marker-parsing
// machinery applies here, and a slow/hung background command can't corrupt
// a later foreground command's capture the way sharing the persistent shell
// would.
type backgroundTask struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	running  bool
	exitCode *int
}

type backgroundTasks struct {
	mu    sync.Mutex
	tasks map[string]*backgroundTask
}

func newBackgroundTasks() *backgroundTasks {
	return &backgroundTasks{tasks: map[string]*backgroundTask{}}
}

// Start spawns command under kind's one-shot invocation form and returns a
// task id immediately; output and completion are retrieved via Poll. Each
// call gets a fresh process, so — unlike the persistent foreground shell —
// it always picks up the console menu's current custom env vars
// (access.environ), no live-push needed.
func (b *backgroundTasks) Start(kind shellKind, command, cwd string) (string, error) {
	var cmd *exec.Cmd
	switch kind {
	case shellPowerShell:
		cmd = exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-Command", command)
	default:
		cmd = exec.Command("cmd.exe", "/c", command)
	}
	cmd.Env = access.environ(os.Environ())
	if cwd != "" {
		cmd.Dir = cwd
	}

	task := &backgroundTask{running: true}
	sink := io.MultiWriter(guiLog, &taskWriter{task: task})
	cmd.Stdout = sink
	cmd.Stderr = sink

	if err := cmd.Start(); err != nil {
		return "", err
	}

	id := randHex(8)
	b.mu.Lock()
	b.tasks[id] = task
	b.mu.Unlock()

	go func() {
		_ = cmd.Wait()
		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		task.mu.Lock()
		task.running = false
		task.exitCode = &code
		task.mu.Unlock()
	}()

	return id, nil
}

// taskWriter appends written bytes to its task's captured-output buffer.
type taskWriter struct{ task *backgroundTask }

func (w *taskWriter) Write(p []byte) (int, error) {
	w.task.mu.Lock()
	w.task.buf.Write(p)
	w.task.mu.Unlock()
	return len(p), nil
}

// Poll returns output captured since cursor, whether the task is still
// running, and its exit code once finished.
func (b *backgroundTasks) Poll(id string, cursor int64) (running bool, output string, nextCursor int64, exitCode *int, err error) {
	b.mu.Lock()
	task := b.tasks[id]
	b.mu.Unlock()
	if task == nil {
		return false, "", cursor, nil, fmt.Errorf("no such task %q", id)
	}
	task.mu.Lock()
	defer task.mu.Unlock()
	data := task.buf.Bytes()
	if cursor < 0 || cursor > int64(len(data)) {
		cursor = 0
	}
	return task.running, string(data[cursor:]), int64(len(data)), task.exitCode, nil
}
