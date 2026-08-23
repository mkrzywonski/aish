package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
	pid      int // set once, before running is ever read; safe to read from Poll without the lock
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
	case shellBash:
		path, ok := bashPath()
		if !ok {
			return "", fmt.Errorf("bash is not available on this host")
		}
		cmd = exec.Command(path, "--noprofile", "--norc", "-c", command)
	default:
		cmd = exec.Command("cmd.exe", "/c", command)
	}
	cmd.Env = access.environ(os.Environ())
	if cwd != "" {
		cmd.Dir = cwd
	}

	// Background tasks can run concurrently with each other (unlike the
	// single serialized foreground shell), so live-streaming their output
	// line-by-line into the shared log view could interleave two commands'
	// output into one unreadable stream. Instead: announce the command in
	// red the moment it starts, capture its output silently (taskWriter,
	// already needed for exec_status polling), and log the command plus
	// its complete output as ONE atomic block in black once it finishes --
	// drainLogQueue (gui.go) processes queued entries one at a time on a
	// single thread, so a whole multi-line block queued as a single
	// AppendLog call can never have another goroutine's entry land in the
	// middle of it.
	AppendLogColor("Running "+command, colorRunning)

	task := &backgroundTask{running: true}
	cmd.Stdout = &taskWriter{task: task}
	cmd.Stderr = &taskWriter{task: task}

	if err := cmd.Start(); err != nil {
		return "", err
	}
	task.pid = cmd.Process.Pid // set once, before task is published below

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
		output := task.buf.String()
		task.mu.Unlock()

		// Always end with an explicit exit-code line: a command that
		// succeeds silently (a clean `go build`, say) would otherwise
		// render as just the command text with nothing after it,
		// indistinguishable at a glance from "still running" or "its
		// output isn't showing up" (found live -- a run of several
		// quiet go build/vet/test calls looked incomplete/broken even
		// though every one had actually finished).
		block := command
		if trimmed := strings.TrimRight(output, "\r\n"); trimmed != "" {
			block += "\r\n" + trimmed
		}
		block += fmt.Sprintf("\r\n(exit %d)", code)
		AppendLog(block)
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
	stillMarkedRunning := task.running
	task.mu.Unlock()

	// Cross-check against the OS directly: cmd.Wait() (in Start's
	// goroutine) can stay blocked well after the process a caller cares
	// about has exited, if a grandchild inherited the output pipe's
	// write-end handle and outlives it (observed live during `go build`
	// background runs -- go.exe's own compile/link workers are exactly
	// this shape of process tree). Trust the OS's answer over a stuck
	// internal accounting rather than reporting running:true forever.
	if stillMarkedRunning {
		if exited, code := processExited(task.pid); exited {
			task.mu.Lock()
			if task.running { // avoid clobbering a real exit code that landed between the checks above and here
				task.running = false
				task.exitCode = &code
			}
			task.mu.Unlock()
		}
	}

	task.mu.Lock()
	defer task.mu.Unlock()
	data := task.buf.Bytes()
	if cursor < 0 || cursor > int64(len(data)) {
		cursor = 0
	}
	return task.running, string(data[cursor:]), int64(len(data)), task.exitCode, nil
}
