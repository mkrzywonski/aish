package aishwnd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/aishwinwire"
	"ai-ssh/internal/paths"
)

// Version is stamped by cmd/aishwnd's main from the build's version string,
// and reported both as the MCP server's Implementation.Version and by
// version_info.
var Version = "dev"

// aishwndSession is the one Windows peer's session this process serves: its
// wire link, its auth state, and the id/session-directory it presents to
// internal/proxy as an ordinary aish session.
type aishwndSession struct {
	id   string
	name string
	wire *aishwinwire.Conn
	auth *authManager
}

// displayHost is the "host" label reported by exec/session_status results:
// the declared session name if there is one, else a generic placeholder —
// aishwnd never learns the Windows machine's real hostname over the wire
// protocol.
func (s *aishwndSession) displayHost() string {
	if s.name != "" {
		return s.name
	}
	return "windows"
}

// Prompt forwards an approval question to the Windows peer's console and
// blocks for its answer, mirroring internal/session/console.go's Prompt but
// over the wire link instead of a shared PTY. Returns ("", false) on
// timeout, send failure, or a malformed answer.
func (s *aishwndSession) Prompt(question, kind string, timeout time.Duration) (string, bool) {
	id := randHex(8)
	ch := s.wire.Await(id)
	defer s.wire.CancelAwait(id)

	data, err := json.Marshal(aishwinwire.PromptData{Question: question, Kind: kind, TimeoutSeconds: int(timeout / time.Second)})
	if err != nil {
		return "", false
	}
	if err := s.wire.Send(aishwinwire.Frame{Type: "prompt", ID: id, Data: data}); err != nil {
		return "", false
	}
	select {
	case f := <-ch:
		var pa aishwinwire.PromptAnswerData
		if err := json.Unmarshal(f.Data, &pa); err != nil || pa.Answer == "" {
			return "", false
		}
		return pa.Answer, true
	case <-time.After(timeout):
		return "", false
	}
}

// roundTrip sends a request frame of frameType and blocks for the matching
// response, returning its raw Data payload. This is the common shape behind
// every exec/file_* tool: send one frame, correlate by id, wait with a
// bounded timeout. Prompt (above) predates this helper and has different
// semantics (a typed y/n answer, not a raw JSON payload to unmarshal at the
// call site), so it stays its own method.
func (s *aishwndSession) roundTrip(frameType string, data json.RawMessage, timeout time.Duration) (json.RawMessage, error) {
	id := randHex(8)
	ch := s.wire.Await(id)
	defer s.wire.CancelAwait(id)
	if err := s.wire.Send(aishwinwire.Frame{Type: frameType, ID: id, Data: data}); err != nil {
		return nil, fmt.Errorf("sending %s request to the Windows peer: %w", frameType, err)
	}
	select {
	case f := <-ch:
		return f.Data, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("no response from the Windows peer for %s", frameType)
	}
}

// Notify sends a one-way informational message to the Windows peer's
// console, mirroring internal/session/console.go's Notify.
func (s *aishwndSession) Notify(format string, args ...any) {
	data, err := json.Marshal(aishwinwire.NotifyData{Text: fmt.Sprintf(format, args...)})
	if err != nil {
		return
	}
	_ = s.wire.Send(aishwinwire.Frame{Type: "notify", Data: data})
}

