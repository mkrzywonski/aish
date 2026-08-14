// Persistent OOB channel: one long-lived `sh -s` opened over the existing
// ControlMaster connection, through which all foreground exec and file
// operations for that remote are streamed. On hosts where every new ssh
// session/channel re-triggers MFA (login_duo-style Duo pushes), this costs
// exactly one authorization at open instead of one per operation (validated
// on a production Duo host).
//
// Framing: each script is followed by a printf of a nonce sentinel carrying
// $?; the reader collects output lines until the sentinel. Scripts that
// must not consume the channel's stdin are wrapped by callers with
// `</dev/null`; file writes feed data via a heredoc (base64, whose alphabet
// cannot collide with the marker).
//
// A channel that dies or times out is never reopened silently — the failed
// call returns an error saying a retry will open a new channel (and may
// cost an MFA push); the retry is the consent.
package sshmux

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ChanResult is the outcome of one script run over a persistent channel.
type ChanResult struct {
	Output   []byte
	Exit     int
	TimedOut bool
}

var (
	errChannelDead     = errors.New("channel dead")
	errNoShellResponse = errors.New("no response from the remote shell")
	errNotPosixShell   = errors.New("the remote did not present a POSIX shell")
)

const chanOutputCap = 64 << 20 // hard cap on one op's collected output

// minOpenTimeout: the first op on a fresh channel may sit behind a human
// approving an MFA push; killing the channel too early would burn the push
// and cost another on retry.
const minOpenTimeout = 60 * time.Second

// Two-phase probe timeouts: wait the long window only for the *first byte* (the
// MFA/network wait), then a short window for the sentinel. A shell that answers
// but never returns our sentinel isn't POSIX — fail fast instead of hanging.
const (
	probeFirstByteTimeout = 60 * time.Second
	probeCompleteTimeout  = 8 * time.Second
)

type channel struct {
	mu    sync.Mutex
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan []byte
	dead  bool

	// Capabilities are NOT stored here — they live in Mux.facts, which outlives
	// the channel. A probe result that died with its channel was the cause of
	// two bugs; see facts.go.

	// stderr collects what the remote wrote to fd 2. On a host whose login shell
	// can't run `sh -s` this is the ONLY evidence of what the far end is (stdout
	// comes back empty), and it used to go to the null device.
	stderr *boundedBuf
	// exit is the remote/ssh exit status, valid once reaped is closed.
	exit   atomic.Int32
	reaped chan struct{}
}

// chanStderrCap bounds the captured stderr. Fingerprints appear in the first
// line; this is generous enough for a chatty banner without being a leak.
const chanStderrCap = 8 << 10

// reapGrace is how long evidence() waits for the process to be reaped so a
// fast-failing remote's exit status isn't lost to a race with the read loop.
const reapGrace = 250 * time.Millisecond

// boundedBuf is an io.Writer that keeps at most cap bytes. Using a writer
// rather than StderrPipe lets exec own the copying goroutine, so cmd.Wait
// cannot race a reader still draining the pipe.
type boundedBuf struct {
	mu  sync.Mutex
	b   []byte
	cap int
}

func (w *boundedBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := w.cap - len(w.b); room > 0 {
		if len(p) > room {
			w.b = append(w.b, p[:room]...)
		} else {
			w.b = append(w.b, p...)
		}
	}
	return len(p), nil // never fail the child on our own buffering limit
}

func (w *boundedBuf) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.b...)
}

// evidence returns what the remote printed and its exit status, waiting briefly
// for the process to be reaped. Exit is -1 if it hasn't finished.
func (ch *channel) evidence() (stderr []byte, exit int) {
	select {
	case <-ch.reaped:
	case <-time.After(reapGrace):
	}
	e := -1
	if v := ch.exit.Load(); v != -1 {
		e = int(v)
	}
	return ch.stderr.Bytes(), e
}

func (m *Mux) openChannel(ci *ConnInfo) (*channel, error) {
	cmd := exec.Command(m.realSSH,
		"-S", ci.Sock,
		"-oControlMaster=no",
		"-oBatchMode=yes",
		"-p", ci.Port,
		"-l", ci.User,
		ci.Host,
		"--", "sh -s")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errBuf := &boundedBuf{cap: chanStderrCap}
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ch := &channel{
		cmd:    cmd,
		stdin:  stdin,
		lines:  make(chan []byte, 256),
		stderr: errBuf,
		reaped: make(chan struct{}),
	}
	ch.exit.Store(-1)
	go func() {
		r := stdout
		buf := make([]byte, 0, 4096)
		rd := make([]byte, 4096)
		for {
			n, err := r.Read(rd)
			if n > 0 {
				buf = append(buf, rd[:n]...)
				for {
					i := bytes.IndexByte(buf, '\n')
					if i < 0 {
						break
					}
					line := make([]byte, i+1)
					copy(line, buf[:i+1])
					ch.lines <- line
					buf = buf[i+1:]
				}
			}
			if err != nil {
				if len(buf) > 0 {
					ch.lines <- append([]byte(nil), buf...)
				}
				close(ch.lines)
				// Wait also flushes exec's stderr copier, so ch.stderr is
				// complete once this returns.
				cmd.Wait()
				if cmd.ProcessState != nil {
					ch.exit.Store(int32(cmd.ProcessState.ExitCode()))
				}
				close(ch.reaped)
				return
			}
		}
	}()
	return ch, nil
}

