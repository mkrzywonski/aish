// Package paths centralizes the on-disk layout of aish runtime state:
// per-session directories holding the MCP socket, the ssh PATH shim, and
// ControlMaster sockets.
package paths

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Base returns the directory that holds all session runtime dirs.
// Prefers XDG_RUNTIME_DIR (0700 tmpfs, cleaned at logout). When that env var
// is missing, use /run/user/$UID if it exists so subprocesses launched without
// the full login environment still discover the active sessions.
func Base() string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "aish")
	}
	if uid := os.Getuid(); uid >= 0 {
		runUser := filepath.Join("/run/user", strconv.Itoa(uid))
		if info, err := os.Stat(runUser); err == nil && info.IsDir() {
			return filepath.Join(runUser, "aish")
		}
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

// Session backends. The backend is which implementation serves a session, and
// it decides what that session can possibly do: BackendSharedTerminal is a
// terminal on a local pty that a human watches and types into, optionally
// SSH'd elsewhere; BackendWindowsPeer is a native Windows machine reached
// through aishwnd, with no terminal to share.
//
// This is NOT the platform and NOT the shell, both of which are separate axes
// reported elsewhere. A shared-terminal session SSH'd to a Windows host IS
// running Windows and behaves nothing like a Windows peer. What the backend
// decides is whether terminal tools exist at all, and whether there is an
// out-of-band route for anything to be invisible on.
const (
	BackendSharedTerminal = "shared_terminal"
	BackendWindowsPeer    = "windows_peer"
)

// BackendFile returns the path of the file recording a session's backend. It is
// written once at startup and never changes, so readers can learn what a
// session is without connecting to it — no socket, no authorization prompt,
// and on an SSH session no risk of triggering an MFA push.
func BackendFile(id string) string { return filepath.Join(SessionDir(id), "backend") }

// ReadBackend returns the session's backend, or "" for a session that predates
// the backend file. Callers must treat "" as unknown rather than assuming a default:
// guessing wrong is what the file exists to prevent.
func ReadBackend(id string) string {
	b, err := os.ReadFile(BackendFile(id))
	if err != nil {
		return ""
	}
	backend, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(backend)
}

// WriteBackend records the session's backend at startup.
func WriteBackend(id, backend string) error {
	return os.WriteFile(BackendFile(id), []byte(backend+"\n"), 0o600)
}

// OOBFile marks that out-of-band operations are authorized for the session.
// Its presence is read by the ssh shim (deciding whether to inject
// ControlMaster) and by the MCP server (deciding whether to act invisibly).
// Written by `aish --oob` at startup or by a runtime "always" grant.
func OOBFile(id string) string { return filepath.Join(SessionDir(id), "oob") }

// OOBGranted reports whether out-of-band operations are authorized.
func OOBGranted(id string) bool {
	_, err := os.Stat(OOBFile(id))
	return err == nil
}

// GrantOOB persists the out-of-band authorization for the session.
func GrantOOB(id string) error {
	return os.WriteFile(OOBFile(id), []byte("1\n"), 0o600)
}

// RevokeOOB clears the out-of-band authorization marker. A missing marker is
// not an error (revoking an already-off session is a no-op).
func RevokeOOB(id string) error {
	if err := os.Remove(OOBFile(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
