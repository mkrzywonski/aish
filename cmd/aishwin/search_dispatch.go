package main

import (
	"encoding/json"
	"fmt"

	"ai-ssh/internal/aishwinwire"
)

const (
	grepDefaultResults = 200
	grepMaxResults     = 2000
	searchDefaultMax   = 1000
	searchMaxResults   = 10000
)

func handleGrep(wc *aishwinwire.Conn, f aishwinwire.Frame) {
	var req aishwinwire.GrepData
	if err := json.Unmarshal(f.Data, &req); err != nil {
		return
	}
	max := clampResults(req.MaxResults, grepDefaultResults, grepMaxResults)
	AppendLogColor(fmt.Sprintf("Grepping %s for %q", req.Path, req.Pattern), colorFileOp)
	matches, truncated, err := grepLocal(req.Path, req.Pattern, req.Include, req.IgnoreCase, max)
	if err != nil {
		send(wc, "file_grep_result", f.ID, aishwinwire.GrepResultData{Error: err.Error()})
		return
	}
	wireMatches := make([]aishwinwire.GrepMatchData, len(matches))
	for i, m := range matches {
		wireMatches[i] = aishwinwire.GrepMatchData{Path: m.Path, Line: m.Line, Text: m.Text}
	}
	send(wc, "file_grep_result", f.ID, aishwinwire.GrepResultData{Matches: wireMatches, Truncated: truncated})
}

func handleSearch(wc *aishwinwire.Conn, f aishwinwire.Frame) {
	var req aishwinwire.SearchData
	if err := json.Unmarshal(f.Data, &req); err != nil {
		return
	}
	max := clampResults(req.MaxResults, searchDefaultMax, searchMaxResults)
	AppendLogColor(fmt.Sprintf("Finding files in %s matching %q", req.Path, req.Name), colorFileOp)
	paths, truncated, err := searchLocal(req.Path, req.Name, req.Type, max)
	if err != nil {
		send(wc, "file_search_result", f.ID, aishwinwire.SearchResultData{Error: err.Error()})
		return
	}
	send(wc, "file_search_result", f.ID, aishwinwire.SearchResultData{Paths: paths, Truncated: truncated})
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
