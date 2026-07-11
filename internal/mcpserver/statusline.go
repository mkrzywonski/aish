package mcpserver

import (
	"os"

	"ai-ssh/internal/paths"
)

// oobUser returns the identity out-of-band ops run as, computed cheaply (never
// runs `ssh -O check`): the probed user, else the ssh login user, else the
// local user.
func (c *Core) oobUser() string {
	ci := c.Mux.Current()
	if ci == nil {
		return os.Getenv("USER")
	}
	if caps, ok := c.Mux.CachedCapabilities(ci); ok && caps.User != "" {
		return caps.User
	}
	return ci.User
}

// StatusLine renders the reserved status-row text: the session badge, the
// interactive (tty) host aish believes it is on (from OSC 7 — may be stale, and
// that staleness is itself the signal), and the out-of-band target as
// user@host, with a ⚠ marker when they diverge. Cheap enough to poll.
func (c *Core) StatusLine() string {
	name := paths.ReadName(c.Sess.ID)
	if name == "" {
		name = c.Sess.ID
	}
	drift, ttyHost, oobHost := c.HostDrift()
	if ttyHost == "" {
		ttyHost = "?"
	}
	oob := oobHost
	if oob == "" {
		oob = "local"
	}
	if u := c.oobUser(); u != "" {
		oob = u + "@" + oob
	}
	s := "⧉" + name + "   tty: " + ttyHost + "   oob: " + oob
	if drift {
		s += "   ⚠ host drift"
	}
	return s
}
