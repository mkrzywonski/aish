// The ssh PATH shim: aish is exec'd under the name "ssh" (via a symlink in
// the session's shim bin dir), injects ControlMaster multiplexing options,
// records the connection in the session runtime dir, and execs the real ssh.
package sshmux

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"ai-ssh/internal/paths"
)

// ConnInfo is the event file a shim invocation writes before exec'ing ssh.
// The pid stays valid across exec, so liveness == /proc/<pid> being ssh.
type ConnInfo struct {
	Pid   int    `json:"pid"`
	Host  string `json:"host"` // canonical hostname from ssh -G
	User  string `json:"user"`
	Port  string `json:"port"`
	Sock  string `json:"sock"`
	Start int64  `json:"start"`
}

func ShimMain(args []string) int {
	id := os.Getenv("AISH_SESSION")
	var shimDir string
	if id != "" {
		shimDir = paths.ShimBin(id)
	}
	realSSH, err := findRealSSH(shimDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aish ssh shim:", err)
		return 127
	}

	if id == "" || passthrough(args) {
		return execReplace(realSSH, args)
	}

	host, user, port, userControlPath := resolveG(realSSH, args)
	if host == "" || userControlPath {
		// Couldn't resolve (bad args — let real ssh print the error) or the
		// user configured their own ControlPath, which wins.
		return execReplace(realSSH, args)
	}

	// ControlMaster multiplexing (the substrate for invisible out-of-band
	// operations) is set up only when the user started the session with
	// --oob. Without it we still record the connection event — sans socket —
	// so the daemon knows which host the terminal is on for in-band routing.
	oob := os.Getenv("AISH_OOB") == "1"

	dir := paths.SessionDir(id)
	var sock string
	if oob {
		sum := sha256.Sum256([]byte(user + "@" + host + ":" + port))
		sock = filepath.Join(dir, fmt.Sprintf("cm-%x", sum[:8]))
	}

	evDir := filepath.Join(dir, "ssh")
	if err := os.MkdirAll(evDir, 0o700); err == nil {
		info := ConnInfo{
			Pid: os.Getpid(), Host: host, User: user, Port: port,
			Sock: sock, Start: time.Now().UnixNano(),
		}
		if b, err := json.Marshal(info); err == nil {
			_ = os.WriteFile(filepath.Join(evDir, strconv.Itoa(os.Getpid())+".json"), b, 0o600)
		}
	}

	if !oob {
		return execReplace(realSSH, args)
	}
	inject := []string{
		"-oControlMaster=auto",
		"-oControlPath=" + sock,
		"-oControlPersist=60s",
	}
	return execReplace(realSSH, append(inject, args...))
}

// passthrough reports whether this invocation must not be touched:
// control/query operations, stdio-forwarding, and explicit user control of
// multiplexing on the command line (which must win over our injection —
// ssh's first-obtained-wins rule would otherwise favor our prepended options).
func passthrough(args []string) bool {
	for i, a := range args {
		switch a {
		case "-O", "-G", "-V", "-Q", "-W", "-S":
			return true
		case "-o":
			if i+1 < len(args) && isControlOpt(args[i+1]) {
				return true
			}
		}
		if strings.HasPrefix(a, "-S") && len(a) > 2 {
			return true
		}
		if strings.HasPrefix(a, "-o") && len(a) > 2 && isControlOpt(a[2:]) {
			return true
		}
		if a == "--" {
			break
		}
	}
	return false
}

func isControlOpt(v string) bool {
	k, _, _ := strings.Cut(strings.TrimSpace(v), "=")
	k = strings.ToLower(strings.TrimSpace(k))
	return k == "controlpath" || k == "controlmaster" || k == "controlpersist"
}

// resolveG runs `ssh -G <args>` to get the canonical host/user/port and
// whether a ControlPath is already configured (command line or ssh_config).
func resolveG(realSSH string, args []string) (host, user, port string, controlPath bool) {
	out, err := exec.Command(realSSH, append([]string{"-G"}, args...)...).Output()
	if err != nil {
		return "", "", "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(line, " ")
		switch k {
		case "hostname":
			host = v
		case "user":
			user = v
		case "port":
			port = v
		case "controlpath":
			if v != "" && v != "none" {
				controlPath = true
			}
		}
	}
	return host, user, port, controlPath
}

// findRealSSH locates the real ssh binary on PATH, skipping the shim dir and
// anything that resolves to the aish binary itself.
func findRealSSH(shimDir string) (string, error) {
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || dir == shimDir {
			continue
		}
		cand := filepath.Join(dir, "ssh")
		st, err := os.Stat(cand)
		if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(cand); err == nil && resolved == self && self != "" {
			continue
		}
		return cand, nil
	}
	return "", fmt.Errorf("no real ssh found on PATH")
}

func execReplace(realSSH string, args []string) int {
	argv := append([]string{realSSH}, args...)
	err := unix.Exec(realSSH, argv, os.Environ())
	fmt.Fprintln(os.Stderr, "aish ssh shim: exec:", err)
	return 127
}