// kill marks the channel dead and reaps the ssh process. Callers hold mu.
func (ch *channel) kill() {
	ch.dead = true
	if ch.cmd.Process != nil {
		ch.cmd.Process.Kill()
	}
}

// run executes script (plus the sentinel) and collects output until the
// sentinel line arrives or timeout expires. One op at a time per channel.
func (ch *channel) run(script string, timeout time.Duration) (*ChanResult, error) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.dead {
		return nil, errChannelDead
	}

	nb := make([]byte, 8)
	rand.Read(nb)
	sent := "@AISH@" + hex.EncodeToString(nb) + "@"
	// Leading \n guarantees the sentinel starts its own line even when the
	// script's output lacks a trailing newline; run strips that one byte.
	full := script + "\nprintf '\\n" + sent + "%s@\\n' \"$?\"\n"
	if _, err := io.WriteString(ch.stdin, full); err != nil {
		ch.kill()
		return nil, errChannelDead
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	var out bytes.Buffer
	for {
		select {
		case line, ok := <-ch.lines:
			if !ok {
				ch.kill()
				return nil, errChannelDead
			}
			trimmed := strings.TrimRight(string(line), "\r\n")
			if code, found := strings.CutPrefix(trimmed, sent); found {
				exit, err := strconv.Atoi(strings.TrimSuffix(code, "@"))
				if err != nil {
					exit = -1
				}
				b := out.Bytes()
				if len(b) > 0 && b[len(b)-1] == '\n' {
					b = b[:len(b)-1] // the \n we injected before the sentinel
				}
				return &ChanResult{Output: b, Exit: exit}, nil
			}
			out.Write(line)
			if out.Len() > chanOutputCap {
				ch.kill()
				return nil, fmt.Errorf("channel output exceeded %d bytes", chanOutputCap)
			}
		case <-deadline.C:
			// The stream is mid-command; framing can't be trusted anymore.
			ch.kill()
			return &ChanResult{Output: out.Bytes(), TimedOut: true}, nil
		}
	}
}

// runProbe runs the capability probe as the first op on a fresh channel, using
// a two-phase timeout so a non-POSIX shell (Windows, a network device, a
// restricted shell) fails in seconds rather than blocking the full MFA window.
// A returned sentinel proves the remote ran our printf, i.e. it is a POSIX
// shell; the collected key=value lines are parsed into Capabilities.
func (ch *channel) runProbe() (Capabilities, *probeFailure) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.dead {
		return Capabilities{}, ch.failure(errChannelDead, nil)
	}
	nb := make([]byte, 8)
	rand.Read(nb)
	sent := "@AISH@" + hex.EncodeToString(nb) + "@"
	full := probeScript + "\nprintf '\\n" + sent + "%s@\\n' \"$?\"\n"
	if _, err := io.WriteString(ch.stdin, full); err != nil {
		ch.kill()
		return Capabilities{}, ch.failure(errChannelDead, nil)
	}

	first := time.NewTimer(probeFirstByteTimeout)
	defer first.Stop()
	var complete *time.Timer
	gotFirst := false
	var out bytes.Buffer
	for {
		var tch <-chan time.Time
		if gotFirst {
			tch = complete.C
		} else {
			tch = first.C
		}
		select {
		case line, ok := <-ch.lines:
			if !ok {
				ch.kill()
				return Capabilities{}, ch.failure(errChannelDead, out.Bytes())
			}
			if !gotFirst {
				gotFirst = true
				first.Stop()
				complete = time.NewTimer(probeCompleteTimeout)
				defer complete.Stop()
			}
			trimmed := strings.TrimRight(string(line), "\r\n")
			if _, found := strings.CutPrefix(trimmed, sent); found {
				return parseCapabilities(out.Bytes()), nil
			}
			out.Write(line)
			if out.Len() > 1<<20 {
				ch.kill()
				return Capabilities{}, ch.failure(errNotPosixShell, out.Bytes())
			}
		case <-tch:
			ch.kill()
			if !gotFirst {
				return Capabilities{}, ch.failure(errNoShellResponse, out.Bytes())
			}
			return Capabilities{}, ch.failure(errNotPosixShell, out.Bytes())
		}
	}
}

// failure snapshots everything observable about a failed probe: the classified
// error, whatever the remote wrote to stdout before giving up, and its stderr
// and exit status. Call after kill() so the process is on its way to being
// reaped.
func (ch *channel) failure(err error, stdout []byte) *probeFailure {
	stderr, exit := ch.evidence()
	return &probeFailure{
		Err:    err,
		Stdout: append([]byte(nil), stdout...),
		Stderr: stderr,
		Exit:   exit,
	}
}

