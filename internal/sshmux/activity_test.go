package sshmux

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionAttemptDebounceAndRestore(t *testing.T) {
	m := New(t.TempDir())
	m.attemptDebounce = 20 * time.Millisecond
	ci := testConn()
	changed := make(chan struct{}, 4)
	m.SetSessionAttemptChanged(func() { changed <- struct{}{} })

	finish, _ := m.BeginSessionAttempt(ci, SessionAttemptDeep)
	if _, ok := m.VisibleSessionAttempt(); ok {
		t.Fatal("attempt became visible before debounce")
	}
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("attempt did not become visible")
	}
	got, ok := m.VisibleSessionAttempt()
	if !ok || got.Kind != SessionAttemptDeep || got.Host != ci.Host || got.User != ci.User || got.Count != 1 {
		t.Fatalf("visible attempt = %+v, ok=%v", got, ok)
	}

	finish()
	finish() // completion is idempotent
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("attempt completion did not notify")
	}
	if _, ok := m.VisibleSessionAttempt(); ok {
		t.Fatal("attempt remained visible after completion")
	}
}

func TestFastSessionAttemptNeverBecomesVisible(t *testing.T) {
	m := New(t.TempDir())
	m.attemptDebounce = 30 * time.Millisecond
	var changes atomic.Int32
	m.SetSessionAttemptChanged(func() { changes.Add(1) })

	finish, _ := m.BeginSessionAttempt(testConn(), SessionAttemptShell)
	finish()
	time.Sleep(60 * time.Millisecond)
	if changes.Load() != 0 {
		t.Fatalf("fast attempt generated %d visible changes", changes.Load())
	}
	if _, ok := m.VisibleSessionAttempt(); ok {
		t.Fatal("fast completed attempt became visible")
	}
}

func TestConcurrentSessionAttemptsAreReferenceCounted(t *testing.T) {
	m := New(t.TempDir())
	m.attemptDebounce = 10 * time.Millisecond
	changed := make(chan struct{}, 8)
	m.SetSessionAttemptChanged(func() { changed <- struct{}{} })

	first, _ := m.BeginSessionAttempt(testConn(), SessionAttemptDeep)
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("first attempt did not become visible")
	}
	second, _ := m.BeginSessionAttempt(&ConnInfo{Host: "other", User: "mike"}, SessionAttemptBackground)
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("concurrent attempt did not update visible state")
	}
	if got, _ := m.VisibleSessionAttempt(); got.Count != 2 || got.Kind != SessionAttemptDeep {
		t.Fatalf("concurrent state = %+v", got)
	}

	first()
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("first completion did not update visible state")
	}
	if got, ok := m.VisibleSessionAttempt(); !ok || got.Count != 1 || got.Kind != SessionAttemptBackground {
		t.Fatalf("remaining state = %+v, ok=%v", got, ok)
	}
	second()
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("final completion did not restore hidden state")
	}
}

// The block refuses new sessions and, just as importantly, does not register an
// attempt: a refused session never rang the phone, so it must not raise the 2FA
// warning either.
func TestBlockRefusesNewSessionsWithoutWarning(t *testing.T) {
	m := New(t.TempDir())
	ci := testConn()

	m.SetBlockNewSessions(true)
	if !m.NewSessionsBlocked() {
		t.Fatal("block did not take effect")
	}
	finish, err := m.BeginSessionAttempt(ci, SessionAttemptShell)
	if !errors.Is(err, ErrNewSessionsBlocked) {
		t.Fatalf("BeginSessionAttempt error = %v, want ErrNewSessionsBlocked", err)
	}
	if finish != nil {
		t.Error("a refused attempt returned a finish function")
	}
	if _, visible := m.VisibleSessionAttempt(); visible {
		t.Error("a refused attempt raised the 2FA warning")
	}

	// The message has to stop a model from treating this as transient.
	for _, want := range []string{"Ctrl-]", "Retrying will not help", "already-open channel"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("blocked error missing %q: %v", want, err)
		}
	}

	m.SetBlockNewSessions(false)
	finish, err = m.BeginSessionAttempt(ci, SessionAttemptShell)
	if err != nil || finish == nil {
		t.Fatalf("attempt after unblocking = %v", err)
	}
	finish()
}

func TestBlockCoversEveryAttemptKind(t *testing.T) {
	m := New(t.TempDir())
	m.SetBlockNewSessions(true)
	for _, kind := range []SessionAttemptKind{
		SessionAttemptShell, SessionAttemptDeep, SessionAttemptSFTP, SessionAttemptBackground,
	} {
		if _, err := m.BeginSessionAttempt(testConn(), kind); !errors.Is(err, ErrNewSessionsBlocked) {
			t.Errorf("%s was not blocked: %v", kind, err)
		}
	}
}

// ChannelRun must refuse before spawning ssh, and must not leave a channel
// behind for a session it never opened.
func TestBlockStopsChannelOpen(t *testing.T) {
	m := New(t.TempDir())
	m.SetBlockNewSessions(true)
	ci := testConn()

	_, err := m.ChannelRun(ci, "echo hi", time.Second)
	if !errors.Is(err, ErrNewSessionsBlocked) {
		t.Fatalf("ChannelRun error = %v, want ErrNewSessionsBlocked", err)
	}
	m.chMu.Lock()
	n := len(m.channels)
	m.chMu.Unlock()
	if n != 0 {
		t.Errorf("a blocked run left %d channel(s) behind", n)
	}
}
