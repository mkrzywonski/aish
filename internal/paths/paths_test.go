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
