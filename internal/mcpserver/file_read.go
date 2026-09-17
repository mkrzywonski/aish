package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/sshmux"
)

const (
	defaultFileRead      = 16 << 10
	maxFileRead          = 256 << 10
	fileReadResultBudget = 60 << 10 // leave 4 KiB of a 64 KiB envelope for proxy notices
	maxInBandRead        = 32 << 10 // base64 plus line wrapping must fit framing's 64 KiB capture
)

type fileReadArgs struct {
	SessionArg
	Path        string `json:"path" jsonschema:"absolute or ~-relative path on the current host"`
	MaxBytes    *int   `json:"max_bytes,omitempty" jsonschema:"source byte ceiling (default 16384, max 262144); response budget may reduce page size"`
	Offset      *int64 `json:"offset,omitempty" jsonschema:"zero-based BYTE offset; cannot combine with start_line or limit"`
	StartLine   *int64 `json:"start_line,omitempty" jsonschema:"one-based starting line; selects OOB text line mode"`
	Limit       *int64 `json:"limit,omitempty" jsonschema:"maximum lines (default 200, maximum 10000 in line mode); alone starts at line 1"`
	LineNumbers bool   `json:"line_numbers,omitempty" jsonschema:"return only numbered_content instead of raw content; use start_line for numbered pages"`
}

type fileReadResult struct {
	Content          *string `json:"content,omitempty"`
	NumberedContent  *string `json:"numbered_content,omitempty"`
	Encoding         string  `json:"encoding"`
	Eof              bool    `json:"eof"`
	BytesRead        int     `json:"bytes_read"`
	NextOffset       int64   `json:"next_offset"`
	StartLine        *int64  `json:"start_line,omitempty"`
	EndLine          *int64  `json:"end_line,omitempty"`
	NextLine         *int64  `json:"next_line,omitempty"`
	Truncated        bool    `json:"truncated"`
	TruncationReason string  `json:"truncation_reason,omitempty"`
	Version          string  `json:"version,omitempty"`
	VersionKind      string  `json:"version_kind,omitempty"`
	Via              string  `json:"via"`
	Host             string  `json:"host"`
	Warning          string  `json:"warning,omitempty"`
}

type fileReadOptions struct {
	offset, start, limit int64
	max                  int
	lines, numbered      bool
}

func (a fileReadArgs) options() (fileReadOptions, error) {
	o := fileReadOptions{start: 1, limit: 200, max: defaultFileRead, numbered: a.LineNumbers, lines: a.StartLine != nil || a.Limit != nil}
	if a.Path == "" {
		return o, errors.New("path must not be empty")
	}
	if a.Offset != nil {
		if o.lines {
			return o, errors.New("offset is in bytes; do not combine it with line parameters. For lines 241–400 use start_line: 241, limit: 160")
		}
		o.offset = *a.Offset
	}
	if a.StartLine != nil {
		o.start = *a.StartLine
	}
	if a.Limit != nil {
		o.limit = *a.Limit
	}
	if a.MaxBytes != nil {
		o.max = *a.MaxBytes
	}
	if o.max < 1 || o.max > maxFileRead {
		return o, fmt.Errorf("max_bytes must be between 1 and %d", maxFileRead)
	}
	if o.offset < 0 || o.offset > math.MaxInt64-int64(maxFileRead)-64 {
		return o, errors.New("offset must be a nonnegative byte offset with room for the requested page")
	}
	if o.start < 1 || o.limit < 1 || o.limit > 10000 || o.start > math.MaxInt64-o.limit {
		return o, errors.New("start_line must be positive, limit must be between 1 and 10000, and their sum must fit in a signed 64-bit integer")
	}
	if o.numbered && o.offset != 0 {
		return o, errors.New("line_numbers cannot number an arbitrary byte offset; use start_line and limit instead")
	}
	return o, nil
}

type fileReadSample struct {
	data   []byte
	offset int64
	eof    bool
}

