package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/authproto"
	"ai-ssh/internal/clientauth"
)

func recordN(l *activityLog, n int) {
	for i := 0; i < n; i++ {
		l.record(activityEntry{Tool: "exec", Via: "channel"})
	}
}

func seqs(entries []activityEntry) []int64 {
	out := make([]int64, len(entries))
	for i, e := range entries {
		out[i] = e.Seq
	}
	return out
}

func equalSeqs(got []int64, want ...int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestActivityLogCursorSemantics(t *testing.T) {
	var l activityLog
	recordN(&l, 5)

	// Omitted cursor: the most recent max entries, cursor at the newest.
	got, next, dropped := l.after(-1, 3)
	if !equalSeqs(seqs(got), 3, 4, 5) || next != 5 || dropped != 0 {
		t.Fatalf("after(-1,3) = %v next=%d dropped=%d; want [3 4 5] next=5 dropped=0", seqs(got), next, dropped)
	}

	// A cursor returns strictly newer entries.
	got, next, _ = l.after(2, 10)
	if !equalSeqs(seqs(got), 3, 4, 5) || next != 5 {
		t.Fatalf("after(2,10) = %v next=%d; want [3 4 5] next=5", seqs(got), next)
	}

	// Caught up: nothing new, cursor unchanged.
	got, next, _ = l.after(5, 10)
	if len(got) != 0 || next != 5 {
		t.Fatalf("after(5,10) = %v next=%d; want [] next=5", seqs(got), next)
	}

	// A truncated read advances the cursor only past what it returned, so the
	// next poll resumes without skipping.
	got, next, _ = l.after(0, 2)
	if !equalSeqs(seqs(got), 1, 2) || next != 2 {
		t.Fatalf("after(0,2) = %v next=%d; want [1 2] next=2", seqs(got), next)
	}
	got, _, _ = l.after(next, 10)
	if !equalSeqs(seqs(got), 3, 4, 5) {
		t.Fatalf("resumed read = %v; want [3 4 5]", seqs(got))
	}
}

func TestActivityLogEmpty(t *testing.T) {
	var l activityLog
	got, next, dropped := l.after(-1, 10)
	if len(got) != 0 || next != 0 || dropped != 0 {
		t.Fatalf("empty log: got %v next=%d dropped=%d", seqs(got), next, dropped)
	}
}

func TestActivityLogEvictionReportsDropped(t *testing.T) {
	var l activityLog
	recordN(&l, oobLogCapacity+10)

	got, next, dropped := l.after(0, oobLogCapacity+100)
	if dropped != 10 {
		t.Fatalf("dropped = %d; want 10", dropped)
	}
	if len(got) != oobLogCapacity {
		t.Fatalf("held %d entries; want %d", len(got), oobLogCapacity)
	}
	if got[0].Seq != 11 || next != int64(oobLogCapacity+10) {
		t.Fatalf("oldest seq %d next %d; want 11 and %d", got[0].Seq, next, oobLogCapacity+10)
	}

	// Sequence numbers stay monotonic and contiguous across the wrap.
	for i, e := range got {
		if want := int64(11 + i); e.Seq != want {
			t.Fatalf("entry %d has seq %d; want %d", i, e.Seq, want)
		}
	}
}

// The log must record what was touched, never what it contained. A log that
// accumulates file contents is a secret store nobody asked for.
func TestLogTargetNeverRecordsContent(t *testing.T) {
	const secret = "hunter2-SUPER-SECRET"
	cases := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"file_write", "file_write", map[string]any{"path": "/etc/app.conf", "content": secret}, "/etc/app.conf"},
		{"file_edit", "file_edit", map[string]any{"path": "/etc/app.conf", "old_text": secret, "new_text": secret}, "/etc/app.conf"},
		{"file_patch", "file_patch", map[string]any{"path": "/etc/app.conf", "patch": secret}, "/etc/app.conf"},
		{"send_input", "send_input", map[string]any{"text": secret}, ""},
		{"send_keys", "send_keys", map[string]any{"name": secret}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := logTarget(tc.tool, tc.args)
			if got != tc.want {
				t.Fatalf("logTarget = %q; want %q", got, tc.want)
			}
			if strings.Contains(got, secret) {
				t.Fatalf("logTarget leaked content: %q", got)
			}
		})
	}
}

