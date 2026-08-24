package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/sshmux"
)

// Creating a directory had no primitive, so the only way to make one was exec.
// Two evaluation agents hit that independently on their very first step, and
// both noted the same thing: making a scratch folder is plumbing, yet exec is
// the tool documented as running commands the human is meant to watch. On a
// Windows session that is worse than untidy — everything there is mirrored to
// a console, so preparing a working directory unavoidably appeared as activity
// a person was invited to interpret.
//
// mkdir -p semantics: creating a directory that already exists succeeds, which
// makes the call idempotent and safe to repeat.

type directoryCreateArgs struct {
	SessionArg
	Path string `json:"path" jsonschema:"absolute directory path to create on the current host; parents are created as needed"`
}

type directoryCreateResult struct {
	Path    string `json:"path"`
	Created bool   `json:"created"` // false when it already existed
	Via     string `json:"via"`
	Host    string `json:"host"`
	Warning string `json:"warning,omitempty"`
}

func (c *Core) directoryCreate(ctx context.Context, req *mcp.CallToolRequest, args directoryCreateArgs) (*mcp.CallToolResult, directoryCreateResult, error) {
	if err := validateAbsolutePathShape(args.Path); err != nil {
		return nil, directoryCreateResult{}, err
	}
	rt, err := c.fileFallbackRoute(ctx, "directory_create")
	if err != nil {
		return nil, directoryCreateResult{}, err
	}
	if rt.via == "in_band" {
		return nil, directoryCreateResult{}, oobPrimitiveError("directory_create", rt.host)
	}
	if rt.via == "sftp" {
		// The retained SFTP client exists only after a conclusive shell
		// failure, and this primitive is shell-backed. Say which route is
		// missing rather than failing as though the path were at fault.
		return nil, directoryCreateResult{}, errors.New("directory_create needs the shell channel, which is unavailable on this host; the retained SFTP route does not serve it")
	}
	if err := validateAbsolutePath(args.Path); err != nil {
		return nil, directoryCreateResult{}, err
	}
	warning, err := c.guardTarget(rt, opMutate)
	if err != nil {
		return nil, directoryCreateResult{}, err
	}

	res := directoryCreateResult{Path: args.Path, Via: resultVia(rt), Host: rt.host, Warning: warning}
	existed, err := c.directoryExists(ctx, rt, args.Path)
	if err != nil {
		return nil, directoryCreateResult{}, err
	}
	if rt.via == "local" {
		if err := os.MkdirAll(args.Path, 0o755); err != nil {
			return nil, directoryCreateResult{}, err
		}
		res.Created = !existed
		return nil, res, nil
	}
	script := "mkdir -p -- " + sshmux.Quote(args.Path)
	out, err := c.Mux.ChannelRun(rt.ci, "{ "+script+"\n} </dev/null 2>&1", 30*time.Second)
	if err != nil {
		return nil, directoryCreateResult{}, err
	}
	if out.Exit != 0 {
		return nil, directoryCreateResult{}, fmt.Errorf("mkdir failed on %s: %s", rt.host, trimOutput(string(out.Output)))
	}
	res.Created = !existed
	return nil, res, nil
}

// directoryExists reports whether the path is already a directory, so the
// result can distinguish "made it" from "it was already there" without making
// the call itself fail on the second run.
func (c *Core) directoryExists(ctx context.Context, rt route, path string) (bool, error) {
	if rt.via == "local" {
		info, err := os.Stat(path)
		if err != nil {
			return false, nil // absent, or unreadable: let MkdirAll report it
		}
		if !info.IsDir() {
			return false, fmt.Errorf("%s exists and is not a directory", path)
		}
		return true, nil
	}
	out, err := c.Mux.ChannelRun(rt.ci, "{ test -d "+sshmux.Quote(path)+"\n} </dev/null 2>&1", 20*time.Second)
	if err != nil {
		return false, nil // treat an unknown prior state as "did not exist"
	}
	return out.Exit == 0, nil
}

// trimOutput keeps a failure message short enough to read.
func trimOutput(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
