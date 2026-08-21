package main

import (
	"encoding/base64"
	"encoding/json"

	"ai-ssh/internal/aicmdwire"
)

const defaultMaxFileRead = 256 << 10 // matches aish's own default (internal/mcpserver/tools_remote.go's maxFileRead)

func handleFileRead(wc *aicmdwire.Conn, f aicmdwire.Frame) {
	var req aicmdwire.FileReadData
	if err := json.Unmarshal(f.Data, &req); err != nil {
		return
	}
	if reason := access.checkOther(); reason != "" {
		send(wc, "file_read_result", f.ID, aicmdwire.FileReadResultData{Error: reason})
		return
	}
	max := req.MaxBytes
	if max <= 0 {
		max = defaultMaxFileRead
	}
	data, eof, err := readFile(req.Path, req.Offset, max)
	if err != nil {
		send(wc, "file_read_result", f.ID, aicmdwire.FileReadResultData{Error: err.Error()})
		return
	}
	send(wc, "file_read_result", f.ID, aicmdwire.FileReadResultData{
		Content: base64.StdEncoding.EncodeToString(data),
		Eof:     eof,
	})
}

func handleFileWrite(wc *aicmdwire.Conn, f aicmdwire.Frame) {
	var req aicmdwire.FileWriteData
	if err := json.Unmarshal(f.Data, &req); err != nil {
		return
	}
	if reason := access.checkOther(); reason != "" {
		send(wc, "file_write_result", f.ID, aicmdwire.FileWriteResultData{Error: reason})
		return
	}
	data, err := base64.StdEncoding.DecodeString(req.Content)
	if err != nil {
		send(wc, "file_write_result", f.ID, aicmdwire.FileWriteResultData{Error: "malformed content: " + err.Error()})
		return
	}

	var n int
	if req.Append {
		n, err = appendFile(req.Path, data, req.Mode)
	} else {
		if err = writeFileAtomic(req.Path, data, req.Mode, req.IfMatch); err == nil {
			n = len(data)
		}
	}
	if err != nil {
		send(wc, "file_write_result", f.ID, aicmdwire.FileWriteResultData{Error: err.Error()})
		return
	}
	send(wc, "file_write_result", f.ID, aicmdwire.FileWriteResultData{BytesWritten: n})
}

func handleFileStat(wc *aicmdwire.Conn, f aicmdwire.Frame) {
	var req aicmdwire.FileStatData
	if err := json.Unmarshal(f.Data, &req); err != nil {
		return
	}
	if reason := access.checkOther(); reason != "" {
		send(wc, "file_stat_result", f.ID, aicmdwire.FileStatResultData{Error: reason})
		return
	}
	kind, mode, size, modifiedUnix, err := statFile(req.Path)
	if err != nil {
		send(wc, "file_stat_result", f.ID, aicmdwire.FileStatResultData{Error: err.Error()})
		return
	}
	send(wc, "file_stat_result", f.ID, aicmdwire.FileStatResultData{
		Type: kind, Size: size, Mode: mode, ModifiedUnix: modifiedUnix,
	})
}

func handleDirectoryList(wc *aicmdwire.Conn, f aicmdwire.Frame) {
	var req aicmdwire.DirectoryListData
	if err := json.Unmarshal(f.Data, &req); err != nil {
		return
	}
	if reason := access.checkOther(); reason != "" {
		send(wc, "directory_list_result", f.ID, aicmdwire.DirectoryListResultData{Error: reason})
		return
	}
	max := req.MaxEntries
	if max <= 0 {
		max = 1000
	}
	entries, truncated, err := listDir(req.Path, max)
	if err != nil {
		send(wc, "directory_list_result", f.ID, aicmdwire.DirectoryListResultData{Error: err.Error()})
		return
	}
	wireEntries := make([]aicmdwire.DirEntryData, len(entries))
	for i, e := range entries {
		wireEntries[i] = aicmdwire.DirEntryData{Name: e.Name, Type: e.Type, Size: e.Size, ModifiedUnix: e.ModifiedUnix}
	}
	send(wc, "directory_list_result", f.ID, aicmdwire.DirectoryListResultData{Entries: wireEntries, Truncated: truncated})
}
