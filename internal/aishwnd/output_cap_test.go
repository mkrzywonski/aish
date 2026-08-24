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

// Middle-out trimming on a cursor-paged stream destroyed the omitted bytes:
// the cursor advanced past the whole chunk, so the caller could never page
// back to what was cut. A prefix plus an honest cursor loses nothing.
func TestCapOutputPrefixIsLossless(t *testing.T) {
	full := strings.Repeat("a", maxOutputBytes) + "TAIL-THAT-MUST-SURVIVE"
	first, consumed, truncated := capOutputPrefix(full)
	if !truncated {
		t.Fatal("oversized chunk was not marked truncated")
	}
	if consumed != len(first) {
		t.Errorf("consumed %d but returned %d bytes", consumed, len(first))
	}
	if strings.Contains(first, "TAIL-THAT-MUST-SURVIVE") {
		t.Error("prefix should stop before the tail, leaving it for the next poll")
	}
	// The remainder a caller would receive next, addressed from the cursor.
	rest := full[consumed:]
	if !strings.Contains(rest, "TAIL-THAT-MUST-SURVIVE") {
		t.Error("the tail is unreachable: bytes were lost between pages")
	}
	if len(first)+len(rest) != len(full) {
		t.Errorf("paging is lossy: %d + %d != %d", len(first), len(rest), len(full))
	}
}

func TestCapOutputPrefixLeavesSmallChunksAlone(t *testing.T) {
	in := "short output\n"
	got, consumed, truncated := capOutputPrefix(in)
	if got != in || truncated || consumed != len(in) {
		t.Errorf("small chunk altered: %q consumed=%d truncated=%v", got, consumed, truncated)
	}
}

func TestInlineCapIsSmallEnoughToReturn(t *testing.T) {
	if maxOutputBytes > 32*1024 {
		t.Errorf("inline cap is %d bytes; a capped result that the client refuses inline defeats the cap", maxOutputBytes)
	}
}
