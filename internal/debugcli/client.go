// Package debugcli implements `aish client`, a one-shot MCP client for
// exercising a running session's tools without a full AI harness.
package debugcli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/clientauth"
	"ai-ssh/internal/procinfo"
	"ai-ssh/internal/proxy"
)

const usage = `usage: aish client [--session <id|name>] [--identity <name>] <tool> [json-args]
       aish client [--session <id|name>] --list
`

func Main(version string, args []string) int {
	var sessionID string
	var identityName string
	var list bool
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, usage)
				return 2
			}
			sessionID = args[i+1]
			i++
		case "--identity":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, usage)
				return 2
			}
			identityName = args[i+1]
			i++
		case "--list":
			list = true
		default:
			rest = append(rest, args[i])
		}
	}
	if !list && len(rest) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}

	sock, err := proxy.Discover(sessionID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aish client:", err)
		return 1
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aish client:", err)
		return 1
	}
	defer conn.Close()

	ctx := context.Background()
	description := clientDescription(identityName)
	client := mcp.NewClient(&mcp.Implementation{Name: "aish-client", Version: version}, nil)
	cs, err := client.Connect(ctx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aish client: connect:", err)
		return 1
	}
	defer cs.Close()

	if list {
		res, err := cs.ListTools(ctx, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "aish client:", err)
			return 1
		}
		for _, t := range res.Tools {
			fmt.Printf("%-16s %s\n", t.Name, t.Description)
		}
		return 0
	}

	identity, err := clientauth.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, "aish client: generating client identity:", err)
		return 1
	}
	resolvedID := filepath.Base(filepath.Dir(sock))
	if err := identity.Authorize(ctx, cs, resolvedID, description); err != nil {
		fmt.Fprintln(os.Stderr, "aish client: authorize:", err)
		return 1
	}

	tool := rest[0]
	toolArgs := map[string]any{}
	if len(rest) > 1 {
		if err := json.Unmarshal([]byte(rest[1]), &toolArgs); err != nil {
			fmt.Fprintf(os.Stderr, "aish client: bad json args: %v\n", err)
			return 2
		}
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: toolArgs})
	if err != nil {
		fmt.Fprintln(os.Stderr, "aish client:", err)
		return 1
	}
	if res.StructuredContent != nil {
		out, _ := json.MarshalIndent(res.StructuredContent, "", "  ")
		fmt.Println(string(out))
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok && res.StructuredContent == nil {
			fmt.Println(tc.Text)
		}
	}
	if res.IsError {
		return 1
	}
	return 0
}

// clientDescription builds the self-declared identity the approval prompt
// shows. An explicit --identity wins; otherwise it names the debug CLI and the
// process that launched it (e.g. a shell or an AI harness), so a human sees
// what is actually driving the connection instead of a bare "aish-client".
func clientDescription(identityName string) string {
	if identityName != "" {
		return identityName
	}
	if parent := procinfo.Name(os.Getppid()); parent != "" {
		return "aish debug CLI (launched by " + parent + ")"
	}
	return "aish debug CLI"
}
