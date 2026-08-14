package sshmux

import (
	"sync"
	"time"
)

const sessionAttemptDebounce = 500 * time.Millisecond

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

// BeginSessionAttempt records a new ControlMaster slave session. The returned
// function is idempotent and must be called when session startup has succeeded,
// failed, or timed out.
func (m *Mux) BeginSessionAttempt(ci *ConnInfo, kind SessionAttemptKind) func() {
	if ci == nil {
		return func() {}
	}

	m.attemptMu.Lock()
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
	}
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
