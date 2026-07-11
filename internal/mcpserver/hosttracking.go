package mcpserver

import "time"

// Remote host tracking: on a remote whose shell has no OSC 7 integration, aish
// can't verify the interactive tty is on the same host its OOB channel targets
// (classifyConfidence returns "unknown"). This offers to install a tiny prompt
// hook on the remote interactive shell so it reports host+cwd via OSC 7 — after
// which the confidence reads "same" and writes proceed without a per-write
// confirm.

// osc7Snippet is a self-detecting, idempotent prompt hook injected (visibly, in
// band) into the remote interactive shell so it emits OSC 7 each prompt. bash
// (PROMPT_COMMAND) and zsh (precmd_functions) are handled; the shell-specific
// syntax is wrapped in eval so a POSIX sh/dash parses the whole line without a
// syntax error and simply defines an unused function (graceful no-op).
// Namespaced __aish_osc7 so it can't clobber a user function; re-running is a
// no-op. ${HOSTNAME:-$HOST} covers bash ($HOSTNAME) and zsh ($HOST).
const osc7Snippet = `__aish_osc7(){ printf '\033]7;file://%s%s\033\\' "${HOSTNAME:-$HOST}" "$PWD"; }; [ -n "$BASH_VERSION" ] && eval 'case "$PROMPT_COMMAND" in *__aish_osc7*) ;; *) PROMPT_COMMAND="__aish_osc7${PROMPT_COMMAND:+; $PROMPT_COMMAND}";; esac'; [ -n "$ZSH_VERSION" ] && eval 'typeset -ag precmd_functions; case " ${precmd_functions[*]} " in *" __aish_osc7 "*) ;; *) precmd_functions+=(__aish_osc7);; esac'`

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

// ProvisionRemoteTracking injects osc7Snippet into the remote interactive shell
// so aish can verify its host. This is the one place aish deliberately types
// INTO the shared shell for the user's benefit — a VISIBLE, user-consented, in
// band injection (never the invisible OOB channel). It refuses in a full-screen
// app, during secret (echo-off) input, or while output is still flowing, so it
// can't corrupt a running command or land in a password prompt; it announces
// itself. Returns whether it injected.
func (c *Core) ProvisionRemoteTracking() bool {
	idle := time.Duration(0)
	if n := c.Sess.LastOutputNanos(); n > 0 {
		idle = time.Since(time.Unix(0, n))
	}
	if c.Term.Screen.Snapshot().AltScreen || c.Tracker.EchoOff() || idle < provisionIdle {
		c.Sess.Notify("can't set up host tracking now — the shell looks busy; try again from a quiet prompt")
		return false
	}
	c.Sess.Notify("setting up host tracking on the remote shell (a visible one-time command); takes effect at the next prompt")
	if _, err := c.Sess.WriteInput([]byte(osc7Snippet + "\r")); err != nil {
		c.Sess.Notify("host tracking setup failed: %v", err)
		return false
	}
	return true
}
