package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/authproto"
)

// The out-of-band activity log closes the one real hole in aish's transparency
// model. The shared terminal shows the human everything that happens in it, and
// the consent gate (--oob, the y/n/a prompt in route()) governs WHETHER
// invisible work may happen at all — but once granted, nothing recorded WHAT
// happened. read_screen and read_output cannot help by definition: out-of-band
// operations never touch the terminal.
//
// This is an audit *trail*, not a tamper-evident audit *log*. It records what
// was asked of the MCP server and what came back — not ground truth on the
// host. A buggy or bypassed code path could act without appearing here. That is
// the same status as the privilege-escalation guardrail and the tool
// annotations, and it must never be described as stronger than it is.
//
// It doubles as a coordination channel: two assistants driving one session (a
// supported and practiced setup) can each see what the other touched, so one
// can avoid clobbering a file the other wrote thirty seconds ago.

// oobLogCapacity bounds the ring. Memory-only and bounded, consistent with
// client grants — a session's audit trail must not become an unbounded record
// of everything an AI ever did, and it must not survive the session.
const oobLogCapacity = 500

// maxLogField caps every recorded string. Exec command lines are the one thing
// worth keeping in full (they are the point of the log), but even those stop
// somewhere.
const maxLogField = 1024

// activityEntry is one recorded tool call.
type activityEntry struct {
	Seq      int64
	At       time.Time
	Client   string // MCP clientInfo name (what the grant binds to)
	Peer     string // kernel-verified peer process, empty when unavailable
	Tool     string
	Target   string // the identifying argument: path, command, pattern
	Via      string // channel | sftp | local | in_band | terminal | control
	Host     string
	Visible  bool   // true when the operation was visible in the shared terminal
	Effect   string // "acted" | "read": whether the call changed anything or only looked
	Error    string
	ExitCode *int
	Bytes    *int64
	Warning  string
	Duration time.Duration
}

// activityLog is a bounded ring with monotonic sequence numbers, read through
// a cursor. The shape deliberately mirrors read_output: "what has the other
// client been doing" is then an incremental poll rather than a re-dump, using
// an idiom the tool API already establishes.
//
// The zero value is ready to use; the ring allocates on first record.
type activityLog struct {
	mu    sync.Mutex
	buf   []activityEntry
	start int   // index of the oldest held entry
	n     int   // entries held
	seq   int64 // sequence of the newest entry; entries are numbered from 1
}

// record assigns the next sequence number and stores the entry, evicting the
// oldest when the ring is full.
func (l *activityLog) record(e activityEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buf == nil {
		l.buf = make([]activityEntry, oobLogCapacity)
	}
	l.seq++
	e.Seq = l.seq
	if l.n < len(l.buf) {
		l.buf[(l.start+l.n)%len(l.buf)] = e
		l.n++
		return
	}
	l.buf[l.start] = e
	l.start = (l.start + 1) % len(l.buf)
}

// after returns up to max entries with a sequence greater than since, oldest
// first. A negative since means "the most recent max entries" (the first-call
// case, matching read_output's omitted cursor).
//
// next is the cursor to pass back: the highest sequence *examined*, so a caller
// filtering the result still advances past entries it discarded. dropped counts
// entries that were evicted from the ring before the caller could read them,
// the analogue of read_output's dropped_bytes.
func (l *activityLog) after(since int64, max int) (out []activityEntry, next, dropped int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if max <= 0 || l.n == 0 {
		return nil, l.seq, 0
	}
	oldest := l.seq - int64(l.n) + 1
	from := since + 1
	switch {
	case since < 0:
		if from = l.seq - int64(max) + 1; from < oldest {
			from = oldest
		}
	case from < oldest:
		dropped = oldest - from
		from = oldest
	}
	if from > l.seq {
		return nil, l.seq, dropped
	}
	count := int(l.seq - from + 1)
	if count > max {
		count = max
	}
	next = from + int64(count) - 1
	out = make([]activityEntry, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, l.buf[(l.start+int(from-oldest)+i)%len(l.buf)])
	}
	return out, next, dropped
}

// invisibleRoutes are the via values naming work that happened OUTSIDE the
// shared terminal. Everything else the human already saw.
var invisibleRoutes = map[string]bool{
	"channel":       true, // the persistent OOB `sh -s`
	"controlmaster": true, // a one-off ControlMaster slave
	"sftp":          true, // the retained SFTP client
	"local":         true, // direct local fs/exec on a local session
}

