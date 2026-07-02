// Package framing implements run_command: injecting a command line into the
// shared terminal and determining, from the output stream, where its output
// starts and ends and what its exit status was.
//
// Strategies, in preference order:
//   - osc133: the current shell has aish integration; capture between the
//     C (pre-exec) and D (done) marks. Exact.
//   - idle: no integration in the current context (e.g. plain remote
//     shell). The command is typed bare — nothing extra appears in the
//     shared terminal — and completion is inferred from output quiescence.
//     No exit code is available on this path.
//
// RunSentinel (OSC-7979 nonce wrapper) still exists for internal in-band
// file/exec fallbacks that need exit codes and exact boundaries, but it is
// no longer used for user-visible run_command: the echoed wrapper text on
// remote shells was deemed too intrusive.
package framing

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"ai-ssh/internal/session"
	"ai-ssh/internal/state"
	"ai-ssh/internal/term"
)

type Result struct {
	Output      string `json:"output"`
	ExitCode    *int   `json:"exit_code,omitempty"`
	Framing     string `json:"framing"`
	Truncated   bool   `json:"truncated"`
	TimedOut    bool   `json:"timed_out,omitempty"`
	CursorStart int64  `json:"cursor_start"`
	CursorEnd   int64  `json:"cursor_end"`
}

const maxReturn = 64 << 10 // cap on returned output; half head, half tail

type Engine struct {
	Sess    *session.Session
	Term    *term.Terminal
	Tracker *state.Tracker
}

// Run executes command in the shared terminal and captures its output.
func (e *Engine) Run(command string, timeout time.Duration) (*Result, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	snap := e.Term.Screen.Snapshot()
	if snap.AltScreen {
		return nil, errors.New("a full-screen application is active in the terminal; use send_keys and read_screen instead, or ask the user to exit it")
	}
	if e.Tracker.EchoOff() {
		return nil, errors.New("the terminal is waiting for secret input (e.g. a password); ask the user to type it, then retry")
	}

	if e.Tracker.PromptReady() {
		return e.runOSC133(command, timeout)
	}
	return e.runIdle(command, timeout)
}

// runIdle types the command bare and waits for the output stream to go
// quiet. Trade-offs: no exit code, a minimum latency of idleQuiet, and
// commands that pause longer than idleQuiet mid-run are reported complete
// with partial output (the rest remains readable via read_output).
func (e *Engine) runIdle(command string, timeout time.Duration) (*Result, error) {
	const idleQuiet = 1200 * time.Millisecond

	echoStart := e.Term.Ring.End()
	if _, err := e.Sess.WriteInput([]byte(command + "\r")); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	timedOut := false
	for {
		last := time.Unix(0, e.Sess.LastOutputNanos())
		quiet := time.Since(last)
		// Only count quiescence after the injection produced some output.
		if e.Term.Ring.End() > echoStart && quiet >= idleQuiet {
			break
		}
		if time.Now().After(deadline) {
			timedOut = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	res := e.window(afterEcho(e.Term, echoStart), e.Term.Ring.End(), "idle")
	res.TimedOut = timedOut
	// The remote prompt has usually been printed by the time we go idle; it
	// is the unterminated last line, so drop it.
	if i := strings.LastIndexByte(res.Output, '\n'); i >= 0 {
		res.Output = res.Output[:i+1]
	} else if !timedOut {
		res.Output = ""
	}
	return res, nil
}

func (e *Engine) runOSC133(command string, timeout time.Duration) (*Result, error) {
	events := e.Term.Parser.Subscribe()
	defer e.Term.Parser.Unsubscribe(events)

	if _, err := e.Sess.WriteInput([]byte(command + "\r")); err != nil {
		return nil, err
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	var outStart int64 = -1
	fallbackStart := e.Term.Ring.End()
	for {
		select {
		case ev := <-events:
			switch ev.Kind {
			case term.EvPreexec:
				outStart = ev.End
			case term.EvDone:
				if outStart < 0 {
					// D without C (e.g. empty command line accepted):
					// nothing ran, treat echo start as window start.
					outStart = fallbackStart
				}
				exit := ev.Exit
				res := e.window(outStart, ev.Start, "osc133")
				res.ExitCode = &exit
				return res, nil
			}
		case <-deadline.C:
			start := outStart
			if start < 0 {
				start = fallbackStart
			}
			res := e.window(start, e.Term.Ring.End(), "osc133")
			res.TimedOut = true
			return res, nil
		}
	}
}

// RunSentinel wraps the command with an invisible OSC-7979 printf carrying
// a nonce and $?, giving exact completion detection and an exit code on
// shells without integration. The wrapper is visible as echoed text on the
// remote, so this is reserved for internal in-band fallback operations.
func (e *Engine) RunSentinel(command string, timeout time.Duration) (*Result, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	nb := make([]byte, 6)
	rand.Read(nb)
	nonce := hex.EncodeToString(nb)

	events := e.Term.Parser.Subscribe()
	defer e.Term.Parser.Unsubscribe(events)

	// The wrapper prints an OSC only the terminal parser sees; the echoed
	// command text contains a literal backslash-033, not an ESC byte, so it
	// cannot trigger the parser. $? at printf time is the command's status.
	line := fmt.Sprintf(`%s; printf '\033]7979;%s;%%s\033\\' "$?"`, command, nonce)
	echoStart := e.Term.Ring.End()
	if _, err := e.Sess.WriteInput([]byte(line + "\r")); err != nil {
		return nil, err
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	for {
		select {
		case ev := <-events:
			if ev.Kind == term.EvSentinel && ev.Nonce == nonce {
				exit := ev.Exit
				res := e.window(afterEcho(e.Term, echoStart), ev.Start, "sentinel")
				res.ExitCode = &exit
				return res, nil
			}
		case <-deadline.C:
			res := e.window(afterEcho(e.Term, echoStart), e.Term.Ring.End(), "sentinel")
			res.TimedOut = true
			return res, nil
		}
	}
}

// afterEcho skips past the terminal's echo of the injected command line:
// output proper begins after the first newline following the injection
// point. If no newline arrived yet, the window is empty.
func afterEcho(t *term.Terminal, injectedAt int64) int64 {
	data, next, _ := t.Ring.ReadFrom(injectedAt, 1<<20)
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return injectedAt + int64(i) + 1
	}
	return next
}

// window slices [start, end) out of the ring, strips escapes, and truncates
// oversized output keeping head and tail halves.
func (e *Engine) window(start, end int64, framing string) *Result {
	res := &Result{Framing: framing, CursorStart: start, CursorEnd: end}
	if end < start {
		end = start
	}
	size := end - start
	if size <= maxReturn {
		data, _, _ := e.Term.Ring.ReadFrom(start, int(size))
		res.Output = string(term.StripEscapes(data))
		return res
	}
	half := maxReturn / 2
	head, _, _ := e.Term.Ring.ReadFrom(start, half)
	tail, _, _ := e.Term.Ring.ReadFrom(end-int64(half), half)
	res.Output = string(term.StripEscapes(head)) +
		fmt.Sprintf("\n... [%d bytes truncated; use read_output with cursor to fetch] ...\n", size-int64(maxReturn)) +
		string(term.StripEscapes(tail))
	res.Truncated = true
	return res
}
