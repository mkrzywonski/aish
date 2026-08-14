package mcpserver

import (
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
	"file_stat", "directory_list", "file_grep", "file_search",
	"file_upload", "file_download", "exec",
}

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
	f, ok := c.Mux.Facts(rt.ci)
	if !ok {
		// Not probed yet: report unknown rather than guess. This is honest, not
		// a tollgate — the first real op still auto-probes (requireTool →
		// EnsureProbed) and gates correctly. The AI can call probe_host to
		// resolve these deliberately before planning a workflow.
		return unknownAvailability("host not probed yet; call probe_host to initialize the out-of-band toolset")
	}
	return availability(f)
}

// availability derives per-tool state from the set of capability axes recorded
// for a host. Only the shell axis exists today; a future SFTP axis merges here
// — sftp is an ssh subsystem, so it can serve file_read/file_write/file_stat/
// directory_list/upload/download on a host whose login shell is not POSIX.
func availability(f sshmux.HostFacts) map[string]toolAvail {
	switch f.Shell.State {
	case sshmux.AxisUp:
		return capabilityAvailability(f.Shell.Caps)
	case sshmux.AxisDown:
		return dialectUnavailability(f)
	default:
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
func dialectUnavailability(f sshmux.HostFacts) map[string]toolAvail {
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
	detail += "Out-of-band tools cannot run here — use run_command to drive this host visibly. " +
		"If the remote shell has since changed, call probe_host with force=true to re-check."

	m := map[string]toolAvail{}
	for _, n := range oobToolNames {
		// exec is NOT exempt: with no channel there is nothing to execute on,
		// and the visible in-band fallback is POSIX too (see inBandAvailability).
		m[n] = toolAvail{State: toolUnavailable, Missing: missing, Detail: detail}
	}
	return m
}

// inBandAvailability reports what the VISIBLE fallbacks can do. They are not
// dialect-neutral: file_read, file_write and foreground exec all go through
// framing.RunSentinel, which types a POSIX command line with a printf sentinel.
// On a Windows or network-device host that is garbage, so claiming them
// available there was simply a wrong answer. Only run_command — which types the
// command bare — survives.
func inBandAvailability(d sshmux.Dialect) map[string]toolAvail {
	m := map[string]toolAvail{}
	posixHostile := d.Platform() == "windows" || d.Platform() == "network"
	for _, n := range oobToolNames {
		switch n {
		case "file_read", "file_write", "exec":
			if posixHostile {
				m[n] = toolAvail{
					State:   toolUnavailable,
					Missing: fmt.Sprintf("a POSIX shell (the visible fallback types a POSIX command line, but this host presents %s)", d.Human()),
					Detail:  "use run_command, which types the command bare and works on any shell",
				}
				continue
			}
			m[n] = toolAvail{State: toolAvailable} // visible fallbacks exist
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
		return fmt.Errorf("%s could not be initialized on %s: %s", tool, rt.host, detail)
	}
	msg := fmt.Sprintf("%s is unavailable on %s: it needs %s", tool, rt.host, av.Missing)
	if av.Install != "" {
		msg += fmt.Sprintf(". With the user's approval you can install it (run_command: %s), then retry", av.Install)
	} else if av.Detail != "" {
		msg += ". " + av.Detail
	}
	return errors.New(msg)
}
