package mcpserver

import (
	"strings"
	"testing"

	"ai-ssh/internal/sshmux"
)

func TestSessionAttemptStatusTakesOverBar(t *testing.T) {
	got := sessionAttemptStatus(sshmux.SessionAttempt{
		Kind: sshmux.SessionAttemptDeep, Host: "duo.example", User: "mk31", Count: 2,
	}, 0)
	for _, want := range []string{
		"AISH OPENING SSH", "2FA MAY BE REQUESTED", "2 sessions",
		"deep identity probe", "mk31@duo.example", "VERIFY BEFORE APPROVING",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("modal status missing %q: %s", want, got)
		}
	}
	// The takeover still replaces the standard bar rather than decorating it.
	for _, absent := range []string{"tty:", "oob:", "Ctrl-] menu"} {
		if strings.Contains(got, absent) {
			t.Errorf("modal status retained standard field %q: %s", absent, got)
		}
	}
}

// The stop hint is purely additive: it appears only when the warning and its
// target already fit, because losing a character of those to advertise a menu
// that is always reachable would be a bad trade.
func TestSessionAttemptStopHintOnlyWhenItFits(t *testing.T) {
	attempt := sshmux.SessionAttempt{
		Kind: sshmux.SessionAttemptShell, Host: "duo.example", User: "mk31", Count: 1,
	}
	bare := sessionAttemptStatus(attempt, 0)
	if strings.Contains(bare, "Ctrl-]") {
		t.Errorf("hint appeared with no width known: %s", bare)
	}
	if narrow := sessionAttemptStatus(attempt, len(bare)+5); strings.Contains(narrow, "Ctrl-]") {
		t.Errorf("hint appeared without room for it: %s", narrow)
	}
	wide := sessionAttemptStatus(attempt, 200)
	if !strings.Contains(wide, "Ctrl-] m") {
		t.Errorf("hint missing on a wide terminal: %s", wide)
	}
	if len(wide) > 200 {
		t.Errorf("hint pushed the line past the terminal width: %d cols", len(wide))
	}
}

func TestSFTPSessionAttemptNamesSubsystem(t *testing.T) {
	got := sessionAttemptStatus(sshmux.SessionAttempt{
		Kind: sshmux.SessionAttemptSFTP, Host: "duo.example", User: "mk31", Count: 1,
	}, 0)
	for _, want := range []string{"2FA MAY BE REQUESTED", "SFTP subsystem", "mk31@duo.example"} {
		if !strings.Contains(got, want) {
			t.Errorf("SFTP modal status missing %q: %s", want, got)
		}
	}
}

func TestInputRequiredStatusTakesOverBar(t *testing.T) {
	got := inputRequiredStatus()
	for _, want := range []string{"AISH INPUT REQUIRED", "answer the prompt", "ESC CANCELS"} {
		if !strings.Contains(got, want) {
			t.Errorf("input modal missing %q: %s", want, got)
		}
	}
	for _, absent := range []string{"tty:", "oob:", "Ctrl-]"} {
		if strings.Contains(got, absent) {
			t.Errorf("input modal retained standard field %q: %s", absent, got)
		}
	}
}
