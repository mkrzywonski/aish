// The aggregating proxy: a single, durable MCP server that Claude Code (or
// any MCP client) talks to, which itself is a client to the individual aish
// session servers. Its lifetime is the AI TUI's lifetime — it never depends
// on any session existing, so sessions can come and go underneath it and the
// AI never has to reconnect.
//
// Routing: every session tool carries a `session` argument (id or name). The
// proxy resolves it, holds ONE authorized connection per session (opened
// lazily on first use, kept alive), and forwards the call there. Because the
// per-session y/n approval lives in the session server and fires on the first
// tool call of a fresh connection, the user is prompted exactly once per
// session per TUI lifetime — on that session's own terminal, which is the
// positive identification of the target. Switching back to an
// already-approved session reuses its connection: no prompt. Closing the
// session (or the TUI) drops the connection and clears the approval.
//
// list_sessions is answered by the proxy directly (no session connection, no
// prompt) so the AI can always enumerate what's live.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/authproto"
	"ai-ssh/internal/clientauth"
	"ai-ssh/internal/procinfo"
)

type aggProxy struct {
	client   *mcp.Client
	identity *clientauth.Identity
	version  string
	hasPSK   bool
	frontSS  *mcp.ServerSession // the connection to the AI client, for log notices

	mu         sync.Mutex
	identified bool                   // downstream client renamed to match the upstream TUI
	conns      map[string]*pooledConn // by session id
	lastNames  map[string]string      // last-seen name per session id, for rename detection
}

type pooledConn struct {
	raw net.Conn
	cs  *mcp.ClientSession
}

const serverInstructions = "Aish gives you access to human-owned shared terminal sessions and to the current host " +
	"inside each session, including a remote host reached by SSH. Your native shell and filesystem tools " +
	"remain local: when the user refers to an aish/shared terminal, its current host, or a remote host they " +
	"SSH'd into there, use aish tools instead. Start with list_sessions, select the session, then " +
	"call session_status; recheck after SSH transitions. New SSH hosts have `unknown` OOB tools; call probe_host once, " +
	"then always plan against oob_tools. `screen` remote_dialect_source is advisory, never implies POSIX, and " +
	"never disables a tool. " +
	"If probe evidence makes oob_tools " +
	"unavailable, do not re-probe; use run_command instead. Deep identity probing is diagnostic, " +
	"explicit, and may trigger MFA; never use instead of oob_tools. Explicit SFTP may trigger MFA; a sticky shell " +
	"failure permits merged SFTP-backed oob_tools. Every " +
	"session tool accepts `session` (id or name). Use run_command for commands the human should see. Use " +
	"exec, file_*, and directory_list for native-like work on the session's current host when OOB is " +
	"authorized. Out-of-band work is invisible; oob_log records it — read it when another client shares " +
	"the session or the user asks what happened off-screen. " +
	"Out-of-band tools act as session_status.oob_user (the SSH login user), which does NOT change " +
	"when the human switches user via su or sudo -i; check oob_user before ownership- or privilege-sensitive " +
	"work, and if their shell switched users say so and prefer run_command (it runs as the shared shell's " +
	"current user). sudo, su and other escalation must go through run_command, never exec: escalating out of " +
	"band is refused, because a privileged command has to be one the human saw, and they type their own " +
	"password. Never send passwords or other " +
	"secrets; if echo_off is true, wait for the human. Name the target session and host in chat before the " +
	"first substantial or destructive op. The user approves each session on its own terminal."

