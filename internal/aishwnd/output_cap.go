package aishwnd

import "fmt"

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

// maxOutputBytes bounds a single result's output. Generous enough for ordinary
// command output, small enough that one noisy call cannot flood a caller.
const maxOutputBytes = 64 * 1024

// capOutput returns the output to report and whether it was shortened.
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