// readOnlyOps only look. Recording that distinction matters because the log's
// own labels invite a wrong reading without it: read_screen is filed as
// via:"terminal", visible:true, which an evaluation agent initially took to
// mean the read had somehow written to the terminal. Reviewing a list of
// operations, "did this change anything" is usually the first question, and it
// was previously answerable only by recognising each tool by name.
var readOnlyOps = map[string]bool{
	"read_screen":    true,
	"read_output":    true,
	"wait_idle":      true,
	"file_read":      true,
	"file_stat":      true,
	"file_grep":      true,
	"file_search":    true,
	"directory_list": true,
	"file_download":  true, // writes locally, reads the session's host
	"oob_log":        true,
	"probe_host":     true,
	"session_status": true,
}

// effectOf classifies one call for the log.
func effectOf(tool string) string {
	if readOnlyOps[tool] {
		return "read"
	}
	return "acted"
}

// controlTools act on neither a host nor the terminal. session_status needs the
// exception specifically because it REPORTS a route — the one an operation
// would take — without taking it; reading via off its result would file a pure
// cache read as an out-of-band operation.
var controlTools = map[string]bool{
	"session_status":   true,
	"set_session_name": true,
}

// terminalTools drive the shared terminal itself, so they have no route to
// report and the human saw them happen.
//
// Naming them explicitly is what makes failures safe. A ROUTED tool that fails
// before resolving its route also returns no via, and treating "no via" as
// "visible" would hide the most audit-worthy events there are: a refused
// out-of-band sudo — the case this log was built for — would never reach the
// default view. Anything absent from this list that reports no route is
// recorded as unresolved and surfaces, so a tool added later fails loud rather
// than silently invisible.
var terminalTools = map[string]bool{
	"run_command": true,
	"send_input":  true,
	"send_keys":   true,
	"read_screen": true,
	"read_output": true,
	"wait_idle":   true,
}

// skipLogging keeps two kinds of noise out. The private authentication tools
// carry public keys, nonces and signatures, and run BEFORE the connection has
// an identity to attribute them to. oob_log reads the log: recording those
// reads would let a polling client crowd out the operations it is polling for.
func skipLogging(tool string) bool {
	return authproto.InternalTools[tool] || tool == "oob_log"
}

// oobLogMiddleware records every tool call.
//
// It is registered as the INNERMOST receiving middleware, which settles three
// ordering requirements at once. Inside connAuthMiddleware, so the client's
// identity is already resolved and every entry can be attributed. Inside
// crossSession, so a call forwarded to another session is recorded once, at the
// session that actually executes it, instead of twice. And innermost overall,
// so it holds the final structured result and can read the route off the `via`
// field every routed handler already returns.
//
// A call rejected at the authorization gate never reaches here and is not
// logged: it executed nothing, and the human already saw it — they answered the
// approval prompt themselves.
//
// One middleware rather than a logOOB call in each of a dozen handlers is the
// point: an audit feature that can be forgotten at a call site is worth much
// less than one that cannot, and a tool added later is covered by default.
func oobLogMiddleware(c *Core) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if !ok || skipLogging(params.Name) {
				return next(ctx, method, req)
			}
			started := time.Now()
			res, err := next(ctx, method, req)
			c.Activity.record(c.entryFor(req, params, res, err, started))
			return res, err
		}
	}
}

// logResultFields are the outcome fields worth keeping, read from whatever the
// handler returned. Every routed result carries via and host; the rest are
// per-tool and simply absent when a tool doesn't report them.
type logResultFields struct {
	Via          string `json:"via"`
	Host         string `json:"host"`
	ExitCode     *int   `json:"exit_code"`
	Bytes        *int64 `json:"bytes"`
	BytesWritten *int64 `json:"bytes_written"`
	Warning      string `json:"warning"`
}

