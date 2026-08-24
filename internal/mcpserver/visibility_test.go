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
