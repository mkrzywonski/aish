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
// description is a short, human-readable self-declaration of this client shown
// in the target's approval prompt (paired there with the kernel-verified peer).
func (i *Identity) Authorize(ctx context.Context, cs *mcp.ClientSession, targetSession, description string) error {
	// i.mu guards only the i.grants map. The RPC round-trips run WITHOUT it:
	// request_access can block on the server's approval prompt for up to ~120s,
	// and a single Identity is shared across targets (the proxy pool, cross-
	// session forwarding), so holding the lock across the handshake would
	// serialize authorizations to unrelated sessions. i.private/i.public are
	// immutable after New(), so signing needs no lock.
	i.mu.Lock()
	grantID := i.grants[targetSession]
	i.mu.Unlock()

	if grantID != "" {
		if err := i.authenticate(ctx, cs, grantID); err == nil {
			return nil
		}
		// Stale or rejected grant: forget it (unless another goroutine already
		// replaced it) and fall through to request a fresh one.
		i.mu.Lock()
		if i.grants[targetSession] == grantID {
			delete(i.grants, targetSession)
		}
		i.mu.Unlock()
	}

	var result authproto.RequestAccessResult
	if err := call(ctx, cs, authproto.RequestAccessTool, authproto.RequestAccessArgs{
		PublicKey:         i.public,
		ClientDescription: description,
	}, &result); err != nil {
		return fmt.Errorf("requesting session access: %w", err)
	}
	if result.GrantID == "" {
		return errors.New("requesting session access: server returned an empty grant")
	}
	i.mu.Lock()
	i.grants[targetSession] = result.GrantID
	i.mu.Unlock()
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
