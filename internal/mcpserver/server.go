// Package mcpserver exposes a running aish session to MCP clients over a
// per-session Unix socket. Each accepted connection is an independent MCP
// session; tool calls serialize on the session core's locks.
package mcpserver

import (
	"context"
	"errors"
	"net"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/framing"
	"ai-ssh/internal/session"
	"ai-ssh/internal/state"
	"ai-ssh/internal/term"
)

// Core bundles everything tool handlers need to observe and drive a session.
type Core struct {
	Sess    *session.Session
	Term    *term.Terminal
	Tracker *state.Tracker
	Engine  *framing.Engine
	Version string
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

	go func() {
		<-ctx.Done()
		l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go server.Connect(ctx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
	}
}
