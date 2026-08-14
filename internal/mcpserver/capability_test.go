package mcpserver

import (
	"context"
	"strings"
	"testing"

	"ai-ssh/internal/sshmux"
	"ai-ssh/internal/term"
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

	// An unclassified in_band route must not assume the visible shell is POSIX.
	ib := c.oobToolAvailability(route{via: "in_band", host: "web"})
	for _, tool := range []string{"file_read", "file_write", "exec"} {
		if ib[tool].State != toolUnknown {
			t.Errorf("unclassified in_band %s state = %q, want unknown", tool, ib[tool].State)
		}
		if !strings.Contains(ib[tool].Detail, "do not assume POSIX") {
			t.Errorf("unclassified in_band %s lacks syntax warning: %q", tool, ib[tool].Detail)
		}
	}
	if ib["file_grep"].State != toolUnavailable {
		t.Errorf("in_band file_grep state = %q, want unavailable", ib["file_grep"].State)
	}
}

// TestOobAvailabilityNonPosixHost: a conclusive shell failure leaves only the
// implemented SFTP file fallbacks unknown. Shell-only tools are unavailable,
// and no state invites the old shell re-probe loop.
func TestOobAvailabilityNonPosixHost(t *testing.T) {
	c := localOOBCore(t)
	ci := &sshmux.ConnInfo{Host: "winbox", User: "mk31", Port: "22", Sock: "/run/aish/cm-win"}
	c.Mux.NoteShellUnusable(ci, sshmux.DialectCmd,
		"the remote ssh login shell is Windows cmd.exe",
		"'sh' is not recognized as an internal or external command,", true)

	av := c.oobToolAvailability(route{via: "controlmaster", ci: ci, host: "winbox"})
	for _, tool := range append(append([]string(nil), sftpReadToolNames...), sftpWriteToolNames...) {
		a := av[tool]
		if a.State != toolUnknown {
			t.Errorf("%s state = %q, want unknown before SFTP opens", tool, a.State)
		}
		if !strings.Contains(a.Detail, "SFTP") || !strings.Contains(a.Detail, "MFA") {
			t.Errorf("%s should explain the lazy SFTP cost, got %q", tool, a.Detail)
		}
	}
	for _, tool := range []string{"exec", "file_grep", "file_search"} {
		a := av[tool]
		if a.State != toolUnavailable {
			t.Errorf("%s state = %q, want unavailable", tool, a.State)
		}
		if !strings.Contains(a.Missing, "cmd.exe") {
			t.Errorf("%s should name the dialect, got Missing=%q", tool, a.Missing)
		}
		if !strings.Contains(a.Detail, "run_command") {
			t.Errorf("%s should point at run_command, got %q", tool, a.Detail)
		}
	}
}

func TestSFTPAvailabilityMerge(t *testing.T) {
	base := sshmux.HostFacts{Shell: sshmux.ShellAxis{State: sshmux.AxisDown, Sticky: true}}
	t.Run("up with atomic rename", func(t *testing.T) {
		facts := base
		facts.SFTP = sshmux.SftpAxis{State: sshmux.AxisUp, Extensions: []string{"posix-rename@openssh.com"}}
		av := availability(facts)
		for _, tool := range append(append([]string(nil), sftpReadToolNames...), sftpWriteToolNames...) {
			if !av[tool].Available() {
				t.Errorf("%s = %+v, want available", tool, av[tool])
			}
		}
		for _, tool := range []string{"exec", "file_grep", "file_search"} {
			if av[tool].State != toolUnavailable {
				t.Errorf("%s = %+v, want unavailable", tool, av[tool])
			}
		}
	})
	t.Run("up without atomic rename", func(t *testing.T) {
		facts := base
		facts.SFTP = sshmux.SftpAxis{State: sshmux.AxisUp}
		av := availability(facts)
		for _, tool := range sftpReadToolNames {
			if !av[tool].Available() {
				t.Errorf("%s = %+v, want available", tool, av[tool])
			}
		}
		for _, tool := range sftpWriteToolNames {
			if av[tool].State != toolUnavailable || !strings.Contains(av[tool].Missing, "posix-rename") {
				t.Errorf("%s = %+v, want atomic-rename unavailable", tool, av[tool])
			}
		}
	})
	t.Run("cached down", func(t *testing.T) {
		facts := base
		facts.SFTP = sshmux.SftpAxis{State: sshmux.AxisDown, Reason: "subsystem disabled"}
		av := availability(facts)
		for _, tool := range append(append([]string(nil), sftpReadToolNames...), sftpWriteToolNames...) {
			if av[tool].State != toolUnavailable || !strings.Contains(av[tool].Detail, "force=true") {
				t.Errorf("%s = %+v, want cached-down guidance", tool, av[tool])
			}
		}
	})
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
	if res.RemoteIdentityStatus != remoteIdentityAuthoritative {
		t.Errorf("identity status = %q, want authoritative", res.RemoteIdentityStatus)
	}
}

