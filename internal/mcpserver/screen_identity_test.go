package mcpserver

import (
	"strings"
	"testing"

	"ai-ssh/internal/sshmux"
	"ai-ssh/internal/term"
)

func TestFingerprintScreenIdentity(t *testing.T) {
	tests := []struct {
		name     string
		snap     term.Snapshot
		dialect  sshmux.Dialect
		platform string
	}{
		{
			name: "captured cmd screen",
			snap: term.Snapshot{
				Text:      "Microsoft Windows [Version 10.0.22631.3155]\n(c) Microsoft Corporation. All rights reserved.\ntxstate\\mk31@TAG232207 C:\\Users\\mk31>\n",
				CursorRow: 2,
			},
			dialect: sshmux.DialectCmd, platform: "windows",
		},
		{
			name:    "PowerShell drive prompt",
			snap:    term.Snapshot{Text: "PowerShell 7.6\nPS C:\\Users\\mk31>\n", CursorRow: 1},
			dialect: sshmux.DialectPowerShell, platform: "windows",
		},
		{
			name:    "PowerShell UNC provider prompt",
			snap:    term.Snapshot{Text: "PS Microsoft.PowerShell.Core\\FileSystem::\\\\server\\share>\n", CursorRow: 0},
			dialect: sshmux.DialectPowerShell, platform: "windows",
		},
		{
			name:     "drive prompt without corroboration is platform only",
			snap:     term.Snapshot{Text: "C:\\Users\\mk31>\n", CursorRow: 0},
			platform: "windows",
		},
		{
			name: "banner alone",
			snap: term.Snapshot{Text: "Microsoft Windows [Version 10.0.22631.3155]\n", CursorRow: 0},
		},
		{
			name: "stale banner with POSIX prompt",
			snap: term.Snapshot{Text: "Microsoft Windows [Version 10.0.22631.3155]\nmk31@host:~$\n", CursorRow: 1},
		},
		{
			name: "printed PowerShell prompt is not current",
			snap: term.Snapshot{Text: "PS C:\\Users\\mk31>\nmk31@host:~$\n", CursorRow: 1},
		},
		{
			name: "alternate screen",
			snap: term.Snapshot{Text: "PS C:\\Users\\mk31>\n", CursorRow: 0, AltScreen: true},
		},
		{
			name: "invalid cursor row",
			snap: term.Snapshot{Text: "C:\\Users\\mk31>\n", CursorRow: 4},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := fingerprintScreenIdentity(tc.snap)
			if got.Dialect != tc.dialect || got.Platform != tc.platform {
				t.Errorf("identity = %q/%q, want %q/%q (evidence %q)", got.Dialect, got.Platform, tc.dialect, tc.platform, got.Evidence)
			}
			if (tc.dialect != sshmux.DialectUnknown || tc.platform != "") && got.Evidence == "" {
				t.Error("recognized hint should carry evidence")
			}
		})
	}
}

func TestFingerprintScreenIdentityFromEmulatorSnapshot(t *testing.T) {
	screen := term.NewScreen(12, 100)
	_, err := screen.Write([]byte("Microsoft Windows [Version 10.0.22631.3155]\r\n(c) Microsoft Corporation\r\ntxstate\\mk31@TAG232207 C:\\Users\\mk31>"))
	if err != nil {
		t.Fatal(err)
	}

	got := fingerprintScreenIdentity(screen.Snapshot())
	if got.Dialect != sshmux.DialectCmd || got.Platform != "windows" {
		t.Errorf("emulated screen identity = %q/%q, want cmd/windows (evidence %q)", got.Dialect, got.Platform, got.Evidence)
	}
}

func TestApplyScreenIdentityHintDoesNotOverrideAuthoritativeFacts(t *testing.T) {
	res := sessionStatusResult{
		RemoteDialect:        string(sshmux.DialectPosix),
		RemotePlatform:       "unix",
		RemoteDialectSource:  string(sshmux.IdentitySourceShellProbe),
		RemotePlatformSource: string(sshmux.IdentitySourceShellProbe),
	}
	applyScreenIdentityHint(&res, screenIdentityHint{
		Dialect: sshmux.DialectPowerShell, Platform: "windows", Evidence: "PowerShell prompt",
	})

	if res.RemoteDialect != string(sshmux.DialectPosix) || res.RemotePlatform != "unix" {
		t.Errorf("screen hint overrode authoritative identity: %+v", res)
	}
	if res.RemoteDialectSource != string(sshmux.IdentitySourceShellProbe) || res.RemotePlatformSource != string(sshmux.IdentitySourceShellProbe) {
		t.Errorf("screen hint overrode authoritative sources: %+v", res)
	}
	if res.RemoteIdentityNote != "" {
		t.Errorf("unused screen hint produced a note: %q", res.RemoteIdentityNote)
	}
}

