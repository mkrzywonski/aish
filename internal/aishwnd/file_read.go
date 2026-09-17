package aishwnd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/aishwinwire"
)

const (
	defaultFileReadBytes = 16 << 10
	maxFileReadBytes     = 256 << 10
	// Leave room for visibility middleware, proxy notices and JSON-RPC framing.
	fileReadResultBudget = 60 << 10
)

type fileReadOptions struct {
	offset    int64
	startLine int64
	limit     int
	maxBytes  int
	lineMode  bool
	numbered  bool
}

func validateFileRead(args fileReadArgs) (fileReadOptions, error) {
	o := fileReadOptions{maxBytes: defaultFileReadBytes, lineMode: args.StartLine != nil || args.Limit != nil, numbered: args.LineNumbers}
	if args.Path == "" {
		return o, errors.New("path must not be empty")
	}
	if args.Offset != nil {
		if o.lineMode {
			return o, errors.New("offset is in bytes and cannot be combined with start_line or limit; use start_line=241, limit=160 for lines 241-400")
		}
		o.offset = *args.Offset
	}
	if o.offset < 0 || o.offset > math.MaxInt64-maxFileReadBytes {
		return o, errors.New("offset must be nonnegative and leave room for a page of bytes")
	}
	if args.MaxBytes != nil {
		o.maxBytes = *args.MaxBytes
		if o.maxBytes <= 0 || o.maxBytes > maxFileReadBytes {
			return o, fmt.Errorf("max_bytes must be between 1 and %d (or omitted)", maxFileReadBytes)
		}
	}
	if o.numbered && o.offset != 0 {
		return o, errors.New("line_numbers cannot label a nonzero byte offset; use start_line and limit instead")
	}
	if o.lineMode || o.numbered {
		o.startLine = 1
	}
	if o.lineMode {
		o.limit = 200
		if args.StartLine != nil {
			o.startLine = *args.StartLine
		}
		if args.Limit != nil {
			o.limit = *args.Limit
		}
		if o.limit < 1 || o.limit > 10000 {
			return o, errors.New("limit must be a line count between 1 and 10000")
		}
		if o.startLine < 1 || o.startLine > math.MaxInt64-int64(o.limit) {
			return o, errors.New("start_line must be a positive one-based line number with room for a page")
		}
	}
	return o, nil
}

func (s *aishwndSession) fileRead(ctx context.Context, req *mcp.CallToolRequest, args fileReadArgs) (*mcp.CallToolResult, fileReadResult, error) {
	o, err := validateFileRead(args)
	if err != nil {
		return nil, fileReadResult{}, err
	}
	var raw []byte
	var eof bool
	if o.lineMode {
		raw, o.offset, eof, err = s.readRemoteLinePage(args.Path, o.startLine, o.maxBytes)
	} else {
		raw, eof, err = s.readRemoteFile(args.Path, o.offset, o.maxBytes)
	}
	if err != nil {
		return nil, fileReadResult{}, err
	}
	if len(raw) > o.maxBytes {
		return nil, fileReadResult{}, errors.New("Windows peer returned more than the requested max_bytes")
	}
	if len(raw) == 0 && !eof {
		return nil, fileReadResult{}, errors.New("Windows peer returned an empty non-EOF page; refusing a non-progressing cursor")
	}
	out, err := renderFileRead(raw, eof, o, s.displayHost())
	return nil, out, err
}

func (s *aishwndSession) readRemoteLinePage(path string, startLine int64, maxBytes int) ([]byte, int64, bool, error) {
	req, err := json.Marshal(aishwinwire.FileReadData{Path: path, MaxBytes: maxBytes, StartLine: startLine})
	if err != nil {
		return nil, 0, false, err
	}
	res, err := s.roundTrip("file_read", req, 30*time.Second)
	if err != nil {
		return nil, 0, false, err
	}
	var reply aishwinwire.FileReadResultData
	if err := json.Unmarshal(res, &reply); err != nil {
		return nil, 0, false, fmt.Errorf("malformed file_read result from the Windows peer: %w", err)
	}
	if reply.Error != "" {
		return nil, 0, false, errors.New(reply.Error)
	}
	if reply.StartLine != startLine {
		return nil, 0, false, errors.New("Windows peer does not support line pagination; update aishwin.exe and aishwnd, or use byte offset/max_bytes")
	}
	if reply.SourceOffset < 0 || reply.SourceOffset > math.MaxInt64-maxFileReadBytes {
		return nil, 0, false, errors.New("Windows peer returned an invalid source_offset")
	}
	raw, err := base64.StdEncoding.DecodeString(reply.Content)
	if err != nil {
		return nil, 0, false, fmt.Errorf("malformed content from the Windows peer: %w", err)
	}
	return raw, reply.SourceOffset, reply.Eof, nil
}