func TestDeepProbeResultReportsIdentityWithoutChangingAvailability(t *testing.T) {
	c := localOOBCore(t)
	ci := &sshmux.ConnInfo{Host: "winbox", User: "mk31", Port: "22", Sock: "/run/aish/cm-deep"}
	c.Mux.NoteIdentity(ci, sshmux.DialectCmd, "windows", sshmux.IdentitySourceDeepProbe,
		"percent variables expanded while PowerShell and POSIX variables remained literal")
	rt := route{via: "controlmaster", ci: ci, host: "winbox"}
	before := c.oobToolAvailability(rt)

	exit := 0
	res := c.deepProbeResult(rt, sshmux.DeepProbeResult{
		Status: sshmux.DeepProbeIdentified, Dialect: sshmux.DialectCmd, Platform: "windows",
		Evidence: "percent variables expanded", Exit: exit, Attempts: 1, Cached: true,
	})

	if res.RemoteDialect != string(sshmux.DialectCmd) || res.RemotePlatform != "windows" {
		t.Errorf("identity = %q/%q, want cmd/windows", res.RemoteDialect, res.RemotePlatform)
	}
	if res.RemoteDialectSource != string(sshmux.IdentitySourceDeepProbe) || res.RemotePlatformSource != string(sshmux.IdentitySourceDeepProbe) {
		t.Errorf("identity sources = %q/%q, want deep_probe", res.RemoteDialectSource, res.RemotePlatformSource)
	}
	if res.RemoteIdentityStatus != remoteIdentityAuthoritative {
		t.Errorf("identity status = %q, want authoritative", res.RemoteIdentityStatus)
	}
	if res.Probed || res.RemoteHost != nil {
		t.Errorf("deep identity claimed shell capability: %+v", res)
	}
	if !res.DeepProbeCached || res.DeepProbeExit == nil || *res.DeepProbeExit != 0 {
		t.Errorf("deep result metadata = %+v", res)
	}
	for tool, want := range before {
		if got := res.OobTools[tool]; got != want {
			t.Errorf("%s availability changed: got %+v, want %+v", tool, got, want)
		}
	}
}

func TestDeepProbeFailureNoteStopsImplicitRetries(t *testing.T) {
	for _, status := range []sshmux.DeepProbeStatus{sshmux.DeepProbeUnknown, sshmux.DeepProbeFailed} {
		note := deepProbeNote(sshmux.DeepProbeResult{Status: status, Reason: "probe did not identify the shell"})
		for _, phrase := range []string{"cached", "MFA", "deep=true", "force=true"} {
			if !strings.Contains(note, phrase) {
				t.Errorf("%s note missing %q: %q", status, phrase, note)
			}
		}
		if strings.Contains(note, "call probe_host to initialize") {
			t.Errorf("%s note invites an implicit retry: %q", status, note)
		}
	}
}