func (c *Core) fileRead(ctx context.Context, req *mcp.CallToolRequest, args fileReadArgs) (*mcp.CallToolResult, fileReadResult, error) {
	o, err := args.options()
	if err != nil {
		return nil, fileReadResult{}, err
	}
	rt := c.route()
	if o.lines && rt.via == "in_band" {
		return nil, fileReadResult{}, errors.New("line reads require authorized OOB access; enable OOB or use byte offset/max_bytes for a visible read")
	}
	if err := c.requireTool(rt, "file_read"); err != nil {
		return nil, fileReadResult{}, err
	}
	warning, _ := c.guardTarget(rt, opRead)
	var sample fileReadSample
	switch rt.via {
	case "local":
		sample, err = readLocalPage(ctx, expandLocal(args.Path), o)
	case "controlmaster":
		caps, _ := c.Mux.CachedCapabilities(rt.ci)
		if o.lines && !caps.LineRead {
			return nil, fileReadResult{}, errors.New("line reads need working head, tail and wc on this host; use byte offset/max_bytes instead")
		}
		var script, marker string
		script, marker, err = remoteReadScript(args.Path, o)
		if err == nil {
			var out []byte
			out, err = c.channelOutput(rt.ci, script, 60*time.Second)
			if err == nil {
				sample, err = parseRemoteRead(out, marker, o)
			}
		}
	case "in_band":
		o.max = min(o.max, maxInBandRead)
		// Keep the existing visible byte-transfer path; line selection is OOB only.
		p := sshmux.Quote(args.Path)
		cmd := fmt.Sprintf("test -f %s && tail -c +%d %s | head -c %d | base64", p, o.offset+1, p, o.max+utf8.UTFMax)
		var resErr error
		res, runErr := c.Engine.RunSentinel(cmd, 30*time.Second)
		if runErr != nil {
			err = runErr
			break
		}
		if res.TimedOut || res.Truncated || res.ExitCode == nil || *res.ExitCode != 0 {
			err = errors.New("in-band file read failed or timed out; retry with a smaller max_bytes or authorized OOB access")
			break
		}
		sample.data, resErr = base64.StdEncoding.DecodeString(strings.Join(strings.Fields(res.Output), ""))
		if resErr != nil {
			err = fmt.Errorf("decoding in-band file read: %w", resErr)
			break
		}
		sample.offset, sample.eof = o.offset, len(sample.data) < o.max+utf8.UTFMax
	default:
		err = errors.New("file_read has no usable route")
	}
	if err != nil {
		return nil, fileReadResult{}, err
	}
	res, err := renderFilePage(sample, o, resultVia(rt), rt.host, warning)
	return nil, res, err
}

