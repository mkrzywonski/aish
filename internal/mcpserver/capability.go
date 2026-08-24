package mcpserver

import (
	"context"
	"errors"
	"fmt"

	"ai-ssh/internal/sshmux"
)

// tool availability states reported in oob_tools.
const (
	toolAvailable   = "available"   // prerequisites present on this host
	toolUnavailable = "unavailable" // a prerequisite command is missing/broken
	toolUnknown     = "unknown"     // host not probed yet — capabilities not known
)

// toolAvail is the availability of one OOB primitive on the current host,
// derived from the capability probe. It is reported in session_status
// (oob_tools) and enforced at call time so the AI never attempts — or hangs on
// — an operation whose prerequisite command is missing. State is capability
// (probe-time), never per-operation outcome: a failed write never flips a tool
// to unavailable, and unknown means "not probed yet," not "blocked."
type toolAvail struct {
	State   string `json:"state"`             // available | unavailable | unknown
	Missing string `json:"missing,omitempty"` // the capability that's absent (unavailable)
	Install string `json:"install,omitempty"` // suggested install command (needs user approval)
	Detail  string `json:"detail,omitempty"`  // guidance, e.g. how to resolve unknown
}

// Available reports whether the tool can run now (state is available).
func (t toolAvail) Available() bool { return t.State == toolAvailable }

// oobToolNames are the primitives whose availability depends on remote tooling.
var oobToolNames = []string{
	"file_read", "file_write", "file_edit", "file_patch",
	"file_stat", "directory_list", "directory_create", "file_grep", "file_search",
	"file_upload", "file_download", "exec",
}

var sftpReadToolNames = []string{"file_read", "file_stat", "directory_list", "file_download"}
var sftpWriteToolNames = []string{"file_write", "file_edit", "file_patch", "file_upload"}

// oobToolAvailability reports per-tool availability for a route.
func (c *Core) oobToolAvailability(rt route) map[string]toolAvail {
	switch rt.via {
	case "local":
		m := map[string]toolAvail{}
		for _, n := range oobToolNames {
			m[n] = toolAvail{State: toolAvailable} // Go does the work locally
		}
		return m
	case "in_band":
		return inBandAvailability(c.remoteDialect(rt))
	}
	blocked := c.Mux.NewSessionsBlocked()
	f, ok := c.Mux.Facts(rt.ci)
	if !ok {
		if blocked {
			return blockedAvailability()
		}
		// Not probed yet: report unknown rather than guess. This is honest, not
		// a tollgate — the first real op still auto-probes (requireTool →
		// EnsureProbed) and gates correctly. The AI can call probe_host to
		// resolve these deliberately before planning a workflow.
		return unknownAvailability("host not probed yet; call probe_host to initialize the out-of-band toolset")
	}
	return availability(f, blocked)
}

// blockedAvailability reports a host aish cannot reach because the user stopped
// it opening SSH sessions. Reporting "unknown; call probe_host" here would be
// actively harmful: probing is precisely the blocked operation, so the AI would
// be invited into the refusal it was meant to avoid.
func blockedAvailability() map[string]toolAvail {
	m := map[string]toolAvail{}
	for _, n := range oobToolNames {
		m[n] = toolAvail{
			State:   toolUnavailable,
			Missing: "an SSH session to this host, which the user has blocked",
			Detail: "the user turned on the Ctrl-] block on new SSH sessions, so aish will not open a channel here. " +
				"Do not call probe_host — it is blocked too. Use run_command for visible work in the shared terminal, " +
				"or ask the user to lift the block",
		}
	}
	return m
}

// availability derives per-tool state from the independent shell and SFTP
// axes. Shell-up hosts keep the default shell-first toolset. After a conclusive
// shell failure, only implemented SFTP file primitives can become available;
// command-backed exec/search tools remain unavailable.
// A live channel is unaffected by the block — it is already open, and the tools
// riding it cost nothing more. Only states that would need a NEW session change.
func availability(f sshmux.HostFacts, blocked bool) map[string]toolAvail {
	switch f.Shell.State {
	case sshmux.AxisUp:
		return capabilityAvailability(f.Shell.Caps)
	case sshmux.AxisDown:
		return dialectUnavailability(f, blocked)
	default:
		if blocked {
			return blockedAvailability()
		}
		return unknownAvailability("host not probed yet; call probe_host to initialize the out-of-band toolset")
	}
}

