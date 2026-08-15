package sshmux

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

const sessionAttemptDebounce = 500 * time.Millisecond

// ErrNewSessionsBlocked reports that the user has stopped aish from opening
// further SSH sessions for this aish session.
//
// The motivating case is a host with per-session MFA. One shared channel means
// one push per host, which is a fine price — but a confused AI can keep paying
// it: a forced re-probe, a deep or SFTP probe, a background command, or a
// channel that timed out and gets reopened all start a new slave session. Each
// one rings the user's phone. This is the stop button for that.
//
// It is a guardrail, not a boundary, and holds exactly the same status as the
// privilege-escalation refusal: it stops AISH from opening sessions. It cannot
// stop the human's own shell, and an AI that types `ssh` through run_command
// still reaches the host — visibly, in the shared terminal, which is the point.
var ErrNewSessionsBlocked = errors.New("new SSH sessions are blocked for this aish session")

// SessionAttemptKind describes why aish is opening a new SSH slave session.
// The value is user-facing in the status bar.
type SessionAttemptKind string

const (
	SessionAttemptShell      SessionAttemptKind = "OOB shell probe"
	SessionAttemptDeep       SessionAttemptKind = "deep identity probe"
	SessionAttemptBackground SessionAttemptKind = "background command"
	SessionAttemptSFTP       SessionAttemptKind = "SFTP subsystem"
)

// SessionAttempt is the oldest currently visible SSH slave-session attempt.
// Count includes any concurrent attempts hidden behind the same modal warning.
type SessionAttempt struct {
	Kind    SessionAttemptKind
	Host    string
	User    string
	Started time.Time
	Count   int
}

type sessionAttemptEntry struct {
	id uint64
	SessionAttempt
}

// SetSessionAttemptChanged installs the presentation callback used to repaint
// the status bar and title. It runs only when the debounced visible state
// changes, never for an attempt that finishes within the debounce window.
func (m *Mux) SetSessionAttemptChanged(fn func()) {
	m.attemptMu.Lock()
	m.attemptChanged = fn
	m.attemptMu.Unlock()
}

// SetBlockNewSessions turns the stop button on or off for this aish session.
// Memory-only and session-scoped by design: it is an in-the-moment reaction to
// a misbehaving client, not a policy worth persisting or scoping per host.
func (m *Mux) SetBlockNewSessions(on bool) {
	m.attemptMu.Lock()
	m.blockNew = on
	m.attemptMu.Unlock()
}

// NewSessionsBlocked reports whether the stop button is on.
func (m *Mux) NewSessionsBlocked() bool {
	m.attemptMu.Lock()
	defer m.attemptMu.Unlock()
	return m.blockNew
}

// blockedSessionError explains the refusal to whoever reads it. It names the
// operation and target, states plainly that retrying will not help, and points
// at the human — a model that treats this as a transient failure would spin.
func blockedSessionError(ci *ConnInfo, kind SessionAttemptKind) error {
	target := ci.Host
	if ci.User != "" {
		target = ci.User + "@" + target
	}
	return fmt.Errorf("%w: opening one for %s -> %s is refused because the user turned on the "+
		"Ctrl-] block after AISH opened too many sessions. Tools that use an already-open channel "+
		"still work. Retrying will not help and re-probing will not clear it; ask the user to lift "+
		"the block if this operation is genuinely necessary", ErrNewSessionsBlocked, string(kind), target)
}

// BeginSessionAttempt records a new ControlMaster slave session, or refuses it
// when the user has blocked new sessions. The returned function is idempotent
// and must be called when session startup has succeeded, failed, or timed out.
//
// Refusing HERE, rather than at each caller, is deliberate: every path that
// opens a slave session must register the attempt to appear in the status bar,
// so a path that skips this gate is already a bug that shows up as a missing
// 2FA warning. One choke point cannot be forgotten by a later caller.
func (m *Mux) BeginSessionAttempt(ci *ConnInfo, kind SessionAttemptKind) (func(), error) {
	if ci == nil {
		return func() {}, nil
	}

	m.attemptMu.Lock()
	if m.blockNew {
		m.attemptMu.Unlock()
		return nil, blockedSessionError(ci, kind)
	}
	m.attemptNext++
	id := m.attemptNext
	entry := sessionAttemptEntry{
		id: id,
		SessionAttempt: SessionAttempt{
			Kind: kind, Host: ci.Host, User: ci.User, Started: time.Now(),
		},
	}
	m.attempts[id] = entry
	if len(m.attempts) == 1 && !m.attemptVisible {
		debounce := m.attemptDebounce
		m.attemptTimer = time.AfterFunc(debounce, m.showSessionAttempts)
	}
	visible := m.attemptVisible
	changed := m.attemptChanged
	m.attemptMu.Unlock()
	if visible && changed != nil {
		changed()
	}

	var once sync.Once
	return func() {
		once.Do(func() { m.endSessionAttempt(id) })
	}, nil
}

func (m *Mux) showSessionAttempts() {
	m.attemptMu.Lock()
	m.attemptTimer = nil
	if len(m.attempts) == 0 || m.attemptVisible {
		m.attemptMu.Unlock()
		return
	}
	m.attemptVisible = true
	changed := m.attemptChanged
	m.attemptMu.Unlock()
	if changed != nil {
		changed()
	}
}

func (m *Mux) endSessionAttempt(id uint64) {
	m.attemptMu.Lock()
	if _, ok := m.attempts[id]; !ok {
		m.attemptMu.Unlock()
		return
	}
	delete(m.attempts, id)
	wasVisible := m.attemptVisible
	if len(m.attempts) == 0 {
		if m.attemptTimer != nil {
			m.attemptTimer.Stop()
			m.attemptTimer = nil
		}
		m.attemptVisible = false
	}
	changed := m.attemptChanged
	stillVisible := m.attemptVisible
	m.attemptMu.Unlock()
	if changed != nil && (wasVisible || stillVisible) {
		changed()
	}
}

// VisibleSessionAttempt returns the oldest active attempt after the debounce
// has elapsed. Before then it reports no activity, preserving the standard bar
// for fast non-MFA hosts.
func (m *Mux) VisibleSessionAttempt() (SessionAttempt, bool) {
	m.attemptMu.Lock()
	defer m.attemptMu.Unlock()
	if !m.attemptVisible || len(m.attempts) == 0 {
		return SessionAttempt{}, false
	}
	var oldest sessionAttemptEntry
	for _, entry := range m.attempts {
		if oldest.id == 0 || entry.id < oldest.id {
			oldest = entry
		}
	}
	result := oldest.SessionAttempt
	result.Count = len(m.attempts)
	return result, true
}