func renderFileRead(raw []byte, eof bool, o fileReadOptions, host string) (fileReadResult, error) {
	completeLines := o.lineMode || o.numbered
	// Boundaries are source byte positions after complete lines, not positions
	// in their numbered or JSON-escaped representation.
	boundaries := []int{0}
	if completeLines {
		for i, b := range raw {
			if b == '\n' {
				boundaries = append(boundaries, i+1)
			}
		}
		if eof && len(raw) > 0 && raw[len(raw)-1] != '\n' {
			boundaries = append(boundaries, len(raw))
		}
	}
	n := len(raw)
	reason := ""
	if !eof {
		reason = "max_bytes"
	}
	if completeLines {
		count := len(boundaries) - 1
		if o.lineMode && count > o.limit {
			count, reason = o.limit, "limit"
		}
		n = boundaries[count]
		if n == 0 && len(raw) > 0 {
			return fileReadResult{}, oversizedReadLine(o.startLine, o.offset)
		}
		if !utf8.Valid(raw[:n]) {
			return fileReadResult{}, errors.New("line reads and line_numbers require UTF-8 text; use byte offset/max_bytes without line_numbers for binary data")
		}
	} else {
		n = textPageEnd(raw, n, eof)
	}
	makeResult := func(n int, why string) fileReadResult {
		data := raw[:n]
		out := fileReadResult{Eof: eof && n == len(raw), Via: "aishwin", Host: host, NextOffset: o.offset + int64(n), BytesRead: n}
		out.Truncated = !out.Eof
		if out.Truncated {
			out.TruncationReason = why
		}
		if completeLines {
			out.StartLine = o.startLine
			lines := bytes.Count(data, []byte{'\n'})
			if n > 0 && data[n-1] != '\n' {
				lines++
			}
			if lines > 0 {
				out.EndLine = o.startLine + int64(lines) - 1
			}
			if !out.Eof {
				out.NextLine = o.startLine + int64(lines)
			}
		}
		if out.Eof && o.offset == 0 && (!o.lineMode || o.startLine == 1) {
			out.Version, out.VersionKind = aishwinwire.SHA256Version(data), "sha256"
		}
		out.Encoding = "utf8"
		text := string(data)
		if !utf8.Valid(data) {
			text, out.Encoding = base64.StdEncoding.EncodeToString(data), "base64"
		}
		if o.numbered {
			text = numberReadLines(data, o.startLine)
			out.NumberedContent = &text
		} else {
			out.Content = &text
		}
		return out
	}
	out := makeResult(n, reason)
	if fileReadResultSize(out) <= fileReadResultBudget {
		return out, nil
	}
	// Search for the largest complete page that fits. Every trial includes
	// JSON escaping and the SDK's structured-content/text compatibility copy.
	lo, hi := 0, n
	if completeLines {
		hi = len(boundaries) - 1
		if o.lineMode && hi > o.limit {
			hi = o.limit
		}
	}
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		end := mid
		if completeLines {
			end = boundaries[mid]
		} else {
			end = textPageEnd(raw, mid, eof)
		}
		if fileReadResultSize(makeResult(end, "response_budget")) <= fileReadResultBudget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if lo == 0 && n > 0 {
		if completeLines {
			return fileReadResult{}, oversizedReadLine(o.startLine, o.offset)
		}
		return fileReadResult{}, errors.New("file_read metadata exceeds the result budget")
	}
	if completeLines {
		lo = boundaries[lo]
	} else {
		lo = textPageEnd(raw, lo, eof)
	}
	out = makeResult(lo, "response_budget")
	if fileReadResultSize(out) > fileReadResultBudget {
		return fileReadResult{}, errors.New("file_read metadata exceeds the result budget")
	}
	return out, nil
}

func oversizedReadLine(line, offset int64) error {
	return fmt.Errorf("line %d exceeds the page budget; read its raw bytes with offset=%d and max_bytes, without start_line, limit or line_numbers", line, offset)
}

// Keep byte pages UTF-8 when only the ceiling split the last rune. Tiny
// pages still advance using base64; their byte cursors always preserve data.
func textPageEnd(data []byte, n int, eof bool) int {
	if utf8.Valid(data[:n]) {
		return n
	}
	for back := 1; back < utf8.UTFMax && n-back > 0; back++ {
		end := n - back
		if !utf8.Valid(data[:end]) {
			continue
		}
		r, size := utf8.DecodeRune(data[end:])
		if ((r != utf8.RuneError || size > 1) && size > back) || (!eof && !utf8.FullRune(data[end:])) {
			return end
		}
	}
	return n
}

func fileReadResultSize(out fileReadResult) int {
	raw, _ := json.Marshal(out)
	result, _ := json.Marshal(&mcp.CallToolResult{StructuredContent: json.RawMessage(raw), Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}})
	return len(result)
}

func numberReadLines(data []byte, first int64) string {
	var b strings.Builder
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			fmt.Fprintf(&b, "%6d\t%s", first, data)
			break
		}
		fmt.Fprintf(&b, "%6d\t%s", first, data[:end+1])
		data = data[end+1:]
		first++
	}
	return b.String()
}
