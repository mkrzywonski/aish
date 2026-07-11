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

// beginCapture diverts stdin from the shell to a fresh channel and returns it,
// a cancel channel (closed by cancelCapture to abort the prompt out of band),
// and a cleanup func. Callers hold promptMu.
func (s *Session) beginCapture() (ch chan byte, cancel <-chan struct{}, done func()) {
	c := make(chan byte, 256)
	cc := make(chan struct{})
	s.capMu.Lock()
	s.capCh = c
	s.capCancel = cc
	s.capMu.Unlock()
	s.capturing.Store(true)
	return c, cc, func() {
		s.capturing.Store(false)
		s.capMu.Lock()
		s.capCh = nil
		s.capCancel = nil
		s.capMu.Unlock()
	}
}

// cancelCapture aborts the active prompt (if any) by closing its cancel
// channel — used by the input pump when a second menu key arrives. Safe to
// call when no prompt is active.
func (s *Session) cancelCapture() {
	s.capMu.Lock()
	cc := s.capCancel
	s.capCancel = nil // clear so the close happens at most once
	s.capMu.Unlock()
	if cc != nil {
		close(cc)
	}
}

// escLead is how long to wait for a byte after an ESC before deciding it was a
// standalone Escape keypress. Terminal escape sequences (arrow keys, and — the
// reason this exists — mouse-motion and focus reports) send their bytes back-to-
// back, so a follow-up byte within this window means "escape sequence" and its
// absence means the user actually pressed Escape.
const escLead = 40 * time.Millisecond

// recvByte reads one captured byte, or reports false if none arrives within
// escLead (used only mid escape-sequence, where bytes are already buffered).
func recvByte(ch <-chan byte) (byte, bool) {
	select {
	case b := <-ch:
		return b, true
	case <-time.After(escLead):
		return 0, false
	}
}

// isLoneEscape is called just after an ESC (0x1b) was read from ch. It reports
// whether that ESC was a standalone Escape keypress (→ a prompt should cancel).
// When more bytes follow — an escape sequence such as a CSI mouse/focus/arrow
// report or an SS3 key — it consumes the rest of the sequence so those bytes are
// not read as prompt input, and returns false. This stops a mouse move over the
// terminal (which emits ESC-prefixed sequences) from cancelling the prompt.
func isLoneEscape(ch <-chan byte) bool {
	b, ok := recvByte(ch)
	if !ok {
		return true // nothing followed the ESC → a real Escape keypress
	}
	switch b {
	case '[': // CSI
		first, ok := recvByte(ch)
		if !ok {
			return false
		}
		if first == 'M' { // X10 mouse report: exactly 3 raw coordinate bytes follow
			for i := 0; i < 3; i++ {
				recvByte(ch)
			}
			return false
		}
		// Otherwise consume up to and including the final byte (0x40..0x7e):
		// covers focus (I/O), arrows, and SGR mouse (… 'M'/'m').
		if first >= 0x40 && first <= 0x7e {
			return false
		}
		for {
			c, ok := recvByte(ch)
			if !ok || (c >= 0x40 && c <= 0x7e) {
				return false
			}
		}
	case 'O': // SS3: one byte follows (e.g. F1-F4, application-mode arrows)
		recvByte(ch)
		return false
	default: // ESC + other (e.g. an Alt-key) — not a cancel
		return false
	}
}

// Prompt shows question on the terminal and waits for the user to press one
// of the accept keys (case-insensitive), returning the lowercased key. The
// keypress is captured before the shell sees it. Esc or timeout returns
// (0, false) so callers fail closed. accept must be lowercase.
func (s *Session) Prompt(question, accept string, timeout time.Duration) (byte, bool) {
	s.promptMu.Lock()
	defer s.promptMu.Unlock()
	ch, cancel, done := s.beginCapture()
	defer done()

	// Hold output for the whole interaction so the frozen screen doesn't
	// scroll under the prompt; draw the question.
	s.outMu.Lock()
	defer s.outMu.Unlock()
	fmt.Fprintf(os.Stdout, "\r\n%s%s [%s]%s ", promptColor, question, keyHint(accept), promptReset)

	deadline := time.After(timeout)
	for {
		select {
		case b := <-ch:
			if b == 0x1b { // Esc cancels — but not a mouse/focus/arrow escape sequence
				if isLoneEscape(ch) {
					fmt.Fprintf(os.Stdout, "(cancelled)\r\n")
					return 0, false
				}
				continue // drained an escape sequence; keep waiting for a choice
			}
			lb := lower(b)
			if strings.IndexByte(accept, lb) >= 0 {
				fmt.Fprintf(os.Stdout, "%c\r\n", lb) // echo the choice
				return lb, true
			}
			// ignore anything else (stray Enter, arrow keys, etc.)
		case <-cancel: // a second menu key aborted the prompt
			fmt.Fprintf(os.Stdout, "(cancelled)\r\n")
			return 0, false
		case <-deadline:
			fmt.Fprintf(os.Stdout, "(timed out)\r\n")
			return 0, false
		}
	}
}

// PromptLine shows question and reads a line the user types (echoed, with
// backspace), returning it on Enter. Esc or timeout returns ("", false). The
// typed bytes are captured before the shell sees them.
func (s *Session) PromptLine(question string, timeout time.Duration) (string, bool) {
	s.promptMu.Lock()
	defer s.promptMu.Unlock()
	ch, cancel, done := s.beginCapture()
	defer done()

	s.outMu.Lock()
	defer s.outMu.Unlock()
	fmt.Fprintf(os.Stdout, "\r\n%s%s%s ", promptColor, question, promptReset)

	var line []byte
	deadline := time.After(timeout)
	for {
		select {
		case b := <-ch:
			switch {
			case b == '\r' || b == '\n':
				fmt.Fprint(os.Stdout, "\r\n")
				return string(line), true
			case b == 0x1b: // Esc cancels — but not a mouse/focus/arrow escape sequence
				if isLoneEscape(ch) {
					fmt.Fprint(os.Stdout, "\r\n")
					return "", false
				}
				// drained an escape sequence; ignore it and keep reading the line
			case b == 0x7f || b == 0x08: // backspace
				if len(line) > 0 {
					line = line[:len(line)-1]
					fmt.Fprint(os.Stdout, "\b \b")
				}
			case b >= 0x20 && b < 0x7f: // printable
				line = append(line, b)
				fmt.Fprintf(os.Stdout, "%c", b)
			}
		case <-cancel: // a second menu key aborted the prompt
			fmt.Fprint(os.Stdout, "\r\n")
			return "", false
		case <-deadline:
			fmt.Fprint(os.Stdout, "\r\n")
			return "", false
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

// menuTrigger returns the index of the menu key in p if a menu handler is
// registered and the key is present, else -1.
func (s *Session) menuTrigger(p []byte) int {
	if s.onMenu == nil {
		return -1
	}
	for i, b := range p {
		if b == s.menuKey {
			return i
		}
	}
	return -1
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
