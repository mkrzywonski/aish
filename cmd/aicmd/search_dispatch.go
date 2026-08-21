package main

import (
	"encoding/json"

	"ai-ssh/internal/aicmdwire"
)

const (
	grepDefaultResults = 200
	grepMaxResults     = 2000
	searchDefaultMax   = 1000
	searchMaxResults   = 10000
)

func handleGrep(wc *aicmdwire.Conn, f aicmdwire.Frame) {
	var req aicmdwire.GrepData
	if err := json.Unmarshal(f.Data, &req); err != nil {
		return
	}
	if reason := access.checkOther(); reason != "" {
		send(wc, "file_grep_result", f.ID, aicmdwire.GrepResultData{Error: reason})
		return
	}
	max := clampResults(req.MaxResults, grepDefaultResults, grepMaxResults)
	matches, truncated, err := grepLocal(req.Path, req.Pattern, req.Include, req.IgnoreCase, max)
	if err != nil {
		send(wc, "file_grep_result", f.ID, aicmdwire.GrepResultData{Error: err.Error()})
		return
	}
	wireMatches := make([]aicmdwire.GrepMatchData, len(matches))
	for i, m := range matches {
		wireMatches[i] = aicmdwire.GrepMatchData{Path: m.Path, Line: m.Line, Text: m.Text}
	}
	send(wc, "file_grep_result", f.ID, aicmdwire.GrepResultData{Matches: wireMatches, Truncated: truncated})
}

func handleSearch(wc *aicmdwire.Conn, f aicmdwire.Frame) {
	var req aicmdwire.SearchData
	if err := json.Unmarshal(f.Data, &req); err != nil {
		return
	}
	if reason := access.checkOther(); reason != "" {
		send(wc, "file_search_result", f.ID, aicmdwire.SearchResultData{Error: reason})
		return
	}
	max := clampResults(req.MaxResults, searchDefaultMax, searchMaxResults)
	paths, truncated, err := searchLocal(req.Path, req.Name, req.Type, max)
	if err != nil {
		send(wc, "file_search_result", f.ID, aicmdwire.SearchResultData{Error: err.Error()})
		return
	}
	send(wc, "file_search_result", f.ID, aicmdwire.SearchResultData{Paths: paths, Truncated: truncated})
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
