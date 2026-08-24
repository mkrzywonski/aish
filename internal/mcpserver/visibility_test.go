package mcpserver

import "testing"

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
