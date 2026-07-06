package mcpserver

import "github.com/modelcontextprotocol/go-sdk/mcp"

func boolPtr(v bool) *bool { return &v }

func readOnlyTool(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    true,
		DestructiveHint: boolPtr(false),
		OpenWorldHint:   boolPtr(false),
	}
}

func mutatingTool(title string, destructive, idempotent bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    false,
		DestructiveHint: boolPtr(destructive),
		IdempotentHint:  idempotent,
		OpenWorldHint:   boolPtr(false),
	}
}

func commandTool(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    false,
		DestructiveHint: boolPtr(true),
		OpenWorldHint:   boolPtr(true),
	}
}
