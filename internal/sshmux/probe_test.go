package sshmux

import "testing"

func TestParseCapabilities(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want Capabilities
	}{
		{
			name: "gnu linux with rg and sha256sum",
			out: "uname=Linux x86_64\nuser=mike\nhostname=web01\npwd=/home/mike\n" +
				"rg=/usr/bin/rg\nsha256sum=/usr/bin/sha256sum\nshasum=\nmktemp=/usr/bin/mktemp\n" +
				"grep_version=grep (GNU grep) 3.7\n",
			want: Capabilities{
				OS: "Linux", Arch: "x86_64", Hostname: "web01", User: "mike", Cwd: "/home/mike",
				HasRg: true, Hasher: "sha256sum", GrepFlavor: "gnu", HasMktemp: true,
			},
		},
		{
			name: "busybox: no rg, no sha256sum, shasum absent, empty grep version",
			out: "uname=Linux armv7l\nuser=root\nhostname=pi\npwd=/root\n" +
				"rg=\nsha256sum=\nshasum=\nmktemp=/bin/mktemp\ngrep_version=\n",
			want: Capabilities{
				OS: "Linux", Arch: "armv7l", Hostname: "pi", User: "root", Cwd: "/root",
				HasRg: false, Hasher: "none", GrepFlavor: "other", HasMktemp: true,
			},
		},
		{
			name: "bsd/macos: shasum fallback, non-GNU grep",
			out: "uname=Darwin arm64\nuser=mike\nhostname=mac\npwd=/Users/mike\n" +
				"rg=\nsha256sum=\nshasum=/usr/bin/shasum\nmktemp=/usr/bin/mktemp\n" +
				"grep_version=grep (BSD grep, GNU compatible) 2.6.0-FreeBSD\n",
			want: Capabilities{
				OS: "Darwin", Arch: "arm64", Hostname: "mac", User: "mike", Cwd: "/Users/mike",
				HasRg: false, Hasher: "shasum", GrepFlavor: "other", HasMktemp: true,
			},
		},
		{
			name: "non-posix: empty uname marks unsupported",
			out:  "uname=\nuser=\nhostname=\npwd=\nrg=\nsha256sum=\nshasum=\nmktemp=\ngrep_version=\n",
			want: Capabilities{Hasher: "none", GrepFlavor: "other", Unsupported: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCapabilities([]byte(tc.out))
			if got != tc.want {
				t.Fatalf("parseCapabilities:\n got  %+v\n want %+v", got, tc.want)
			}
		})
	}
}

func TestParseCapabilitiesMissingLineDoesNotDrift(t *testing.T) {
	// A missing tool yields an empty value; a dropped line must not shift the
	// remaining keys. Here 'shasum' line is absent entirely.
	out := "uname=Linux x86_64\nsha256sum=/usr/bin/sha256sum\nmktemp=/usr/bin/mktemp\n"
	got := parseCapabilities([]byte(out))
	if got.Hasher != "sha256sum" {
		t.Fatalf("hasher = %q, want sha256sum", got.Hasher)
	}
	if !got.HasMktemp {
		t.Fatalf("HasMktemp = false, want true")
	}
	if got.HasRg {
		t.Fatalf("HasRg = true, want false (rg line absent)")
	}
}
