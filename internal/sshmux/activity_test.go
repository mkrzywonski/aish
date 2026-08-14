package sshmux

import (
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

	finish := m.BeginSessionAttempt(ci, SessionAttemptDeep)
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

	finish := m.BeginSessionAttempt(testConn(), SessionAttemptShell)
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

	first := m.BeginSessionAttempt(testConn(), SessionAttemptDeep)
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("first attempt did not become visible")
	}
	second := m.BeginSessionAttempt(&ConnInfo{Host: "other", User: "mike"}, SessionAttemptBackground)
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