func unknownAvailability(detail string) map[string]toolAvail {
	m := map[string]toolAvail{}
	for _, n := range oobToolNames {
		m[n] = toolAvail{State: toolUnknown, Detail: detail}
	}
	return m
}

// dialectUnavailability reports the toolset for a host whose persistent shell
// channel has failed. A CONCLUSIVE failure (the dialect was identified, or an
// unclassified failure has already burned its retry budget) reads
// "unavailable" and must never repeat the "call probe_host to initialize"
// invitation — that phrasing is what made models re-probe a host forever. A
// still-retryable failure stays "unknown", but says plainly what a retry costs.
func dialectUnavailability(f sshmux.HostFacts, blocked bool) map[string]toolAvail {
	if !f.Shell.Sticky {
		detail := "a probe of this host already failed"
		if f.Shell.Reason != "" {
			detail += " (" + f.Shell.Reason + ")"
		}
		detail += "; a retry opens another channel and may trigger an MFA prompt, so call probe_host only if you have reason to believe it will now succeed"
		return unknownAvailability(detail)
	}

	missing := "a POSIX shell"
	if identity := f.Identity.DialectFact(); identity.Dialect != sshmux.DialectUnknown {
		missing = fmt.Sprintf("a POSIX shell (this host presents %s)", identity.Dialect.Human())
	}
	detail := f.Shell.Reason
	if detail != "" {
		detail += ". "
	}
	detail += "Shell-backed out-of-band tools cannot run here — use run_command for visible command work. " +
		"If the remote shell has since changed, call probe_host with force=true to re-check."

	m := map[string]toolAvail{}
	for _, n := range oobToolNames {
		// exec is NOT exempt: with no channel there is nothing to execute on,
		// and the visible in-band fallback is POSIX too (see inBandAvailability).
		m[n] = toolAvail{State: toolUnavailable, Missing: missing, Detail: detail}
	}
	mergeSFTPAvailability(m, f.SFTP, blocked)
	return m
}

func mergeSFTPAvailability(m map[string]toolAvail, axis sshmux.SftpAxis, blocked bool) {
	allFileTools := append(append([]string(nil), sftpReadToolNames...), sftpWriteToolNames...)
	switch axis.State {
	case sshmux.AxisUnknown:
		// A retained client would still serve these, but there isn't one yet and
		// opening the subsystem is a new session — exactly what is blocked.
		if blocked {
			for _, name := range allFileTools {
				m[name] = toolAvail{
					State:   toolUnavailable,
					Missing: "an SFTP subsystem, which would need a new SSH session the user has blocked",
					Detail:  "the shell route is conclusively unavailable and the Ctrl-] block prevents opening SFTP. Use run_command, or ask the user to lift the block",
				}
			}
			return
		}
		detail := "the POSIX shell route is conclusively unavailable; the first eligible file operation may open the SFTP subsystem, which can require approval or MFA, and the result will be cached"
		for _, name := range allFileTools {
			m[name] = toolAvail{State: toolUnknown, Detail: detail}
		}
	case sshmux.AxisDown:
		detail := "the cached SFTP fallback is unavailable"
		if axis.Reason != "" {
			detail += " (" + axis.Reason + ")"
		}
		detail += "; it will not retry automatically. Use probe_host with sftp=true and force=true only when an explicit retry is warranted; opening a subsystem may trigger MFA"
		for _, name := range allFileTools {
			m[name] = toolAvail{State: toolUnavailable, Missing: "a working SFTP subsystem", Detail: detail}
		}
	case sshmux.AxisUp:
		for _, name := range sftpReadToolNames {
			m[name] = toolAvail{State: toolAvailable, Detail: "served by the retained SFTP client because the POSIX shell route is unavailable"}
		}
		if sftpAxisHasExtension(axis, "posix-rename@openssh.com") {
			for _, name := range sftpWriteToolNames {
				m[name] = toolAvail{State: toolAvailable, Detail: "served atomically by the retained SFTP client because the POSIX shell route is unavailable"}
			}
			return
		}
		for _, name := range sftpWriteToolNames {
			m[name] = toolAvail{
				State:   toolUnavailable,
				Missing: "the posix-rename@openssh.com SFTP extension required for atomic replacement",
				Detail:  "AISH refuses remove-and-rename because it would expose a missing-file window and weaken file_write, file_upload, file_edit, and file_patch guarantees",
			}
		}
	}
}

