package session

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Console: aish talking to the human through the real terminal WITHOUT going
// through the PTY. Output is written straight to os.Stdout (the user sees it;
// the shell, the scrollback Ring, and read_screen never do). Input, while a
// prompt is active, is captured off the stdin pump before it reaches the
// shell — so the user's keypress is consumed by aish and no bytes land at the
// shell prompt. This is the sanctioned second exception to byte-transparency
// (window-title marking was the first): aish speaks ABOUT the session, never
// injects INTO it.
//
// Prompts serialize on promptMu; only one is on screen at a time. While a
// prompt is up, outMu is held so shell output can't interleave (the pump
// blocks on it and flushes afterward — nothing is lost).

const promptColor = "\033[1;35m" // bold magenta, matching the ⧉ badge
const promptReset = "\033[0m"

// Notify writes a one-line message to the user's terminal, out of band from
// the shell. Used to surface aish-level events (authorization challenges,
// grants) that must not enter the session stream.
func (s *Session) Notify(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	s.outMu.Lock()
	defer s.outMu.Unlock()
	fmt.Fprintf(os.Stdout, "\r\n%s🔒 aish%s %s\r\n", promptColor, promptReset, msg)
}

// Prompt shows question on the terminal and waits for the user to press one
// of the accept keys (case-insensitive), returning the lowercased key. The
// keypress is captured before the shell sees it. On timeout it returns
// (0, false) so callers fail closed. accept must be lowercase.
func (s *Session) Prompt(question, accept string, timeout time.Duration) (byte, bool) {
	s.promptMu.Lock()
	defer s.promptMu.Unlock()

	ch := make(chan byte, 64)
	s.capMu.Lock()
	s.capCh = ch
	s.capMu.Unlock()
	s.capturing.Store(true)
	defer func() {
		s.capturing.Store(false)
		s.capMu.Lock()
		s.capCh = nil
		s.capMu.Unlock()
	}()

	// Hold output for the whole interaction so the frozen screen doesn't
	// scroll under the prompt; draw the question.
	s.outMu.Lock()
	defer s.outMu.Unlock()
	fmt.Fprintf(os.Stdout, "\r\n%s🔒 aish%s %s [%s] ", promptColor, promptReset, question, keyHint(accept))

	deadline := time.After(timeout)
	for {
		select {
		case b := <-ch:
			lb := lower(b)
			if strings.IndexByte(accept, lb) >= 0 {
				fmt.Fprintf(os.Stdout, "%c\r\n", lb) // echo the choice
				return lb, true
			}
			// ignore anything else (stray Enter, arrow keys, etc.)
		case <-deadline:
			fmt.Fprintf(os.Stdout, "(timed out)\r\n")
			return 0, false
		}
	}
}

// deliverCaptured routes stdin bytes to an active prompt instead of the PTY.
// Called from the input pump while s.capturing is set.
func (s *Session) deliverCaptured(p []byte) {
	s.capMu.Lock()
	ch := s.capCh
	s.capMu.Unlock()
	if ch == nil {
		return
	}
	for _, b := range p {
		select {
		case ch <- b:
		default: // prompt buffer full; drop excess typeahead
		}
	}
}

func lower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// keyHint renders accept keys as e.g. "y/n/a". Kept neutral (no capitalized
// default) because prompts fail closed on timeout regardless of any implied
// default.
func keyHint(accept string) string {
	parts := make([]string, len(accept))
	for i := 0; i < len(accept); i++ {
		parts[i] = string(accept[i])
	}
	return strings.Join(parts, "/")
}
