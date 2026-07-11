package mcpserver

import (
	"strings"
	"testing"
)

// The snippet is injected with a single trailing CR, so it must be exactly one
// line — a stray newline would submit a partial command into the user's shell.
func TestTrackingSnippetShape(t *testing.T) {
	snip := trackingSnippet("deploy-web")
	if strings.ContainsAny(snip, "\n\r") {
		t.Fatal("trackingSnippet must be a single line (no embedded newline/CR)")
	}
	// Namespaced OSC 7 hook, and both shell backends wired up.
	for _, want := range []string{"__aish_osc7", "PROMPT_COMMAND", "precmd_functions", "BASH_VERSION", "ZSH_VERSION"} {
		if !strings.Contains(snip, want) {
			t.Errorf("trackingSnippet missing %q", want)
		}
	}
	// Idempotency guard present (won't re-add on a second run).
	if !strings.Contains(snip, "*__aish_osc7*") {
		t.Error("trackingSnippet should guard against double-adding to PROMPT_COMMAND")
	}
	// Visible badge: the label + glyph appear for both bash and zsh, in the
	// bracketed [user@host:cwd] form, with the magenta badge color.
	if !strings.Contains(snip, `deploy-web⧉`) {
		t.Error("trackingSnippet should embed the label badge")
	}
	for _, want := range []string{
		`PS1='\[\033[35m\]deploy-web⧉\[\033[0m\] [\[\033[01;32m\]\u@\h\[\033[0m\]:\[\033[01;34m\]\w\[\033[0m\]]\$ '`,
		`PS1='%F{magenta}deploy-web⧉%f [%B%F{green}%n@%m%f%b:%B%F{blue}%~%f%b]%# '`,
	} {
		if !strings.Contains(snip, want) {
			t.Errorf("trackingSnippet missing PS1 form %q", want)
		}
	}
	// PS1 assignments must sit OUTSIDE the eval bodies: a single-quoted PS1
	// nested inside a single-quoted eval body would break shell quoting. Each
	// PS1 is its own shell-guarded statement.
	for _, want := range []string{`$BASH_VERSION" ] && PS1=`, `$ZSH_VERSION" ] && PS1=`} {
		if !strings.Contains(snip, want) {
			t.Errorf("trackingSnippet PS1 should be a guarded statement outside eval: missing %q", want)
		}
	}
}
