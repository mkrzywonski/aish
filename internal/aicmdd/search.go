package aicmdd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/aicmdwire"
)

// These mirror aish's own file_grep/file_search schemas
// (internal/mcpserver/search.go) minus SessionArg, same reasoning as the
// other tool files. The actual search/walk runs on cmd/aicmd
// (cmd/aicmd/search.go, a direct port of aish's own local-route grepLocal/
// searchLocal) since that's where the Windows filesystem is.

type fileGrepArgs struct {
	Path       string `json:"path" jsonschema:"absolute file or directory to search under, on the Windows host"`
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
}

type fileSearchArgs struct {
	Path       string `json:"path" jsonschema:"absolute directory to search under, on the Windows host"`
	Name       string `json:"name,omitempty" jsonschema:"filename glob to match, e.g. *.go (omit to match all)"`
	Type       string `json:"type,omitempty" jsonschema:"limit to a type: file, directory, or symlink"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"cap results (default 1000, max 10000)"`
}

type fileSearchResult struct {
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated"`
	Via       string   `json:"via"`
	Host      string   `json:"host"`
}

func registerSearchTools(s *mcp.Server, sess *aicmdSession) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_grep",
		Annotations: readOnlyTool("Search file contents on Windows host"),
		Description: "Search file contents for a regular expression on the Windows host — the Windows equivalent " +
			"of your Grep tool. Pure Go walk + regexp (not shelling out to rg/grep/findstr), so results are " +
			"consistent regardless of what's installed. Returns path/line/text matches, capped.",
	}, sess.fileGrep)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_search",
		Annotations: readOnlyTool("Find files by name on Windows host"),
		Description: "Find files under a directory by name glob on the Windows host — the Windows equivalent of " +
			"your Glob tool. Returns matching absolute paths, capped.",
	}, sess.fileSearch)
}

func (s *aicmdSession) fileGrep(ctx context.Context, req *mcp.CallToolRequest, args fileGrepArgs) (*mcp.CallToolResult, fileGrepResult, error) {
	if args.Path == "" {
		return nil, fileGrepResult{}, errors.New("path must not be empty")
	}
	if args.Pattern == "" {
		return nil, fileGrepResult{}, errors.New("pattern must not be empty")
	}

	data, err := json.Marshal(aicmdwire.GrepData{
		Path: args.Path, Pattern: args.Pattern, Include: args.Include,
		IgnoreCase: args.IgnoreCase, MaxResults: args.MaxResults,
	})
	if err != nil {
		return nil, fileGrepResult{}, err
	}
	raw, err := s.roundTrip("file_grep", data, 60*time.Second)
	if err != nil {
		return nil, fileGrepResult{}, err
	}
	var res aicmdwire.GrepResultData
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fileGrepResult{}, fmt.Errorf("malformed file_grep result from the Windows peer: %w", err)
	}
	if res.Error != "" {
		return nil, fileGrepResult{}, errors.New(res.Error)
	}
	matches := make([]grepMatch, len(res.Matches))
	for i, m := range res.Matches {
		matches[i] = grepMatch{Path: m.Path, Line: m.Line, Text: m.Text}
	}
	return nil, fileGrepResult{Matches: matches, Truncated: res.Truncated, Via: "aicmd", Host: s.displayHost()}, nil
}

func (s *aicmdSession) fileSearch(ctx context.Context, req *mcp.CallToolRequest, args fileSearchArgs) (*mcp.CallToolResult, fileSearchResult, error) {
	if args.Path == "" {
		return nil, fileSearchResult{}, errors.New("path must not be empty")
	}
	if args.Type != "" && args.Type != "file" && args.Type != "directory" && args.Type != "symlink" {
		return nil, fileSearchResult{}, fmt.Errorf("invalid type %q: use file, directory, or symlink", args.Type)
	}

	data, err := json.Marshal(aicmdwire.SearchData{Path: args.Path, Name: args.Name, Type: args.Type, MaxResults: args.MaxResults})
	if err != nil {
		return nil, fileSearchResult{}, err
	}
	raw, err := s.roundTrip("file_search", data, 60*time.Second)
	if err != nil {
		return nil, fileSearchResult{}, err
	}
	var res aicmdwire.SearchResultData
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fileSearchResult{}, fmt.Errorf("malformed file_search result from the Windows peer: %w", err)
	}
	if res.Error != "" {
		return nil, fileSearchResult{}, errors.New(res.Error)
	}
	return nil, fileSearchResult{Paths: res.Paths, Truncated: res.Truncated, Via: "aicmd", Host: s.displayHost()}, nil
}