func TestLogTargetIdentifiesTheOperation(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"exec", map[string]any{"command": "systemctl restart nginx", "cwd": "/tmp"}, "systemctl restart nginx"},
		{"file_read", map[string]any{"path": "/var/log/syslog"}, "/var/log/syslog"},
		{"file_grep", map[string]any{"pattern": "TODO", "path": "/srv"}, `"TODO" in /srv`},
		{"file_search", map[string]any{"pattern": "*.go"}, `"*.go"`},
		{"file_upload", map[string]any{"local_path": "/a", "remote_path": "/b"}, "/a -> /b"},
		{"file_download", map[string]any{"local_path": "/a", "remote_path": "/b"}, "/b -> /a"},
		{"exec_status", map[string]any{"task_id": "t-1"}, "t-1"},
		{"read_screen", map[string]any{}, ""},
	}
	for _, tc := range cases {
		if got := logTarget(tc.tool, tc.args); got != tc.want {
			t.Errorf("logTarget(%s) = %q; want %q", tc.tool, got, tc.want)
		}
	}
}

func TestTrimFieldCapsAndFlattens(t *testing.T) {
	if got := trimField("a\nb"); got != "a b" {
		t.Fatalf("trimField newline = %q; want %q", got, "a b")
	}
	long := strings.Repeat("x", maxLogField+50)
	got := trimField(long)
	if len([]rune(got)) != maxLogField+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("trimField did not cap: len=%d", len([]rune(got)))
	}
}

// structuredResult builds what the SDK's typed-tool wrapper hands back: the
// output marshaled into StructuredContent as a json.RawMessage.
func structuredResult(t *testing.T, out any) *mcp.CallToolResult {
	t.Helper()
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return &mcp.CallToolResult{StructuredContent: json.RawMessage(b)}
}

func entryForTool(t *testing.T, tool string, args map[string]any, res *mcp.CallToolResult) activityEntry {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	c := &Core{}
	return c.entryFor(
		&mcp.CallToolRequest{},
		&mcp.CallToolParamsRaw{Name: tool, Arguments: raw},
		res, nil, time.Now(),
	)
}

func TestEntryClassifiesVisibility(t *testing.T) {
	cases := []struct {
		name        string
		tool        string
		result      any
		wantVia     string
		wantVisible bool
	}{
		{"persistent channel", "file_write", map[string]any{"via": "channel", "host": "web01"}, "channel", false},
		{"one-off master", "exec", map[string]any{"via": "controlmaster", "host": "web01"}, "controlmaster", false},
		{"sftp transport", "file_read", map[string]any{"via": "sftp", "host": "win01"}, "sftp", false},
		{"local session", "exec", map[string]any{"via": "local", "host": "local"}, "local", false},
		{"visible fallback", "file_read", map[string]any{"via": "in_band", "host": "web01"}, "in_band", true},
		{"probe opens a channel", "probe_host", map[string]any{"via": "controlmaster", "host": "web01"}, "controlmaster", false},
		{"terminal tool", "run_command", map[string]any{"output": "hi", "framing": "osc133"}, "terminal", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := entryForTool(t, tc.tool, map[string]any{}, structuredResult(t, tc.result))
			if e.Via != tc.wantVia || e.Visible != tc.wantVisible {
				t.Fatalf("via=%q visible=%v; want via=%q visible=%v", e.Via, e.Visible, tc.wantVia, tc.wantVisible)
			}
		})
	}
}

