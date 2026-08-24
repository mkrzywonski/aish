package aishwnd

import (
	"strings"
	"testing"
)

func TestCapOutputLeavesSmallOutputAlone(t *testing.T) {
	in := "alpha\nbeta\n"
	got, truncated := capOutput(in)
	if got != in || truncated {
		t.Errorf("short output was altered: %q truncated=%v", got, truncated)
	}
}

// The answer is often the last thing a command prints -- in the case that
// motivated this, five useful rows arrived after ninety error traces.
func TestCapOutputKeepsBothEnds(t *testing.T) {
	head := "START-MARKER"
	tail := "END-MARKER"
	in := head + strings.Repeat("x", maxOutputBytes*2) + tail
	got, truncated := capOutput(in)
	if !truncated {
		t.Fatal("oversized output was not marked truncated")
	}
	if !strings.Contains(got, head) {
		t.Error("the start of the output was lost")
	}
	if !strings.Contains(got, tail) {
		t.Error("the end of the output was lost -- that is usually the answer")
	}
	if !strings.Contains(got, "omitted from the middle") {
		t.Error("truncation is not announced in the text")
	}
	if len(got) > maxOutputBytes+200 {
		t.Errorf("capped output is %d bytes, cap is %d", len(got), maxOutputBytes)
	}
}

func TestCapOutputDoesNotSplitRunes(t *testing.T) {
	in := strings.Repeat("é", maxOutputBytes)
	got, truncated := capOutput(in)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if strings.ContainsRune(got, '�') {
		t.Error("cut produced an invalid UTF-8 sequence")
	}
}