// entryFor builds the record for one completed call.
func (c *Core) entryFor(req mcp.Request, params *mcp.CallToolParamsRaw, res mcp.Result, err error, started time.Time) activityEntry {
	e := activityEntry{
		At:       started,
		Tool:     params.Name,
		Client:   "an MCP client",
		Duration: time.Since(started),
	}
	if ss, ok := req.GetSession().(*mcp.ServerSession); ok && ss != nil {
		e.Client = clientName(ss)
		st := c.connState(ss)
		st.mu.Lock()
		e.Peer = st.peer.String()
		st.mu.Unlock()
	}

	var args map[string]any
	if len(params.Arguments) > 0 {
		_ = json.Unmarshal(params.Arguments, &args)
	}
	e.Target = logTarget(params.Name, args)

	var f logResultFields
	if ctr, ok := res.(*mcp.CallToolResult); ok && ctr != nil {
		if raw, ok := ctr.StructuredContent.(json.RawMessage); ok {
			_ = json.Unmarshal(raw, &f)
		}
		e.Error = resultErrorText(ctr)
	}
	if err != nil {
		e.Error = trimField(err.Error())
	}

	e.Host = trimField(f.Host)
	e.ExitCode = f.ExitCode
	e.Bytes = f.Bytes
	if e.Bytes == nil {
		e.Bytes = f.BytesWritten
	}
	e.Warning = trimField(f.Warning)

	switch {
	case controlTools[params.Name]:
		e.Via = "control"
	case f.Via != "":
		e.Via = f.Via
	case terminalTools[params.Name]:
		e.Via = "terminal"
	default:
		// A routed tool that reported no route — it was refused or failed
		// before route() resolved. Never assume the human saw it.
		e.Via = "unresolved"
	}
	e.Visible = e.Via != "unresolved" && !invisibleRoutes[e.Via]
	e.Effect = effectOf(e.Tool)
	return e
}

// logTarget renders the identifying argument of a call: the path acted on, the
// command run, the pattern searched for.
//
// It is an explicit allowlist of keys, and that is the security property. File
// CONTENT — content, text, old_text, new_text, patch — is never recorded. An
// in-memory record of every file the AI touched is useful; an in-memory copy of
// what those files CONTAIN is a secret store nobody asked for, and a log of
// send_input text would be a keystroke record of whatever the AI typed.
func logTarget(tool string, args map[string]any) string {
	s := func(k string) string { v, _ := args[k].(string); return v }
	if cmd := s("command"); cmd != "" {
		return trimField(cmd)
	}
	if local, remote := s("local_path"), s("remote_path"); local != "" || remote != "" {
		if tool == "file_download" {
			return trimField(remote + " -> " + local)
		}
		return trimField(local + " -> " + remote)
	}
	if pattern := s("pattern"); pattern != "" {
		if path := s("path"); path != "" {
			return trimField(fmt.Sprintf("%q in %s", pattern, path))
		}
		return trimField(fmt.Sprintf("%q", pattern))
	}
	if path := s("path"); path != "" {
		return trimField(path)
	}
	return trimField(s("task_id"))
}

// resultErrorText recovers the message from a tool-level error result. Handlers
// return ordinary errors, which the SDK's typed-tool wrapper converts into a
// CallToolResult with IsError set — inside this middleware — so refused and
// failed operations arrive here as results, not as errors.
func resultErrorText(ctr *mcp.CallToolResult) string {
	if !ctr.IsError {
		return ""
	}
	var parts []string
	for _, content := range ctr.Content {
		if tc, ok := content.(*mcp.TextContent); ok && tc.Text != "" {
			parts = append(parts, tc.Text)
		}
	}
	return trimField(strings.Join(parts, " "))
}

func trimField(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > maxLogField {
		return s[:maxLogField] + "…"
	}
	return s
}

// ---- oob_log ----

type oobLogArgs struct {
	SessionArg
	Cursor         *int64 `json:"cursor,omitempty" jsonschema:"return only entries after this sequence number (pass back next_cursor); omit for the most recent entries"`
	MaxEntries     int    `json:"max_entries,omitempty" jsonschema:"maximum entries to return (default 50)"`
	IncludeVisible bool   `json:"include_visible,omitempty" jsonschema:"also return operations that were visible in the shared terminal; by default only invisible out-of-band operations are returned"`
}

