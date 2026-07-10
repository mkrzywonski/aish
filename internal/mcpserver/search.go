package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/sshmux"
)

// Search primitives: the remote-host equivalents of the native Grep and Glob
// tools. They run on the host the shared session is currently on — ripgrep or
// grep/find over the OOB channel when remote, Go's own walk when local. Both
// are OOB-only (they refuse the visible in_band route, like the other native
// primitives) and are best-effort: results and the exact regex/glob dialect
// depend on the backend available on the host, so they don't promise byte-for-
// byte parity with the native tools.

const (
	grepDefaultResults = 200
	grepMaxResults     = 2000
	grepLineCap        = 1000      // per-match text is truncated to this many bytes
	grepFileSizeCap    = 8 << 20   // local: skip files larger than this
	grepScanCap        = 512 << 10 // remote: byte cap on the piped match output
	searchDefaultMax   = 1000
	searchMaxResults   = 10000
)

func registerSearchTools(s *mcp.Server, c *Core) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_grep",
		Annotations: readOnlyTool("Search file contents on session host"),
		Description: "Search file contents for a regular expression on the host the shared session is currently on — the " +
			"remote-host equivalent of your Grep tool. Uses ripgrep when present, else grep; best-effort and backend-" +
			"dependent (ripgrep honors .gitignore). Returns path/line/text matches, capped. Requires an authorized local " +
			"or remote OOB route.",
	}, c.fileGrep)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_search",
		Annotations: readOnlyTool("Find files by name on session host"),
		Description: "Find files under a directory by name glob on the host the shared session is currently on — the remote-" +
			"host equivalent of your Glob tool. Returns matching absolute paths, capped. Requires an authorized local or " +
			"remote OOB route.",
	}, c.fileSearch)
}

// ---- file_grep ----

type fileGrepArgs struct {
	SessionArg
	Path       string `json:"path" jsonschema:"absolute file or directory to search under"`
	Pattern    string `json:"pattern" jsonschema:"regular expression to search for"`
	Include    string `json:"include,omitempty" jsonschema:"only search files whose name matches this glob, e.g. *.go"`
	IgnoreCase bool   `json:"ignore_case,omitempty"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"cap matches returned (default 200, max 2000)"`
}

type grepMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type fileGrepResult struct {
	Matches   []grepMatch `json:"matches"`
	Truncated bool        `json:"truncated"`
	Via       string      `json:"via"`
	Host      string      `json:"host"`
	Warning   string      `json:"warning,omitempty"`
}

func (c *Core) fileGrep(ctx context.Context, req *mcp.CallToolRequest, args fileGrepArgs) (*mcp.CallToolResult, fileGrepResult, error) {
	if err := validateAbsolutePath(args.Path); err != nil {
		return nil, fileGrepResult{}, err
	}
	if args.Pattern == "" {
		return nil, fileGrepResult{}, errors.New("pattern must not be empty")
	}
	max := clampResults(args.MaxResults, grepDefaultResults, grepMaxResults)
	rt := c.route()
	if rt.via == "in_band" {
		return nil, fileGrepResult{}, oobPrimitiveError("file_grep", rt.host)
	}
	warning, _ := c.guardTarget(rt, opRead)

	var (
		matches   []grepMatch
		truncated bool
		err       error
	)
	if rt.via == "local" {
		matches, truncated, err = grepLocal(expandLocal(args.Path), args.Pattern, args.Include, args.IgnoreCase, max)
	} else {
		matches, truncated, err = c.grepRemote(rt, args, max)
	}
	if err != nil {
		return nil, fileGrepResult{}, err
	}
	return nil, fileGrepResult{Matches: matches, Truncated: truncated, Via: resultVia(rt), Host: rt.host, Warning: warning}, nil
}

// grepRemote runs ripgrep (preferred) or grep over the OOB channel. Both are
// asked to emit "path\0line:text" per match (NUL after the path so a colon in
// the path can't be confused with the line separator), newline-terminated, and
// the pipeline is bounded by result count and bytes.
func (c *Core) grepRemote(rt route, args fileGrepArgs, max int) ([]grepMatch, bool, error) {
	caps, _ := c.Mux.CachedCapabilities(rt.ci)
	p := sshmux.Quote(args.Path)
	pat := sshmux.Quote(args.Pattern)
	var tool string
	if caps.HasRg {
		tool = "rg --no-heading --null -n --color never"
		if args.IgnoreCase {
			tool += " -i"
		}
		if args.Include != "" {
			tool += " -g " + sshmux.Quote(args.Include)
		}
		tool += " -e " + pat + " -- " + p
	} else {
		tool = "grep -rnI --null"
		if args.IgnoreCase {
			tool += " -i"
		}
		if args.Include != "" {
			tool += " --include=" + sshmux.Quote(args.Include)
		}
		tool += " -e " + pat + " -- " + p
	}
	// head -n caps records (one extra to detect truncation); head -c caps bytes.
	cmd := fmt.Sprintf("%s | head -n %d | head -c %d", tool, max+1, grepScanCap)
	out, err := c.channelPipe(rt.ci, cmd, 60*time.Second)
	if err != nil {
		return nil, false, err
	}
	return parseGrep(out, max)
}