// session_status reports the route an operation WOULD take without taking it.
// Reading via off its result would file every status poll as out-of-band work.
func TestSessionStatusIsNotAnOperation(t *testing.T) {
	e := entryForTool(t, "session_status",
		map[string]any{},
		structuredResult(t, map[string]any{"via": "controlmaster", "host": "web01"}))
	if e.Via != "control" || !e.Visible {
		t.Fatalf("session_status logged as via=%q visible=%v; want control/visible", e.Via, e.Visible)
	}
}

func TestEntryCapturesOutcome(t *testing.T) {
	code := 3
	e := entryForTool(t, "exec",
		map[string]any{"command": "false"},
		structuredResult(t, map[string]any{"via": "channel", "host": "web01", "exit_code": code}))
	if e.ExitCode == nil || *e.ExitCode != 3 {
		t.Fatalf("exit code = %v; want 3", e.ExitCode)
	}
	if e.Host != "web01" || e.Target != "false" {
		t.Fatalf("host=%q target=%q", e.Host, e.Target)
	}

	e = entryForTool(t, "file_write",
		map[string]any{"path": "/tmp/x", "content": "secret"},
		structuredResult(t, map[string]any{"via": "sftp", "host": "win01", "bytes_written": 42, "warning": "mode ignored"}))
	if e.Bytes == nil || *e.Bytes != 42 {
		t.Fatalf("bytes = %v; want 42", e.Bytes)
	}
	if e.Warning != "mode ignored" {
		t.Fatalf("warning = %q", e.Warning)
	}
}

// Refused and failed operations are exactly the ones worth auditing, and the
// SDK delivers them as IsError results rather than Go errors.
//
// A refusal reports no route, and it must NOT be filed as visible on that
// basis: an attempted out-of-band sudo is the motivating case for this log, and
// the human's default view is the invisible one. Fail loud.
func TestEntryRecordsRefusals(t *testing.T) {
	res := &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: "sudo is refused on the out-of-band route"}},
	}
	e := entryForTool(t, "exec", map[string]any{"command": "sudo rm -rf /"}, res)
	if !strings.Contains(e.Error, "sudo is refused") {
		t.Fatalf("error not recorded: %q", e.Error)
	}
	if e.Target != "sudo rm -rf /" {
		t.Fatalf("target = %q; want the full command line", e.Target)
	}
	if e.Via != "unresolved" || e.Visible {
		t.Fatalf("refused exec logged as via=%q visible=%v; want unresolved and surfaced", e.Via, e.Visible)
	}
}

// A terminal tool that fails is genuinely visible — the human watched it fail.
func TestFailedTerminalToolStaysVisible(t *testing.T) {
	res := &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: "refusing to run while the terminal is reading a password"}},
	}
	e := entryForTool(t, "run_command", map[string]any{"command": "ls"}, res)
	if e.Via != "terminal" || !e.Visible {
		t.Fatalf("failed run_command logged as via=%q visible=%v; want terminal and visible", e.Via, e.Visible)
	}
}

func TestSkipLoggingCoversAuthAndSelf(t *testing.T) {
	for tool := range authproto.InternalTools {
		if !skipLogging(tool) {
			t.Errorf("auth tool %q must not be logged", tool)
		}
	}
	if !skipLogging("oob_log") {
		t.Error("oob_log must not log its own reads")
	}
	if skipLogging("file_write") {
		t.Error("file_write must be logged")
	}
}

