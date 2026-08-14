package mcpserver

import (
	"strings"
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

// TestOobAvailabilityNonPosixHost: once a probe has conclusively identified a
// non-POSIX login shell, every tool reads "unavailable" — and crucially the
// detail must STOP inviting probe_host, which is what made models re-probe a
// host that can never succeed.
func TestOobAvailabilityNonPosixHost(t *testing.T) {
	c := localOOBCore(t)
	ci := &sshmux.ConnInfo{Host: "winbox", User: "mk31", Port: "22", Sock: "/run/aish/cm-win"}
	c.Mux.NoteShellUnusable(ci, sshmux.DialectCmd,
		"the remote ssh login shell is Windows cmd.exe",
		"'sh' is not recognized as an internal or external command,", true)

	av := c.oobToolAvailability(route{via: "controlmaster", ci: ci, host: "winbox"})
	for _, tool := range oobToolNames {
		a := av[tool]
		if a.State != toolUnavailable {
			t.Errorf("%s state = %q, want unavailable", tool, a.State)
		}
		if !strings.Contains(a.Missing, "cmd.exe") {
			t.Errorf("%s should name the dialect, got Missing=%q", tool, a.Missing)
		}
		if a.Install != "" {
			t.Errorf("%s should carry no install hint — no package fixes a login shell", tool)
		}
		if strings.Contains(a.Detail, "probe_host to initialize") {
			t.Errorf("%s still invites the re-probe loop: %q", tool, a.Detail)
		}
		if !strings.Contains(a.Detail, "run_command") {
			t.Errorf("%s should point at run_command, got %q", tool, a.Detail)
		}
	}
}

func TestBlockedProbeResultReportsIdentitySources(t *testing.T) {
	c := localOOBCore(t)
	ci := &sshmux.ConnInfo{Host: "winbox", User: "mk31", Port: "22", Sock: "/run/aish/cm-source"}
	f := c.Mux.NoteShellUnusable(ci, sshmux.DialectCmd,
		"the remote ssh login shell is Windows cmd.exe",
		"'sh' is not recognized as an internal or external command,", true)

	res := c.blockedProbeResult(route{via: "controlmaster", ci: ci, host: "winbox"}, f)
	if res.RemoteDialectSource != string(sshmux.IdentitySourceShellProbe) {
		t.Errorf("dialect source = %q, want shell_probe", res.RemoteDialectSource)
	}
	if res.RemotePlatformSource != string(sshmux.IdentitySourceShellProbe) {
		t.Errorf("platform source = %q, want shell_probe", res.RemotePlatformSource)
	}
	if res.RemoteDialect != string(sshmux.DialectCmd) || res.RemotePlatform != "windows" {
		t.Errorf("identity = %q/%q, want cmd/windows", res.RemoteDialect, res.RemotePlatform)
	}
}

// TestOobAvailabilityRetryableFailureStaysUnknown: an unclassified failure is a
// transport fact, so the toolset stays "unknown" — but the detail has to say
// what a retry costs rather than cheerfully suggesting one.
func TestOobAvailabilityRetryableFailureStaysUnknown(t *testing.T) {
	c := localOOBCore(t)
	ci := &sshmux.ConnInfo{Host: "flaky", User: "mk31", Port: "22", Sock: "/run/aish/cm-flaky"}
	c.Mux.NoteShellUnusable(ci, sshmux.DialectUnknown, "", "", false)

	av := c.oobToolAvailability(route{via: "controlmaster", ci: ci, host: "flaky"})
	a := av["file_read"]
	if a.State != toolUnknown {
		t.Errorf("state = %q, want unknown while retries remain", a.State)
	}
	if !strings.Contains(a.Detail, "already failed") {
		t.Errorf("detail should say a probe already failed, got %q", a.Detail)
	}
	if !strings.Contains(a.Detail, "MFA") {
		t.Errorf("detail should say what a retry costs, got %q", a.Detail)
	}
}

// TestInBandUnavailableOnNonPosix: the visible fallbacks are not dialect
// neutral. file_read/file_write/exec go through RunSentinel, which types a
// POSIX command line — on cmd.exe that is garbage, so reporting them available
// was a wrong answer rather than a degraded one.
func TestInBandUnavailableOnNonPosix(t *testing.T) {
	c := localOOBCore(t)
	ci := &sshmux.ConnInfo{Host: "winbox", User: "mk31", Port: "22", Sock: "/run/aish/cm-win2"}
	c.Mux.NoteShellUnusable(ci, sshmux.DialectCmd, "cmd.exe", "'sh' is not recognized", true)

	ib := c.oobToolAvailability(route{via: "in_band", ci: ci, host: "winbox"})
	for _, tool := range []string{"file_read", "file_write", "exec"} {
		if ib[tool].State != toolUnavailable {
			t.Errorf("in_band %s on a cmd.exe host state = %q, want unavailable", tool, ib[tool].State)
		}
		if !strings.Contains(ib[tool].Detail, "run_command") {
			t.Errorf("in_band %s should point at run_command, got %q", tool, ib[tool].Detail)
		}
	}

	// A POSIX host (or one not yet classified) keeps the fallbacks.
	plain := c.oobToolAvailability(route{via: "in_band", host: "linuxbox"})
	if plain["file_read"].State != toolAvailable {
		t.Errorf("in_band file_read on an unclassified host = %q, want available", plain["file_read"].State)
	}
}

// TestTrackingApplicableFor: [p] types a bash/zsh snippet, so offering it on
// cmd.exe or PowerShell just puts broken shell in the user's terminal — and it
// could not help anyway, since target_confidence on a non-POSIX host is pinned
// at "unknown". An unclassified remote is still offered it, because most
// remotes are POSIX and that is the case the feature exists for.
func TestTrackingApplicableFor(t *testing.T) {
	cases := []struct {
		via     string
		dialect sshmux.Dialect
		want    bool
	}{
		{"local", sshmux.DialectUnknown, false},
		{"local", sshmux.DialectPosix, false},
		{"controlmaster", sshmux.DialectUnknown, true},
		{"controlmaster", sshmux.DialectPosix, true},
		{"controlmaster", sshmux.DialectCmd, false},
		{"controlmaster", sshmux.DialectPowerShell, false},
		{"controlmaster", sshmux.DialectNetworkOS, false},
		{"in_band", sshmux.DialectUnknown, true},
		{"in_band", sshmux.DialectCmd, false},
	}
	for _, tc := range cases {
		if got := trackingApplicableFor(tc.via, tc.dialect); got != tc.want {
			t.Errorf("trackingApplicableFor(%q, %q) = %v, want %v", tc.via, tc.dialect, got, tc.want)
		}
	}
}