// Serve runs the aggregating proxy over stdio until the client disconnects.
// If psk is non-nil, the proxy derives a deterministic identity from it so
// sessions can recognize reconnects without re-prompting.
func Serve(version string, psk []byte) int {
	var identity *clientauth.Identity
	var err error
	if len(psk) > 0 {
		identity, err = clientauth.FromPSK(psk)
		if err != nil {
			fmt.Fprintln(os.Stderr, "aish mcp-proxy: deriving identity from PSK:", err)
			return 1
		}
	} else {
		identity, err = clientauth.New()
		if err != nil {
			fmt.Fprintln(os.Stderr, "aish mcp-proxy: generating client identity:", err)
			return 1
		}
	}
	p := &aggProxy{
		client:    mcp.NewClient(&mcp.Implementation{Name: "aish-proxy", Version: version}, nil),
		identity:  identity,
		version:   version,
		hasPSK:    len(psk) > 0,
		conns:     map[string]*pooledConn{},
		lastNames: map[string]string{},
	}
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{Name: "aish", Version: version}, &mcp.ServerOptions{
		Instructions: serverInstructions,
	})

	// list_sessions: answered locally, never gated.
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_sessions",
		Description: "List the live aish sessions on this machine (id and name). Use a session's id or name as the `session` argument to other tools. Safe to call anytime; never prompts the user.",
		Annotations: &mcp.ToolAnnotations{Title: "List aish sessions", ReadOnlyHint: true},
	}, p.listSessions)

	// version_info: answered locally, reports proxy + session versions.
	mcp.AddTool(server, &mcp.Tool{
		Name:        "version_info",
		Description: "Report version information for all components in the aish chain: the MCP proxy, the connected session server, and whether PSK authentication is active. Useful for diagnosing version mismatches.",
		Annotations: &mcp.ToolAnnotations{Title: "Version info", ReadOnlyHint: true},
	}, p.versionInfo)

	// Mirror the session tools with a generic forwarding handler.
	specs, err := p.toolSpecs(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aish mcp-proxy:", err)
		return 1
	}
	for _, t := range specs {
		name := t.Name
		server.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return p.forward(ctx, name, req)
		})
	}

	ss, err := server.Connect(ctx, &mcp.IOTransport{Reader: os.Stdin, Writer: os.Stdout}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aish mcp-proxy:", err)
		return 1
	}
	p.frontSS = ss
	ss.Wait()
	p.closeAll()
	return 0
}

// forward routes a tool call to the session named by its `session` argument
// (or the sole live session), stripping the argument before forwarding.
func (p *aggProxy) forward(ctx context.Context, tool string, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args map[string]any
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return toolError("bad arguments: %v", err), nil
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	target, _ := args["session"].(string)
	delete(args, "session")

	// One session snapshot for the whole call: resolve the target and detect
	// renames from the same list (each List() does a readdir plus a per-session
	// socket ping and name read, so we don't want it twice per forward).
	live := List()

	info, err := p.resolve(target, live)
	if err != nil {
		// Annotate so a lookup that failed *because* of a rename carries the
		// explanation, not just "no such session".
		return p.annotate(ctx, toolError("%v", err), live), nil
	}

	cs, err := p.conn(ctx, info)
	if err != nil {
		return p.annotate(ctx, toolError("connecting to session %s: %v", info.Label(), err), live), nil
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		// Only a dead connection warrants dropping the pooled session. A
		// tool-level refusal — an unknown tool name, a rejected argument —
		// arrives over a perfectly healthy connection, and evicting on it
		// forced a fresh dial and a full re-authorization on the very next
		// call. Sessions differ in which tools they implement, so "unknown
		// tool" is an ordinary answer here, not evidence of a broken link.
		if isTransportError(err) {
			p.drop(info.ID)
		}
		return p.annotate(ctx, toolError("session %s: %v", info.Label(), err), live), nil
	}
	return p.annotate(ctx, res, live), nil
}

// renameNotices reports sessions whose name changed since the proxy last
// observed them, and refreshes the record. This is how the AI learns a
// session was renamed out from under the name it's been using.
func (p *aggProxy) renameNotices(live []SessionInfo) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var notices []string
	cur := make(map[string]string, len(live))
	for _, s := range live {
		cur[s.ID] = s.Name
		if old, seen := p.lastNames[s.ID]; seen && old != s.Name {
			notices = append(notices, fmt.Sprintf("session %s was renamed %s → %q", s.ID, quoteName(old), s.Name))
		}
	}
	p.lastNames = cur
	return notices
}

