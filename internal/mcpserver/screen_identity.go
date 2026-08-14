package mcpserver

import (
	"regexp"
	"strings"

	"ai-ssh/internal/sshmux"
	"ai-ssh/internal/term"
)

const screenIdentitySource = "screen"

var (
	windowsVersionLine = regexp.MustCompile(`(?i)Microsoft Windows \[Version [^\]]+\]`)
	powerShellPrompt   = regexp.MustCompile(`(?i)^\s*PS\s+(?:[A-Z]:\\|Microsoft\.PowerShell\.Core\\FileSystem::\\\\)[^>\r\n]*>\s*$`)
	windowsDrivePrompt = regexp.MustCompile(`(?i)^(?:[^>\r\n]*\s)?[A-Z]:\\[^>\r\n]*>\s*$`)
)

// screenIdentityHint is an ephemeral, advisory observation. It never enters
// sshmux.HostFacts and therefore cannot affect availability or retry policy.
type screenIdentityHint struct {
	Dialect  sshmux.Dialect
	Platform string
	Evidence string
}

// fingerprintScreenIdentity recognizes only prompt-shaped evidence on the
// cursor row. Other visible rows are arbitrary command output and can contain
// copied prompts or documentation, so they provide corroboration only.
func fingerprintScreenIdentity(snap term.Snapshot) screenIdentityHint {
	if snap.AltScreen || snap.Text == "" {
		return screenIdentityHint{}
	}

	lines := strings.Split(strings.TrimSuffix(snap.Text, "\n"), "\n")
	if snap.CursorRow < 0 || snap.CursorRow >= len(lines) {
		return screenIdentityHint{}
	}
	prompt := strings.TrimSuffix(lines[snap.CursorRow], "\r")

	if powerShellPrompt.MatchString(prompt) {
		return screenIdentityHint{
			Dialect:  sshmux.DialectPowerShell,
			Platform: "windows",
			Evidence: "the cursor row matches a PowerShell drive-path prompt",
		}
	}

	if !windowsDrivePrompt.MatchString(prompt) {
		return screenIdentityHint{}
	}
	for _, line := range lines {
		if windowsVersionLine.MatchString(line) {
			return screenIdentityHint{
				Dialect:  sshmux.DialectCmd,
				Platform: "windows",
				Evidence: "the cursor row matches a Windows drive prompt and the visible screen contains a Windows version banner",
			}
		}
	}
	return screenIdentityHint{
		Platform: "windows",
		Evidence: "the cursor row matches a Windows drive-path prompt, but the shell dialect is unconfirmed",
	}
}

// applyScreenIdentityHint fills only identity fields that authoritative facts
// left unknown. Source fields are deliberately explicit so clients do not treat
// advisory screen evidence as a transport capability result.
func applyScreenIdentityHint(res *sessionStatusResult, hint screenIdentityHint) {
	used := false
	if res.RemoteDialect == "" && hint.Dialect != sshmux.DialectUnknown {
		res.RemoteDialect = string(hint.Dialect)
		res.RemoteDialectSource = screenIdentitySource
		used = true
	}
	if res.RemotePlatform == "" && hint.Platform != "" {
		res.RemotePlatform = hint.Platform
		res.RemotePlatformSource = screenIdentitySource
		used = true
	}
	if used {
		res.RemoteIdentityNote = "Advisory screen fingerprint only: " + hint.Evidence + ". This does not change out-of-band tool availability; probe evidence takes precedence."
	}
}
