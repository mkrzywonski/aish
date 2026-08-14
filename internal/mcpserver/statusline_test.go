package mcpserver

import (
	"strings"
	"testing"

	"ai-ssh/internal/sshmux"
)

func TestSessionAttemptStatusTakesOverBar(t *testing.T) {
	got := sessionAttemptStatus(sshmux.SessionAttempt{
		Kind: sshmux.SessionAttemptDeep, Host: "duo.example", User: "mk31", Count: 2,
	})
	for _, want := range []string{
		"AISH OPENING SSH", "2FA MAY BE REQUESTED", "2 sessions",
		"deep identity probe", "mk31@duo.example", "VERIFY BEFORE APPROVING",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("modal status missing %q: %s", want, got)
		}
	}
	for _, absent := range []string{"tty:", "oob:", "Ctrl-]"} {
		if strings.Contains(got, absent) {
			t.Errorf("modal status retained standard field %q: %s", absent, got)
		}
	}
}
