// Package state tracks "where are we" for a session: whether the shell is
// at a prompt or running a command (from OSC 133 marks), the current
// cwd/host (from OSC 7), the foreground process on the PTY, and whether the
// terminal is collecting secret (echo-off) input.
package state

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"ai-ssh/internal/term"
)

type Mode string

const (
	ModePrompt     Mode = "prompt"
	ModeRunning    Mode = "running"
	ModeFullscreen Mode = "fullscreen"
	ModeUnknown    Mode = "unknown"
)

type Foreground struct {
	Pid  int    `json:"pid"`
	Comm string `json:"comm"`
	Args string `json:"args,omitempty"`
}

// Tracker consumes parser events and answers status queries. ptmxFd is the
// PTY master fd, used for TIOCGPGRP and termios inspection.
type Tracker struct {
	mu          sync.Mutex
	promptReady bool
	cmdOpen     bool
	lastExit    *int
	cwd         string
	oscHost     string

	// ptmxFd returns the PTY master fd, or -1 if the PTY isn't up yet.
	// Lazy because the fd exists only once the session has started.
	ptmxFd    func() int
	localHost string
}

func NewTracker(ptmxFd func() int) *Tracker {
	hn, _ := os.Hostname()
	return &Tracker{ptmxFd: ptmxFd, localHost: hn}
}

// Consume runs until the events channel closes; call in a goroutine with a
// subscription from the OSC parser.
func (t *Tracker) Consume(events <-chan term.Event) {
	for ev := range events {
		t.mu.Lock()
		switch ev.Kind {
		case term.EvPrompt:
			t.promptReady = true
			t.cmdOpen = false
		case term.EvPreexec:
			t.promptReady = false
			t.cmdOpen = true
		case term.EvDone:
			exit := ev.Exit
			t.lastExit = &exit
			t.cmdOpen = false
		case term.EvCwd:
			t.oscHost = ev.Host
			t.cwd = ev.Cwd
		}
		t.mu.Unlock()
	}
}

// PromptReady reports whether integration marks indicate the shell is at a
// prompt right now (i.e. OSC 133 framing is usable).
func (t *Tracker) PromptReady() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.promptReady
}

// Cwd returns the last OSC 7-reported working directory and host.
func (t *Tracker) Cwd() (host, cwd string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.oscHost, t.cwd
}

// LocalHost returns this machine's hostname.
func (t *Tracker) LocalHost() string { return t.localHost }

// Mode derives the current mode; altScreen comes from the screen model.
func (t *Tracker) Mode(altScreen bool) Mode {
	if altScreen {
		return ModeFullscreen
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.promptReady {
		return ModePrompt
	}
	if t.cmdOpen {
		return ModeRunning
	}
	return ModeUnknown
}

// EchoOff reports whether the PTY line discipline is in cooked mode with
// echo disabled — the signature of a password prompt (sudo, su, ssh asking
// locally). Raw-mode apps (vim, ssh sessions) have ICANON off and don't
// count.
func (t *Tracker) EchoOff() bool {
	fd := t.ptmxFd()
	if fd < 0 {
		return false
	}
	tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return false
	}
	return tio.Lflag&unix.ECHO == 0 && tio.Lflag&unix.ICANON != 0
}

// Foreground returns the foreground process group leader on the PTY.
func (t *Tracker) Foreground() *Foreground {
	fd := t.ptmxFd()
	if fd < 0 {
		return nil
	}
	pgid, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil || pgid <= 0 {
		return nil
	}
	fg := &Foreground{Pid: pgid}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pgid)); err == nil {
		fg.Comm = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pgid)); err == nil {
		fg.Args = strings.TrimRight(strings.ReplaceAll(string(b), "\x00", " "), " ")
	}
	return fg
}
