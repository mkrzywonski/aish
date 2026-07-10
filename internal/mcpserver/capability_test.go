package mcpserver

import (
	"testing"

	"ai-ssh/internal/sshmux"
)

func TestCapabilityAvailabilityGNU(t *testing.T) {
	caps := sshmux.Capabilities{
		OS: "Linux", HasBase64: true, Base64D: true, StatC: true, HasFind: true,
		FindPrint: true, HeadZ: true, HasGrep: true, GrepNull: true, Hasher: "sha256sum",
		PkgMgr: "apt-get",
	}
	av := capabilityAvailability(caps)
	for _, tool := range oobToolNames {
		if !av[tool].Available() {
			t.Errorf("GNU host: %s should be available, got %+v", tool, av[tool])
		}
	}
}

func TestCapabilityAvailabilityBusyBox(t *testing.T) {
	// Alpine/BusyBox: base64 + stat -c work, but no find -printf/head -z/grep --null.
	// With fallbacks, everything is still available (grep and stat -c cover it).
	caps := sshmux.Capabilities{
		OS: "Linux", HasBase64: true, Base64D: true, StatC: true, HasFind: true,
		HasGrep: true, Hasher: "sha256sum", PkgMgr: "apk",
	}
	av := capabilityAvailability(caps)
	for _, tool := range oobToolNames {
		if !av[tool].Available() {
			t.Errorf("BusyBox host: %s should still be available via fallback, got %+v", tool, av[tool])
		}
	}
}

func TestCapabilityAvailabilityMissingTools(t *testing.T) {
	// No base64, no stat, no find/grep: content, stat, listing, and search are
	// unavailable — with an install hint — while exec stays available.
	caps := sshmux.Capabilities{OS: "Linux", PkgMgr: "apt-get"}
	av := capabilityAvailability(caps)
	if !av["exec"].Available() {
		t.Fatal("exec should always be available")
	}
	for _, tool := range []string{"file_read", "file_write", "file_stat", "directory_list", "file_grep", "file_search"} {
		if av[tool].Available() {
			t.Errorf("%s should be unavailable, got %+v", tool, av[tool])
		}
		if av[tool].Install == "" {
			t.Errorf("%s should carry an install hint", tool)
		}
	}
	if got := av["file_read"].Install; got != "apt-get install -y coreutils" {
		t.Errorf("file_read install hint = %q", got)
	}
}

func TestCapabilityAvailabilityUnsupported(t *testing.T) {
	av := capabilityAvailability(sshmux.Capabilities{Unsupported: true})
	if !av["exec"].Available() {
		t.Error("exec should still be offered on unsupported hosts")
	}
	if av["file_read"].Available() {
		t.Error("file_read should be unavailable on an unsupported host")
	}
}

func TestInstallHint(t *testing.T) {
	cases := map[string]string{
		"apt-get": "apt-get install -y coreutils",
		"apk":     "apk add coreutils",
		"brew":    "brew install coreutils",
		"pacman":  "pacman -S --noconfirm coreutils",
		"":        "",
	}
	for mgr, want := range cases {
		if got := installHint(mgr, "coreutils"); got != want {
			t.Errorf("installHint(%q) = %q, want %q", mgr, got, want)
		}
	}
	if installHint("apt-get", "") != "" {
		t.Error("empty package → empty hint")
	}
}

func TestOobAvailabilityUnknownUntilProbed(t *testing.T) {
	c := localOOBCore(t)

	// A controlmaster route whose channel is not open/probed reports every tool
	// as unknown (not optimistic-available) with a pointer to probe_host.
	rt := route{via: "controlmaster", ci: &sshmux.ConnInfo{Sock: "/nonexistent.sock"}, host: "web"}
	av := c.oobToolAvailability(rt)
	for _, tool := range oobToolNames {
		if av[tool].State != toolUnknown {
			t.Errorf("unprobed host: %s state = %q, want unknown", tool, av[tool].State)
		}
		if av[tool].Available() {
			t.Errorf("unprobed %s should not report Available()", tool)
		}
		if av[tool].Detail == "" {
			t.Errorf("unprobed %s should carry a detail pointing at probe_host", tool)
		}
	}

	// A local route knows its state without a channel: everything available.
	for _, tool := range oobToolNames {
		if a := c.oobToolAvailability(route{via: "local", host: "local"})[tool]; a.State != toolAvailable {
			t.Errorf("local %s state = %q, want available", tool, a.State)
		}
	}

	// in_band: the visible fallbacks are available, the rest unavailable.
	ib := c.oobToolAvailability(route{via: "in_band", host: "web"})
	for _, tool := range []string{"file_read", "file_write", "exec"} {
		if ib[tool].State != toolAvailable {
			t.Errorf("in_band %s state = %q, want available", tool, ib[tool].State)
		}
	}
	if ib["file_grep"].State != toolUnavailable {
		t.Errorf("in_band file_grep state = %q, want unavailable", ib["file_grep"].State)
	}
}
