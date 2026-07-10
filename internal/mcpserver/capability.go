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
		m := map[string]toolAvail{}
		for _, n := range oobToolNames {
			switch n {
			case "file_read", "file_write", "exec":
				m[n] = toolAvail{State: toolAvailable} // visible fallbacks exist
			default:
				m[n] = toolAvail{State: toolUnavailable, Missing: "an out-of-band route (no multiplexed channel to this host)"}
			}
		}
		return m
	}
	caps, ok := c.Mux.CachedCapabilities(rt.ci)
	if !ok {
		// Not probed yet: report unknown rather than guess. This is honest, not
		// a tollgate — the first real op still auto-probes (requireTool →
		// EnsureProbed) and gates correctly. The AI can call probe_host to
		// resolve these deliberately before planning a workflow.
		m := map[string]toolAvail{}
		for _, n := range oobToolNames {
			m[n] = toolAvail{State: toolUnknown, Detail: "host not probed yet; call probe_host to initialize the out-of-band toolset"}
		}
		return m
	}
	return capabilityAvailability(caps)
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
		// Only reachable if EnsureProbed didn't populate the cache; surface it
		// rather than proceed blind.
		return fmt.Errorf("%s: %s on %s could not be initialized (call probe_host)", tool, tool, rt.host)
	}
	msg := fmt.Sprintf("%s is unavailable on %s: it needs %s", tool, rt.host, av.Missing)
	if av.Install != "" {
		msg += fmt.Sprintf(". With the user's approval you can install it (run_command: %s), then retry", av.Install)
	}
	return errors.New(msg)
}
