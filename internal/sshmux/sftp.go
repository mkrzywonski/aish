package sshmux

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
)

const sftpProbeTimeout = 60 * time.Second

var knownSFTPExtensions = []string{
	"copy-data",
	"expand-path@openssh.com",
	"fsync@openssh.com",
	"fstatvfs@openssh.com",
	"hardlink@openssh.com",
	"home-directory",
	"limits@openssh.com",
	"posix-rename@openssh.com",
	"statvfs@openssh.com",
}

type SFTPProbeResult struct {
	Axis   SftpAxis
	Cached bool
}

type sftpSession struct {
	client *sftp.Client
	cmd    *exec.Cmd
	done   chan error
	once   sync.Once
}

func (s *sftpSession) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		// Killing the slave first guarantees the client's receive loop sees EOF;
		// Client.Close waits for that loop and could otherwise block indefinitely.
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		if s.client != nil {
			_ = s.client.Close()
		}
		if s.done != nil {
			select {
			case <-s.done:
			case <-time.After(2 * time.Second):
			}
		}
	})
}

type sftpProbeRunner func(context.Context, *ConnInfo) (*sftpSession, SftpAxis)

type sftpFlight struct {
	done   chan struct{}
	result SFTPProbeResult
}

func sftpPathStyle(path string) string {
	if len(path) >= 4 && path[0] == '/' && ((path[1] >= 'A' && path[1] <= 'Z') || (path[1] >= 'a' && path[1] <= 'z')) && path[2] == ':' && path[3] == '/' {
		return "windows"
	}
	if strings.HasPrefix(path, "/") {
		return "posix"
	}
	return "unknown"
}

func sftpExtensions(client *sftp.Client) []string {
	extensions := make([]string, 0, len(knownSFTPExtensions))
	for _, name := range knownSFTPExtensions {
		if _, ok := client.HasExtension(name); ok {
			extensions = append(extensions, name)
		}
	}
	sort.Strings(extensions)
	return extensions
}

func sftpProbeFailure(prefix string, err error, stderr *boundedBuf, attemptedAt time.Time) SftpAxis {
	reason := prefix
	if line := firstLine(string(stderr.Bytes())); line != "" {
		reason += ": " + line
	} else if err != nil {
		reason += ": " + err.Error()
	}
	return SftpAxis{State: AxisDown, Reason: reason, ProbedAt: attemptedAt}
}

func sftpStartupFailure(parent context.Context, timedOut bool, timeout time.Duration, stage string, err error, stderr *boundedBuf, attemptedAt time.Time) SftpAxis {
	switch {
	case timedOut:
		return sftpProbeFailure(fmt.Sprintf("SFTP startup timed out during %s (maximum duration %s)", stage, timeout), err, stderr, attemptedAt)
	case errors.Is(parent.Err(), context.DeadlineExceeded):
		return sftpProbeFailure("SFTP startup deadline expired during "+stage, parent.Err(), stderr, attemptedAt)
	case parent.Err() != nil:
		return sftpProbeFailure("SFTP startup was canceled during "+stage, parent.Err(), stderr, attemptedAt)
	default:
		return sftpProbeFailure("SFTP "+stage+" failed", err, stderr, attemptedAt)
	}
}

