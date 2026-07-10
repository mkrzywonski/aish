package mcpserver

import (
	"strings"
	"testing"
)

// The snippet is injected with a single trailing CR, so it must be exactly one
// line — a stray newline would submit a partial command into the user's shell.
func TestOSC7SnippetShape(t *testing.T) {
	if strings.ContainsAny(osc7Snippet, "\n\r") {
		t.Fatal("osc7Snippet must be a single line (no embedded newline/CR)")
	}
	// Namespaced hook, and both shell backends wired up.
	for _, want := range []string{"__aish_osc7", "PROMPT_COMMAND", "precmd_functions", "BASH_VERSION", "ZSH_VERSION"} {
		if !strings.Contains(osc7Snippet, want) {
			t.Errorf("osc7Snippet missing %q", want)
		}
	}
	// Idempotency guard present (won't re-add on a second run).
	if !strings.Contains(osc7Snippet, "*__aish_osc7*") {
		t.Error("osc7Snippet should guard against double-adding to PROMPT_COMMAND")
	}
}