// parseGrep turns "path\0line:text\n" records into matches, capping at max.
func parseGrep(out []byte, max int) ([]grepMatch, bool, error) {
	var matches []grepMatch
	for _, rec := range strings.Split(string(out), "\n") {
		if rec == "" {
			continue
		}
		nul := strings.IndexByte(rec, 0)
		if nul < 0 {
			continue // partial/truncated record
		}
		path := rec[:nul]
		rest := rec[nul+1:]
		colon := strings.IndexByte(rest, ':')
		if colon < 0 {
			continue
		}
		line, convErr := strconv.Atoi(rest[:colon])
		if convErr != nil {
			continue
		}
		if len(matches) >= max {
			return matches, true, nil
		}
		matches = append(matches, grepMatch{Path: path, Line: line, Text: capText(rest[colon+1:])})
	}
	return matches, false, nil
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

// ---- file_search ----

type fileSearchArgs struct {
	SessionArg
	Path       string `json:"path" jsonschema:"absolute directory to search under"`
	Name       string `json:"name,omitempty" jsonschema:"filename glob to match, e.g. *.go (omit to match all)"`
	Type       string `json:"type,omitempty" jsonschema:"limit to a type: file, directory, or symlink"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"cap results (default 1000, max 10000)"`
}

type fileSearchResult struct {
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated"`
	Via       string   `json:"via"`
	Host      string   `json:"host"`
	Warning   string   `json:"warning,omitempty"`
}

func (c *Core) fileSearch(ctx context.Context, req *mcp.CallToolRequest, args fileSearchArgs) (*mcp.CallToolResult, fileSearchResult, error) {
	if err := validateAbsolutePath(args.Path); err != nil {
		return nil, fileSearchResult{}, err
	}
	if args.Type != "" && findTypeFlag(args.Type) == "" {
		return nil, fileSearchResult{}, fmt.Errorf("invalid type %q: use file, directory, or symlink", args.Type)
	}
	max := clampResults(args.MaxResults, searchDefaultMax, searchMaxResults)
	rt := c.route()
	if rt.via == "in_band" {
		return nil, fileSearchResult{}, oobPrimitiveError("file_search", rt.host)
	}
	warning, _ := c.guardTarget(rt, opRead)

	var (
		paths     []string
		truncated bool
		err       error
	)
	if rt.via == "local" {
		paths, truncated, err = searchLocal(expandLocal(args.Path), args.Name, args.Type, max)
	} else {
		paths, truncated, err = c.searchRemote(rt, args, max)
	}
	if err != nil {
		return nil, fileSearchResult{}, err
	}
	return nil, fileSearchResult{Paths: paths, Truncated: truncated, Via: resultVia(rt), Host: rt.host, Warning: warning}, nil
}

func (c *Core) searchRemote(rt route, args fileSearchArgs, max int) ([]string, bool, error) {
	caps, _ := c.Mux.CachedCapabilities(rt.ci)
	p := sshmux.Quote(args.Path)
	expr := "find " + p + " -mindepth 1"
	if tf := findTypeFlag(args.Type); tf != "" {
		expr += " " + tf
	}
	if args.Name != "" {
		expr += " -name " + sshmux.Quote(args.Name)
	}
	// GNU find/head support NUL framing (-print0 / head -z), which keeps names
	// with newlines unambiguous; other finds fall back to newline framing.
	var cmd, sep string
	if caps.GrepFlavor == "gnu" {
		cmd = fmt.Sprintf("%s -print0 | head -z -n %d", expr, max+1)
		sep = "\x00"
	} else {
		cmd = fmt.Sprintf("%s -print | head -n %d", expr, max+1)
		sep = "\n"
	}
	out, err := c.channelPipe(rt.ci, cmd, 60*time.Second)
	if err != nil {
		return nil, false, err
	}
	var paths []string
	for _, rec := range strings.Split(string(out), sep) {
		if rec == "" {
			continue
		}
		if len(paths) >= max {
			return paths, true, nil
		}
		paths = append(paths, rec)
	}
	return paths, false, nil
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

// ---- helpers ----

// channelPipe runs a pipeline over the OOB channel and returns its stdout,
// with stderr discarded. Unlike channelOutput it does not treat a nonzero exit
// as failure: these pipelines end in head, whose success masks a grep/find
// "no matches" exit, and error text must not pollute the parsed match stream.
func (c *Core) channelPipe(ci *sshmux.ConnInfo, remoteCmd string, timeout time.Duration) ([]byte, error) {
	res, err := c.Mux.ChannelRun(ci, "{ "+remoteCmd+"\n} </dev/null 2>/dev/null", timeout)
	if err != nil {
		return nil, err
	}
	if res.TimedOut {
		return nil, errors.New("oob channel search timed out")
	}
	return res.Output, nil
}

func findTypeFlag(typ string) string {
	switch typ {
	case "file":
		return "-type f"
	case "directory":
		return "-type d"
	case "symlink":
		return "-type l"
	}
	return ""
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

func clampResults(v, def, hi int) int {
	if v <= 0 {
		return def
	}
	if v > hi {
		return hi
	}
	return v
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
