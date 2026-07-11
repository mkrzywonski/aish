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
	if err := c.requireTool(rt, "file_grep"); err != nil {
		return nil, fileGrepResult{}, err
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

// grepRemote runs ripgrep (preferred), grep --null, or plain grep over the OOB
// channel, chosen from the probe. It classifies the producer's real exit —
// 0 (matches) / 1 (none) are fine; ≥2 is a tool error surfaced to the model —
// so a missing/incompatible grep never reads as a silent "no matches".
func (c *Core) grepRemote(rt route, args fileGrepArgs, max int) ([]grepMatch, bool, error) {
	caps, _ := c.Mux.CachedCapabilities(rt.ci)
	producer, nullFramed := grepCommand(caps, args)
	out, exit, capped, err := c.channelClassified(rt.ci, producer, grepScanCap, 60*time.Second)
	if err != nil {
		return nil, false, err
	}
	if exit >= 2 { // grep/rg: 0 = matches, 1 = none, ≥2 = error
		return nil, false, fmt.Errorf("file_grep failed on %s (exit %d): %.200s", rt.host, exit, out)
	}
	var matches []grepMatch
	var truncated bool
	if nullFramed {
		matches, truncated, _ = parseGrep(out, max)
	} else {
		matches, truncated = parseGrepColon(out, max)
	}
	return matches, truncated || capped, nil
}

// grepCommand builds the remote grep/ripgrep command and reports whether its
// output is NUL-framed ("path\0line:text", unambiguous) or plain colon-framed
// ("path:line:text", the last-resort fallback for grep without --null).
func grepCommand(caps sshmux.Capabilities, args fileGrepArgs) (string, bool) {
	p := sshmux.Quote(args.Path)
	pat := sshmux.Quote(args.Pattern)
	ic := ""
	if args.IgnoreCase {
		ic = " -i"
	}
	switch {
	case caps.HasRg:
		cmd := "rg --no-heading --null -n --color never" + ic
		if args.Include != "" {
			cmd += " -g " + sshmux.Quote(args.Include)
		}
		return cmd + " -e " + pat + " -- " + p, true
	case caps.GrepNull:
		cmd := "grep -rnI --null" + ic
		if args.Include != "" {
			cmd += " --include=" + sshmux.Quote(args.Include)
		}
		return cmd + " -e " + pat + " -- " + p, true
	default:
		cmd := "grep -rnI" + ic
		if args.Include != "" {
			cmd += " --include=" + sshmux.Quote(args.Include)
		}
		return cmd + " -e " + pat + " -- " + p, false
	}
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
			continue // partial/truncated record (or a stderr warning line)
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

// parseGrepColon parses plain "path:line:text" (grep without --null). A colon
// in the path is ambiguous here — best-effort, as documented for this fallback.
func parseGrepColon(out []byte, max int) ([]grepMatch, bool) {
	var matches []grepMatch
	for _, rec := range strings.Split(string(out), "\n") {
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, ":", 3)
		if len(f) != 3 {
			continue
		}
		line, convErr := strconv.Atoi(f[1])
		if convErr != nil {
			continue
		}
		if len(matches) >= max {
			return matches, true
		}
		matches = append(matches, grepMatch{Path: f[0], Line: line, Text: capText(f[2])})
	}
	return matches, false
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
	if err := c.requireTool(rt, "file_search"); err != nil {
		return nil, fileSearchResult{}, err
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
	// -H follows a symlinked search root (e.g. macOS /etc -> /private/etc) so it
	// isn't treated as a leaf and skipped by -mindepth; find still doesn't follow
	// symlinks it meets underneath.
	p := sshmux.Quote(args.Path)
	expr := "find -H " + p + " -mindepth 1"
	if tf := findTypeFlag(args.Type); tf != "" {
		expr += " " + tf
	}
	if args.Name != "" {
		expr += " -name " + sshmux.Quote(args.Name)
	}
	// GNU find supports NUL framing (-print0), which keeps names with newlines
	// unambiguous; other finds fall back to newline framing (best-effort).
	// 2>/dev/null drops find's per-entry diagnostics (e.g. a "Permission denied"
	// on an unreadable subdir) so they can't pollute the path list.
	var producer, sep string
	if caps.FindPrint {
		producer, sep = expr+" -print0 2>/dev/null", "\x00"
	} else {
		producer, sep = expr+" -print 2>/dev/null", "\n"
	}
	out, exit, capped, err := c.channelClassified(rt.ci, producer, grepScanCap, 60*time.Second)
	if err != nil {
		return nil, false, err
	}
	var paths []string
	truncated := capped
	for _, rec := range strings.Split(string(out), sep) {
		if rec == "" {
			continue
		}
		if len(paths) >= max {
			truncated = true
			break
		}
		paths = append(paths, rec)
	}
	// find exits nonzero when it couldn't read some traversed dir, even though
	// the matches it already printed are valid. Only treat nonzero as failure
	// when nothing matched at all (then the path likely doesn't exist / isn't
	// readable) — otherwise return the valid partial results.
	if exit != 0 && len(paths) == 0 {
		return nil, false, fmt.Errorf("file_search found nothing on %s and find exited %d (the path may not exist or be inaccessible)", rt.host, exit)
	}
	return paths, truncated, nil
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

// channelClassified runs a search producer over the OOB channel and returns its
// output (stdout+stderr merged), the producer's own exit status, and whether a
// byte cap truncated the output. The producer's exit is carried by a trailing
// "@AISHRC@<code>" marker (emitted before the byte cap), so a producer failure
// is never masked by the trailing head the way channelPipe/head pipelines are.
// If the marker is missing, a large *successful* output was byte-capped.
func (c *Core) channelClassified(ci *sshmux.ConnInfo, producer string, byteCap int, timeout time.Duration) (out []byte, exit int, capped bool, err error) {
	cmd := fmt.Sprintf("{ %s\necho '@AISHRC@'$?; } </dev/null 2>&1 | head -c %d", producer, byteCap)
	res, err := c.Mux.ChannelRun(ci, cmd, timeout)
	if err != nil {
		return nil, 0, false, err
	}
	if res.TimedOut {
		return nil, 0, false, errors.New("oob channel search timed out")
	}
	data := res.Output
	i := bytes.LastIndex(data, []byte("@AISHRC@"))
	if i < 0 {
		return data, 0, true, nil // marker lost → byte cap truncated a big result
	}
	code := strings.TrimRight(string(data[i+len("@AISHRC@"):]), "\r\n")
	exit, convErr := strconv.Atoi(code)
	if convErr != nil {
		exit = 0
	}
	return data[:i], exit, false, nil
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
