package sshmux

import (
	"errors"
	"fmt"
	"time"
)

// Durable per-target facts.
//
// Capabilities used to live on the *channel that discovered them, which meant
// every probe result died with its channel. Two consequences, both bugs:
//
//   - A FAILED probe left nothing behind, so session_status kept reporting
//     every tool "unknown — host not probed yet; call probe_host to
//     initialize", inviting an endless re-probe of a host that can never
//     succeed. Each retry costs a channel open and, on MFA-protected hosts, a
//     push.
//   - A SUCCESSFUL probe was lost when a channel timed out, silently regressing
//     target_confidence from "same" to "unknown" and re-arming the one-time
//     write confirmation the user had already answered.
//
// HostFacts therefore lives on the Mux, keyed by the ControlMaster socket path
// (deterministically derived from user@host:port by the ssh shim), and OUTLIVES
// the channel. It is in-process only, like client authorization grants.

// AxisState mirrors the tool-availability model: absent means "not probed",
// never "blocked". State is always a probe-time capability — a failed file
// write must never move it.
type AxisState uint8

const (
	AxisUnknown AxisState = iota
	AxisUp
	AxisDown
)

func (s AxisState) String() string {
	switch s {
	case AxisUp:
		return "up"
	case AxisDown:
		return "down"
	default:
		return "unknown"
	}
}

// ShellAxis describes the persistent `sh -s` channel capability. SftpAxis sits
// beside it in HostFacts because an SSH subsystem needs no login-shell grammar.
type ShellAxis struct {
	State    AxisState
	Reason   string // model-facing explanation; set when State == AxisDown
	Evidence string // the matched remote output, trimmed to one line
	Sticky   bool   // true => never retry; the conclusion is a host property
	Attempts int
	Caps     Capabilities // meaningful only when State == AxisUp
	ProbedAt time.Time
}

// SftpAxis describes the independent SSH file-transfer subsystem. Every
// outcome is sticky until explicitly forced because opening the subsystem may
// trigger MFA. A successful axis can serve file operations even when Shell is
// down on a non-POSIX login shell.
type SftpAxis struct {
	State      AxisState
	Reason     string
	RealPath   string
	PathStyle  string
	Extensions []string
	Attempts   int
	ProbedAt   time.Time
}

func (a SftpAxis) Attempted() bool { return a.State != AxisUnknown }

// IdentitySource names how an authoritative target identity was learned.
// Passive screen hints deliberately do not belong here: they are ephemeral,
// advisory observations owned by the status layer and must never become durable
// transport facts.
type IdentitySource string

const (
	IdentitySourceShellProbe IdentitySource = "shell_probe"
	IdentitySourceDeepProbe  IdentitySource = "deep_probe"
	IdentitySourceSFTP       IdentitySource = "sftp"
)

// IdentityFact is one source's observation. Dialect and platform are selected
// independently from the source records because SFTP can establish Windows
// structurally without distinguishing cmd.exe from PowerShell.
type IdentityFact struct {
	Dialect    Dialect
	Platform   string
	Source     IdentitySource
	Evidence   string
	ObservedAt time.Time
}

// IdentityFacts retains every authoritative observation rather than only the
// currently selected value. That makes precedence independent of call order and
// leaves conflicting evidence available for diagnostics.
type IdentityFacts struct {
	ShellProbe IdentityFact
	DeepProbe  IdentityFact
	SFTP       IdentityFact
}

// DialectFact returns the most specific authoritative dialect observation.
// The active probe identifies login-shell grammar directly; the shell probe
// only proves that `sh -s` ran (or classifies why it did not).
func (f IdentityFacts) DialectFact() IdentityFact {
	for _, candidate := range []IdentityFact{f.DeepProbe, f.ShellProbe, f.SFTP} {
		if candidate.Dialect != DialectUnknown {
			return candidate
		}
	}
	return IdentityFact{}
}

