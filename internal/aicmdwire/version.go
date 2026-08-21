package aicmdwire

import (
	"crypto/sha256"
	"encoding/hex"
)

// SHA256Version renders data's content-addressed version token, in the same
// format as aish's own sha256Version (internal/mcpserver/tools_remote.go).
// Both cmd/aicmd (hashing the current on-disk file to check an if_match
// token before writing) and internal/aicmdd (hashing a fully-read file's
// content to hand the AI a token for a later write) need the identical
// format, so it lives here rather than being duplicated in both.
func SHA256Version(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
