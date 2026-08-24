package paths

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestBasePrefersXDG(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/aish-xdg")
	if got, want := Base(), "/tmp/aish-xdg/aish"; got != want {
		t.Fatalf("Base() = %q, want %q", got, want)
	}
}

func TestBaseFallsBackToRunUser(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	runUser := filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	info, err := os.Stat(runUser)
	if err != nil || !info.IsDir() {
		t.Skipf("%s unavailable on this host", runUser)
	}
	if got, want := Base(), filepath.Join(runUser, "aish"); got != want {
		t.Fatalf("Base() = %q, want %q", got, want)
	}
}

// The kind file is how a reader learns what a session is without connecting to
// it — no socket, no approval prompt, no MFA push.
func TestKindRoundTrip(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	id := "deadbeef"
	if err := os.MkdirAll(SessionDir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := ReadKind(id); got != "" {
		t.Errorf("a session with no kind file should read as unknown, got %q", got)
	}
	if err := WriteKind(id, KindAishwin); err != nil {
		t.Fatal(err)
	}
	if got := ReadKind(id); got != KindAishwin {
		t.Errorf("ReadKind = %q, want %q", got, KindAishwin)
	}
}
