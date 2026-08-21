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

// shellKind selects which persistent Windows shell backs foreground exec.
type shellKind string

const (
	shellCmd        shellKind = "cmd"
	shellPowerShell shellKind = "powershell"
)

// shellSession owns one persistent Windows shell process fed via piped
// stdin/stdout. Its output is mirrored byte-for-byte to this process's own
// console (the human watches commands run in real time — visible, not
// interactive, per the whole point of aicmd) and simultaneously parsed for
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
	go s.mirror(stdout)
	return s, nil
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

// mirrorLine writes one line to the human's console, through the
// CRLF-normalizing writer (stdout, crlf.go) rather than os.Stdout directly.
// Real cmd.exe/PowerShell prompt/echo text already arrives as \r\n and so is
// unaffected by the normalization, but a child process piped through the
// shell (go build's own "downloading" progress lines, observed live) can
// emit bare \n, which — unlike text written straight to a real console —
// does not get an implicit carriage return here, so it needs the same fix
// as this package's own console output (see crlf.go's doc comment).
func mirrorLine(line string) {
	fmt.Fprintln(stdout, line)
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
	marker := "AICMD@" + nonce + "@"

	var script, markerCmd, resetCmd string
	switch s.kind {
	case shellPowerShell:
		// $LASTEXITCODE only reflects the last NATIVE process's exit code and
		// is sticky across cmdlet-only commands (a cmdlet failure leaves a
		// stale value from whatever native command last set it) — verified
		// live. Reset it first, then prefer it when a native command actually
		// set it, falling back to $? (cmdlet success/failure) otherwise.
		resetCmd = "$global:LASTEXITCODE = $null"
		markerCmd = fmt.Sprintf(`Write-Host "%s$(if ($LASTEXITCODE -ne $null) { $LASTEXITCODE } elseif ($?) { 0 } else { 1 })@"`, marker)
		script = resetCmd + "\r\n" + command + "\r\n" + markerCmd + "\r\n"
	default:
		markerCmd = "echo " + marker + "%errorlevel%@"
		script = command + "\r\n" + markerCmd + "\r\n"
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
				return strings.Join(out, "\n"), 0, false, fmt.Errorf("shell exited unexpectedly")
			}

			// Hide this session's own internal plumbing from the visible
			// mirror — the human never typed the reset line or the marker
			// command, so seeing them (and their raw "AICMD@<hash>@0@"
			// expansion) would be confusing noise unrelated to their actual
			// command. Checked before mirroring anything else below.
			if resetCmd != "" && strings.HasSuffix(line, resetCmd) {
				continue // echo of the PowerShell $LASTEXITCODE reset line
			}
			if strings.HasSuffix(line, markerCmd) {
				continue // echo of the marker command itself
			}
			if code, ok := parseMarker(line, nonce); ok {
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
			return trimTrailingBlankLines(out), 0, true, nil
		}
	}
}

// parseMarker reports the exit code if line is exactly the expansion of the
// marker for nonce (e.g. "AICMD@<nonce>@0@") — never the echo of the marker
// command itself, which contains the unexpanded %errorlevel%/$(...) text and
// so never matches this exact-prefix check.
func parseMarker(line, nonce string) (int, bool) {
	prefix := "AICMD@" + nonce + "@"
	if !strings.HasPrefix(line, prefix) {
		return 0, false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(line, prefix), "@")
	code, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return code, true
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