// Run is aishwnd's entire job for one invocation: it is spawned as a child
// process by aishwin.exe (by default via `wsl.exe -- aishwnd`, or via
// `ssh [user@]host aishwnd`) and speaks the private wire protocol over in/out
// — normally os.Stdin/os.Stdout. It reads the hello frame, stands up an
// ordinary aish-shaped session directory and Unix socket (indistinguishable
// to internal/proxy from a normal aish session), serves MCP over that Unix
// socket until the stdio link closes (the parent process exited or closed
// the pipe) or ctx is canceled, then cleans up. One process, one session —
// unlike the earlier TCP-listener design, there's no shared rendezvous point
// multiple connections could race over, so nothing analogous to name-based
// eviction is needed here.
func Run(ctx context.Context, in io.Reader, out io.Writer) error {
	wc := aishwinwire.NewConn(in, out)

	hello, err := aishwinwire.ReadHello(wc)
	if err != nil {
		return fmt.Errorf("rejecting connection: %w", err)
	}

	id := newSessionID()
	sess := &aishwndSession{id: id, wire: wc}
	sess.auth = newAuthManager(sess)

	dir := paths.SessionDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating session dir: %w", err)
	}
	defer os.RemoveAll(dir)
	// ctx may be canceled (SIGTERM/SIGINT) while the readLoop below is
	// blocked reading stdin, which Go cannot interrupt directly — remove the
	// directory promptly on cancellation instead of waiting for the process
	// to actually die. Racing with the deferred RemoveAll above is fine.
	go func() {
		<-ctx.Done()
		os.RemoveAll(dir)
	}()

	if hello.Name != "" && paths.ValidName(hello.Name) {
		_ = paths.WriteName(id, hello.Name)
		sess.name = hello.Name
	}

	sock := paths.Socket(id)
	_ = os.Remove(sock)
	ul, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", sock, err)
	}
	defer ul.Close()

	server := mcp.NewServer(&mcp.Implementation{Name: "aishwnd", Version: Version}, nil)
	registerAuthTools(server, sess.auth)
	registerExecTools(server, sess)
	registerFileTools(server, sess)
	registerEditTools(server, sess)
	registerSearchTools(server, sess)
	registerTransferTools(server, sess)
	server.AddReceivingMiddleware(sess.auth.middleware())

	unixCtx, cancelUnix := context.WithCancel(ctx)
	defer cancelUnix()
	go serveUnix(unixCtx, ul, server)

	ackData, err := json.Marshal(aishwinwire.HelloAckData{SessionID: id, Name: sess.name, Version: Version})
	if err != nil {
		return err
	}
	if err := wc.Send(aishwinwire.Frame{Type: "hello_ack", Data: ackData}); err != nil {
		return fmt.Errorf("sending hello_ack: %w", err)
	}

	// Block until the stdio link closes (parent exited or closed the pipe)
	// or sends a malformed stream. ReadLoop's own dispatch handles
	// prompt_answer via the pending-request map; "rename" (from the Windows
	// console's menu) is the only frame type this side needs to act on
	// beyond that.
	return wc.ReadLoop(func(f aishwinwire.Frame) {
		if f.Type == "rename" {
			sess.handleRename(f)
		}
	})
}

// handleRename applies a rename requested from the Windows console's menu
// and replies with the outcome.
func (s *aishwndSession) handleRename(f aishwinwire.Frame) {
	var req aishwinwire.RenameData
	result := aishwinwire.RenameResultData{}
	if err := json.Unmarshal(f.Data, &req); err != nil {
		result.Error = "malformed rename request"
	} else if !paths.ValidName(req.Name) {
		result.Error = fmt.Sprintf("%q is not a valid session name", req.Name)
	} else if err := paths.WriteName(s.id, req.Name); err != nil {
		result.Error = err.Error()
	} else {
		s.name = req.Name
	}
	data, err := json.Marshal(result)
	if err != nil {
		return
	}
	_ = s.wire.Send(aishwinwire.Frame{Type: "rename_result", ID: f.ID, Data: data})
}

// serveUnix accepts MCP client connections (normally the aish proxy) on the
// session's Unix socket until ctx is canceled, mirroring the shape of
// internal/mcpserver/server.go's Serve() loop.
func serveUnix(ctx context.Context, l net.Listener, server *mcp.Server) {
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			ss, err := server.Connect(ctx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
			if err != nil {
				conn.Close()
				return
			}
			ss.Wait()
		}()
	}
}

// newSessionID mirrors internal/session.NewID's convention (4 random bytes,
// hex-encoded) for consistency with normal aish session ids, even though
// nothing depends on the format matching.
func newSessionID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