func TestRecentActivityRendersInvisibleOnly(t *testing.T) {
	c := &Core{}
	code := 0
	c.Activity.record(activityEntry{Tool: "run_command", Via: "terminal", Visible: true, Client: "claude"})
	c.Activity.record(activityEntry{Tool: "file_write", Via: "channel", Host: "web01",
		Target: "/etc/app.conf", Client: "claude", Bytes: ptrInt64(1204)})
	c.Activity.record(activityEntry{Tool: "file_read", Via: "local", Host: "local",
		Target: "/tmp/x", Client: "claude"})
	c.Activity.record(activityEntry{Tool: "exec", Via: "channel", Host: "web01",
		Target: "systemctl restart nginx", Client: "codex", ExitCode: &code})

	lines := c.RecentActivity(10)
	if len(lines) != 3 {
		t.Fatalf("got %d lines; want 3 (visible entries excluded): %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "file_write") || !strings.Contains(lines[0], "1204 bytes") {
		t.Fatalf("first line = %q", lines[0])
	}
	if !strings.Contains(lines[2], "codex") || !strings.Contains(lines[2], "exit 0") {
		t.Fatalf("last line = %q", lines[2])
	}
	// A local route on a local host should read "local", not "local  local".
	if strings.Contains(lines[1], "local  local") {
		t.Fatalf("route and host duplicated: %q", lines[1])
	}
}

func ptrInt64(v int64) *int64 { return &v }

// ---- end-to-end through the real middleware chain ----

func TestOOBLogRecordsCallsWithClientAttribution(t *testing.T) {
	ts := startTestServer(t, true, nil)
	cs := connectTestClient(t, ts.socket, "test-client")

	callTool(t, cs, "set_session_name", map[string]any{"name": "renamed"})

	// The default view hides visible operations — the human already saw them.
	res := callTool(t, cs, "oob_log", map[string]any{})
	out := decodeResult[oobLogResult](t, res)
	if len(out.Entries) != 0 {
		t.Fatalf("default view returned visible entries: %+v", out.Entries)
	}
	if out.NextCursor != 1 {
		t.Fatalf("next_cursor = %d; want 1 (the filtered entry was still examined)", out.NextCursor)
	}

	res = callTool(t, cs, "oob_log", map[string]any{"include_visible": true})
	out = decodeResult[oobLogResult](t, res)
	if len(out.Entries) != 1 {
		t.Fatalf("got %d entries; want 1: %+v", len(out.Entries), out.Entries)
	}
	e := out.Entries[0]
	if e.Tool != "set_session_name" || e.Client != "test-client" || e.Via != "control" {
		t.Fatalf("entry = %+v", e)
	}
	if e.Seq != 1 || e.Time == "" {
		t.Fatalf("entry missing sequence or time: %+v", e)
	}
}

func TestOOBLogRecordsFailures(t *testing.T) {
	ts := startTestServer(t, true, nil)
	cs := connectTestClient(t, ts.socket, "test-client")

	res := callTool(t, cs, "set_session_name", map[string]any{"name": "not a valid name!"})
	if !res.IsError {
		t.Fatal("expected the invalid name to be refused")
	}

	out := decodeResult[oobLogResult](t, callTool(t, cs, "oob_log", map[string]any{"include_visible": true}))
	if len(out.Entries) != 1 {
		t.Fatalf("got %d entries; want 1", len(out.Entries))
	}
	if !strings.Contains(out.Entries[0].Error, "invalid name") {
		t.Fatalf("failure not recorded: %+v", out.Entries[0])
	}
}

// The log must not record the private authentication handshake (it carries
// keys, nonces and signatures, and runs before the client has an identity) or
// its own reads (a polling client would crowd out what it polls for).
func TestOOBLogSkipsAuthHandshakeAndItself(t *testing.T) {
	approve := func(string, string, time.Duration) (byte, bool) { return 'y', true }
	ts := startTestServer(t, false, approve)
	cs := connectTestClient(t, ts.socket, "test-client")

	identity, err := clientauth.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.Authorize(context.Background(), cs, "testsession", "test client"); err != nil {
		t.Fatal(err)
	}

	callTool(t, cs, "oob_log", map[string]any{})
	callTool(t, cs, "oob_log", map[string]any{})

	out := decodeResult[oobLogResult](t, callTool(t, cs, "oob_log", map[string]any{"include_visible": true}))
	if len(out.Entries) != 0 || out.NextCursor != 0 {
		t.Fatalf("handshake or self-reads were logged: %+v (cursor %d)", out.Entries, out.NextCursor)
	}
}
