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
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/authproto"
	"ai-ssh/internal/clientauth"
)

type aggProxy struct {
	client   *mcp.Client
	identity *clientauth.Identity
	version  string
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
	"SSH'd into there, use aish tools instead. Start with list_sessions, choose the intended session, then " +
	"call session_status; recheck status after SSH transitions. On a newly-SSH'd host the OOB file/search " +
	"tools start `unknown`; call probe_host once to initialize them, then plan against oob_tools. Every " +
	"session tool accepts `session` (id or name). Use run_command for commands the human should see. Use " +
	"exec, file_*, and directory_list for native-like work on the session's current host when OOB is " +
	"authorized. Never send passwords or other " +
	"secrets; if echo_off is true, wait for the human. Name the target session and host in chat before the " +
	"first substantial or destructive operation. The user approves each session on its own terminal."

// Serve runs the aggregating proxy over stdio until the client disconnects.
func Serve(version string) int {
	identity, err := clientauth.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, "aish mcp-proxy: generating client identity:", err)
		return 1
	}
	p := &aggProxy{
		client:    mcp.NewClient(&mcp.Implementation{Name: "aish-proxy", Version: version}, nil),
		identity:  identity,
		version:   version,
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
		// Connection likely died with the session; drop it and report so the
		// AI can re-list and retry.
		p.drop(info.ID)
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
	if err := p.identity.Authorize(ctx, cs, info.ID); err != nil {
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

// ---- tool-list mirroring (schema cache) ----

// toolSpecs returns the session tool set to advertise (all session tools
// except the internal authentication tools). It mirrors a live session's
// schemas and caches them to disk so it works even when no session is
// currently running. On a first run with no session and no cache yet, the
// proxy still starts with only its local tools; reconnect after starting a
// session to advertise the mirrored session tools.
func (p *aggProxy) toolSpecs(ctx context.Context) ([]*mcp.Tool, error) {
	if live := List(); len(live) > 0 {
		if tools, err := p.fetchTools(ctx, live[0]); err == nil {
			saveToolCache(tools)
			return filterTools(tools), nil
		}
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
