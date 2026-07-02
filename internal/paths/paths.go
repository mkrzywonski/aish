// Package paths centralizes the on-disk layout of aish runtime state:
// per-session directories holding the MCP socket, the ssh PATH shim, and
// ControlMaster sockets.
package paths

import (
	"os"
	"path/filepath"
)

// Base returns the directory that holds all session runtime dirs.
// Prefers XDG_RUNTIME_DIR (0700 tmpfs, cleaned at logout).
func Base() string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "aish")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".aish", "run")
}

// SessionDir returns the runtime dir for one session.
func SessionDir(id string) string { return filepath.Join(Base(), id) }

// Socket returns the MCP socket path for a session.
func Socket(id string) string { return filepath.Join(SessionDir(id), "mcp.sock") }

// ShimBin returns the directory that is prepended to PATH inside the
// session, containing the `ssh` symlink to the aish binary.
func ShimBin(id string) string { return filepath.Join(SessionDir(id), "bin") }
