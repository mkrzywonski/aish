package aishwnd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/aishwinwire"
)

// read_console returns the console feed: the scrolling account of operations
// that the human watching this Windows machine sees.
//
// It was briefly called read_output, on the reasoning that it answers the same
// question as the shared terminal's tool — show me what the human has been
// looking at. Driving it against a live session disproved that: a terminal's
// read_output returns a BYTE STREAM addressed by byte offsets, while this
// returns discrete timestamped entries addressed by sequence number. The
// arguments could not match, so a caller working from the advertised schema
// had its call rejected by the implementation. One name promising two
// contracts is the exact trap this work exists to remove, so the names differ
// where the contracts do.
//
// It is also not an oob_log counterpart: there is no out-of-band route here,
// so there is no invisible work to recover.
//
// Before this existed, an agent asked to find out what had already happened on
// this session had no tool that would say, and reconstructed it from files
// left on disk.

type readOutputArgs struct {
	Cursor int64 `json:"cursor,omitempty" jsonschema:"return entries at or after this cursor; omit to start from the oldest retained entry"`
	Max    int   `json:"max,omitempty" jsonschema:"maximum entries to return (default and cap 500)"`
}

type readOutputEntry struct {
	Seq  int64  `json:"seq"`
	At   string `json:"at"`
	Text string `json:"text"`
	// Kind groups an entry the way the window colours it: "command" for a
	// command announcement, "file" for a file operation, "output" for
	// everything else, which is most of the feed.
	Kind string `json:"kind,omitempty"`
}

type readOutputResult struct {
	Entries    []readOutputEntry `json:"entries"`
	NextCursor int64             `json:"next_cursor"`
	Dropped    int64             `json:"dropped,omitempty"`
	Via        string            `json:"via"`
	Host       string            `json:"host"`
}

func registerConsoleTools(s *mcp.Server, sess *aishwndSession) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "read_console",
		Annotations: readOnlyTool("Read Windows console feed"),
		Description: "Read what the human watching this Windows machine has seen: the console feed of " +
			"operations scrolling in aishwin's window, oldest retained first. Pass the next_cursor from " +
			"the previous call to get only what is new; omit cursor to start from the oldest entry still " +
			"retained. `dropped` counts entries evicted before you read them. Read this when another " +
			"client may share this session, or when the user asks what has been happening -- it is the " +
			"only tool that reports activity from before you connected. Note this is the visible feed, " +
			"not an out-of-band log: this backend has no invisible route, so there is no hidden " +
			"activity for a log to recover. Each entry's `kind` groups it the way the window colours it: " +
			"\"command\" for a command announcement, \"file\" for a file operation, \"output\" for everything " +
			"else. Named read_console, not read_output, because it returns discrete " +
			"timestamped entries addressed by sequence number rather than a terminal's byte stream: the " +
			"shared-terminal read_output is a different contract, not this tool on another host.",
	}, sess.readOutput)
}

func (s *aishwndSession) readOutput(ctx context.Context, req *mcp.CallToolRequest, args readOutputArgs) (*mcp.CallToolResult, readOutputResult, error) {
	data, err := json.Marshal(aishwinwire.ConsoleReadData{Cursor: args.Cursor, Max: args.Max})
	if err != nil {
		return nil, readOutputResult{}, err
	}
	raw, err := s.roundTrip("console_read", data, 20*time.Second)
	if err != nil {
		return nil, readOutputResult{}, err
	}
	var wireRes aishwinwire.ConsoleReadResultData
	if err := json.Unmarshal(raw, &wireRes); err != nil {
		return nil, readOutputResult{}, fmt.Errorf("malformed console_read result from the Windows peer: %w", err)
	}
	if wireRes.Error != "" {
		return nil, readOutputResult{}, errors.New(wireRes.Error)
	}
	out := readOutputResult{
		NextCursor: wireRes.NextCursor,
		Dropped:    wireRes.Dropped,
		Via:        "aishwin",
		Host:       s.displayHost(),
	}
	for _, e := range wireRes.Entries {
		out.Entries = append(out.Entries, readOutputEntry{Seq: e.Seq, At: e.At, Text: e.Text, Kind: e.Kind})
	}
	return nil, out, nil
}
