// Package authproto defines the private authentication protocol shared by
// aish session servers and the clients bundled with aish.
package authproto

import "strings"

const (
	RequestAccessTool = "request_access"
	ChallengeTool     = "auth_challenge"
	AuthenticateTool  = "authenticate"
)

var InternalTools = map[string]bool{
	RequestAccessTool: true,
	ChallengeTool:     true,
	AuthenticateTool:  true,
	"authorize":       true, // hide stale cached schemas from the removed bearer-token protocol
}

type RequestAccessArgs struct {
	PublicKey string `json:"public_key" jsonschema:"base64url-encoded Ed25519 public key"`
	// ClientDescription is a short, human-readable self-declaration of who is
	// asking (e.g. "Gemini (Antigravity)"), shown in the approval prompt. It is
	// self-reported and NOT a security guarantee; the server pairs it with the
	// kernel-verified peer process. Bundled clients always populate it.
	ClientDescription string `json:"client_description,omitempty" jsonschema:"short human-readable identity of the requesting client, shown to the user"`
}

type RequestAccessResult struct {
	GrantID string `json:"grant_id"`
}

type ChallengeArgs struct {
	GrantID string `json:"grant_id"`
}

type ChallengeResult struct {
	ChallengeID string `json:"challenge_id"`
	Nonce       string `json:"nonce"`
	SessionID   string `json:"session_id"`
}

type AuthenticateArgs struct {
	GrantID     string `json:"grant_id"`
	ChallengeID string `json:"challenge_id"`
	Signature   string `json:"signature" jsonschema:"base64url-encoded Ed25519 signature"`
}

type AuthenticateResult struct {
	Authorized bool `json:"authorized"`
}

// SigningPayload is deliberately versioned and NUL-delimited so signatures
// cannot be replayed across protocol versions, sessions, grants, or challenges.
func SigningPayload(sessionID, grantID, challengeID, nonce string) []byte {
	return []byte(strings.Join([]string{
		"aish-auth-v1", sessionID, grantID, challengeID, nonce,
	}, "\x00"))
}
