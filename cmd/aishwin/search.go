package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// grepLocal and searchLocal are direct ports of aish's own local-route
// implementations (internal/mcpserver/search.go's grepLocal/searchLocal) —
// pure Go (filepath.WalkDir + regexp), not shelling out to rg/grep/find at
// all for the local case, so they're already cross-platform and need no
// Windows-specific rewrite. Only the remote-over-SSH path in aish shells
// out to ripgrep/grep/find; that doesn't apply here since aishwin always
// walks its own (Windows) local filesystem, the same relationship
// file_stat/directory_list already have to aish's local route. Copied
// rather than imported for the same reason as patch.go's diff logic: it
// lives in package mcpserver, and extracting a shared package means
// editing that existing, working file for this feature's sake.

const grepFileSizeCap = 8 << 20 // skip files larger than this
const grepLineCap = 1000        // per-match text is truncated to this many bytes

type grepMatch struct {
	Path string
	Line int
	Text string
}

func grepLocal(root, pattern, include string, ignoreCase bool, max int) ([]grepMatch, bool, error) {
	expr := pattern
	if ignoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, false, fmt.Errorf("invalid pattern: %w", err)
	}
	var matches []grepMatch
	truncated := false
	scanned := 0
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if len(matches) >= max {
			truncated = true
			return filepath.SkipAll
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if include != "" {
			if ok, _ := filepath.Match(include, d.Name()); !ok {
				return nil
			}
		}
		if info, err := d.Info(); err == nil && info.Size() > grepFileSizeCap {
			return nil
		}
		if scanned++; scanned > 20000 {
			truncated = true
			return filepath.SkipAll
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if isBinary(data) {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				matches = append(matches, grepMatch{Path: p, Line: i + 1, Text: capText(line)})
				if len(matches) >= max {
					truncated = true
					break
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		return nil, false, walkErr
	}
	return matches, truncated, nil
}

func searchLocal(root, name, typ string, max int) ([]string, bool, error) {
	var paths []string
	truncated := false
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p == root {
			return nil // -mindepth 1
		}
		if len(paths) >= max {
			truncated = true
			return filepath.SkipAll
		}
		if !localTypeMatch(typ, d) {
			return nil
		}
		if name != "" {
			if ok, _ := filepath.Match(name, d.Name()); !ok {
				return nil
			}
		}
		paths = append(paths, p)
		return nil
	})
	if walkErr != nil {
		return nil, false, walkErr
	}
	return paths, truncated, nil
}

func localTypeMatch(typ string, d fs.DirEntry) bool {
	switch typ {
	case "file":
		return d.Type().IsRegular()
	case "directory":
		return d.IsDir()
	case "symlink":
		return d.Type()&fs.ModeSymlink != 0
	}
	return true
}

func capText(s string) string {
	if len(s) > grepLineCap {
		return s[:grepLineCap]
	}
	return s
}

func isBinary(data []byte) bool {
	n := min(len(data), 8192)
	return bytes.IndexByte(data[:n], 0) >= 0
}
