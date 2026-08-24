//go:build windows

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// shellKind selects which persistent Windows shell backs one exec call --
// see commandDispatcher (exec.go), which keeps one independent persistent
// shellSession per kind, so switching which one a call uses never loses
// another kind's cwd/env state.
type shellKind string

const (
	shellCmd        shellKind = "cmd"
	shellPowerShell shellKind = "powershell"
)

// shellKindStrings converts kinds to their wire-format string names, for
// HelloData.AvailableShells and error messages that list them.
func shellKindStrings(kinds []shellKind) []string {
	strs := make([]string, len(kinds))
	for i, k := range kinds {
		strs[i] = string(k)
	}
	return strs
}

// detectAvailableShells reports which shellKinds this host can actually
// run: cmd.exe and powershell.exe ship with every real Windows install, so
// both are always available.
func detectAvailableShells() []shellKind {
	return []shellKind{shellCmd, shellPowerShell}
}

// shellSession owns one persistent Windows shell process fed via piped
// stdin/stdout. Its output is mirrored byte-for-byte to this process's own
// console (the human watches commands run in real time — visible, not
// interactive, per the whole point of aishwin) and simultaneously parsed for
// a nonce marker line to detect command completion and exit code: cmd.exe
// and PowerShell have no OSC133-style structural framing, so this is the
// only way to know where one command's output ends and the next begins.
// The marker technique (line format, PowerShell's $LASTEXITCODE staleness
// workaround) was verified against a live Windows host — see the plan doc
// — before being encoded here.
type shellSession struct {
	kind  shellKind
	cmd   *exec.Cmd
	stdin io.WriteCloser

	mu    sync.Mutex  // serializes foreground Run calls: one persistent shell, one command at a time
	lines chan string // complete, \r\n-stripped lines from the reader goroutine
	dead  atomic.Bool // set on timeout or unexpected exit; Run never reuses a dead shell (mirrors internal/sshmux/channel.go's "dead channels are dropped, never silently reopened" — a timed-out command's late output would otherwise corrupt the next command's capture)

	cwdMu   sync.Mutex // guards lastCwd, read from a different goroutine than Run() writes it from
	lastCwd string     // the shell's cwd as of its last completed command; see CWD()
}

// startShell launches the persistent shell and its output-mirroring reader
// goroutine. Inherits the console menu's current custom env vars
// (access.environ) at spawn time; a var set after this point only reaches
// an already-running shell if the menu also pushes it live (see
// menu.go:pushLiveEnv).
func startShell(kind shellKind) (*shellSession, error) {
	var cmd *exec.Cmd
	switch kind {
	case shellPowerShell:
		cmd = exec.Command("powershell.exe", "-NoLogo", "-NoProfile")
	default:
		kind = shellCmd
		cmd = exec.Command("cmd.exe")
	}
	cmd.Env = access.environ(os.Environ())
	return newShellSession(kind, cmd)
}

// newShellSession wires up an already-configured (not yet started) command
// as a persistent shell. Split out of startShell so tests can drive the
// real Run/parseMarker logic against a real Windows shell reached over ssh,
// instead of only against the hardcoded local cmd.exe/powershell.exe.
func newShellSession(kind shellKind, cmd *exec.Cmd) (*shellSession, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Merge stderr into the same stream: matches what a human watching the
	// real console sees, and neither shell structurally distinguishes the
	// two once piped non-interactively anyway.
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	s := &shellSession{kind: kind, cmd: cmd, stdin: stdin, lines: make(chan string, 256)}
	// Best-effort starting value so even the very first background exec
	// (before any foreground command has run to refresh it) gets a real
	// cwd instead of falling back to the process's own launch directory.
	s.lastCwd, _ = os.Getwd()
	go s.mirror(stdout)
	return s, nil
}

// CWD returns the shell's cwd as of its last completed command -- the best
// available signal for a background exec call that didn't specify its own
// cwd (background commands get their own fresh process via cmd.Dir, with
// no persistent-shell state of their own to inherit from otherwise).
func (s *shellSession) CWD() string {
	s.cwdMu.Lock()
	defer s.cwdMu.Unlock()
	return s.lastCwd
}

// mirror reads the shell's combined output and splits it into complete
// lines for Run to scan. It deliberately does not write anything to the
// console itself — Run decides, per line, whether to mirror it (skipping
// this session's own nonce-marker plumbing) once it knows the current
// command's marker text, so mirroring has to happen there instead of here.
func (s *shellSession) mirror(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		s.lines <- sc.Text() // bufio.ScanLines already strips a trailing \r\n
	}
	close(s.lines)
}

// mirrorLine shows one line of shell output in the GUI's log view.
func mirrorLine(line string) {
	AppendLog(line)
}

