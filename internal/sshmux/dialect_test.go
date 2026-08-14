package sshmux

import (
	"strings"
	"testing"
)

// cmdStderr is the EXACT stderr captured from Windows OpenSSH 10.0p2 over a
// live ControlMaster, running the same remote command openChannel sends
// (`sh -s`) against a cmd.exe login shell. Note the CRLF endings: remote output
// is not newline-normalised, so the classifier must not assume LF.
//
// Captured alongside it, and the reason exit status is not used as evidence:
// stdout was EMPTY and the exit status was 1 — not the 9009 cmd.exe is often
// said to return, and not a truncation of it.
const cmdStderr = "'sh' is not recognized as an internal or external command,\r\noperable program or batch file.\r\n"

// pwshStderr is the equivalent from PowerShell 7.6.
const pwshStderr = "sh: The term 'sh' is not recognized as a name of a cmdlet, function, script file, " +
	"or executable program.\r\nCheck the spelling of the name, or if a path was included, verify that " +
	"the path is correct and try again.\r\n"

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		name     string
		pf       probeFailure
		dialect  Dialect
		platform string
		sticky   bool
	}{
		{
			name:     "cmd.exe, captured live",
			pf:       probeFailure{Err: errChannelDead, Stderr: []byte(cmdStderr), Exit: 1},
			dialect:  DialectCmd,
			platform: "windows",
			sticky:   true,
		},
		{
			name:     "powershell 7",
			pf:       probeFailure{Err: errChannelDead, Stderr: []byte(pwshStderr), Exit: 1},
			dialect:  DialectPowerShell,
			platform: "windows",
			sticky:   true,
		},
		{
			name: "powershell 5.1 phrasing",
			pf: probeFailure{Err: errChannelDead, Exit: 1,
				Stderr: []byte("sh : The term 'sh' is not recognized as the name of a cmdlet, function, or operable program.")},
			dialect:  DialectPowerShell,
			platform: "windows",
			sticky:   true,
		},
		{
			name: "powershell error record",
			pf: probeFailure{Err: errChannelDead, Exit: 1,
				Stderr: []byte("CategoryInfo          : ObjectNotFound: (sh:String)\n+ CategoryInfo : CommandNotFoundException")},
			dialect:  DialectPowerShell,
			platform: "windows",
			sticky:   true,
		},
		{
			name: "cisco ios",
			pf: probeFailure{Err: errNotPosixShell, Exit: -1,
				Stdout: []byte("% Invalid input detected at '^' marker.\r\n")},
			dialect:  DialectNetworkOS,
			platform: "network",
			sticky:   true,
		},
		{
			name: "junos",
			pf: probeFailure{Err: errNotPosixShell, Exit: -1,
				Stdout: []byte("unknown command.\r\n")},
			dialect:  DialectNetworkOS,
			platform: "network",
			sticky:   true,
		},
		{
			name: "restricted bash",
			pf: probeFailure{Err: errChannelDead, Exit: 1,
				Stderr: []byte("rbash: sh: restricted: cannot specify `/' in command names")},
			dialect:  DialectRestricted,
			platform: "",
			sticky:   true,
		},
		{
			name: "nologin account",
			pf: probeFailure{Err: errChannelDead, Exit: 1,
				Stderr: []byte("This account is currently not available.\n")},
			dialect:  DialectNoShell,
			platform: "",
			sticky:   true,
		},
		{
			name: "sshd refused a shell session",
			pf: probeFailure{Err: errChannelDead, Exit: 255,
				Stderr: []byte("shell request failed on channel 0")},
			dialect:  DialectNoShell,
			platform: "",
			sticky:   true,
		},
		{
			name: "busybox without sh applet",
			pf: probeFailure{Err: errChannelDead, Exit: 127,
				Stderr: []byte("sh: applet not found")},
			dialect:  DialectRestricted,
			platform: "",
			sticky:   true,
		},

		// --- Must NOT be mistaken for Windows -------------------------------

		{
			name: "POSIX host merely missing /bin/sh",
			pf: probeFailure{Err: errChannelDead, Exit: 127,
				Stderr: []byte("bash: sh: command not found\n")},
			dialect:  DialectPosix,
			platform: "unix",
			sticky:   true,
		},
		{
			name:    "bare exit 1 with no output tells us nothing",
			pf:      probeFailure{Err: errChannelDead, Exit: 1},
			dialect: DialectUnknown,
			sticky:  false,
		},
		{
			name:    "ssh itself failed",
			pf:      probeFailure{Err: errChannelDead, Exit: 255},
			dialect: DialectUnknown,
			sticky:  false,
		},
		{
			name:    "no response at all is a transport fact, retryable",
			pf:      probeFailure{Err: errNoShellResponse, Exit: -1},
			dialect: DialectUnknown,
			sticky:  false,
		},
		{
			name:    "answered but not POSIX is a host fact, sticky",
			pf:      probeFailure{Err: errNotPosixShell, Exit: -1, Stdout: []byte("gibberish\n")},
			dialect: DialectUnknown,
			sticky:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, evidence, reason, sticky := classifyFailure(tc.pf)
			if d != tc.dialect {
				t.Errorf("dialect = %q, want %q", d, tc.dialect)
			}
			if got := d.Platform(); got != tc.platform {
				t.Errorf("platform = %q, want %q", got, tc.platform)
			}
			if sticky != tc.sticky {
				t.Errorf("sticky = %v, want %v", sticky, tc.sticky)
			}
			if strings.ContainsAny(evidence, "\r\n") {
				t.Errorf("evidence should be a single trimmed line, got %q", evidence)
			}
			if tc.dialect != DialectUnknown && reason == "" {
				t.Error("a classified failure must carry a reason")
			}
		})
	}
}

// TestClassifyEvidenceIsQuotable checks the captured cmd.exe text yields a
// single tidy line suitable for dropping into an error message.
func TestClassifyEvidenceIsQuotable(t *testing.T) {
	_, evidence, _, _ := classifyFailure(probeFailure{Err: errChannelDead, Stderr: []byte(cmdStderr), Exit: 1})
	if !strings.Contains(evidence, "is not recognized as an internal or external command") {
		t.Errorf("evidence lost the fingerprint: %q", evidence)
	}
	if strings.Contains(evidence, "operable program") {
		t.Errorf("evidence should stop at the first line, got %q", evidence)
	}
}

func TestDialectHuman(t *testing.T) {
	for d, want := range map[Dialect]string{
		DialectCmd:        "Windows cmd.exe",
		DialectPowerShell: "PowerShell",
		DialectPosix:      "a POSIX shell",
	} {
		if got := d.Human(); got != want {
			t.Errorf("%q.Human() = %q, want %q", d, got, want)
		}
	}
}
