package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"ai-ssh/internal/sshmux"
)

func TestSFTPProbeResultReportsIdentityWithoutChangingTools(t *testing.T) {
	c := localOOBCore(t)
	ci := &sshmux.ConnInfo{Host: "winbox", User: "mk31", Port: "22", Sock: "/run/aish/cm-sftp"}
	c.Mux.NoteIdentity(ci, sshmux.DialectUnknown, "windows", sshmux.IdentitySourceSFTP, "SFTP realpath(.) returned /C:/Users/mk31")
	rt := route{via: "controlmaster", ci: ci, host: "winbox"}
	before := c.oobToolAvailability(rt)

	res := c.sftpProbeResult(rt, sshmux.SFTPProbeResult{Axis: sshmux.SftpAxis{
		State: sshmux.AxisUp, Attempts: 1, RealPath: "/C:/Users/mk31", PathStyle: "windows",
		Extensions: []string{"posix-rename@openssh.com"}, ProbedAt: time.Now(),
	}})

	if res.SFTPStatus != "up" || res.SFTPAttempts != 1 || res.SFTPRealPath != "/C:/Users/mk31" || res.SFTPPathStyle != "windows" {
		t.Errorf("SFTP result = %+v", res)
	}
	if res.RemotePlatform != "windows" || res.RemotePlatformSource != string(sshmux.IdentitySourceSFTP) {
		t.Errorf("platform identity = %q/%q, want windows/sftp", res.RemotePlatform, res.RemotePlatformSource)
	}
	if res.RemoteDialect != "" || res.RemoteIdentityStatus != remoteIdentityAuthoritative {
		t.Errorf("SFTP path evidence claimed dialect or lost authority: %+v", res)
	}
	if !strings.Contains(res.RemoteIdentityNote, "does not establish") {
		t.Errorf("platform-only identity lacks syntax warning: %q", res.RemoteIdentityNote)
	}
	for tool, want := range before {
		if got := res.OobTools[tool]; got != want {
			t.Errorf("%s availability changed: got %+v, want %+v", tool, got, want)
		}
	}
}

func TestSFTPProbeFailureExplainsCachedMFARetry(t *testing.T) {
	c := localOOBCore(t)
	ci := &sshmux.ConnInfo{Host: "appliance", Sock: "/run/aish/cm-sftp-fail"}
	res := c.sftpProbeResult(route{via: "controlmaster", ci: ci, host: "appliance"}, sshmux.SFTPProbeResult{
		Axis: sshmux.SftpAxis{State: sshmux.AxisDown, Attempts: 1, Reason: "subsystem request failed"},
	})
	for _, phrase := range []string{"cached", "MFA", "sftp=true", "force=true"} {
		if !strings.Contains(res.SFTPNote, phrase) {
			t.Errorf("SFTP failure note missing %q: %q", phrase, res.SFTPNote)
		}
	}
}

func TestProbeHostRejectsDeepAndSFTPTogether(t *testing.T) {
	c := localOOBCore(t)
	_, _, err := c.probeHost(context.Background(), nil, probeHostArgs{Deep: true, SFTP: true})
	if err == nil || !strings.Contains(err.Error(), "only one") {
		t.Fatalf("deep+sftp error = %v", err)
	}
}

func TestSFTPProbeWithoutControlMasterDoesNotOpenAnything(t *testing.T) {
	c := localOOBCore(t)
	_, res, err := c.probeHostSFTP(context.Background(), route{via: "in_band", host: "unknown-remote"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.SFTPStatus != "" || res.RemoteIdentityStatus != remoteIdentityUnknown {
		t.Errorf("no-ControlMaster result = %+v", res)
	}
	if !strings.Contains(res.Note, "requires a live ControlMaster") {
		t.Errorf("result lacks transport explanation: %q", res.Note)
	}
}
