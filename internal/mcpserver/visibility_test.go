package mcpserver

import (
	"testing"

	"ai-ssh/internal/state"
)

func TestVisibilityOf(t *testing.T) {
	for _, tc := range []struct {
		name, tool, via, want string
	}{
		{"out-of-band channel is silent", "exec", "channel", visibilitySilent},
		{"controlmaster is silent", "file_write", "controlmaster", visibilitySilent},
		{"sftp is silent", "file_read", "sftp", visibilitySilent},
		{"local out-of-band is silent", "file_edit", "local", visibilitySilent},
		{"in-band typing is visible", "file_read", "in_band", visibilityVisible},
		{"terminal tools are visible", "run_command", "", visibilityVisible},
		{"send_input is visible", "send_input", "", visibilityVisible},
		{"control tools report nothing", "session_status", "controlmaster", ""},
		{"set_session_name reports nothing", "set_session_name", "", ""},
		{"unresolved route claims nothing", "file_write", "", visibilityUnknown},
		{"reading the log is not an operation", "oob_log", "", ""},
	} {
		if got := visibilityOf(tc.tool, tc.via); got != tc.want {
			t.Errorf("%s: visibilityOf(%q, %q) = %q, want %q", tc.name, tc.tool, tc.via, got, tc.want)
		}
	}
}

// Every route the log treats as invisible must read as silent to a caller, or
// the field a model trusts and the audit trail would disagree.
func TestVisibilityMatchesTheAuditTrail(t *testing.T) {
	for via := range invisibleRoutes {
		if got := visibilityOf("file_write", via); got != visibilitySilent {
			t.Errorf("route %q is invisible in the activity log but reported as %q", via, got)
		}
	}
}

// "Did this change anything" is the first question when reviewing a list of
// operations, and it was previously answerable only by recognising tool names.
func TestEffectOf(t *testing.T) {
	for tool, want := range map[string]string{
		"read_screen":    "read",
		"file_read":      "read",
		"directory_list": "read",
		"oob_log":        "read",
		"file_write":     "acted",
		"file_edit":      "acted",
		"exec":           "acted",
		"run_command":    "acted",
		"send_input":     "acted",
		"file_upload":    "acted",
	} {
		if got := effectOf(tool); got != want {
			t.Errorf("effectOf(%q) = %q, want %q", tool, got, want)
		}
	}
}

// effect was computed on the internal entry and never mapped to the tool's
// result, so the distinction existed everywhere except where a caller reads it.
func TestOobLogEntryCarriesEffect(t *testing.T) {
	e := activityEntry{Seq: 1, Tool: "file_read", Via: "local", Effect: effectOf("file_read")}
	out := oobLogEntry{Seq: e.Seq, Tool: e.Tool, Via: e.Via, Visible: e.Visible, Effect: e.Effect}
	if out.Effect != "read" {
		t.Errorf("effect not carried into the result: %+v", out)
	}
}

// The connection target and the machine's own hostname are different facts.
// One field used to carry both, starting as the target and being overwritten
// by the probed hostname, so its meaning changed under the caller.
func TestHostConfidenceKeepsTargetAndHostnameApart(t *testing.T) {
	c := &Core{Tracker: state.NewTracker(func() int { return -1 })}
	// Unprobed: the target is known, the hostname is not, and confidence says so.
	_, target, hostname, confidence := c.hostConfidence(route{via: "controlmaster", host: "vps.wg"})
	if target != "vps.wg" {
		t.Errorf("oob_host should be the connection target, got %q", target)
	}
	if hostname != "" {
		t.Errorf("remote_hostname should be empty before a probe, got %q", hostname)
	}
	if confidence != "unknown" {
		t.Errorf("confidence should be unknown before a probe, got %q", confidence)
	}
}

// The question host tracking exists to answer is "do my invisible commands
// land on the machine the human is watching" — which came up originally with a
// jump host. It is answered by comparing the terminal's reported host with the
// probed out-of-band hostname. The connection target is NOT part of that
// comparison: an alias, an FQDN, an IP or a ProxyJump expression need not
// equal any hostname, so comparing it would manufacture disagreement on
// exactly the setups where the answer is hardest.
func TestConfidenceComparesReportedHostsNotTheConnectionTarget(t *testing.T) {
	// Terminal says "vps", probed host says "vps": same machine, however the
	// target was written.
	if got := classifyConfidence("vps", "mjk-desktop", "vps"); got != "same" {
		t.Errorf("matching reported hosts should be same, got %q", got)
	}
	// Terminal says one machine, the out-of-band channel reports another:
	// that is the divergence worth refusing writes over.
	if got := classifyConfidence("vps", "mjk-desktop", "otherbox"); got == "same" {
		t.Errorf("diverging reported hosts must not read as same, got %q", got)
	}
	// A terminal with no OSC 7 leaves the tracker holding the LOCAL host; that
	// is absence of evidence, not evidence of divergence.
	if got := classifyConfidence("mjk-desktop", "mjk-desktop", "vps"); got == "same" {
		t.Errorf("stale local host must not be read as agreement, got %q", got)
	}
}

// The human is prompted at most once per host so they are not nagged. That
// quiet is for them, not for the AI: after a single "yes" every later
// operation looked identical to one on a verified host, and silence about an
// unverified target is how a command lands on the wrong machine.
func TestDivergencePolicyPromptsHumanOnceButNeverGoesQuietForReads(t *testing.T) {
	// First mutation on an unverified host: ask the human.
	if got := divergencePolicy("unknown", opMutate, false); got != divConfirm {
		t.Errorf("first unverified mutation should confirm, got %v", got)
	}
	// Once confirmed, do not re-prompt.
	if got := divergencePolicy("unknown", opMutate, true); got != divAllow {
		t.Errorf("confirmed host should not re-prompt, got %v", got)
	}
	// A detected mismatch is refused outright, confirmed or not.
	if got := divergencePolicy("mismatch", opMutate, true); got != divFail {
		t.Errorf("mismatch must fail closed even after confirmation, got %v", got)
	}
	// Reads are never blocked, but a mismatch is always surfaced.
	if got := divergencePolicy("mismatch", opRead, true); got != divWarn {
		t.Errorf("mismatched read should warn, got %v", got)
	}
}