// PlatformFact prefers SFTP's structural path evidence, then the explicit deep
// probe, then the platform inferred by the ordinary shell probe.
func (f IdentityFacts) PlatformFact() IdentityFact {
	for _, candidate := range []IdentityFact{f.SFTP, f.DeepProbe, f.ShellProbe} {
		if candidate.Platform != "" {
			return candidate
		}
	}
	return IdentityFact{}
}

// HostFacts is what aish has learned about one ssh target.
type HostFacts struct {
	Sock, Host, User, Port string
	Shell                  ShellAxis
	SFTP                   SftpAxis
	Identity               IdentityFacts
	Deep                   DeepProbeResult
}

// maxSoftAttempts is how many times an UNCLASSIFIED failure is retried before
// it becomes sticky. A transport hiccup deserves a second chance; an
// unclassifiable host must not cost an unbounded number of MFA prompts.
const maxSoftAttempts = 2

// ErrShellUnusable marks every error produced by a known-bad shell axis, so
// callers can distinguish "this host can't do out-of-band shell work" from a
// transient failure.
var ErrShellUnusable = errors.New("out-of-band shell channel unusable on this host")

// ShellUsable reports whether the persistent channel is known to work.
func (f HostFacts) ShellUsable() bool { return f.Shell.State == AxisUp }

// ShellBlocked reports whether the shell axis has failed conclusively, so
// callers can refuse without opening anything.
func (f HostFacts) ShellBlocked() bool {
	return f.Shell.State == AxisDown && f.Shell.Sticky
}

// Note summarises the shell axis for a caller that REPORTS rather than refuses
// (session_status). It names the dialect, quotes what the host actually said,
// states explicitly that aish is not retrying and why, and points at the ways
// forward. Saying "not retrying, because a retry costs a channel open" is what
// stops a model looping on probe_host. Empty when the axis is fine.
func (f HostFacts) Note() string {
	if f.Shell.State != AxisDown {
		return ""
	}
	// Lead with the host and the reason, NOT with "the channel is unusable":
	// ShellError wraps this with ErrShellUnusable, which already says that, and
	// stating it twice read badly to both models and humans.
	reason := f.Shell.Reason
	if reason == "" {
		reason = "the out-of-band channel could not be opened"
	}
	msg := fmt.Sprintf("%s — %s", f.Host, reason)
	if f.Shell.Evidence != "" {
		msg += fmt.Sprintf(" (it answered %q)", f.Shell.Evidence)
	}
	if !f.Shell.ProbedAt.IsZero() {
		msg += fmt.Sprintf("; probed %s ago", roundDur(time.Since(f.Shell.ProbedAt)))
	}
	// The advice differs by kind of failure, and getting this wrong is how a
	// model ends up doing the useless thing: a sticky host needs force=true to
	// re-check at all, while a still-retryable one just needs probe_host again.
	if f.Shell.Sticky {
		msg += ", and aish is not retrying because each attempt opens a channel and may trigger an MFA prompt" +
			". Use run_command to drive this host visibly; if the remote shell has since changed, call probe_host with force=true to re-check"
	} else {
		msg += ". Use run_command to drive this host visibly; probe_host will try again, but each attempt opens a channel and may trigger an MFA prompt"
	}
	return msg
}

// ShellError is what every operation gets in place of opening a channel that is
// known to be unusable. It wraps ErrShellUnusable so callers can classify it.
func (f HostFacts) ShellError() error {
	note := f.Note()
	if note == "" {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrShellUnusable, note)
}

func roundDur(d time.Duration) time.Duration {
	switch {
	case d < time.Minute:
		return d.Round(time.Second)
	case d < time.Hour:
		return d.Round(time.Minute)
	default:
		return d.Round(time.Hour)
	}
}

// --- Mux storage ---------------------------------------------------------

// Facts returns what is known about ci's target. ok is false when nothing has
// been learned yet. It never opens a channel and never runs a command, so
// session_status can call it freely without risking an MFA prompt.
func (m *Mux) Facts(ci *ConnInfo) (HostFacts, bool) {
	if ci == nil || ci.Sock == "" {
		return HostFacts{}, false
	}
	m.factsMu.RLock()
	defer m.factsMu.RUnlock()
	f, ok := m.facts[ci.Sock]
	if !ok {
		return HostFacts{}, false
	}
	copy := *f
	copy.SFTP.Extensions = append([]string(nil), f.SFTP.Extensions...)
	return copy, true
}

