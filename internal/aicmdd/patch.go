package aicmdd

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The parsing/applying logic below is a direct port of the pure functions
// in internal/mcpserver/patch.go (hunk, hunkHeader, parseUnifiedDiff,
// applyUnifiedDiff, locateBlock, matchAt, patchMismatch, splitLines,
// joinLines) — copied rather than reimplemented so aicmdd's file_patch
// applies hunks with the exact same, already-tested logic as aish's own,
// not a fresh reimplementation that could reintroduce bugs aish already
// fixed. Not imported directly: it lives in package mcpserver alongside
// Core, and extracting a shared package would mean editing that existing,
// working file for this feature's sake — same reasoning as the duplicated
// auth state machine in auth.go. Keep this in sync by hand if
// internal/mcpserver/patch.go's algorithm ever changes.

// hunk is one @@ block: the old-side lines it expects (context + deletions)
// and the new-side lines it produces (context + additions), plus the old
// start line it claims (a hint; application also searches nearby).
type hunk struct {
	oldStart int // 1-based line number from the @@ header
	oldBlock []string
	newBlock []string
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// parseUnifiedDiff extracts the hunks from a unified diff. Preamble lines
// (diff/---/+++/index) before the first @@ are ignored; inside a hunk,
// lines are classified by their leading space/-/+ (a "\ No newline" marker
// is skipped).
func parseUnifiedDiff(patch string) ([]hunk, error) {
	var hunks []hunk
	cur := -1 // index into hunks of the hunk currently being filled, or -1
	for _, line := range strings.Split(strings.TrimRight(patch, "\n"), "\n") {
		if m := hunkHeader.FindStringSubmatch(line); m != nil {
			oldStart, _ := strconv.Atoi(m[1])
			hunks = append(hunks, hunk{oldStart: oldStart})
			cur = len(hunks) - 1
			continue
		}
		if cur < 0 {
			continue // preamble before the first hunk
		}
		if line == "" {
			// A bare empty line is an empty context line (git-apply leniency).
			hunks[cur].oldBlock = append(hunks[cur].oldBlock, "")
			hunks[cur].newBlock = append(hunks[cur].newBlock, "")
			continue
		}
		switch line[0] {
		case ' ':
			hunks[cur].oldBlock = append(hunks[cur].oldBlock, line[1:])
			hunks[cur].newBlock = append(hunks[cur].newBlock, line[1:])
		case '-':
			hunks[cur].oldBlock = append(hunks[cur].oldBlock, line[1:])
		case '+':
			hunks[cur].newBlock = append(hunks[cur].newBlock, line[1:])
		case '\\':
			// "\ No newline at end of file": no line content to add.
		default:
			// An unrecognized line ends the hunk (e.g. the next file's header).
			cur = -1
		}
	}
	if len(hunks) == 0 {
		return nil, errors.New("no @@ hunks found; provide a unified diff (lines like \"@@ -1,3 +1,4 @@\")")
	}
	return hunks, nil
}

// applyUnifiedDiff applies hunks to src in order and returns the patched
// bytes. Every hunk must match (fail closed): its old-side block is located
// at the header's line (or the nearest position at/after the previous
// hunk) and replaced by its new-side block. A hunk that doesn't match
// anywhere is an error with nearby context so the model can re-read and
// regenerate.
func applyUnifiedDiff(src []byte, hunks []hunk) ([]byte, error) {
	lines, finalNL := splitLines(src)
	var out []string
	cursor := 0
	for i, h := range hunks {
		pos, ok := locateBlock(lines, h.oldBlock, h.oldStart-1, cursor)
		if !ok {
			return nil, patchMismatch(i, h, lines)
		}
		out = append(out, lines[cursor:pos]...)
		out = append(out, h.newBlock...)
		cursor = pos + len(h.oldBlock)
	}
	out = append(out, lines[cursor:]...)
	return joinLines(out, finalNL), nil
}

// locateBlock finds where block occurs in lines: it prefers hint, then
// scans forward from min. A zero-length block (pure insertion) resolves to
// a clamped position. The returned position is always >= min so hunks stay
// ordered.
func locateBlock(lines []string, block []string, hint, min int) (int, bool) {
	if len(block) == 0 {
		p := hint
		if p < min {
			p = min
		}
		if p > len(lines) {
			p = len(lines)
		}
		return p, true
	}
	if hint >= min && matchAt(lines, block, hint) {
		return hint, true
	}
	for p := min; p+len(block) <= len(lines); p++ {
		if matchAt(lines, block, p) {
			return p, true
		}
	}
	return 0, false
}

func matchAt(lines, block []string, p int) bool {
	if p < 0 || p+len(block) > len(lines) {
		return false
	}
	for i, want := range block {
		if lines[p+i] != want {
			return false
		}
	}
	return true
}

func patchMismatch(i int, h hunk, lines []string) error {
	want := "(start of file)"
	if len(h.oldBlock) > 0 {
		want = strconv.Quote(h.oldBlock[0])
	}
	found := "(past end of file)"
	if idx := h.oldStart - 1; idx >= 0 && idx < len(lines) {
		found = strconv.Quote(lines[idx])
	}
	return fmt.Errorf("patch hunk %d did not apply: expected %s at/near line %d, but the file has %s there; re-read the file and regenerate the patch",
		i+1, want, h.oldStart, found)
}

func splitLines(b []byte) ([]string, bool) {
	s := string(b)
	if s == "" {
		return nil, false
	}
	finalNL := strings.HasSuffix(s, "\n")
	if finalNL {
		s = s[:len(s)-1]
	}
	return strings.Split(s, "\n"), finalNL
}

func joinLines(lines []string, finalNL bool) []byte {
	s := strings.Join(lines, "\n")
	if finalNL && len(lines) > 0 {
		s += "\n"
	}
	return []byte(s)
}
