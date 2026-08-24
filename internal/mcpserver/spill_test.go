package mcpserver

import (
	"strings"
	"testing"
)

// exec has no scrollback behind it, so a trimmed result must still say the
// full text exists somewhere and keep the command's conclusion.
func TestCapExecOutputKeepsBothEndsAndPointsAtTheFile(t *testing.T) {
	full := []byte("HEAD-MARKER" + strings.Repeat("x", execOutputInline*2) + "TAIL-MARKER")
	got, truncated := capExecOutput(full)
	if !truncated {
		t.Fatal("oversized output not marked truncated")
	}
	if !strings.Contains(got, "HEAD-MARKER") || !strings.Contains(got, "TAIL-MARKER") {
		t.Error("both ends must survive; the conclusion is usually the last line")
	}
	if !strings.Contains(got, "output_path") {
		t.Error("the notice must say where the full output went")
	}
	if len(got) > execOutputInline+256 {
		t.Errorf("trimmed output is %d bytes, cap is %d", len(got), execOutputInline)
	}
}

func TestCapExecOutputLeavesSmallOutputAlone(t *testing.T) {
	in := []byte("mike\nLinux 6.1.0\n")
	got, truncated := capExecOutput(in)
	if got != string(in) || truncated {
		t.Errorf("small output altered: %q truncated=%v", got, truncated)
	}
}

func TestCapExecOutputDoesNotSplitRunes(t *testing.T) {
	full := []byte(strings.Repeat("é", execOutputInline))
	got, _ := capExecOutput(full)
	if strings.ContainsRune(got, '�') {
		t.Error("cut produced invalid UTF-8")
	}
}

// The Windows side and this one must trim at the same size, or output spills
// to a file at one threshold and is trimmed at another.
func TestInlineCapsAgreeAcrossBackends(t *testing.T) {
	if execOutputInline != 16<<10 {
		t.Errorf("exec inline cap is %d; the direct_host backend uses 16 KiB", execOutputInline)
	}
}