// ForgetFacts discards the entire target record. Normal forced shell probing
// uses ForgetShellFacts so independently learned identity can survive.
func (m *Mux) ForgetFacts(ci *ConnInfo) {
	if ci == nil || ci.Sock == "" {
		return
	}
	m.factsMu.Lock()
	delete(m.facts, ci.Sock)
	m.factsMu.Unlock()
}

// ForgetShellFacts resets only conclusions produced by the persistent-shell
// probe. Identity learned independently by an explicit deep probe or by SFTP
// must survive force=true, which means "retry sh -s", not "forget everything
// known about this target".
func (m *Mux) ForgetShellFacts(ci *ConnInfo) {
	if ci == nil || ci.Sock == "" {
		return
	}
	m.factsMu.Lock()
	defer m.factsMu.Unlock()
	f := m.facts[ci.Sock]
	if f == nil {
		return
	}
	f.Shell = ShellAxis{}
	f.Identity.ShellProbe = IdentityFact{}
}

func (m *Mux) forgetDeepFacts(ci *ConnInfo) {
	if ci == nil || ci.Sock == "" {
		return
	}
	m.factsMu.Lock()
	defer m.factsMu.Unlock()
	f := m.facts[ci.Sock]
	if f == nil {
		return
	}
	f.Deep = DeepProbeResult{}
	f.Identity.DeepProbe = IdentityFact{}
}

func (m *Mux) forgetSftpFacts(ci *ConnInfo) {
	if ci == nil || ci.Sock == "" {
		return
	}
	m.factsMu.Lock()
	defer m.factsMu.Unlock()
	f := m.facts[ci.Sock]
	if f == nil {
		return
	}
	f.SFTP = SftpAxis{}
	f.Identity.SFTP = IdentityFact{}
}

// CachedDeepProbe returns a prior explicit deep-probe outcome without opening
// anything. Failed and unknown outcomes are cache hits too.
func (m *Mux) CachedDeepProbe(ci *ConnInfo) (DeepProbeResult, bool) {
	if ci == nil || ci.Sock == "" {
		return DeepProbeResult{}, false
	}
	m.factsMu.RLock()
	defer m.factsMu.RUnlock()
	f := m.facts[ci.Sock]
	if f == nil || !f.Deep.Attempted() {
		return DeepProbeResult{}, false
	}
	return f.Deep, true
}

// CachedSFTPProbe returns either a successful or failed prior subsystem probe.
// Both are durable because retrying may trigger another MFA request.
func (m *Mux) CachedSFTPProbe(ci *ConnInfo) (SftpAxis, bool) {
	if ci == nil || ci.Sock == "" {
		return SftpAxis{}, false
	}
	m.factsMu.RLock()
	defer m.factsMu.RUnlock()
	f := m.facts[ci.Sock]
	if f == nil || !f.SFTP.Attempted() {
		return SftpAxis{}, false
	}
	axis := f.SFTP
	axis.Extensions = append([]string(nil), axis.Extensions...)
	return axis, true
}

// factsFor returns the mutable record for ci, creating it on first use.
// Callers hold factsMu.
func (m *Mux) factsForLocked(ci *ConnInfo) *HostFacts {
	f := m.facts[ci.Sock]
	if f == nil {
		f = &HostFacts{Sock: ci.Sock, Host: ci.Host, User: ci.User, Port: ci.Port}
		m.facts[ci.Sock] = f
	}
	return f
}

// NoteShellUsable records a successful probe and its capabilities.
func (m *Mux) NoteShellUsable(ci *ConnInfo, caps Capabilities) {
	if ci == nil || ci.Sock == "" {
		return
	}
	m.factsMu.Lock()
	defer m.factsMu.Unlock()
	f := m.factsForLocked(ci)
	now := time.Now()
	f.Shell = ShellAxis{State: AxisUp, Caps: caps, Attempts: f.Shell.Attempts + 1, ProbedAt: now}
	f.noteIdentityLocked(DialectPosix, DialectPosix.Platform(), IdentitySourceShellProbe,
		"the POSIX capability probe completed", now)
}

