// Package clientauth implements the proof-of-possession side of aish's
// private session authorization protocol.
package clientauth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/authproto"
)

// Identity is one logical aish client. Its private key and grants are kept in
// memory only; restarting the client creates a new identity that must be
// approved again.
type Identity struct {
	mu      sync.Mutex
	private ed25519.PrivateKey
	public  string
	grants  map[string]string // target session id -> grant id
}

func New() (*Identity, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Identity{
		private: private,
		public:  base64.RawURLEncoding.EncodeToString(public),
		grants:  map[string]string{},
	}, nil
}

// Authorize authorizes cs for targetSession. It proves possession of a
// cached grant when possible and otherwise requests interactive approval.
func (i *Identity) Authorize(ctx context.Context, cs *mcp.ClientSession, targetSession string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if grantID := i.grants[targetSession]; grantID != "" {
		if err := i.authenticate(ctx, cs, grantID); err == nil {
			return nil
		}
		delete(i.grants, targetSession)
	}
	var result authproto.RequestAccessResult
	if err := call(ctx, cs, authproto.RequestAccessTool, authproto.RequestAccessArgs{PublicKey: i.public}, &result); err != nil {
		return fmt.Errorf("requesting session access: %w", err)
	}
	if result.GrantID == "" {
		return errors.New("requesting session access: server returned an empty grant")
	}
	i.grants[targetSession] = result.GrantID
	return nil
}

func (i *Identity) authenticate(ctx context.Context, cs *mcp.ClientSession, grantID string) error {
	var challenge authproto.ChallengeResult
	if err := call(ctx, cs, authproto.ChallengeTool, authproto.ChallengeArgs{GrantID: grantID}, &challenge); err != nil {
		return err
	}
	payload := authproto.SigningPayload(challenge.SessionID, grantID, challenge.ChallengeID, challenge.Nonce)
	signature := base64.RawURLEncoding.EncodeToString(ed25519.Sign(i.private, payload))
	var result authproto.AuthenticateResult
	if err := call(ctx, cs, authproto.AuthenticateTool, authproto.AuthenticateArgs{
		GrantID: grantID, ChallengeID: challenge.ChallengeID, Signature: signature,
	}, &result); err != nil {
		return err
	}
	if !result.Authorized {
		return errors.New("server did not authorize the connection")
	}
	return nil
}

func call(ctx context.Context, cs *mcp.ClientSession, name string, args, out any) error {
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err
	}
	if res.IsError {
		for _, content := range res.Content {
			if text, ok := content.(*mcp.TextContent); ok {
				return errors.New(text.Text)
			}
		}
		return errors.New("authorization tool failed")
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("decoding %s result: %w", name, err)
	}
	return nil
}
