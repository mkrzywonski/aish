package mcpserver

import (
	"strings"
	"testing"

	"ai-ssh/internal/outcap"
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

// Every path now trims at one shared constant, so agreement is structural
// rather than something to keep checking. What still needs asserting is that
// the value stays small enough to be returned inline: a cap above the client's
// own limit means a capped result is refused and written to disk, which is the
// detour the cap exists to prevent.
func TestInlineCapIsReturnableInline(t *testing.T) {
	if outcap.MaxInline > 32<<10 {
		t.Errorf("inline cap is %d bytes; results that large are refused inline", outcap.MaxInline)
	}
	if execOutputInline != outcap.MaxInline {
		t.Errorf("exec cap %d diverges from the shared constant %d", execOutputInline, outcap.MaxInline)
	}
}
