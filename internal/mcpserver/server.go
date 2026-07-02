// Package mcpserver exposes a running aish session to MCP clients over a
// per-session Unix socket. Each accepted connection is an independent MCP
// session; tool calls serialize on the session core's locks.
package mcpserver

import (
	"context"
	"errors"
	"net"
	"os"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/framing"
	"ai-ssh/internal/session"
	"ai-ssh/internal/sshmux"
	"ai-ssh/internal/state"
	"ai-ssh/internal/term"
)

// Core bundles everything tool handlers need to observe and drive a session.
type Core struct {
	Sess    *session.Session
	Term    *term.Terminal
	Tracker *state.Tracker
	Engine  *framing.Engine
	Mux     *sshmux.Mux
	Tasks   *sshmux.Table
	Version string

	// OnClients, when set, is called with the number of connected MCP
	// clients whenever it changes (drives the title-bar activity marker).
	OnClients func(n int)
}

// Serve listens on socketPath until ctx is canceled. It removes any stale
// socket first; callers own directory creation and cleanup.
func Serve(ctx context.Context, core *Core, socketPath string) error {
	_ = os.Remove(socketPath)
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "aish", Version: core.Version}, nil)
	registerTools(server, core)
	registerRemoteTools(server, core)

	go func() {
		<-ctx.Done()
		l.Close()
	}()

	var clients atomic.Int64
	notify := func(delta int64) {
		n := clients.Add(delta)
		if core.OnClients != nil {
			core.OnClients(int(n))
		}
	}

	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go func() {
			ss, err := server.Connect(ctx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
			if err != nil {
				conn.Close()
				return
			}
			notify(+1)
			ss.Wait()
			notify(-1)
		}()
	}
}