func (m *Mux) runSFTPProbe(parent context.Context, ci *ConnInfo) (*sftpSession, SftpAxis) {
	attemptedAt := time.Now()
	stderr := &boundedBuf{cap: deepProbeOutputCap}
	cmd := m.SubsystemCommand(ci, "sftp")
	cmd.Stderr = stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, sftpProbeFailure("could not create SFTP input pipe", err, stderr, attemptedAt)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, sftpProbeFailure("could not create SFTP output pipe", err, stderr, attemptedAt)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, sftpProbeFailure("could not start the SFTP subsystem", err, stderr, attemptedAt)
	}

	session := &sftpSession{cmd: cmd, done: make(chan error, 1)}
	go func() {
		session.done <- cmd.Wait()
		close(session.done)
	}()

	var startupComplete atomic.Bool
	var startupTimedOut atomic.Bool
	timeout := m.sftpTimeout
	if timeout <= 0 {
		timeout = sftpProbeTimeout
	}
	timer := time.NewTimer(timeout)
	abortDone := make(chan struct{})
	go func() {
		select {
		case <-parent.Done():
		case <-timer.C:
			startupTimedOut.Store(true)
		case <-abortDone:
			return
		}
		if !startupComplete.Load() && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()
	finishStartup := func() {
		startupComplete.Store(true)
		if timer.Stop() {
			close(abortDone)
		}
	}

	client, err := sftp.NewClientPipe(stdout, stdin)
	if err != nil {
		finishStartup()
		session.Close()
		return nil, sftpStartupFailure(parent, startupTimedOut.Load(), timeout, "handshake", err, stderr, attemptedAt)
	}
	session.client = client
	realPath, err := client.RealPath(".")
	if err != nil {
		finishStartup()
		session.Close()
		return nil, sftpStartupFailure(parent, startupTimedOut.Load(), timeout, "realpath(.)", err, stderr, attemptedAt)
	}
	finishStartup()

	return session, SftpAxis{
		State:      AxisUp,
		RealPath:   realPath,
		PathStyle:  sftpPathStyle(realPath),
		Extensions: sftpExtensions(client),
		ProbedAt:   attemptedAt,
	}
}

// ProbeSFTP opens at most one subsystem per target at a time. Success and
// failure are both cached; force closes any retained client and resets only the
// SFTP axis and SFTP-derived identity.
func (m *Mux) ProbeSFTP(ctx context.Context, ci *ConnInfo, force bool) (SFTPProbeResult, error) {
	if ci == nil || ci.Sock == "" {
		return SFTPProbeResult{}, errors.New("SFTP probing requires a ControlMaster target")
	}

	m.sftpMu.Lock()
	if flight := m.sftpFlights[ci.Sock]; flight != nil {
		m.sftpMu.Unlock()
		select {
		case <-flight.done:
			result := flight.result
			result.Cached = true
			return result, nil
		case <-ctx.Done():
			return SFTPProbeResult{}, ctx.Err()
		}
	}
	if !force {
		if axis, ok := m.CachedSFTPProbe(ci); ok {
			m.sftpMu.Unlock()
			return SFTPProbeResult{Axis: axis, Cached: true}, nil
		}
	}
	flight := &sftpFlight{done: make(chan struct{})}
	m.sftpFlights[ci.Sock] = flight
	oldSession := m.sftpSessions[ci.Sock]
	delete(m.sftpSessions, ci.Sock)
	m.sftpMu.Unlock()

	if oldSession != nil {
		oldSession.Close()
	}
	if force {
		m.forgetSftpFacts(ci)
	}

	finishAttempt := m.BeginSessionAttempt(ci, SessionAttemptSFTP)
	var session *sftpSession
	var axis SftpAxis
	func() {
		defer finishAttempt()
		session, axis = m.sftpRun(ctx, ci)
	}()
	if axis.State == AxisUnknown {
		axis.State = AxisDown
		if axis.Reason == "" {
			axis.Reason = "SFTP probe returned no capability state"
		}
	}
	if axis.ProbedAt.IsZero() {
		axis.ProbedAt = time.Now()
	}
	axis = m.noteSFTPProbe(ci, axis)
	result := SFTPProbeResult{Axis: axis}

	m.sftpMu.Lock()
	if session != nil && axis.State == AxisUp {
		m.sftpSessions[ci.Sock] = session
	}
	flight.result = result
	delete(m.sftpFlights, ci.Sock)
	close(flight.done)
	m.sftpMu.Unlock()
	if session != nil && axis.State != AxisUp {
		session.Close()
	}
	return result, nil
}

func (m *Mux) closeSFTPSessions() {
	m.sftpMu.Lock()
	sessions := make([]*sftpSession, 0, len(m.sftpSessions))
	for sock, session := range m.sftpSessions {
		sessions = append(sessions, session)
		delete(m.sftpSessions, sock)
	}
	m.sftpMu.Unlock()
	for _, session := range sessions {
		session.Close()
	}
}
