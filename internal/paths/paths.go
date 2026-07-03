// Package paths centralizes the on-disk layout of aish runtime state:
// per-session directories holding the MCP socket, the ssh PATH shim, and
// ControlMaster sockets.
package paths

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

// NameFile returns the path of the file holding a session's human-readable
// name. The id is the immutable key (dir, socket, env); the name is a
// mutable label shown in the prompt badge and window title and accepted by
// session discovery.
func NameFile(id string) string { return filepath.Join(SessionDir(id), "name") }

// nameRe: short, prompt- and shell-safe labels. No spaces or metacharacters
// so the name can be spliced into PS1 and command lines verbatim.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// ValidName reports whether s is acceptable as a session name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// ReadName returns the session's name, or "" if unnamed.
func ReadName(id string) string {
	b, err := os.ReadFile(NameFile(id))
	if err != nil {
		return ""
	}
	name, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(name)
}

// WriteName sets the session's name. Callers validate with ValidName first.
func WriteName(id, name string) error {
	return os.WriteFile(NameFile(id), []byte(name+"\n"), 0o600)
}
