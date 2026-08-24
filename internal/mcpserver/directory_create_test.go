package mcpserver

import (
	"os"
	"path/filepath"
	"strings"
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

// directory_create was added to the remote probe map but not to the canonical
// primitive list, so every other route reported it unavailable — with a message
// that ran out mid-sentence because it had no missing dependency to name.
func TestDirectoryCreateIsAKnownPrimitive(t *testing.T) {
	found := false
	for _, n := range oobToolNames {
		if n == "directory_create" {
			found = true
		}
	}
	if !found {
		t.Fatal("directory_create missing from oobToolNames; routes that build from it will refuse the tool")
	}
	c := &Core{}
	if av := c.oobToolAvailability(route{via: "local"}); !av["directory_create"].Available() {
		t.Errorf("directory_create unavailable on the local route: %+v", av["directory_create"])
	}
}

// A tool with no specific missing dependency must still produce a whole
// sentence.
func TestRequireToolMessageIsCompleteWithoutAMissingField(t *testing.T) {
	c := &Core{}
	err := c.requireTool(route{via: "in_band", host: "somehost"}, "directory_create")
	if err == nil {
		t.Skip("in_band allows directory_create on this dialect; nothing to check")
	}
	if strings.HasSuffix(err.Error(), "it needs") || strings.HasSuffix(err.Error(), "it needs ") {
		t.Errorf("error stops mid-sentence: %q", err)
	}
}