func readLocalPage(ctx context.Context, path string, o fileReadOptions) (fileReadSample, error) {
	s := fileReadSample{offset: o.offset}
	f, err := os.Open(path)
	if err != nil {
		return s, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return s, err
	}
	if !info.Mode().IsRegular() {
		return s, errors.New("file_read requires a regular file")
	}
	if _, err := f.Seek(o.offset, io.SeekStart); err != nil {
		return s, err
	}
	r := bufio.NewReader(f)
	if o.lines {
		// ReadSlice bounds memory even for a huge line in the skipped prefix.
		for line := int64(1); line < o.start; {
			if err := ctx.Err(); err != nil {
				return s, err
			}
			part, err := r.ReadSlice('\n')
			s.offset += int64(len(part))
			if err == io.EOF {
				s.eof = true
				return s, nil
			}
			if err != nil && err != bufio.ErrBufferFull {
				return s, err
			}
			if err == nil {
				line++
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return s, err
	}
	s.data, err = io.ReadAll(io.LimitReader(r, int64(o.max+utf8.UTFMax)))
	s.eof = len(s.data) < o.max+utf8.UTFMax
	return s, err
}

// The OOB script sends a tiny header and bounded base64 payload. The prefix
// status uses fd 3 (outside the wc pipe); the tail status follows the actual
// bytes before base64 encoding. A random marker cannot be forged by file text.
// A missing tail marker means the byte cap cut off the stream, never known EOF.
func remoteReadScript(path string, o fileReadOptions) (string, string, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", "", err
	}
	marker := "\n@AISHREAD" + hex.EncodeToString(nonce) + "@"
	p := sshmux.Quote(path)
	var b strings.Builder
	fmt.Fprintf(&b, "(\nexport LC_ALL=C\n[ -f %s ] && [ -r %s ] || { echo 'file_read: missing, unreadable, or non-regular file' >&2; exit 1; }\n", p, p)
	if o.lines && o.start > 1 {
		fmt.Fprintf(&b, "_off=$( { head -n %d %s; printf 'prefix_status=%%s\\n' \"$?\" >&3; } | wc -c) || exit 1\n", o.start-1, p)
	} else {
		fmt.Fprintf(&b, "_off=%d\n", o.offset)
	}
	b.WriteString("printf 'offset=%s\\nDATA\\n' \"$_off\"\n")
	fmt.Fprintf(&b, "{ tail -c +$((_off + 1)) %s; printf %s \"$?\"; } | head -c %d | base64\n) 3>&1", p, sshmux.Quote(marker+"%s\n"), o.max+utf8.UTFMax+len(marker)+4)
	return b.String(), marker, nil
}

func parseRemoteRead(out []byte, marker string, o fileReadOptions) (fileReadSample, error) {
	var s fileReadSample
	header, body, ok := strings.Cut(string(out), "\nDATA\n")
	if !ok {
		return s, errors.New("invalid remote file_read header")
	}
	foundOffset, prefixOK := false, !o.lines || o.start == 1
	for _, line := range strings.Split(header, "\n") {
		key, val, _ := strings.Cut(line, "=")
		switch key {
		case "prefix_status":
			if val != "0" {
				return s, fmt.Errorf("remote prefix read failed (exit %s)", val)
			}
			prefixOK = true
		case "offset":
			var err error
			s.offset, err = strconv.ParseInt(strings.TrimSpace(val), 10, 64)
			if err != nil || s.offset < 0 || s.offset > math.MaxInt64-int64(maxFileRead)-64 {
				return s, errors.New("invalid remote file_read offset")
			}
			foundOffset = true
		default:
			return s, fmt.Errorf("remote file read failed: %.200s", line)
		}
	}
	if !foundOffset || !prefixOK {
		return s, errors.New("remote file read omitted its offset or prefix status")
	}
	data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(body), ""))
	if err != nil {
		return s, fmt.Errorf("decoding remote file read: %w", err)
	}
	if i := bytes.LastIndex(data, []byte(marker)); i >= 0 && bytes.HasSuffix(data, []byte{'\n'}) {
		rc := strings.TrimSuffix(string(data[i+len(marker):]), "\n")
		if rc != "0" {
			return s, fmt.Errorf("remote file read failed (exit %s)", rc)
		}
		s.data, s.eof = data[:i], true
	} else {
		// The cap can land inside the status trailer, including just after
		// the marker. Only a complete trailer proves EOF or a producer error.
		if len(data) < o.max+utf8.UTFMax {
			return s, errors.New("remote file read ended without its completion marker")
		}
		s.data = data[:o.max+utf8.UTFMax]
	}
	return s, nil
}

