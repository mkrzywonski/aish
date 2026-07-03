// Daemon-side ControlMaster management: track live ssh connections started
// through the shim, and run out-of-band commands / file transfers over the
// multiplexed connection without touching the interactive channel.
package sshmux

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Mux struct {
	dir     string // session runtime dir
	realSSH string
}

func New(sessionDir string) *Mux {
	realSSH, err := exec.LookPath("ssh")
	if err != nil {
		realSSH = "ssh"
	}
	return &Mux{dir: sessionDir, realSSH: realSSH}
}

// Current returns the most recently started still-live ssh connection, or
// nil when the session is local. Event files of dead connections are
// removed as a side effect.
func (m *Mux) Current() *ConnInfo {
	evDir := filepath.Join(m.dir, "ssh")
	entries, err := os.ReadDir(evDir)
	if err != nil {
		return nil
	}
	var best *ConnInfo
	for _, e := range entries {
		p := filepath.Join(evDir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var ci ConnInfo
		if json.Unmarshal(b, &ci) != nil {
			os.Remove(p)
			continue
		}
		if !pidIsSSH(ci.Pid) {
			os.Remove(p)
			continue
		}
		if best == nil || ci.Start > best.Start {
			c := ci
			best = &c
		}
	}
	return best
}

func pidIsSSH(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	return err == nil && strings.TrimSpace(string(b)) == "ssh"
}

// SocketLive checks the master socket is up (ssh -O check).
func (m *Mux) SocketLive(ci *ConnInfo) bool {
	if ci.Sock == "" { // connection tracked without multiplexing (no --oob)
		return false
	}
	if _, err := os.Stat(ci.Sock); err != nil {
		return false
	}
	return exec.Command(m.realSSH, "-S", ci.Sock, "-O", "check", ci.Host).Run() == nil
}

// Command builds an exec.Cmd that runs remoteCmd on the remote over the
// existing multiplexed connection. It never starts a new master and never
// prompts for auth.
func (m *Mux) Command(ctx context.Context, ci *ConnInfo, remoteCmd string) *exec.Cmd {
	return exec.CommandContext(ctx, m.realSSH,
		"-S", ci.Sock,
		"-oControlMaster=no",
		"-oBatchMode=yes",
		"-p", ci.Port,
		"-l", ci.User,
		ci.Host,
		"--", remoteCmd)
}

// CloseAll asks every known master to exit (used at session teardown;
// ControlPersist would reap them within 60s anyway).
func (m *Mux) CloseAll() {
	evDir := filepath.Join(m.dir, "ssh")
	entries, err := os.ReadDir(evDir)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(evDir, e.Name()))
		if err != nil {
			continue
		}
		var ci ConnInfo
		if json.Unmarshal(b, &ci) != nil || ci.Sock == "" || seen[ci.Sock] {
			continue
		}
		seen[ci.Sock] = true
		_ = exec.Command(m.realSSH, "-S", ci.Sock, "-O", "exit", ci.Host).Run()
	}
}

// Quote makes s safe as a single word for a remote POSIX shell.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