func quoteName(n string) string {
	if n == "" {
		return "(unnamed)"
	}
	return fmt.Sprintf("%q", n)
}

// annotate prepends a rename notice to a result (the channel the model
// reliably sees) and also emits a logging notification.
func (p *aggProxy) annotate(ctx context.Context, res *mcp.CallToolResult, live []SessionInfo) *mcp.CallToolResult {
	notices := p.renameNotices(live)
	if len(notices) == 0 {
		return res
	}
	msg := "aish notice — " + strings.Join(notices, "; ") + ". Names may have moved between sessions; call list_sessions to reorient before acting."
	if res == nil {
		res = &mcp.CallToolResult{}
	}
	res.Content = append([]mcp.Content{&mcp.TextContent{Text: msg}}, res.Content...)
	if p.frontSS != nil {
		_ = p.frontSS.Log(ctx, &mcp.LoggingMessageParams{Level: "warning", Logger: "aish", Data: msg})
	}
	return res
}

// resolve picks the target session: the named one, or the sole live session
// when unnamed. Ambiguity is an error — never a guess.
func (p *aggProxy) resolve(target string, live []SessionInfo) (SessionInfo, error) {
	if len(live) == 0 {
		return SessionInfo{}, errors.New("no aish sessions are running; start one with `aish`")
	}
	if target == "" {
		if len(live) == 1 {
			return live[0], nil
		}
		return SessionInfo{}, fmt.Errorf("several sessions are live (%s); name one in the `session` argument", labels(live))
	}
	return Resolve(target, live)
}

// conn returns the pooled connection for a session, opening, MCP-handshaking,
// and authorizing it on first use. The proxy keeps its private key and grants
// in memory, so reconnects prove possession without prompting again.
func (p *aggProxy) conn(ctx context.Context, info SessionInfo) (*mcp.ClientSession, error) {
	p.mu.Lock()
	// Adopt the upstream TUI's identity (claude/codex) for downstream
	// connections the first time we open one, so the session's approval prompt
	// names the real client instead of "aish-proxy". Cheap and local, so it's
	// safe under the lock; the upstream initialize handshake has completed by
	// the first tool call.
	if !p.identified {
		// Only latch once a real name is applied; if the upstream identity
		// isn't known yet (empty name), leave it unlatched so a later conn()
		// retries rather than pinning the generic "aish-proxy" for good.
		if name := friendlyClientName(p.frontSS); name != "" {
			p.client = mcp.NewClient(&mcp.Implementation{Name: name, Version: p.version}, nil)
			p.identified = true
		}
	}
	if pc := p.conns[info.ID]; pc != nil {
		p.mu.Unlock()
		return pc.cs, nil
	}
	client := p.client
	p.mu.Unlock()

	// Dial, MCP-handshake, and authorize WITHOUT holding p.mu. Authorize runs
	// the approval handshake, which on first access blocks on the target's y/n
	// terminal prompt for up to defaultApprovalTimeout (120s); holding the pool
	// lock across it would freeze every routed call to every other session
	// (including list_sessions) until that unrelated approval resolves.
	raw, err := net.Dial("unix", info.Sock)
	if err != nil {
		return nil, err
	}
	cs, err := client.Connect(ctx, &mcp.IOTransport{Reader: raw, Writer: raw}, nil)
	if err != nil {
		raw.Close()
		return nil, err
	}
	if err := p.identity.Authorize(ctx, cs, info.ID, p.clientDescription()); err != nil {
		cs.Close()
		raw.Close()
		return nil, err
	}

	p.mu.Lock()
	// A concurrent first-touch call for the same session (e.g. parallel tool
	// calls naming a not-yet-connected session) may have pooled a connection
	// while we were authorizing. If so, keep theirs and discard ours rather
	// than leaking a second connection.
	if existing := p.conns[info.ID]; existing != nil {
		p.mu.Unlock()
		cs.Close()
		raw.Close()
		return existing.cs, nil
	}
	p.conns[info.ID] = &pooledConn{raw: raw, cs: cs}
	p.mu.Unlock()
	return cs, nil
}