// NoteShellUnusable records a failed probe. A classified failure is sticky
// immediately; an unclassified one becomes sticky once it has burned
// maxSoftAttempts channel opens. Exported because internal/mcpserver's tests
// need to seed a host state from another package.
func (m *Mux) NoteShellUnusable(ci *ConnInfo, d Dialect, reason, evidence string, sticky bool) HostFacts {
	if ci == nil || ci.Sock == "" {
		return HostFacts{}
	}
	m.factsMu.Lock()
	defer m.factsMu.Unlock()
	f := m.factsForLocked(ci)
	attempts := f.Shell.Attempts + 1
	if !sticky && attempts >= maxSoftAttempts {
		sticky = true
		if reason == "" {
			reason = "repeated attempts to open the out-of-band channel failed"
		}
	}
	if reason == "" {
		reason = "the out-of-band channel could not be opened"
	}
	f.Shell = ShellAxis{
		State:    AxisDown,
		Reason:   reason,
		Evidence: evidence,
		Sticky:   sticky,
		Attempts: attempts,
		ProbedAt: time.Now(),
	}
	if d != DialectUnknown {
		f.noteIdentityLocked(d, d.Platform(), IdentitySourceShellProbe, evidence, f.Shell.ProbedAt)
	}
	return *f
}

// NoteIdentity records authoritative identity evidence without changing any
// transport axis. The deep probe and SFTP workstreams use this path so learning
// what a target is cannot accidentally claim that `sh -s` works there.
func (m *Mux) NoteIdentity(ci *ConnInfo, d Dialect, platform string, source IdentitySource, evidence string) HostFacts {
	if ci == nil || ci.Sock == "" {
		return HostFacts{}
	}
	m.factsMu.Lock()
	defer m.factsMu.Unlock()
	f := m.factsForLocked(ci)
	f.noteIdentityLocked(d, platform, source, evidence, time.Now())
	return *f
}

func (m *Mux) noteDeepProbe(ci *ConnInfo, result DeepProbeResult) DeepProbeResult {
	m.factsMu.Lock()
	defer m.factsMu.Unlock()
	f := m.factsForLocked(ci)
	result.Attempts = f.Deep.Attempts + 1
	result.Cached = false
	f.Deep = result
	if result.Status == DeepProbeIdentified {
		f.noteIdentityLocked(result.Dialect, result.Platform, IdentitySourceDeepProbe, result.Evidence, result.AttemptedAt)
	}
	return result
}

func (m *Mux) noteSFTPProbe(ci *ConnInfo, axis SftpAxis) SftpAxis {
	m.factsMu.Lock()
	defer m.factsMu.Unlock()
	f := m.factsForLocked(ci)
	axis.Attempts = f.SFTP.Attempts + 1
	axis.Extensions = append([]string(nil), axis.Extensions...)
	f.SFTP = axis
	if axis.State == AxisUp {
		platform := ""
		switch axis.PathStyle {
		case "posix":
			platform = "unix"
		case "windows":
			platform = "windows"
		}
		if platform != "" {
			f.noteIdentityLocked(DialectUnknown, platform, IdentitySourceSFTP,
				"SFTP realpath(.) returned "+axis.RealPath, axis.ProbedAt)
		}
	}
	return axis
}

func (f *HostFacts) noteIdentityLocked(d Dialect, platform string, source IdentitySource, evidence string, observedAt time.Time) {
	fact := IdentityFact{Dialect: d, Platform: platform, Source: source, Evidence: evidence, ObservedAt: observedAt}
	switch source {
	case IdentitySourceShellProbe:
		f.Identity.ShellProbe = fact
	case IdentitySourceDeepProbe:
		f.Identity.DeepProbe = fact
	case IdentitySourceSFTP:
		f.Identity.SFTP = fact
	}
}