func renderFilePage(s fileReadSample, o fileReadOptions, via, host, warning string) (fileReadResult, error) {
	fullLines := o.lines || o.numbered
	n := min(o.max, len(s.data))
	reason := "max_bytes"
	ends := []int{0}
	if fullLines {
		for i, ch := range s.data[:n] {
			if ch == '\n' {
				ends = append(ends, i+1)
			}
		}
		if s.eof && n == len(s.data) && n > 0 && s.data[n-1] != '\n' {
			ends = append(ends, n)
		}
		if o.lines && int64(len(ends)-1) > o.limit {
			ends = ends[:int(o.limit)+1]
			reason = "limit"
		}
		n = ends[len(ends)-1]
		if n == 0 && len(s.data) != 0 {
			return fileReadResult{}, longLineError(o.start, s.offset)
		}
		if !utf8.Valid(s.data[:n]) {
			return fileReadResult{}, errors.New("line reads/numbering require UTF-8 text; use byte offset/max_bytes with line_numbers false")
		}
	} else {
		n = utf8PageEnd(s.data, n)
	}
	build := func(size int, why string) fileReadResult {
		data := s.data[:size]
		res := fileReadResult{Encoding: "utf8", Eof: s.eof && size == len(s.data), BytesRead: size, NextOffset: s.offset + int64(size), Via: via, Host: host, Warning: warning}
		res.Truncated = !res.Eof
		if res.Truncated {
			res.TruncationReason = why
		}
		if fullLines {
			start := o.start
			res.StartLine = &start
			count := int64(bytes.Count(data, []byte{'\n'}))
			if size > 0 && data[size-1] != '\n' {
				count++
			}
			if count > 0 {
				end := start + count - 1
				res.EndLine = &end
			}
			if !res.Eof {
				next := start + count
				res.NextLine = &next
			}
		}
		if o.numbered {
			text := numberLinesFrom(data, o.start)
			res.NumberedContent = &text
		} else {
			text := string(data)
			if !utf8.Valid(data) {
				text, res.Encoding = base64.StdEncoding.EncodeToString(data), "base64"
			}
			res.Content = &text
		}
		if s.offset == 0 && o.start == 1 && res.Eof {
			res.Version, res.VersionKind = sha256Version(data), "sha256"
		}
		return res
	}
	res := build(n, reason)
	fits := func(r fileReadResult) bool { return fileReadWireSize(r) <= fileReadResultBudget }
	if fits(res) {
		return res, nil
	}
	// Find the largest prefix that fits after JSON escaping, numbering/base64,
	// and the MCP SDK's compatibility text copy. No content is silently lost.
	lo, hi := 0, n
	if fullLines {
		hi = len(ends) - 1
	}
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		size := mid
		if fullLines {
			size = ends[mid]
		} else {
			size = utf8PageEnd(s.data, mid)
		}
		if fits(build(size, "response_budget")) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	n = lo
	if fullLines {
		n = ends[lo]
	} else {
		n = utf8PageEnd(s.data, n)
	}
	if n == 0 && len(s.data) > 0 {
		if fullLines {
			return fileReadResult{}, longLineError(o.start, s.offset)
		}
		return fileReadResult{}, errors.New("file read metadata exceeds response budget")
	}
	res = build(n, "response_budget")
	if !fits(res) {
		return fileReadResult{}, errors.New("file read metadata exceeds response budget")
	}
	return res, nil
}

func longLineError(line, offset int64) error {
	return fmt.Errorf("line %d exceeds the read/response budget; use byte mode with offset: %d, max_bytes: %d, line_numbers: false", line, offset, defaultFileRead)
}

// Avoid classifying a valid UTF-8 page as binary solely because its ceiling
// splits the last rune. Very small byte pages still make progress via base64.
func utf8PageEnd(data []byte, n int) int {
	if utf8.Valid(data[:n]) {
		return n
	}
	for back := 1; back < utf8.UTFMax && n-back > 0; back++ {
		end := n - back
		if !utf8.Valid(data[:end]) {
			continue
		}
		r, size := utf8.DecodeRune(data[end:])
		if r != utf8.RuneError || size > 1 {
			if size > back {
				return end
			}
		}
	}
	return n
}

func fileReadWireSize(res fileReadResult) int {
	raw, err := json.Marshal(res)
	if err != nil {
		return math.MaxInt
	}
	wire, err := json.Marshal(&mcp.CallToolResult{StructuredContent: json.RawMessage(raw), Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}})
	if err != nil {
		return math.MaxInt
	}
	return len(wire)
}

func numberLines(data []byte) string { return numberLinesFrom(data, 1) }

func numberLinesFrom(data []byte, first int64) string {
	var b strings.Builder
	for len(data) > 0 {
		n := bytes.IndexByte(data, '\n')
		if n < 0 {
			fmt.Fprintf(&b, "%6d\t%s", first, data)
			break
		}
		fmt.Fprintf(&b, "%6d\t%s\n", first, data[:n])
		data = data[n+1:]
		first++
	}
	return b.String()
}