// friendlyClientName maps the upstream MCP client's self-reported name to a
// short label for the session's approval prompt: "claude" or "codex" for the
// known TUIs, otherwise the raw name (e.g. a custom client). Returns "" when
// the upstream identity isn't known yet.
func friendlyClientName(ss *mcp.ServerSession) string {
	if ss == nil {
		return ""
	}
	ip := ss.InitializeParams()
	if ip == nil || ip.ClientInfo == nil {
		return ""
	}
	name := ip.ClientInfo.Name
	switch {
	case strings.Contains(strings.ToLower(name), "claude"):
		return "claude"
	case strings.Contains(strings.ToLower(name), "codex"):
		return "codex"
	default:
		return name
	}
}

// clientDescription is the self-declared identity the proxy sends downstream to
// a session's approval prompt. It combines the upstream TUI's self-reported
// name (claude/codex/…) with the process that actually launched this proxy
// (getppid → /proc), which the proxy observes directly rather than trusting a
// wire value — so a session sees, e.g., "gemini (via aish proxy launched by
// antigravity)" instead of a bare "aish-proxy".
func (p *aggProxy) clientDescription() string {
	name := friendlyClientName(p.frontSS)
	parent := procinfo.Name(os.Getppid())
	switch {
	case name != "" && parent != "":
		return name + " (via aish proxy launched by " + parent + ")"
	case name != "":
		return name + " (via aish proxy)"
	case parent != "":
		return "aish proxy (launched by " + parent + ")"
	default:
		return "aish proxy"
	}
}

// isTransportError reports whether a failed call means the connection itself
// is unusable, as opposed to the session having answered with a refusal. Only
// the former justifies tearing a pooled connection down; a timeout counts,
// because a late response would desynchronize the stream.
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}

func (p *aggProxy) drop(id string) {
	p.mu.Lock()
	pc := p.conns[id]
	delete(p.conns, id)
	p.mu.Unlock()
	if pc != nil {
		pc.cs.Close()
		pc.raw.Close()
	}
}

func (p *aggProxy) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pc := range p.conns {
		pc.cs.Close()
		pc.raw.Close()
	}
	p.conns = map[string]*pooledConn{}
}

// ---- list_sessions ----

type listSessionsArgs struct{}

type sessionEntry struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type listSessionsResult struct {
	Sessions []sessionEntry `json:"sessions"`
}

func (p *aggProxy) listSessions(ctx context.Context, req *mcp.CallToolRequest, args listSessionsArgs) (*mcp.CallToolResult, listSessionsResult, error) {
	var out listSessionsResult
	live := List()
	for _, s := range live {
		out.Sessions = append(out.Sessions, sessionEntry{ID: s.ID, Name: s.Name})
	}
	// Refresh the rename baseline so list_sessions establishes ground truth
	// without also flagging its own results (the AI is already looking here).
	p.renameNotices(live)
	return nil, out, nil
}

// ---- version_info ----

type versionInfoArgs struct{}

type versionInfoResult struct {
	Proxy   proxyInfo    `json:"proxy"`
	Session *sessionInfo `json:"session,omitempty"`
}

type proxyInfo struct {
	Version string `json:"version"`
	PSK     bool   `json:"psk_auth"`
	Binary  string `json:"binary"`
}

type sessionInfo struct {
	Version string `json:"version"`
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
}

