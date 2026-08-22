package aicmdd

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"ai-ssh/internal/aishwinwire"
	"ai-ssh/internal/proxy"
)

// TestRunRegistersDiscoverableSession proves the core Stage 1 claim from the
// plan doc: a Windows peer's stdio hello handshake produces a session
// directory that internal/proxy discovers with zero changes to that package
// — and that closing the input pipe (simulating the parent process exiting,
// as it would when aishwin.exe's child aicmdd loses its stdin) tears the
// session back down.
func TestRunRegistersDiscoverableSession(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	inRead, inWrite := io.Pipe()
	out := &syncBuffer{}

	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), inRead, out)
	}()

	helloPayload, err := json.Marshal(aishwinwire.HelloData{Proto: aishwinwire.ProtoVersion, Name: "win-test", Shell: "cmd"})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(aishwinwire.Frame{Type: "hello", Data: helloPayload})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inWrite.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "session to appear in proxy.List()", func() bool {
		sessions := proxy.List()
		return len(sessions) == 1 && sessions[0].Name == "win-test"
	})

	if err := inWrite.Close(); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "session to be cleaned up after the stdio link closed", func() bool {
		return len(proxy.List()) == 0
	})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned an unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its stdin closed")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// syncBuffer is a minimal concurrency-safe io.Writer sink standing in for
// aicmdd's stdout in tests that don't need to inspect the frames it sends.
type syncBuffer struct {
	mu sync.Mutex
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(p), nil
}
