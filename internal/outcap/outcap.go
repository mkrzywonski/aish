// Package outcap holds the single bound on how much command output any tool
// returns inline.
//
// The number lives here because three packages need the same answer and had
// drifted to two different ones: framing (run_command, via the terminal),
// mcpserver (exec, out-of-band) and the aishwin backend each capped
// independently. A cap that differs by path means output is spilled to a file
// at one threshold and trimmed at another, and a caller cannot reason about
// what it will get back.
package outcap

// MaxInline bounds output carried inline in a tool result.
//
// It is deliberately well under what an MCP client accepts in a single result.
// An earlier 64 KiB was chosen as "generous enough for ordinary output", but a
// capped 64 KiB result still exceeded the client's own limit, so it was written
// to disk and had to be read back in pieces — a cap landing just above the
// client's ceiling turns a large answer into a detour instead of an answer.
//
// Nothing is lost to the cap. run_command's output stays in the terminal
// scrollback and is re-readable by cursor with read_output; exec has no
// scrollback, so its full output is written to a file the result names.
const MaxInline = 16 << 10