func sftpAxisHasExtension(axis sshmux.SftpAxis, want string) bool {
	for _, extension := range axis.Extensions {
		if extension == want {
			return true
		}
	}
	return false
}

// inBandAvailability reports what the VISIBLE fallbacks can do. They are not
// dialect-neutral: file_read, file_write and foreground exec all go through
// framing.RunSentinel, which types a POSIX command line with a printf sentinel.
// That framing is unsafe on any shell other than an authoritatively identified
// POSIX shell, so availability is allowlisted rather than inferred from platform.
// Only run_command — which types the command bare — survives unknown and
// non-POSIX identities.
func inBandAvailability(d sshmux.Dialect) map[string]toolAvail {
	m := map[string]toolAvail{}
	for _, n := range oobToolNames {
		switch n {
		case "file_read", "file_write", "exec":
			switch d {
			case sshmux.DialectPosix:
				m[n] = toolAvail{State: toolAvailable}
			case sshmux.DialectUnknown:
				m[n] = toolAvail{
					State:  toolUnknown,
					Detail: "remote command syntax is unknown; refusing to type POSIX sentinel framing into the shared terminal. Use run_command only with syntax appropriate for the target; do not assume POSIX",
				}
			default:
				m[n] = toolAvail{
					State:   toolUnavailable,
					Missing: fmt.Sprintf("a POSIX shell (the visible fallback types a POSIX command line, but this host presents %s)", d.Human()),
					Detail:  "use run_command with syntax appropriate for this shell; it types the command bare",
				}
			}
		default:
			m[n] = toolAvail{State: toolUnavailable, Missing: "an out-of-band route (no multiplexed channel to this host)"}
		}
	}
	return m
}

// remoteDialect returns the dialect learned from a PROBE for this route.
// Deliberately probe-sourced only: the passive screen fingerprint is advisory
// and may annotate a result, but must never drive a tool's state.
func (c *Core) remoteDialect(rt route) sshmux.Dialect {
	f, ok := c.Mux.Facts(rt.ci)
	if !ok {
		return sshmux.DialectUnknown
	}
	return f.Identity.DialectFact().Dialect
}

func capabilityAvailability(caps sshmux.Capabilities) map[string]toolAvail {
	m := map[string]toolAvail{}
	if caps.Unsupported {
		for _, n := range oobToolNames {
			if n == "exec" {
				m[n] = toolAvail{State: toolAvailable}
			} else {
				m[n] = toolAvail{State: toolUnavailable, Missing: "a POSIX shell (host not supported)"}
			}
		}
		return m
	}
	set := func(name string, ok bool, missing, pkg string) {
		if ok {
			m[name] = toolAvail{State: toolAvailable}
			return
		}
		m[name] = toolAvail{State: toolUnavailable, Missing: missing, Install: installHint(caps.PkgMgr, pkg)}
	}
	encode := caps.HasBase64
	decode := caps.Base64Decode() != ""
	statOK := caps.StatC || caps.StatF

	set("exec", true, "", "")
	set("file_read", encode, "base64", "coreutils")
	set("file_download", encode, "base64", "coreutils")
	set("file_write", encode && decode, "base64 (with a decode flag)", "coreutils")
	set("file_upload", encode && decode, "base64 (with a decode flag)", "coreutils")
	set("file_edit", encode && decode, "base64 (with a decode flag)", "coreutils")
	set("file_patch", encode && decode, "base64 (with a decode flag)", "coreutils")
	set("directory_create", true, "", "") // mkdir is POSIX; no probe can make it absent
	set("file_stat", statOK, "stat", "coreutils")
	set("directory_list", caps.HasFind && (statOK || (caps.FindPrint && caps.HeadZ)), "find and stat", "findutils")
	set("file_grep", caps.HasRg || caps.HasGrep, "ripgrep or grep", "ripgrep")
	set("file_search", caps.HasFind, "find", "findutils")
	return m
}

// installHint maps a package to an install command for the detected package
// manager. Package names are the common ones (coreutils/findutils/ripgrep);
// they're a suggestion, not a guarantee.
func installHint(pkgMgr, pkg string) string {
	if pkg == "" || pkgMgr == "" {
		return ""
	}
	switch pkgMgr {
	case "apt-get":
		return "apt-get install -y " + pkg
	case "dnf":
		return "dnf install -y " + pkg
	case "yum":
		return "yum install -y " + pkg
	case "apk":
		return "apk add " + pkg
	case "pacman":
		return "pacman -S --noconfirm " + pkg
	case "zypper":
		return "zypper install -y " + pkg
	case "brew":
		return "brew install " + pkg
	case "pkg":
		return "pkg install -y " + pkg
	}
	return ""
}

