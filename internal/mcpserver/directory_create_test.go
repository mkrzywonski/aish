package mcpserver

import (
	"os"
	"path/filepath"
	"testing"
)

// mkdir -p semantics: the second call must succeed, and the result has to say
// which case it was rather than leaving the caller to guess.
func TestDirectoryExistsLocal(t *testing.T) {
	c := &Core{}
	rt := route{via: "local"}
	dir := t.TempDir()

	nested := filepath.Join(dir, "a", "b")
	if got, err := c.directoryExists(t.Context(), rt, nested); err != nil || got {
		t.Errorf("absent directory reported as existing (%v, %v)", got, err)
	}
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := c.directoryExists(t.Context(), rt, nested); err != nil || !got {
		t.Errorf("existing directory not detected (%v, %v)", got, err)
	}

	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.directoryExists(t.Context(), rt, file); err == nil {
		t.Error("a plain file in the way should be an error, not a silent success")
	}
}