type oobLogEntry struct {
	Seq        int64  `json:"seq"`
	Time       string `json:"time"`
	Client     string `json:"client"`
	Peer       string `json:"peer,omitempty"`
	Tool       string `json:"tool"`
	Target     string `json:"target,omitempty"`
	Via        string `json:"via"`
	Host       string `json:"host,omitempty"`
	Visible    bool   `json:"visible"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Bytes      *int64 `json:"bytes,omitempty"`
	Warning    string `json:"warning,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

type oobLogResult struct {
	Entries        []oobLogEntry `json:"entries"`
	NextCursor     int64         `json:"next_cursor"`
	DroppedEntries int64         `json:"dropped_entries"`
}

func registerActivityTools(s *mcp.Server, c *Core) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "oob_log",
		Annotations: readOnlyTool("Read out-of-band activity"),
		Description: "Read this session's out-of-band activity log: which client ran which tool, against which host and route, " +
			"and how it turned out. Out-of-band operations never touch the shared terminal, so read_screen and read_output " +
			"cannot show them — this is the record of what happened invisibly. Incremental like read_output: pass the previous " +
			"next_cursor to get only new entries, omit cursor for the most recent ones. By default it returns only invisible " +
			"operations (visible ones are already on the human's screen); pass include_visible for the full call history. " +
			"Useful when another AI client shares this session — check what it already did before acting, so you don't clobber " +
			"its work. File contents are never recorded, only paths, commands, sizes and outcomes. The log is memory-only, " +
			"bounded, and records what was asked of aish rather than ground truth on the host.",
	}, c.oobLogTool)
}

func (c *Core) oobLogTool(ctx context.Context, req *mcp.CallToolRequest, args oobLogArgs) (*mcp.CallToolResult, oobLogResult, error) {
	max := args.MaxEntries
	if max <= 0 {
		max = 50
	}
	cursor := int64(-1)
	if args.Cursor != nil {
		cursor = *args.Cursor
	}
	entries, next, dropped := c.Activity.after(cursor, max)
	out := oobLogResult{Entries: []oobLogEntry{}, NextCursor: next, DroppedEntries: dropped}
	for _, e := range entries {
		if e.Visible && !args.IncludeVisible {
			continue
		}
		out.Entries = append(out.Entries, oobLogEntry{
			Seq:        e.Seq,
			Time:       e.At.Format(time.RFC3339),
			Client:     e.Client,
			Peer:       e.Peer,
			Tool:       e.Tool,
			Target:     e.Target,
			Via:        e.Via,
			Host:       e.Host,
			Visible:    e.Visible,
			ExitCode:   e.ExitCode,
			Bytes:      e.Bytes,
			Warning:    e.Warning,
			Error:      e.Error,
			DurationMs: e.Duration.Milliseconds(),
		})
	}
	return nil, out, nil
}

// RecentActivity renders the most recent invisible operations as display lines
// for the Ctrl-] menu. Exported for cmd/aish.
//
// Pull, not push: a Notify per operation would be the wrong shape — the
// terminal is the human's, and the PSK "recognized client" notice already shows
// how quickly per-event notices become noise. The human asks when they want to
// know.
func (c *Core) RecentActivity(n int) []string {
	entries, _, dropped := c.Activity.after(-1, oobLogCapacity)
	lines := []string{}
	for i := len(entries) - 1; i >= 0 && len(lines) < n; i-- {
		if entries[i].Visible {
			continue
		}
		lines = append(lines, formatActivity(entries[i]))
	}
	// Newest-last reads naturally under a heading.
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	if dropped > 0 {
		lines = append([]string{fmt.Sprintf("(%d older entries dropped)", dropped)}, lines...)
	}
	return lines
}

// formatActivity renders one entry for the console, e.g.
//
//	14:03:21 claude-code  file_write  channel web01  /etc/nginx.conf  1204 bytes
func formatActivity(e activityEntry) string {
	parts := []string{e.At.Format("15:04:05"), e.Client, e.Tool, e.Via}
	if e.Host != "" && e.Host != e.Via {
		parts = append(parts, e.Host)
	}
	if e.Target != "" {
		parts = append(parts, truncate(e.Target, 60))
	}
	if e.Effect == "read" {
		parts = append(parts, "(read-only)")
	}
	switch {
	case e.Error != "":
		parts = append(parts, "FAILED: "+truncate(e.Error, 60))
	case e.ExitCode != nil:
		parts = append(parts, fmt.Sprintf("exit %d", *e.ExitCode))
	case e.Bytes != nil:
		parts = append(parts, fmt.Sprintf("%d bytes", *e.Bytes))
	}
	return strings.Join(parts, "  ")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