// requireTool gates an OOB primitive on its availability. For a remote route it
// first ensures the channel is probed (so availability reflects the real host,
// and a non-POSIX host fails fast here), then returns a clear, actionable error
// when the tool's prerequisite is missing.
func (c *Core) requireTool(rt route, tool string) error {
	if rt.via == "controlmaster" {
		if _, err := c.Mux.EnsureProbed(rt.ci); err != nil {
			return err
		}
	}
	av := c.oobToolAvailability(rt)[tool]
	if av.Available() {
		return nil
	}
	if av.State == toolUnknown {
		// Only reachable if EnsureProbed didn't populate the facts; surface it
		// rather than proceed blind.
		detail := av.Detail
		if detail == "" {
			detail = "call probe_host"
		}
		return fmt.Errorf("%s is not safe to use on %s: %s", tool, rt.host, detail)
	}
	msg := fmt.Sprintf("%s is unavailable on %s", tool, rt.host)
	if av.Missing != "" {
		msg += ": it needs " + av.Missing
	}
	if av.Install != "" {
		msg += fmt.Sprintf(". With the user's approval you can install it (run_command: %s), then retry", av.Install)
	} else if av.Detail != "" {
		msg += ". " + av.Detail
	}
	return errors.New(msg)
}

// fileFallbackRoute preserves the default shell-first policy while allowing
// implemented file primitives to fall back to a retained SFTP client on a
// conclusively non-POSIX target. A fresh SFTP open happens at most once and is
// sticky in either direction; a lost or failed client is never reopened here.
func (c *Core) fileFallbackRoute(ctx context.Context, tool string) (route, error) {
	rt := c.route()
	if rt.via != "controlmaster" {
		if err := c.requireTool(rt, tool); err != nil {
			return route{}, err
		}
		return rt, nil
	}

	_, shellErr := c.Mux.EnsureProbed(rt.ci)
	if shellErr == nil {
		if err := c.requireTool(rt, tool); err != nil {
			return route{}, err
		}
		return rt, nil
	}

	facts, ok := c.Mux.Facts(rt.ci)
	action := fileFallbackAction(facts, ok)
	if action == fallbackRefuseShell {
		// Preserve the shell probe's exact retry/MFA guidance. A soft shell
		// failure is not enough evidence to pay for a second subsystem.
		if ok {
			if err := facts.ShellError(); err != nil {
				return route{}, err
			}
		}
		if shellErr != nil {
			return route{}, shellErr
		}
		return route{}, fmt.Errorf("%s could not establish the shell-first out-of-band route to %s", tool, rt.host)
	}

	axis := facts.SFTP
	if action == fallbackProbeSFTP {
		probe, err := c.Mux.ProbeSFTP(ctx, rt.ci, false)
		if err != nil {
			return route{}, err
		}
		axis = probe.Axis
	}
	if axis.State != sshmux.AxisUp {
		reason := axis.Reason
		if reason == "" {
			reason = "the SFTP subsystem is unavailable"
		}
		return route{}, fmt.Errorf("%s is unavailable on %s: the POSIX shell route is conclusively down and the cached SFTP fallback failed (%s). It will not be retried automatically; retry only with probe_host using sftp=true and force=true, which may trigger MFA", tool, rt.host, reason)
	}
	return route{via: "sftp", ci: rt.ci, host: rt.host}, nil
}

type sftpFallbackAction uint8

const (
	fallbackRefuseShell sftpFallbackAction = iota
	fallbackProbeSFTP
	fallbackUseSFTP
	fallbackRefuseSFTP
)

func fileFallbackAction(facts sshmux.HostFacts, ok bool) sftpFallbackAction {
	if !ok || !facts.ShellBlocked() {
		return fallbackRefuseShell
	}
	switch facts.SFTP.State {
	case sshmux.AxisUnknown:
		return fallbackProbeSFTP
	case sshmux.AxisUp:
		return fallbackUseSFTP
	default:
		return fallbackRefuseSFTP
	}
}
