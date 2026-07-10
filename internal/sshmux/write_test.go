package sshmux

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// runScript executes an AtomicWriteScript through /bin/sh (as the OOB channel's
// `sh -s` would) and returns the exit code. The script is self-contained (the
// data rides along in its base64 heredoc).
func runScript(t *testing.T, script string) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		t.Fatalf("running script: %v (out: %s)", err, out)
	}
	return 0
}

func sha256Token(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func noLeftoverTemp(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if len(e.Name()) >= 8 && e.Name()[:8] == ".aishtmp" {
			t.Errorf("leftover temp file %q", e.Name())
		}
	}
}

func TestAtomicWriteNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")
	if code := runScript(t, AtomicWriteScript(WriteRequest{Path: path, Data: []byte("hello\n")})); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hello\n" {
		t.Fatalf("content = %q", got)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("new-file mode = %o, want 644", info.Mode().Perm())
	}
	noLeftoverTemp(t, dir)
}

func TestAtomicWritePreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runScript(t, AtomicWriteScript(WriteRequest{Path: path, Data: []byte("new")})); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Fatalf("content = %q", got)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600 (preserved)", info.Mode().Perm())
	}
}

func TestAtomicWriteExplicitMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.txt")
	if code := runScript(t, AtomicWriteScript(WriteRequest{Path: path, Data: []byte("x"), Mode: "0640"})); code != 0 {
		t.Fatalf("exit %d", code)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}
}

func TestAtomicWriteRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	code := runScript(t, AtomicWriteScript(WriteRequest{Path: link, Data: []byte("evil")}))
	if code != WriteExitSymlink {
		t.Fatalf("exit %d, want %d (symlink refused)", code, WriteExitSymlink)
	}
	// Neither the link nor its target should have changed.
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link is no longer a symlink")
	}
	if got, _ := os.ReadFile(target); string(got) != "original" {
		t.Fatalf("target content = %q, want original", got)
	}
	noLeftoverTemp(t, dir)
}

func TestAtomicWriteIfMatchSha256(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cas.txt")
	content := []byte("version one")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	// Matching token: swap succeeds.
	ok := AtomicWriteScript(WriteRequest{Path: path, Data: []byte("version two"), IfMatch: sha256Token(content), Hasher: "sha256sum"})
	if code := runScript(t, ok); code != 0 {
		t.Fatalf("matching CAS exit %d, want 0", code)
	}
	if got, _ := os.ReadFile(path); string(got) != "version two" {
		t.Fatalf("content = %q", got)
	}

	// Stale token (hash of the old content, but file now holds "version two").
	stale := AtomicWriteScript(WriteRequest{Path: path, Data: []byte("version three"), IfMatch: sha256Token(content), Hasher: "sha256sum"})
	if code := runScript(t, stale); code != WriteExitStale {
		t.Fatalf("stale CAS exit %d, want %d", code, WriteExitStale)
	}
	if got, _ := os.ReadFile(path); string(got) != "version two" {
		t.Fatalf("file changed under stale CAS: %q", got)
	}
	noLeftoverTemp(t, dir)
}

func TestAtomicWriteIfMatchMtimeSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ms.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	token := fmt.Sprintf("mtime-size:%d:%d", info.ModTime().Unix(), info.Size())
	if code := runScript(t, AtomicWriteScript(WriteRequest{Path: path, Data: []byte("defg"), IfMatch: token})); code != 0 {
		t.Fatalf("matching mtime-size CAS exit %d, want 0", code)
	}
	if got, _ := os.ReadFile(path); string(got) != "defg" {
		t.Fatalf("content = %q", got)
	}

	stale := fmt.Sprintf("mtime-size:%d:%d", info.ModTime().Unix(), int64(999))
	if code := runScript(t, AtomicWriteScript(WriteRequest{Path: path, Data: []byte("zzz"), IfMatch: stale})); code != WriteExitStale {
		t.Fatalf("stale mtime-size CAS exit %d, want %d", code, WriteExitStale)
	}
}

func TestSanitizeMode(t *testing.T) {
	for in, want := range map[string]string{
		"0644": "0644", "644": "644", "0640": "0640",
		"": "", "64": "", "07777": "", "6a4": "", "999": "", "rwx": "",
	} {
		if got := sanitizeMode(in); got != want {
			t.Errorf("sanitizeMode(%q) = %q, want %q", in, got, want)
		}
	}
}
