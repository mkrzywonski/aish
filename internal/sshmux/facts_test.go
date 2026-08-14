package sshmux

import (
	"errors"
	"strings"
	"testing"
)

func testConn() *ConnInfo {
	return &ConnInfo{Pid: 1, Host: "localhost", User: "mk31", Port: "22", Sock: "/run/aish/cm-deadbeef"}
}

func TestFactsAbsentUntilProbed(t *testing.T) {
	m := New(t.TempDir())
	if _, ok := m.Facts(testConn()); ok {
		t.Error("expected no facts before any probe")
	}
	if _, ok := m.CachedCapabilities(testConn()); ok {
		t.Error("expected no cached capabilities before any probe")
	}
}

// TestFactsOutliveTheChannel is the point of the whole record: a probe result
// must survive the channel that produced it. Previously capabilities lived on
// the *channel, so a failure left nothing behind (endless re-probe) and a
// timeout discarded a success (target_confidence regressed to unknown).
func TestFactsOutliveTheChannel(t *testing.T) {
	m := New(t.TempDir())
	ci := testConn()

	m.NoteShellUsable(ci, Capabilities{Hostname: "buildbox", User: "mk31", HasBase64: true})

	// Simulate the channel dying — dropChannel is what a timeout or a lost
	// channel calls. Facts must be untouched.
	m.dropChannel(ci.Sock, nil)

	caps, ok := m.CachedCapabilities(ci)
	if !ok {
		t.Fatal("capabilities did not survive the channel being dropped")
	}
	if caps.Hostname != "buildbox" {
		t.Errorf("hostname = %q, want %q", caps.Hostname, "buildbox")
	}
}

func TestFactsKeyedBySock(t *testing.T) {
	m := New(t.TempDir())
	a := testConn()
	b := &ConnInfo{Host: "other", User: "mk31", Port: "22", Sock: "/run/aish/cm-01234567"}

	m.NoteShellUsable(a, Capabilities{Hostname: "a-host"})
	m.NoteShellUnusable(b, DialectCmd, "cmd.exe", "'sh' is not recognized", true)

	if caps, ok := m.CachedCapabilities(a); !ok || caps.Hostname != "a-host" {
		t.Error("first target's facts were disturbed by the second")
	}
	f, ok := m.Facts(b)
	if !ok || f.Shell.Dialect != DialectCmd {
		t.Error("second target did not record its own dialect")
	}
	if _, ok := m.CachedCapabilities(b); ok {
		t.Error("a failed shell axis must not report cached capabilities")
	}
}

// TestSoftFailureRetryBudget: an UNCLASSIFIED failure is a transport fact and
// deserves one retry, but must not cost an unbounded number of channel opens
// (each of which can be an MFA push).
func TestSoftFailureRetryBudget(t *testing.T) {
	m := New(t.TempDir())
	ci := testConn()

	f := m.NoteShellUnusable(ci, DialectUnknown, "", "", false)
	if f.Shell.Sticky {
		t.Error("first unclassified failure should be retryable")
	}
	if f.ShellBlocked() {
		t.Error("a retryable failure must not block the channel")
	}

	f = m.NoteShellUnusable(ci, DialectUnknown, "", "", false)
	if !f.Shell.Sticky {
		t.Errorf("second unclassified failure should become sticky (attempts=%d)", f.Shell.Attempts)
	}
	if !f.ShellBlocked() {
		t.Error("a sticky failure must block further channel opens")
	}
}

func TestClassifiedFailureIsStickyImmediately(t *testing.T) {
	m := New(t.TempDir())
	ci := testConn()
	f := m.NoteShellUnusable(ci, DialectCmd, "the remote ssh login shell is Windows cmd.exe",
		"'sh' is not recognized as an internal or external command,", true)
	if !f.ShellBlocked() {
		t.Error("a classified host fact should block immediately — retrying can never help")
	}
	if f.Shell.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", f.Shell.Attempts)
	}
}

func TestForgetFactsAllowsRetry(t *testing.T) {
	m := New(t.TempDir())
	ci := testConn()
	m.NoteShellUnusable(ci, DialectCmd, "cmd.exe", "'sh' is not recognized", true)
	if f, _ := m.Facts(ci); !f.ShellBlocked() {
		t.Fatal("precondition: host should be blocked")
	}

	m.ForgetFacts(ci) // what probe_host{force:true} does

	if _, ok := m.Facts(ci); ok {
		t.Error("ForgetFacts should clear the record entirely")
	}
}

// TestShellErrorText is the anti-loop regression test. The message a blocked
// host returns has to tell a model three things: what the host actually is,
// that aish is deliberately not retrying, and what to do instead. Above all it
// must NOT repeat the "call probe_host to initialize" invitation that caused
// the endless re-probe in the first place.
func TestShellErrorText(t *testing.T) {
	m := New(t.TempDir())
	ci := testConn()
	f := m.NoteShellUnusable(ci, DialectCmd,
		"the remote ssh login shell is Windows cmd.exe",
		"'sh' is not recognized as an internal or external command,", true)

	err := f.ShellError()
	if err == nil {
		t.Fatal("a blocked host must produce an error")
	}
	if !errors.Is(err, ErrShellUnusable) {
		t.Error("the error should wrap ErrShellUnusable so callers can classify it")
	}
	msg := err.Error()

	for _, want := range []string{
		"cmd.exe",           // names the host
		"is not recognized", // quotes the evidence
		"not retrying",      // states the policy
		"MFA",               // says why
		"run_command",       // the way forward
		"force=true",        // the escape hatch
		"localhost",         // which host
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "probe_host to initialize") {
		t.Errorf("message still invites the re-probe loop:\n%s", msg)
	}
}

// TestNeedsProbeAfterForget is the regression test for a bug that
// probe_host{force:true} introduced: forgetting a host's facts does NOT close
// its channel, so keying the probe on "did we just open this channel" left the
// facts permanently empty. EnsureProbed then returned zero capabilities with a
// nil error, every tool reverted to "unknown", and the session stayed broken
// until the channel happened to die.
//
// Re-probing a live channel is another command on the same `sh -s`, so it costs
// no ssh session and cannot trigger an extra MFA push — which is what makes
// re-probing the right answer rather than dropping the channel.
func TestNeedsProbeAfterForget(t *testing.T) {
	m := New(t.TempDir())
	ci := testConn()

	if !m.needsProbe(ci, true) {
		t.Error("a freshly opened channel always needs the probe")
	}

	m.NoteShellUsable(ci, Capabilities{Hostname: "h", HasBase64: true})
	if m.needsProbe(ci, false) {
		t.Error("a live channel with recorded capabilities should not re-probe")
	}

	m.ForgetFacts(ci) // what probe_host{force:true} does
	if !m.needsProbe(ci, false) {
		t.Error("after a forced reset the live channel must re-probe, or the facts stay empty forever")
	}
}

func TestShellErrorNilWhenUsable(t *testing.T) {
	m := New(t.TempDir())
	ci := testConn()
	m.NoteShellUsable(ci, Capabilities{Hostname: "h"})
	f, _ := m.Facts(ci)
	if err := f.ShellError(); err != nil {
		t.Errorf("a working host must not produce an error, got %v", err)
	}
	if !f.ShellUsable() {
		t.Error("ShellUsable should be true after a successful probe")
	}
}
