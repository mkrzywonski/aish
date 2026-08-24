package aishwnd

import (
	"fmt"

	"ai-ssh/internal/aishwinwire"
)

// A single command can bury its answer. One `Get-Process` in an evaluation run
// returned roughly ninety near-identical access-denied traces around five
// useful rows, and the tool offered no way to bound that: the caller paid for
// the whole thing and had to find the answer inside it.
//
// Capping keeps BOTH ends. The head carries what a command announces about
// itself, and the tail carries its conclusion — which is where the useful rows
// were in that example, after all the noise. Cutting from the middle keeps the
// two parts most likely to matter and says exactly how much went missing,
// rather than silently returning a prefix that stops before the answer.

// maxOutputBytes bounds a single result's output, shared with the Windows side
// so the point at which output is spilled to a file and the point at which it
// is trimmed inline cannot drift apart.
//
// It was 64 KiB, which proved too generous: a capped 64 KiB result still
// exceeded what the MCP client would accept inline, so it was written to disk
// and had to be read back in pieces. A cap that lands just above the client's
// limit turns a large answer into a detour instead of an answer.
const maxOutputBytes = aishwinwire.MaxInlineOutput

// capOutput trims a ONE-SHOT result from the middle, keeping both ends. Use
// capOutputPrefix instead for anything a caller pages by cursor.
func capOutput(s string) (string, bool) {
	if len(s) <= maxOutputBytes {
		return s, false
	}
	half := maxOutputBytes / 2
	head := trimToRuneBoundary(s[:half])
	tail := trimFromRuneBoundary(s[len(s)-half:])
	omitted := len(s) - len(head) - len(tail)
	return fmt.Sprintf("%s\n\n[... %d bytes omitted from the middle; the start and end are shown ...]\n\n%s",
		head, omitted, tail), true
}

// trimToRuneBoundary drops a trailing partial UTF-8 sequence so the cut cannot
// produce invalid text.
func trimToRuneBoundary(s string) string {
	for i := len(s) - 1; i >= 0 && i > len(s)-4; i-- {
		if s[i]&0xC0 != 0x80 {
			if isRuneStartComplete(s[i:]) {
				return s
			}
			return s[:i]
		}
	}
	return s
}

// trimFromRuneBoundary drops a leading continuation byte for the same reason.
func trimFromRuneBoundary(s string) string {
	for i := 0; i < len(s) && i < 4; i++ {
		if s[i]&0xC0 != 0x80 {
			return s[i:]
		}
	}
	return s
}

// isRuneStartComplete reports whether s begins with a complete encoding.
func isRuneStartComplete(s string) bool {
	if len(s) == 0 {
		return false
	}
	switch {
	case s[0] < 0x80:
		return true
	case s[0]&0xE0 == 0xC0:
		return len(s) >= 2
	case s[0]&0xF0 == 0xE0:
		return len(s) >= 3
	case s[0]&0xF8 == 0xF0:
		return len(s) >= 4
	}
	return true
}

// capOutputPrefix trims a CURSOR-PAGED stream, returning a leading slice and
// how many bytes it consumed.
//
// Middle-out trimming is right for a one-shot result and wrong here: task
// output is addressed by byte offset, so cutting the middle while advancing
// the cursor past the whole chunk destroyed the omitted bytes permanently --
// the caller could never page back to them. A prefix plus an honest cursor
// bounds each response and loses nothing, because the rest is still there on
// the next poll.
func capOutputPrefix(s string) (string, int, bool) {
	if len(s) <= maxOutputBytes {
		return s, len(s), false
	}
	cut := trimToRuneBoundary(s[:maxOutputBytes])
	return cut, len(cut), true
}
