// Persistent OOB channel: one long-lived `sh -s` opened over the existing
// ControlMaster connection, through which all foreground exec and file
// operations for that remote are streamed. On hosts where every new ssh
// session/channel re-triggers MFA (login_duo-style Duo pushes), this costs
// exactly one authorization at open instead of one per operation. See
// oob.md for the validation.
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

var errChannelDead = errors.New("channel dead")

const chanOutputCap = 64 << 20 // hard cap on one op's collected output

// minOpenTimeout: the first op on a fresh channel may sit behind a human
// approving an MFA push; killing the channel too early would burn the push
// and cost another on retry.
const minOpenTimeout = 120 * time.Second

type channel struct {
	mu    sync.Mutex
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan []byte
	dead  bool
	// caps holds the host capabilities probed once on first use. Stored via an
	// atomic pointer so session_status can read it without taking ch.mu (which
	// a long-running op holds for its whole duration).
	caps atomic.Pointer[Capabilities]
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
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ch := &channel{cmd: cmd, stdin: stdin, lines: make(chan []byte, 256)}
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
				cmd.Wait()
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

// ChannelRun runs script over the persistent channel for ci, opening it on
// first use. A dead channel is removed and reported — the caller's retry is
// the consent for a fresh open (which may trigger an MFA push).
func (m *Mux) ChannelRun(ci *ConnInfo, script string, timeout time.Duration) (*ChanResult, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	m.chMu.Lock()
	ch := m.channels[ci.Sock]
	opened := false
	if ch == nil {
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

	if opened {
		// The first op on a fresh channel may sit behind an MFA push (it opens a
		// new session on the master). Run the capability probe as that first op:
		// it absorbs the push wait with the long open timeout and caches host
		// facts for session_status. Best-effort — a failure just leaves caps
		// unset and the real op below surfaces any hard channel error.
		m.probeChannel(ch)
	}
	res, err := ch.run(script, timeout)
	if errors.Is(err, errChannelDead) {
		m.chMu.Lock()
		if m.channels[ci.Sock] == ch {
			delete(m.channels, ci.Sock)
		}
		m.chMu.Unlock()
		return nil, fmt.Errorf("the persistent oob channel to %s was lost; retrying will open a new one (on MFA-protected hosts that triggers one push)", ci.Host)
	}
	if res != nil && res.TimedOut {
		// run() killed the channel; drop it so a retry starts fresh.
		m.chMu.Lock()
		if m.channels[ci.Sock] == ch {
			delete(m.channels, ci.Sock)
		}
		m.chMu.Unlock()
	}
	return res, err
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
// channel. base64's alphabet cannot contain '@', so the static marker is
// collision-free.
func WriteScript(path string, data []byte, appendMode bool, mode string) string {
	redir := ">"
	if appendMode {
		redir = ">>"
	}
	cmd := fmt.Sprintf("base64 -d %s %s 2>&1 <<'@AISH_EOF@'", redir, Quote(path))
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