func (p *aggProxy) versionInfo(ctx context.Context, req *mcp.CallToolRequest, args versionInfoArgs) (*mcp.CallToolResult, versionInfoResult, error) {
	exe, _ := os.Executable()
	result := versionInfoResult{
		Proxy: proxyInfo{
			Version: p.version,
			PSK:     p.hasPSK,
			Binary:  exe,
		},
	}
	// Try to get the session version from a connected session.
	if live := List(); len(live) > 0 {
		info := live[0]
		if cs, err := p.conn(ctx, info); err == nil {
			if ir := cs.InitializeResult(); ir != nil && ir.ServerInfo != nil {
				result.Session = &sessionInfo{
					Version: ir.ServerInfo.Version,
					ID:      info.ID,
					Name:    info.Name,
				}
			}
		}
	}
	return nil, result, nil
}

// ---- tool-list mirroring (schema cache) ----

// toolSpecs returns the session tool set to advertise (all session tools
// except the internal authentication tools), cached to disk so it works even
// when no session is currently running. On a first run with no session and no
// cache yet, the proxy still starts with only its local tools; reconnect after
// starting a session to advertise the mirrored session tools.
//
// It unions EVERY live session's tools rather than mirroring one. Sessions
// come in kinds — a PTY-backed shared terminal and a native Windows peer —
// that implement genuinely different tools, so sampling a single session
// advertised one kind's surface as if it were universal: whichever session id
// happened to sort first decided what the client could do. A tool only the
// other kind implements then had no handler registered at all (see the
// registration loop in Run), leaving it unroutable even though the target
// served it perfectly well.
func (p *aggProxy) toolSpecs(ctx context.Context) ([]*mcp.Tool, error) {
	var sets []labeledTools
	for _, info := range List() {
		tools, err := p.fetchTools(ctx, info)
		if err != nil {
			// One unreachable session must not cost us the others' tools.
			fmt.Fprintf(os.Stderr, "aish mcp-proxy: listing tools from session %s: %v\n", info.Label(), err)
			continue
		}
		sets = append(sets, labeledTools{label: info.Label(), tools: filterTools(tools)})
	}
	if len(sets) > 0 {
		merged := mergeToolSpecs(sets)
		saveToolCache(merged)
		return merged, nil
	}
	if tools := loadToolCache(); tools != nil {
		return filterTools(tools), nil
	}
	fmt.Fprintln(os.Stderr, "aish mcp-proxy: no aish session is running and no cached tool list is available; exposing list_sessions only until a session exists and the client reconnects")
	return nil, nil
}

func (p *aggProxy) fetchTools(ctx context.Context, info SessionInfo) ([]*mcp.Tool, error) {
	raw, err := net.Dial("unix", info.Sock)
	if err != nil {
		return nil, err
	}
	defer raw.Close()
	cs, err := p.client.Connect(ctx, &mcp.IOTransport{Reader: raw, Writer: raw}, nil)
	if err != nil {
		return nil, err
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	return res.Tools, nil
}

func filterTools(tools []*mcp.Tool) []*mcp.Tool {
	out := make([]*mcp.Tool, 0, len(tools))
	for _, t := range tools {
		if authproto.InternalTools[t.Name] {
			continue // private client authorization protocol
		}
		out = append(out, t)
	}
	return out
}

// labeledTools is one session's advertised tool set, tagged for divergence
// reporting; labeledTool is a single tool from it.
type labeledTools struct {
	label string
	tools []*mcp.Tool
}

type labeledTool struct {
	label string
	tool  *mcp.Tool
}

// mergeToolSpecs unions tool sets by name. Only one variant of a shared name
// can be advertised, so the routing-aware one (already declaring `session`)
// wins and the advertised description says plainly that other sessions
// implement it differently. Silently presenting one kind's variant as
// universal is exactly what lets a client read one implementation's
// documentation while calling another's — the same tool name can mean
// "invisible, authorization-gated" on one session and "mirrored to a human in
// real time" on the next.
func mergeToolSpecs(sets []labeledTools) []*mcp.Tool {
	var order []string
	variants := map[string][]labeledTool{}
	for _, s := range sets {
		for _, t := range s.tools {
			if _, seen := variants[t.Name]; !seen {
				order = append(order, t.Name)
			}
			variants[t.Name] = append(variants[t.Name], labeledTool{label: s.label, tool: t})
		}
	}
	out := make([]*mcp.Tool, 0, len(order))
	for _, name := range order {
		vs := variants[name]
		base := vs[0]
		for _, v := range vs {
			if schemaDeclaresSession(v.tool.InputSchema) {
				base = v
				break
			}
		}
		merged := *base.tool
		if others := divergentLabels(vs, base); len(others) > 0 {
			merged.Description = strings.TrimRight(merged.Description, " ") +
				" NOTE: sessions on this machine implement different variants of this tool." +
				" This text describes " + base.label + "; " + strings.Join(others, ", ") +
				" differ (behaviour, visibility or accepted arguments may not match)." +
				" Confirm the target session before relying on the details above."
		}
		ensureSessionArg(&merged)
		out = append(out, &merged)
	}
	return out
}

// divergentLabels names the sessions whose variant of a tool differs from the
// one being advertised.
func divergentLabels(vs []labeledTool, base labeledTool) []string {
	var out []string
	for _, v := range vs {
		if v.label != base.label && !sameToolShape(v.tool, base.tool) {
			out = append(out, v.label)
		}
	}
	return out
}

// sameToolShape reports whether two same-named tools are interchangeable from
// a caller's point of view: identical prose and identical accepted arguments.
func sameToolShape(a, b *mcp.Tool) bool {
	if a.Description != b.Description {
		return false
	}
	pa, pb := schemaPropertyNames(a.InputSchema), schemaPropertyNames(b.InputSchema)
	if len(pa) != len(pb) {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return false
		}
	}
	return true
}

