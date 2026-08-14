package main

import (
	"runtime/debug"
	"testing"
)

func TestVersionFromSettings(t *testing.T) {
	tests := []struct {
		name     string
		settings []debug.BuildSetting
		want     string
	}{
		{name: "no VCS metadata", want: "dev"},
		{
			name: "clean revision",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "5561c90c134d9399d74543e83d48bf59947282db"},
				{Key: "vcs.modified", Value: "false"},
			},
			want: "g5561c90c134d",
		},
		{
			name: "dirty revision",
			settings: []debug.BuildSetting{
				{Key: "vcs.modified", Value: "true"},
				{Key: "vcs.revision", Value: "5561c90"},
			},
			want: "g5561c90-dirty",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionFromSettings(tc.settings); got != tc.want {
				t.Fatalf("versionFromSettings() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveVersionPreservesLinkerStamp(t *testing.T) {
	if got := resolveVersion("v0.2.2-18-g5561c90"); got != "v0.2.2-18-g5561c90" {
		t.Fatalf("resolveVersion() = %q", got)
	}
}
