package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func applyPatch(t *testing.T, src, patch string) (string, error) {
	t.Helper()
	hunks, err := parseUnifiedDiff(patch)
	if err != nil {
		return "", err
	}
	out, err := applyUnifiedDiff([]byte(src), hunks)
	return string(out), err
}

func TestApplyUnifiedDiffSingleHunk(t *testing.T) {
	src := "line one\nline two\nline three\n"
	patch := "@@ -1,3 +1,3 @@\n line one\n-line two\n+line TWO\n line three\n"
	got, err := applyPatch(t, src, patch)
	if err != nil {
		t.Fatal(err)
	}
	if want := "line one\nline TWO\nline three\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestApplyUnifiedDiffMultiHunk(t *testing.T) {
	src := "a\nb\nc\nd\ne\nf\ng\n"
	patch := "" +
		"@@ -1,2 +1,2 @@\n-a\n+A\n b\n" +
		"@@ -6,2 +6,2 @@\n f\n-g\n+G\n"
	got, err := applyPatch(t, src, patch)
	if err != nil {
		t.Fatal(err)
	}
	if want := "A\nb\nc\nd\ne\nf\nG\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestApplyUnifiedDiffInsertionAndDeletion(t *testing.T) {
	src := "keep\nremove me\nkeep2\n"
	// delete one line, add two around context
	patch := "@@ -1,3 +1,3 @@\n keep\n-remove me\n+added a\n+added b\n keep2\n"
	got, err := applyPatch(t, src, patch)
	if err != nil {
		t.Fatal(err)
	}
	if want := "keep\nadded a\nadded b\nkeep2\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestApplyUnifiedDiffDriftedLineNumbers(t *testing.T) {
	// The @@ header claims line 2, but the matching context is actually at line
	// 4. The applier should still locate it by content.
	src := "x\ny\nz\ntarget\nafter\n"
	patch := "@@ -2,2 +2,2 @@\n-target\n+TARGET\n after\n"
	got, err := applyPatch(t, src, patch)
	if err != nil {
		t.Fatal(err)
	}
	if want := "x\ny\nz\nTARGET\nafter\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestApplyUnifiedDiffMismatchFailsClosed(t *testing.T) {
	src := "alpha\nbeta\ngamma\n"
	patch := "@@ -1,2 +1,2 @@\n-NOT PRESENT\n+replacement\n beta\n"
	_, err := applyPatch(t, src, patch)
	if err == nil {
		t.Fatal("expected mismatch error")
	}
	if !strings.Contains(err.Error(), "hunk 1 did not apply") {
		t.Fatalf("error = %v", err)
	}
}

func TestApplyUnifiedDiffPreservesNoFinalNewline(t *testing.T) {
	src := "one\ntwo" // no trailing newline
	patch := "@@ -1,2 +1,2 @@\n one\n-two\n+TWO\n"
	got, err := applyPatch(t, src, patch)
	if err != nil {
		t.Fatal(err)
	}
	if want := "one\nTWO"; got != want {
		t.Fatalf("got %q, want %q (final newline should not appear)", got, want)
	}
}

func TestParseUnifiedDiffSkipsPreamble(t *testing.T) {
	patch := "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-old\n+new\n"
	hunks, err := parseUnifiedDiff(patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(hunks) != 1 || hunks[0].oldStart != 1 {
		t.Fatalf("hunks = %+v", hunks)
	}
	if len(hunks[0].oldBlock) != 1 || hunks[0].oldBlock[0] != "old" {
		t.Fatalf("oldBlock = %q", hunks[0].oldBlock)
	}
}

func TestFilePatchLocal(t *testing.T) {
	c := localOOBCore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "code.txt")
	if err := os.WriteFile(path, []byte("func main() {\n\tprintln(\"hi\")\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := "@@ -1,3 +1,3 @@\n func main() {\n-\tprintln(\"hi\")\n+\tprintln(\"hello\")\n }\n"
	_, res, err := c.filePatch(context.Background(), nil, filePatchArgs{Path: path, Patch: patch})
	if err != nil {
		t.Fatal(err)
	}
	if res.HunksApplied != 1 || res.Via != "local" {
		t.Fatalf("unexpected result: %+v", res)
	}
	got, _ := os.ReadFile(path)
	if want := "func main() {\n\tprintln(\"hello\")\n}\n"; string(got) != want {
		t.Fatalf("patched content = %q, want %q", got, want)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode changed to %o", fi.Mode().Perm())
	}
}