// ChannelRun runs script over the persistent channel for ci, opening it on
// first use. A dead channel is removed and reported — the caller's retry is
// the consent for a fresh open (which may trigger an MFA push).
func (m *Mux) ChannelRun(ci *ConnInfo, script string, timeout time.Duration) (*ChanResult, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	// A host already known to be unusable never opens another channel. No
	// network, no MFA prompt, and the caller gets the same actionable message
	// every time instead of an invitation to retry forever.
	if f, ok := m.Facts(ci); ok && f.ShellBlocked() {
		return nil, f.ShellError()
	}

	m.chMu.Lock()
	ch := m.channels[ci.Sock]
	opened := false
	var finishAttempt func()
	if ch == nil {
		finishAttempt = m.BeginSessionAttempt(ci, SessionAttemptShell)
		defer func() {
			if finishAttempt != nil {
				finishAttempt()
			}
		}()
		var err error
		ch, err = m.openChannel(ci)
		if err != nil {
			m.chMu.Unlock()
			return nil, fmt.Errorf("opening oob channel to %s: %v", ci.Host, err)
		}
		m.channels[ci.Sock] = ch
		opened = true
	}
	m.chMu.Unlock()

	if m.needsProbe(ci, opened) {
		// The first op on a fresh channel may sit behind an MFA push (it opens a
		// new session on the master). The probe runs as that first op with a
		// two-phase timeout, confirming a POSIX shell and recording what the host
		// can do. Either outcome becomes a durable fact, so a host that can't do
		// this is never silently re-probed.
		caps, pf := ch.runProbe()
		if pf != nil {
			m.dropChannel(ci.Sock, ch)
			d, evidence, reason, sticky := classifyFailure(*pf)
			return nil, m.NoteShellUnusable(ci, d, reason, evidence, sticky).ShellError()
		}
		m.NoteShellUsable(ci, caps)
		if opened {
			finishAttempt()
			finishAttempt = nil
		}
	}
	res, err := ch.run(script, timeout)
	if errors.Is(err, errChannelDead) {
		m.dropChannel(ci.Sock, ch)
		return nil, fmt.Errorf("the persistent oob channel to %s was lost; retrying will open a new one (on MFA-protected hosts that triggers one push)", ci.Host)
	}
	if res != nil && res.TimedOut {
		// run() killed the channel; drop it so a retry starts fresh. The probed
		// capabilities survive in facts, so this no longer regresses the host's
		// target confidence.
		m.dropChannel(ci.Sock, ch)
	}
	return res, err
}

// needsProbe reports whether the capability probe must run before using ch.
//
// A freshly opened channel always needs it. So does a LIVE channel with no
// recorded capabilities — which happens when probe_host{force:true} clears the
// facts: forgetting them does not close the channel, so keying the probe on
// "did we just open this" alone left the facts permanently empty. EnsureProbed
// then returned zero capabilities with a nil error, every tool fell back to
// "unknown", and the session stayed broken until the channel happened to die.
//
// Re-probing an existing channel is just another command on the same `sh -s`,
// so this costs no new ssh session and cannot trigger an extra MFA push.
func (m *Mux) needsProbe(ci *ConnInfo, opened bool) bool {
	if opened {
		return true
	}
	_, haveCaps := m.CachedCapabilities(ci)
	return !haveCaps
}

// dropChannel forgets ch if it is still the live channel for sock.
func (m *Mux) dropChannel(sock string, ch *channel) {
	m.chMu.Lock()
	if m.channels[sock] == ch {
		delete(m.channels, sock)
	}
	m.chMu.Unlock()
}

// closeChannels kills all persistent channels (session teardown).
func (m *Mux) closeChannels() {
	m.chMu.Lock()
	defer m.chMu.Unlock()
	for sock, ch := range m.channels {
		ch.mu.Lock()
		ch.kill()
		ch.mu.Unlock()
		delete(m.channels, sock)
	}
}

// WriteScript builds the heredoc script that writes data to path over a
// channel (the non-atomic append path). base64's alphabet cannot contain '@',
// so the static marker is collision-free. decodeFlag is this host's base64
// decode flag ("-d" or "-D"); empty defaults to "-d".
func WriteScript(path string, data []byte, appendMode bool, mode, decodeFlag string) string {
	redir := ">"
	if appendMode {
		redir = ">>"
	}
	if decodeFlag != "-d" && decodeFlag != "-D" {
		decodeFlag = "-d"
	}
	cmd := fmt.Sprintf("base64 %s %s %s 2>&1 <<'@AISH_EOF@'", decodeFlag, redir, Quote(path))
	if mode != "" {
		cmd += fmt.Sprintf(" && chmod %s %s 2>&1", mode, Quote(path))
	}
	return cmd + "\n" + wrap76(data) + "\n@AISH_EOF@"
}

func wrap76(data []byte) string {
	b64 := make([]byte, 0, len(data)*4/3+len(data)/57+4)
	enc := []byte(base64.StdEncoding.EncodeToString(data))
	for len(enc) > 76 {
		b64 = append(b64, enc[:76]...)
		b64 = append(b64, '\n')
		enc = enc[76:]
	}
	b64 = append(b64, enc...)
	return string(b64)
}
