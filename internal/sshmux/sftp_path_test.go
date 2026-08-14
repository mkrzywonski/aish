package sshmux

import (
	"strings"
	"testing"
)

func TestNormalizeSFTPPath(t *testing.T) {
	tests := []struct {
		name, style, input, server, native, errorContains string
	}{
		{name: "posix root", style: "posix", input: "/", server: "/", native: "/"},
		{name: "posix clean", style: "posix", input: "/srv/a/../b file/./c", server: "/srv/b file/c", native: "/srv/b file/c"},
		{name: "posix unicode", style: "posix", input: "/srv/資料/é", server: "/srv/資料/é", native: "/srv/資料/é"},
		{name: "posix relative", style: "posix", input: "srv/file", errorContains: "absolute POSIX"},
		{name: "posix windows ambiguity", style: "posix", input: `C:\temp\file`, errorContains: "Windows syntax"},
		{name: "posix slash-drive ambiguity", style: "posix", input: "/C:/temp/file", errorContains: "Windows syntax"},
		{name: "windows root", style: "windows", input: `C:\`, server: "/C:/", native: `C:\`},
		{name: "windows native", style: "windows", input: `c:\Users\Mike\a b.txt`, server: "/C:/Users/Mike/a b.txt", native: `C:\Users\Mike\a b.txt`},
		{name: "windows slash native", style: "windows", input: "D:/work/file", server: "/D:/work/file", native: `D:\work\file`},
		{name: "windows server form", style: "windows", input: "/C:/Users/mk31/file", server: "/C:/Users/mk31/file", native: `C:\Users\mk31\file`},
		{name: "windows clean", style: "windows", input: `C:\work\.\one\..\two`, server: "/C:/work/two", native: `C:\work\two`},
		{name: "windows unicode", style: "windows", input: `C:\資料\é.txt`, server: "/C:/資料/é.txt", native: `C:\資料\é.txt`},
		{name: "windows relative", style: "windows", input: `work\file`, errorContains: "drive-absolute"},
		{name: "windows posix ambiguity", style: "windows", input: "/home/mike", errorContains: "drive-absolute"},
		{name: "windows drive relative", style: "windows", input: `C:file`, errorContains: "drive-absolute"},
		{name: "windows rooted relative", style: "windows", input: `\Users\Mike`, errorContains: "drive-absolute"},
		{name: "windows UNC", style: "windows", input: `\\server\share\file`, errorContains: "UNC"},
		{name: "windows escape root", style: "windows", input: `C:\..\file`, errorContains: "escapes"},
		{name: "unknown style", style: "unknown", input: "/file", errorContains: "style is unknown"},
		{name: "empty", style: "posix", input: "", errorContains: "must not be empty"},
		{name: "NUL", style: "posix", input: "/a\x00b", errorContains: "NUL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeSFTPPath(tt.style, tt.input)
			if tt.errorContains != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errorContains) {
					t.Fatalf("error = %v, want containing %q", err, tt.errorContains)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Server != tt.server || got.Native != tt.native {
				t.Errorf("path = %+v, want server=%q native=%q", got, tt.server, tt.native)
			}
		})
	}
}