func TestDeepProbeWithoutControlMasterReportsUnknownSyntax(t *testing.T) {
	c := localOOBCore(t)
	_, res, err := c.probeHostDeep(context.Background(), route{via: "in_band", host: "mystery"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteIdentityStatus != remoteIdentityUnknown {
		t.Errorf("identity status = %q, want unknown", res.RemoteIdentityStatus)
	}
	for _, phrase := range []string{"command syntax", "do not assume POSIX"} {
		if !strings.Contains(res.RemoteIdentityNote, phrase) {
			t.Errorf("identity note missing %q: %q", phrase, res.RemoteIdentityNote)
		}
	}
}

func TestProbeResultRetainsIdentityWhenTransportFallsBackInBand(t *testing.T) {
	c := localOOBCore(t)
	ci := &sshmux.ConnInfo{Host: "winbox", Sock: "/run/aish/cm-gone"}
	c.Mux.NoteIdentity(ci, sshmux.DialectPowerShell, "windows", sshmux.IdentitySourceDeepProbe, "PSOS=Windows_NT")
	res := probeHostResult{Via: "in_band", Host: "winbox"}
	c.setProbeIdentityStatus(&res, route{via: "in_band", ci: ci, host: "winbox"})

	if res.RemoteDialect != string(sshmux.DialectPowerShell) || res.RemotePlatform != "windows" {
		t.Errorf("identity = %q/%q, want powershell/windows", res.RemoteDialect, res.RemotePlatform)
	}
	if res.RemoteIdentityStatus != remoteIdentityAuthoritative {
		t.Errorf("identity status = %q, want authoritative", res.RemoteIdentityStatus)
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

// TestInBandAvailabilityRequiresPosixIdentity is the route/dialect matrix for
// visible sentinel framing. Only proven POSIX is allowed: unknown cannot be
// guessed, and every known non-POSIX dialect must fail closed even when its
// coarse platform is empty (restricted/no_shell).
func TestInBandAvailabilityRequiresPosixIdentity(t *testing.T) {
	c := localOOBCore(t)
	tests := []struct {
		name    string
		dialect sshmux.Dialect
		want    string
	}{
		{name: "unknown", dialect: sshmux.DialectUnknown, want: toolUnknown},
		{name: "posix", dialect: sshmux.DialectPosix, want: toolAvailable},
		{name: "cmd", dialect: sshmux.DialectCmd, want: toolUnavailable},
		{name: "powershell", dialect: sshmux.DialectPowerShell, want: toolUnavailable},
		{name: "network", dialect: sshmux.DialectNetworkOS, want: toolUnavailable},
		{name: "restricted", dialect: sshmux.DialectRestricted, want: toolUnavailable},
		{name: "no shell", dialect: sshmux.DialectNoShell, want: toolUnavailable},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ci := &sshmux.ConnInfo{Host: tc.name, Sock: "/run/aish/cm-matrix-" + string(rune('a'+i))}
			if tc.dialect != sshmux.DialectUnknown {
				c.Mux.NoteIdentity(ci, tc.dialect, tc.dialect.Platform(), sshmux.IdentitySourceDeepProbe, "test identity")
			}
			ib := c.oobToolAvailability(route{via: "in_band", ci: ci, host: tc.name})
			for _, tool := range []string{"file_read", "file_write", "exec"} {
				if ib[tool].State != tc.want {
					t.Errorf("%s state = %q, want %q", tool, ib[tool].State, tc.want)
				}
				if tc.want != toolAvailable && !strings.Contains(ib[tool].Detail, "run_command") {
					t.Errorf("%s should point at run_command, got %q", tool, ib[tool].Detail)
				}
			}
		})
	}
}

// TestUnknownInBandFileReadStopsBeforeFraming drives the real file_read
// handler with Engine deliberately nil. A regression that reaches RunSentinel
// will panic; the safe path returns the identity error before emitting bytes.
func TestUnknownInBandFileReadStopsBeforeFraming(t *testing.T) {
	c := localOOBCore(t)
	events := make(chan term.Event, 1)
	events <- term.Event{Kind: term.EvCwd, Host: "mystery-appliance", Cwd: "/"}
	close(events)
	c.Tracker.Consume(events)

	_, _, err := c.fileRead(context.Background(), nil, fileReadArgs{Path: "/etc/hostname"})
	if err == nil {
		t.Fatal("unknown in-band file_read unexpectedly proceeded")
	}
	for _, phrase := range []string{"not safe", "syntax is unknown", "do not assume POSIX"} {
		if !strings.Contains(err.Error(), phrase) {
			t.Errorf("error missing %q: %v", phrase, err)
		}
	}
}

// TestUnknownInBandExecStopsBeforeFraming covers exec separately: unlike the
// file handlers, it historically failed to enforce its reported availability.
func TestUnknownInBandExecStopsBeforeFraming(t *testing.T) {
	c := localOOBCore(t)
	events := make(chan term.Event, 1)
	events <- term.Event{Kind: term.EvCwd, Host: "mystery-appliance", Cwd: "/"}
	close(events)
	c.Tracker.Consume(events)

	_, _, err := c.execTool(context.Background(), nil, execArgs{Command: "show version"})
	if err == nil {
		t.Fatal("unknown in-band exec unexpectedly proceeded")
	}
	for _, phrase := range []string{"not safe", "syntax is unknown", "do not assume POSIX"} {
		if !strings.Contains(err.Error(), phrase) {
			t.Errorf("error missing %q: %v", phrase, err)
		}
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

func TestReadOnlyFallbackActionRequiresConclusiveShellFailure(t *testing.T) {
	tests := []struct {
		name  string
		facts sshmux.HostFacts
		ok    bool
		want  sftpFallbackAction
	}{
		{name: "no facts", want: fallbackRefuseShell},
		{name: "shell unknown", ok: true, want: fallbackRefuseShell},
		{name: "shell up", ok: true, facts: sshmux.HostFacts{Shell: sshmux.ShellAxis{State: sshmux.AxisUp}}, want: fallbackRefuseShell},
		{name: "soft shell failure", ok: true, facts: sshmux.HostFacts{Shell: sshmux.ShellAxis{State: sshmux.AxisDown}}, want: fallbackRefuseShell},
		{name: "probe sftp", ok: true, facts: sshmux.HostFacts{Shell: sshmux.ShellAxis{State: sshmux.AxisDown, Sticky: true}}, want: fallbackProbeSFTP},
		{name: "use sftp", ok: true, facts: sshmux.HostFacts{Shell: sshmux.ShellAxis{State: sshmux.AxisDown, Sticky: true}, SFTP: sshmux.SftpAxis{State: sshmux.AxisUp}}, want: fallbackUseSFTP},
		{name: "cached sftp failure", ok: true, facts: sshmux.HostFacts{Shell: sshmux.ShellAxis{State: sshmux.AxisDown, Sticky: true}, SFTP: sshmux.SftpAxis{State: sshmux.AxisDown}}, want: fallbackRefuseSFTP},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fileFallbackAction(tt.facts, tt.ok); got != tt.want {
				t.Errorf("action = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateAbsolutePathShapeRecognizesBothTargetStyles(t *testing.T) {
	for _, path := range []string{"/etc/hosts", `C:\Users\mk31\file`, "D:/work/file", "/C:/Users/mk31/file"} {
		if err := validateAbsolutePathShape(path); err != nil {
			t.Errorf("validateAbsolutePathShape(%q): %v", path, err)
		}
	}
	for _, path := range []string{"", "relative/file", `C:file`, `\\server\share\file`} {
		if err := validateAbsolutePathShape(path); err == nil {
			t.Errorf("validateAbsolutePathShape(%q) succeeded", path)
		}
	}
}
