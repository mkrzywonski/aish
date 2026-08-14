package sshmux

import (
	"errors"
	"strings"
)

// Dialect identification for hosts whose ssh login shell is not a POSIX shell.
//
// aish's out-of-band channel drives one long-lived `sh -s` on the remote. When
// the login shell can't run that, the attempt fails in well under a second and
// the reason is written to the channel's STDERR — which used to be routed to
// the null device and thrown away, leaving aish unable to say anything about
// the host beyond "it didn't work". Captured instead, that text identifies the
// far end precisely.
//
// Ground truth for the tables below was captured over a live ControlMaster to
// Windows OpenSSH 10.0p2 (see dialect_test.go). Two findings shaped the design:
//
//   - The evidence is on stderr and stdout is EMPTY, so stderr capture is the
//     whole signal, not a refinement of it.
//   - The remote exit status is 1 — not the 9009 that cmd.exe is often said to
//     return, and not a truncation of it. 1 discriminates nothing, so exit
//     status is never used as evidence for a Windows dialect. It is only
//     consulted for the POSIX case, where 127 plus "not found" positively
//     identifies a POSIX host that is merely missing /bin/sh.

type Dialect string

const (
	DialectUnknown    Dialect = ""
	DialectPosix      Dialect = "posix"
	DialectCmd        Dialect = "cmd"
	DialectPowerShell Dialect = "powershell"
	DialectNetworkOS  Dialect = "network_os"
	DialectRestricted Dialect = "restricted"
	DialectNoShell    Dialect = "no_shell"
)

// Platform is the coarse family a dialect belongs to, for callers that only
// need to know "is this a Unix box".
func (d Dialect) Platform() string {
	switch d {
	case DialectPosix:
		return "unix"
	case DialectCmd, DialectPowerShell:
		return "windows"
	case DialectNetworkOS:
		return "network"
	default:
		return ""
	}
}

// Human renders the dialect for an error message aimed at a model or a person.
func (d Dialect) Human() string {
	switch d {
	case DialectPosix:
		return "a POSIX shell"
	case DialectCmd:
		return "Windows cmd.exe"
	case DialectPowerShell:
		return "PowerShell"
	case DialectNetworkOS:
		return "a network-device CLI"
	case DialectRestricted:
		return "a restricted shell"
	case DialectNoShell:
		return "an account with no usable shell"
	default:
		return "an unrecognized shell"
	}
}

// fingerprint is one stderr/stdout signature. Matching is case-insensitive
// substring, in table order, so more specific patterns must come first.
type fingerprint struct {
	needle  string
	dialect Dialect
	reason  string
}

// fingerprints is ordered most-specific first. Note that cmd.exe and PowerShell
// share the prefix "is not recognized as" and diverge only in the tail, so both
// entries carry enough of the tail to be unambiguous.
var fingerprints = []fingerprint{
	{"is not recognized as an internal or external command", DialectCmd,
		"the remote ssh login shell is Windows cmd.exe"},
	{"is not recognized as the name of a cmdlet", DialectPowerShell,
		"the remote ssh login shell is PowerShell"},
	{"is not recognized as a name of a cmdlet", DialectPowerShell,
		"the remote ssh login shell is PowerShell"},
	{"commandnotfoundexception", DialectPowerShell,
		"the remote ssh login shell is PowerShell"},
	{"+ categoryinfo", DialectPowerShell,
		"the remote ssh login shell is PowerShell"},
	{"parsererror", DialectPowerShell,
		"the remote ssh login shell is PowerShell"},
	{"the system cannot find the path specified", DialectCmd,
		"the remote ssh login shell is Windows cmd.exe"},
	{"% invalid input detected at", DialectNetworkOS,
		"the remote is a network device CLI, not a shell"},
	{"unknown command.", DialectNetworkOS,
		"the remote is a network device CLI, not a shell"},
	{"syntax error, expecting", DialectNetworkOS,
		"the remote is a network device CLI, not a shell"},
	{"rbash:", DialectRestricted,
		"the remote ssh login shell is a restricted shell"},
	{"restricted: cannot", DialectRestricted,
		"the remote ssh login shell is a restricted shell"},
	{"applet not found", DialectRestricted,
		"the remote shell is a reduced BusyBox environment without sh"},
	{"this account is currently not available", DialectNoShell,
		"the remote account has no login shell (nologin)"},
	{"shell request failed on channel", DialectNoShell,
		"the remote refused to start a shell session"},
	{"administratively prohibited", DialectNoShell,
		"the remote refused to open the channel"},
}

// probeFailure carries everything observed when the capability probe could not
// complete: the classified error, whatever the remote printed, and its exit
// status. Previously all of this was discarded with the channel.
type probeFailure struct {
	Err    error
	Stdout []byte
	Stderr []byte
	Exit   int // -1 when the process was not reaped in time
}

// classifyFailure turns a probe failure into a durable statement about the
// host. sticky reports whether the conclusion is a property of the HOST (never
// retry: retrying costs a channel open and, on MFA-protected hosts, a push)
// rather than of the transport (retry once).
func classifyFailure(pf probeFailure) (d Dialect, evidence, reason string, sticky bool) {
	text := string(pf.Stderr) + "\n" + string(pf.Stdout)
	lower := strings.ToLower(text)

	for _, fp := range fingerprints {
		if i := strings.Index(lower, fp.needle); i >= 0 {
			return fp.dialect, firstLine(text[:min(i+len(fp.needle)+40, len(text))]), fp.reason, true
		}
	}

	// A POSIX host that simply lacks /bin/sh: "not found" with the POSIX
	// command-not-found status. Deliberately checked AFTER the tables, because
	// several non-POSIX shells also say "not found" in other phrasings — and it
	// must never be mistaken for Windows.
	if pf.Exit == 127 && strings.Contains(lower, "not found") {
		return DialectPosix, firstLine(text), "the remote is a POSIX host but has no usable /bin/sh", true
	}

	// The shell answered but never returned our sentinel. We can't name it, but
	// something is reading our stdin and not behaving like sh — a host property,
	// so don't burn another channel open on it.
	if errors.Is(pf.Err, errNotPosixShell) {
		return DialectUnknown, firstLine(text),
			"the remote responded but did not behave like a POSIX shell", true
	}

	// Everything below is a TRANSPORT fact, not a host fact: retry once (the
	// attempt counter in facts.go makes it sticky if it keeps happening).
	switch {
	case errors.Is(pf.Err, errNoShellResponse):
		return DialectUnknown, firstLine(text),
			"the remote did not respond in time; it may be slow, or may not be a POSIX shell", false
	case errors.Is(pf.Err, errChannelDead):
		return DialectUnknown, firstLine(text),
			"the channel closed immediately; the remote may not allow a shell session or may lack /bin/sh", false
	}
	return DialectUnknown, firstLine(text), "", false
}

// firstLine trims the evidence to a single tidy line. Remote output arrives
// with CRLF endings on Windows hosts, so \r is stripped explicitly.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