// Run submits command to the persistent shell and blocks until its nonce
// marker line appears or timeout elapses. It returns the output between the
// command's own echo and the marker's echo (both shells echo piped input),
// and the exit code parsed from the marker's expansion. On timeout the
// shell is killed and marked dead — see the dead field's doc.
func (s *shellSession) Run(command string, timeout time.Duration) (output string, exitCode int, timedOut bool, err error) {
	if !s.mu.TryLock() {
		return "", 0, false, fmt.Errorf("a foreground command is already running on this shell")
	}
	defer s.mu.Unlock()

	nonce := randHex(8)
	marker := "aishwin@" + nonce + "@"

	// The marker line also carries the shell's cwd after the exit code
	// (aishwin@<nonce>@<exitcode>@<cwd>@) so CWD() has a real answer for
	// background exec calls that don't specify their own -- Windows paths
	// don't contain '@', so parseMarker's split is unambiguous.
	var script, markerCmd, resetCmd string
	const lineEnd = "\r\n"
	switch s.kind {
	case shellPowerShell:
		// $LASTEXITCODE only reflects the last NATIVE process's exit code and
		// is sticky across cmdlet-only commands (a cmdlet failure leaves a
		// stale value from whatever native command last set it) — verified
		// live. Reset it first, then prefer it when a native command actually
		// set it, falling back to $? (cmdlet success/failure) otherwise.
		resetCmd = "$global:LASTEXITCODE = $null"
		markerCmd = fmt.Sprintf(`Write-Host "%s$(if ($LASTEXITCODE -ne $null) { $LASTEXITCODE } elseif ($?) { 0 } else { 1 })@$($pwd.Path)@"`, marker)
		script = resetCmd + lineEnd + command + lineEnd + markerCmd + lineEnd
	default:
		markerCmd = "echo " + marker + "%errorlevel%@%cd%@"
		script = command + lineEnd + markerCmd + lineEnd
	}

	if _, err := s.stdin.Write([]byte(script)); err != nil {
		s.dead.Store(true)
		return "", 0, false, fmt.Errorf("writing to shell: %w", err)
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	var out []string
	started := false
	for {
		select {
		case line, ok := <-s.lines:
			if !ok {
				s.dead.Store(true)
				mirrorLine("(shell exited unexpectedly)")
				return strings.Join(out, "\n"), 0, false, fmt.Errorf("shell exited unexpectedly")
			}

			// Hide this session's own internal plumbing from the visible
			// mirror — the human never typed the reset line or the marker
			// command, so seeing them (and their raw "aishwin@<hash>@0@"
			// expansion) would be confusing noise unrelated to their actual
			// command. Checked before mirroring anything else below.
			if resetCmd != "" && strings.HasSuffix(line, resetCmd) {
				continue // echo of the PowerShell $LASTEXITCODE reset line
			}
			if strings.HasSuffix(line, markerCmd) {
				continue // echo of the marker command itself
			}
			if code, cwd, ok := parseMarker(line, nonce); ok {
				if cwd != "" {
					s.cwdMu.Lock()
					s.lastCwd = cwd
					s.cwdMu.Unlock()
				}
				// A command that produces no output at all (a clean `go
				// build`, say) would otherwise leave nothing visible
				// after its own echoed command line -- indistinguishable
				// at a glance from "still running" or "output isn't
				// showing up". Always show the exit code explicitly.
				mirrorLine(fmt.Sprintf("(exit %d)", code))
				return trimTrailingBlankLines(out), code, false, nil // the marker's own expansion — the completion signal, never shown or captured as output
			}

			mirrorLine(line)
			if !started {
				// Everything up to and including the echo of our own
				// command (the shell's startup banner, prompt) is mirrored
				// above but not captured as the command's result.
				if strings.HasSuffix(line, command) {
					started = true
				}
				continue
			}
			out = append(out, line)
		case <-deadline.C:
			s.dead.Store(true)
			_ = s.cmd.Process.Kill()
			mirrorLine("(timed out)")
			return trimTrailingBlankLines(out), 0, true, nil
		}
	}
}

// parseMarker reports the exit code and cwd if line is exactly the
// expansion of the marker for nonce (e.g. "aishwin@<nonce>@0@C:\foo@") —
// never the echo of the marker command itself, which contains the
// unexpanded %errorlevel%/$(...) text and so never matches this
// exact-prefix check.
func parseMarker(line, nonce string) (code int, cwd string, ok bool) {
	prefix := "aishwin@" + nonce + "@"
	if !strings.HasPrefix(line, prefix) {
		return 0, "", false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(line, prefix), "@")
	codeStr, cwd, found := strings.Cut(rest, "@")
	if !found {
		// Older/unexpected form without a cwd segment: still accept the
		// exit code alone rather than failing the whole marker match.
		codeStr = rest
		cwd = ""
	}
	n, err := strconv.Atoi(codeStr)
	if err != nil {
		return 0, "", false
	}
	return n, cwd, true
}

// trimTrailingBlankLines drops wholly-blank trailing lines (cmd.exe emits
// one between a command's output and the next prompt) without touching
// meaningful trailing whitespace within a real line.
func trimTrailingBlankLines(lines []string) string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}