// schemaPropertyNames returns a mirrored schema's declared property names,
// sorted, for comparison.
func schemaPropertyNames(schema any) []string {
	props, _ := schemaProperties(schema)
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// schemaProperties returns the properties map of a mirrored (JSON-decoded)
// input schema.
func schemaProperties(schema any) (map[string]any, bool) {
	m, ok := schema.(map[string]any)
	if !ok {
		return nil, false
	}
	props, ok := m["properties"].(map[string]any)
	return props, ok
}

func schemaDeclaresSession(schema any) bool {
	props, _ := schemaProperties(schema)
	_, ok := props["session"]
	return ok
}

// sessionArgDescription mirrors the wording of internal/mcpserver's SessionArg.
const sessionArgDescription = "run this call in another live session, addressed by id or name (see list_sessions); default: the session this connection is attached to"

// ensureSessionArg adds the `session` routing property to a mirrored schema
// that lacks one. A session server serving exactly one session has nothing to
// route and so omits the argument — but the proxy REQUIRES it whenever more
// than one session is live, and those same schemas set
// additionalProperties:false. Advertising them unchanged told every client that
// the one argument which makes the call valid was forbidden: a strict client
// could not express the call at all, and a lenient one worked only by ignoring
// the schema it had just been given.
func ensureSessionArg(t *mcp.Tool) {
	m, ok := t.InputSchema.(map[string]any)
	if !ok {
		return
	}
	props, ok := m["properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
		m["properties"] = props
	}
	if _, exists := props["session"]; exists {
		return
	}
	props["session"] = map[string]any{
		"type":        "string",
		"description": sessionArgDescription,
	}
}

func toolCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "aish", "tools.json")
}

func saveToolCache(tools []*mcp.Tool) {
	p := toolCachePath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	if b, err := json.Marshal(tools); err == nil {
		_ = os.WriteFile(p, b, 0o600)
	}
}

func loadToolCache() []*mcp.Tool {
	b, err := os.ReadFile(toolCachePath())
	if err != nil {
		return nil
	}
	var tools []*mcp.Tool
	if json.Unmarshal(b, &tools) != nil {
		return nil
	}
	return tools
}

func toolError(format string, args ...any) *mcp.CallToolResult {
	res := &mcp.CallToolResult{}
	res.SetError(fmt.Errorf(format, args...))
	return res
}
