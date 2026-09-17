package sshmux

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCapabilities(t *testing.T) {
	out := "uname=Linux x86_64\nuser=mike\nhostname=web01\npwd=/home/mike\n" +
		"rg=/usr/bin/rg\ngrep=/bin/grep\nfind=/usr/bin/find\nsha256sum=/usr/bin/sha256sum\nshasum=\nmktemp=/usr/bin/mktemp\n" +
		"pkg=/usr/bin/apt-get\n" +
		"base64=1\nbase64d=1\nbase64D=\nstatc=1\nstatf=\nfindprintf=1\nheadz=1\ngrepnull=1\nlineread=1\n"
	c := parseCapabilities([]byte(out))
	want := Capabilities{
		OS: "Linux", Arch: "x86_64", Hostname: "web01", User: "mike", Cwd: "/home/mike",
		HasRg: true, HasGrep: true, HasFind: true, Hasher: "sha256sum", HasMktemp: true, PkgMgr: "apt-get",
		HasBase64: true, Base64D: true, StatC: true, FindPrint: true, HeadZ: true, GrepNull: true, LineRead: true,
	}
	if c != want {
		t.Fatalf("gnu host:\n got  %+v\n want %+v", c, want)
	}
	if c.Base64Decode() != "-d" {
		t.Fatalf("Base64Decode = %q, want -d", c.Base64Decode())
	}
}

func TestParseCapabilitiesBSD(t *testing.T) {
	// macOS/BSD: base64 decodes with -D, stat uses -f, no GNU find -printf/head -z,
	// hasher is shasum, package manager is brew.
	out := "uname=Darwin arm64\nuser=mike\nhostname=mac\npwd=/Users/mike\n" +
		"rg=\nsha256sum=\nshasum=/usr/bin/shasum\nmktemp=/usr/bin/mktemp\n" +
		"pkg=/opt/homebrew/bin/brew\n" +
		"base64=1\nbase64d=\nbase64D=1\nstatc=\nstatf=1\nfindprintf=\nheadz=\ngrepnull=1\n"
	c := parseCapabilities([]byte(out))
	if c.OS != "Darwin" || c.Hasher != "shasum" || c.PkgMgr != "brew" {
		t.Fatalf("unexpected: %+v", c)
	}
	if c.StatC || !c.StatF || c.FindPrint || c.HeadZ {
		t.Fatalf("BSD flavor flags wrong: %+v", c)
	}
	if c.Base64Decode() != "-D" {
		t.Fatalf("Base64Decode = %q, want -D", c.Base64Decode())
	}
}

func TestParseCapabilitiesBusyBox(t *testing.T) {
	// Alpine/BusyBox: stat -c works, but no find -printf / head -z / grep --null.
	out := "uname=Linux aarch64\nuser=root\nhostname=pi\npwd=/root\n" +
		"rg=\nsha256sum=/usr/bin/sha256sum\nshasum=\nmktemp=/bin/mktemp\n" +
		"pkg=/sbin/apk\n" +
		"base64=1\nbase64d=1\nbase64D=\nstatc=1\nstatf=\nfindprintf=\nheadz=\ngrepnull=\n"
	c := parseCapabilities([]byte(out))
	if !c.StatC || c.FindPrint || c.HeadZ || c.GrepNull {
		t.Fatalf("busybox flags wrong: %+v", c)
	}
	if c.PkgMgr != "apk" || c.Hasher != "sha256sum" {
		t.Fatalf("unexpected: %+v", c)
	}
}

func TestParseCapabilitiesEmptyOutput(t *testing.T) {
	// A probe that returns no recognizable key=value (e.g. a stripped host, or a
	// missing uname) must NOT flip Unsupported — that wrongly disabled every file
	// tool on hosts whose base64/stat/find/grep all work. A truly non-POSIX shell
	// is caught earlier in channel.go (the sentinel never arrives), before caps
	// are cached. Here every capability is simply absent, so per-tool availability
	// reports them unavailable individually.
	c := parseCapabilities([]byte("garbage output, not key=value\n"))
	if c.Unsupported {
		t.Fatalf("empty/garbage probe output should not mark Unsupported: %+v", c)
	}
	if c.HasBase64 || c.HasFind || c.HasGrep || c.StatC || c.StatF || c.LineRead || c.Hasher != "none" {
		t.Fatalf("no capabilities should be reported present: %+v", c)
	}
	if c.Base64Decode() != "" {
		t.Fatalf("no base64 → Base64Decode should be empty, got %q", c.Base64Decode())
	}
}

func TestLineReadProbe(t *testing.T) {
	realTools := make(map[string]string)
	for _, name := range []string{"head", "tail", "wc"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s unavailable: %v", name, err)
		}
		realTools[name] = path
	}
	for _, tc := range []struct {
		name, tool, override string
		want                 bool
	}{
		{name: "working tools", want: true},
		{name: "padded wc output", tool: "wc", override: "printf '   3\\n'; exit 0", want: true},
		{name: "head line option ignored", tool: "head", override: "if [ \"$1\" = -n ]; then printf 'ab\\ncd\\n'; exit 0; fi"},
		{name: "head byte option ignored", tool: "head", override: "if [ \"$1\" = -c ]; then printf abc; exit 0; fi"},
		{name: "head correct output but failure", tool: "head", override: "printf ab; exit 1"},
		{name: "wc wrong count", tool: "wc", override: "printf '4\\n'; exit 0"},
		{name: "wc failure", tool: "wc", override: "printf '3\\n'; exit 1"},
		{name: "tail offset ignored", tool: "tail", override: "printf abc; exit 0"},
		{name: "tail failure", tool: "tail", override: "printf bc; exit 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.tool != "" {
				script := "#!/bin/sh\n" + tc.override + "\nexec " + Quote(realTools[tc.tool]) + " \"$@\"\n"
				if err := os.WriteFile(filepath.Join(dir, tc.tool), []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			// Exercise its actual embedding in the full probe, including the
			// labeled output and parser, rather than just the test expression.
			out, err := exec.Command("/bin/sh", "-c", probeScript).CombinedOutput()
			if err != nil {
				t.Fatalf("probe: %v\n%s", err, out)
			}
			if got := parseCapabilities(out).LineRead; got != tc.want {
				t.Fatalf("LineRead = %v, want %v; probe output:\n%s", got, tc.want, out)
			}
		})
	}
}

func TestLineReadCapabilityIsIndependent(t *testing.T) {
	for _, value := range []string{"", "0", "unsupported", "1"} {
		c := parseCapabilities([]byte("base64=1\nlineread=" + value + "\n"))
		if !c.HasBase64 || c.LineRead != (value == "1") {
			t.Fatalf("line capability %q changed byte capability: %+v", value, c)
		}
		data, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"line_read":`) {
			t.Fatalf("missing line_read JSON field: %s", data)
		}
	}
}

func TestParseCapabilitiesMissingLineDoesNotDrift(t *testing.T) {
	out := "uname=Linux x86_64\nsha256sum=/usr/bin/sha256sum\nmktemp=/usr/bin/mktemp\n"
	c := parseCapabilities([]byte(out))
	if c.Hasher != "sha256sum" || !c.HasMktemp || c.HasRg {
		t.Fatalf("drift: %+v", c)
	}
}
