// Package session owns the PTY: it spawns the user's shell, pumps bytes
// between the real terminal and the PTY, and fans output out to registered
// taps (screen model, ring buffer, OSC parser). It is the single source of
// truth for both human and AI input, serialized through WriteInput.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// Session is one shared terminal session: a shell on a PTY plus the plumbing
// that lets an AI observe and inject alongside the human.
type Session struct {
	ID   string
	Cmd  *exec.Cmd
	Ptmx *os.File

	// Stdout is where the user-visible output goes (defaults to os.Stdout).
	// runMain wraps it with term.TitleMarker to badge window titles.
	Stdout io.Writer

	inputMu sync.Mutex

	tapsMu sync.RWMutex
	taps   []io.Writer

	resizeMu  sync.Mutex
	resizeCbs []func(rows, cols uint16)

	// Console (console.go): out-of-band interaction with the user that does
	// not touch the PTY. outMu gates the output pump while a prompt is on
	// screen; promptMu serializes prompts; capturing/capCh divert stdin from
	// the shell to an active prompt.
	outMu     sync.Mutex
	promptMu  sync.Mutex
	capturing atomic.Bool
	capMu     sync.Mutex
	capCh     chan byte

	// menuKey (default Ctrl-]) opened the aish menu when typed at the shell;
	// onMenu runs the menu. Swallowed from the shell input when triggered.
	menuKey byte
	onMenu  func()

	lastOutput atomic.Int64 // unix nanos of last PTY output
	closed     atomic.Bool
}

// NewID returns a short random session identifier.
func NewID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// New prepares a session running argv (typically the user's shell) with the
// given extra environment appended to the current one.
func New(id string, argv []string, extraEnv []string) *Session {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), extraEnv...)
	return &Session{ID: id, Cmd: cmd, menuKey: 0x1d} // Ctrl-]
}

// SetMenu registers the handler invoked when the user presses the aish menu
// key (Ctrl-]) at the terminal. It runs on its own goroutine.
func (s *Session) SetMenu(fn func()) { s.onMenu = fn }

// AddTap registers an additional writer that receives every byte the PTY
// emits. Taps must not block; slow consumers should buffer internally.
func (s *Session) AddTap(w io.Writer) {
	s.tapsMu.Lock()
	defer s.tapsMu.Unlock()
	s.taps = append(s.taps, w)
}

// OnResize registers a callback invoked with the new size whenever the
// window size changes (and once at startup).
func (s *Session) OnResize(cb func(rows, cols uint16)) {
	s.resizeMu.Lock()
	defer s.resizeMu.Unlock()
	s.resizeCbs = append(s.resizeCbs, cb)
}

// WriteInput writes bytes to the shell's input. Human keystrokes and AI
// injections both come through here, serialized so they interleave at byte
// granularity, never mid-write.
func (s *Session) WriteInput(p []byte) (int, error) {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	if s.closed.Load() {
		return 0, errors.New("session ended")
	}
	return s.Ptmx.Write(p)
}

// LastOutputNanos reports when the PTY last produced output (unix nanos).
func (s *Session) LastOutputNanos() int64 { return s.lastOutput.Load() }

// Closed reports whether the shell has exited.
func (s *Session) Closed() bool { return s.closed.Load() }

// Run starts the shell and pumps the real terminal <-> PTY until the shell
// exits. It puts stdin into raw mode and restores it on return. The returned
// int is the shell's exit code.
func (s *Session) Run() (int, error) {
	ptmx, err := pty.Start(s.Cmd)
	if err != nil {
		return 1, fmt.Errorf("starting shell: %w", err)
	}
	s.Ptmx = ptmx
	defer ptmx.Close()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			s.applySize()
		}
	}()
	s.applySize()

	stdinFd := int(os.Stdin.Fd())
	var oldState *term.State
	if term.IsTerminal(stdinFd) {
		oldState, err = term.MakeRaw(stdinFd)
		if err != nil {
			return 1, fmt.Errorf("setting raw mode: %w", err)
		}
		defer term.Restore(stdinFd, oldState)
	}

	// Human input pump. Reads block indefinitely; the goroutine dies with
	// the process, which is fine.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if s.capturing.Load() {
					// A prompt/menu is up: the keypress answers aish, not the
					// shell.
					s.deliverCaptured(buf[:n])
				} else if i := s.menuTrigger(buf[:n]); i >= 0 {
					// Swallow the menu key, forward the rest to the shell,
					// open the menu on its own goroutine.
					rest := append(append([]byte{}, buf[:i]...), buf[i+1:n]...)
					if len(rest) > 0 {
						s.WriteInput(rest)
					}
					go s.onMenu()
				} else if _, werr := s.WriteInput(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Output pump: PTY -> real stdout + taps. Runs on the main goroutine so
	// Run returns only after the final output is flushed.
	stdout := s.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	buf := make([]byte, 32*1024)
	for {
		n, rerr := ptmx.Read(buf)
		if n > 0 {
			s.lastOutput.Store(nowNanos())
			// outMu is held while a console prompt owns the screen, so shell
			// output waits here and flushes after — never interleaving with
			// the prompt.
			s.outMu.Lock()
			stdout.Write(buf[:n])
			s.outMu.Unlock()
			s.tapsMu.RLock()
			for _, t := range s.taps {
				t.Write(buf[:n])
			}
			s.tapsMu.RUnlock()
		}
		if rerr != nil {
			// EIO is the normal "child exited" signal on Linux.
			break
		}
	}

	s.closed.Store(true)
	err = s.Cmd.Wait()
	code := s.Cmd.ProcessState.ExitCode()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return code, err
	}
	return code, nil
}

func nowNanos() int64 { return time.Now().UnixNano() }

func (s *Session) applySize() {
	ws, err := pty.GetsizeFull(os.Stdin)
	if err != nil || ws.Rows == 0 || ws.Cols == 0 {
		// No usable size (e.g. stdin is not a real terminal): pick a sane
		// default rather than propagating a 0x0 winsize to the shell.
		ws = &pty.Winsize{Rows: 24, Cols: 80}
	}
	pty.Setsize(s.Ptmx, ws)
	s.resizeMu.Lock()
	cbs := append([]func(rows, cols uint16){}, s.resizeCbs...)
	s.resizeMu.Unlock()
	for _, cb := range cbs {
		cb(ws.Rows, ws.Cols)
	}
}
