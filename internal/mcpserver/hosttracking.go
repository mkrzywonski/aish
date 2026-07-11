package mcpserver

import (
	"time"

	"ai-ssh/internal/paths"
)

// Remote host tracking: on a remote whose shell has no OSC 7 integration, aish
// can't verify the interactive tty is on the same host its OOB channel targets
// (classifyConfidence returns "unknown"). This offers to install a tiny prompt
// hook on the remote interactive shell so it reports host+cwd via OSC 7 — after
// which the confidence reads "same" and writes proceed without a per-write
// confirm — and gives the remote shell aish's visible badge prompt so the user
// can see, at a glance, that this shell is the shared aish session.

// trackingSnippet builds a self-detecting, idempotent prompt hook injected
// (visibly, in band) into the remote interactive shell. __aish_osc7 emits OSC 7
// (host+cwd) each prompt; the guarded PS1 assignment sets aish's badge prompt
// "<label>⧉ [user@host:cwd]$" (magenta badge), with the session label baked in
// (the remote can't read $AISH_DIR/name, so a later rename won't follow). bash
// (PROMPT_COMMAND) and zsh (precmd_functions) are handled; the zsh array syntax
// is wrapped in eval so a POSIX sh/dash parses the whole line without a syntax
// error and just defines an unused function. The PS1 assignments live OUTSIDE
// the eval bodies (single-quoted strings can't nest) but are still shell-guarded
// so dash never adopts a bash/zsh prompt. Namespaced __aish_osc7 so re-running
// is a no-op; label is ValidName/id, so it's safe inside single quotes.
// ${HOSTNAME:-$HOST} covers bash ($HOSTNAME) and zsh ($HOST).
func trackingSnippet(label string) string {
	return `__aish_osc7(){ printf '\033]7;file://%s%s\033\\' "${HOSTNAME:-$HOST}" "$PWD"; }; ` +
		`[ -n "$BASH_VERSION" ] && eval 'case "$PROMPT_COMMAND" in *__aish_osc7*) ;; *) PROMPT_COMMAND="__aish_osc7${PROMPT_COMMAND:+; $PROMPT_COMMAND}";; esac'; ` +
		`[ -n "$BASH_VERSION" ] && PS1='\[\033[35m\]` + label + `⧉\[\033[0m\] [\u@\h:\w]\$ '; ` +
		`[ -n "$ZSH_VERSION" ] && eval 'typeset -ag precmd_functions; case " ${precmd_functions[*]} " in *" __aish_osc7 "*) ;; *) precmd_functions+=(__aish_osc7);; esac'; ` +
		`[ -n "$ZSH_VERSION" ] && PS1='%F{magenta}` + label + `⧉%f [%n@%m:%~]%# '`
}

// RemoteTrackingApplicable reports the OOB host and whether offering to set up
// remote host tracking makes sense: only on a live ControlMaster remote whose
// interactive host aish can't already confirm as "same". Non-prompting (uses
// capability(), not route()), so the menu can call it freely.
func (c *Core) RemoteTrackingApplicable() (host string, applicable bool) {
	rt := c.capability()
	if rt.via != "controlmaster" {
		return "", false
	}
	_, oobHost, confidence := c.hostConfidence(rt)
	return oobHost, confidence != "same"
}

// provisionIdle is how long the shared terminal must have been quiet before we
// type the tracking snippet. OSC 133 prompt marks (Tracker.PromptReady) are
// useless here — this feature exists precisely for remotes with no shell
// integration, where PromptReady is never true and, since ssh is the local
// foreground process, mode reads "running" — so output quiescence is the only
// available "at a prompt" proxy.
const provisionIdle = 750 * time.Millisecond

// ProvisionRemoteTracking injects the tracking snippet into the remote
// interactive shell so aish can verify its host and the shell shows aish's badge
// prompt. This is the one place aish deliberately types INTO the shared shell
// for the user's benefit — a VISIBLE, user-consented, in-band injection (never
// the invisible OOB channel). It refuses in a full-screen app, during secret
// (echo-off) input, or while output is still flowing, so it can't corrupt a
// running command or land in a password prompt; it announces itself. Returns
// whether it injected.
func (c *Core) ProvisionRemoteTracking() bool {
	idle := time.Duration(0)
	if n := c.Sess.LastOutputNanos(); n > 0 {
		idle = time.Since(time.Unix(0, n))
	}
	if c.Term.Screen.Snapshot().AltScreen || c.Tracker.EchoOff() || idle < provisionIdle {
		c.Sess.Notify("can't set up the aish prompt now — the shell looks busy; try again from a quiet prompt")
		return false
	}
	label := paths.ReadName(c.Sess.ID)
	if label == "" {
		label = c.Sess.ID
	}
	c.Sess.Notify("setting up the aish prompt + host tracking on the remote shell (a visible one-time command); takes effect at the next prompt")
	if _, err := c.Sess.WriteInput([]byte(trackingSnippet(label) + "\r")); err != nil {
		c.Sess.Notify("aish prompt setup failed: %v", err)
		return false
	}
	return true
}