func TestApplyScreenIdentityHintFillsOnlyUnknownAxes(t *testing.T) {
	res := sessionStatusResult{
		RemotePlatform:       "windows",
		RemotePlatformSource: string(sshmux.IdentitySourceSFTP),
	}
	applyScreenIdentityHint(&res, screenIdentityHint{
		Dialect: sshmux.DialectPowerShell, Platform: "windows", Evidence: "PowerShell prompt",
	})

	if res.RemoteDialect != string(sshmux.DialectPowerShell) || res.RemoteDialectSource != screenIdentitySource {
		t.Errorf("screen dialect was not applied: %+v", res)
	}
	if res.RemotePlatformSource != string(sshmux.IdentitySourceSFTP) {
		t.Errorf("screen platform replaced SFTP evidence: %+v", res)
	}
	if !strings.Contains(res.RemoteIdentityNote, "Advisory") || !strings.Contains(res.RemoteIdentityNote, "does not change") {
		t.Errorf("screen note does not explain advisory semantics: %q", res.RemoteIdentityNote)
	}
}

func TestScreenIdentityHintDoesNotAffectCapabilityState(t *testing.T) {
	c := localOOBCore(t)
	ci := &sshmux.ConnInfo{Host: "winbox", User: "mk31", Port: "22", Sock: "/run/aish/cm-screen-only"}
	rt := route{via: "controlmaster", ci: ci, host: "winbox"}
	before := c.oobToolAvailability(rt)

	res := sessionStatusResult{}
	applyScreenIdentityHint(&res, screenIdentityHint{
		Dialect: sshmux.DialectCmd, Platform: "windows", Evidence: "screen-only cmd hint",
	})

	after := c.oobToolAvailability(rt)
	for _, tool := range oobToolNames {
		if before[tool] != after[tool] || after[tool].State != toolUnknown {
			t.Errorf("screen hint changed %s availability: before=%+v after=%+v", tool, before[tool], after[tool])
		}
	}
	if _, ok := c.Mux.Facts(ci); ok {
		t.Error("screen hint created durable host facts")
	}
	if got := c.remoteDialect(rt); got != sshmux.DialectUnknown {
		t.Errorf("screen hint changed authoritative remote dialect to %q", got)
	}
}

func TestRemoteIdentityStatus(t *testing.T) {
	tests := []struct {
		name           string
		dialectSource  string
		platformSource string
		want           string
	}{
		{name: "no evidence", want: remoteIdentityUnknown},
		{name: "screen dialect", dialectSource: screenIdentitySource, want: remoteIdentityAdvisory},
		{name: "screen platform", platformSource: screenIdentitySource, want: remoteIdentityAdvisory},
		{name: "shell probe", dialectSource: string(sshmux.IdentitySourceShellProbe), want: remoteIdentityAuthoritative},
		{name: "deep probe", dialectSource: string(sshmux.IdentitySourceDeepProbe), want: remoteIdentityAuthoritative},
		{name: "SFTP platform", platformSource: string(sshmux.IdentitySourceSFTP), want: remoteIdentityAuthoritative},
		{name: "authoritative wins", dialectSource: screenIdentitySource, platformSource: string(sshmux.IdentitySourceSFTP), want: remoteIdentityAuthoritative},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := remoteIdentityStatus(tc.dialectSource, tc.platformSource); got != tc.want {
				t.Errorf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAuthoritativePlatformDoesNotClaimCommandSyntax(t *testing.T) {
	status := remoteIdentityStatus("", string(sshmux.IdentitySourceSFTP))
	if status != remoteIdentityAuthoritative {
		t.Fatalf("status = %q, want authoritative platform evidence", status)
	}
	note := defaultRemoteIdentityNote(status, "")
	for _, phrase := range []string{"does not establish", "command syntax", "do not assume POSIX"} {
		if !strings.Contains(note, phrase) {
			t.Errorf("platform-only note missing %q: %q", phrase, note)
		}
	}
}
