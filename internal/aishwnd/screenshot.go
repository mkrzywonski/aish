package aishwnd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/aishwinwire"
)

// captureScreenTimeout bounds the wire round trip, not just the mechanical
// capture: full-screen mode may block on a one-time human consent prompt
// on the Windows console (aishwin's fullScreenCaptureAllowed), so this
// mirrors auth.go's own defaultApprovalTimeout for the same reason, plus a
// buffer for the capture itself.
const captureScreenTimeout = 130 * time.Second

// captureScreenArgs mirrors cmd/aishwin's aishwinwire.CaptureScreenData.
type captureScreenArgs struct {
	Mode string `json:"mode,omitempty" jsonschema:"'window' (default) captures just the aishwin window; 'full' or 'screen' captures the whole Windows desktop including the taskbar, and requires a one-time human consent prompt on the Windows console the first time it's used each session"`
}

func registerScreenshotTools(s *mcp.Server, sess *aishwndSession) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "capture_screen",
		Annotations: readOnlyTool("Capture a screenshot of the Windows session"),
		Description: "Capture a screenshot of the aishwin control window (default) -- that is aishwin's own " +
			"application window and its activity log, NOT some other application running on the desktop -- " +
			"or, with mode=\"full\", the whole " +
			"Windows desktop including the taskbar. Returned directly as an image. Full-screen capture " +
			"requires the human to approve a one-time consent prompt on the Windows console the first time " +
			"it's used each session; declining fails this call with an error rather than blocking silently.",
	}, sess.captureScreen)
}

func (s *aishwndSession) captureScreen(ctx context.Context, req *mcp.CallToolRequest, args captureScreenArgs) (*mcp.CallToolResult, any, error) {
	data, err := json.Marshal(aishwinwire.CaptureScreenData{Mode: args.Mode})
	if err != nil {
		return nil, nil, err
	}

	raw, err := s.roundTrip("capture_screen", data, captureScreenTimeout)
	if err != nil {
		return nil, nil, err
	}
	var res aishwinwire.CaptureScreenResultData
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, nil, fmt.Errorf("malformed capture_screen result from the Windows peer: %w", err)
	}
	if res.Error != "" {
		return nil, nil, errors.New(res.Error)
	}

	png, err := base64.StdEncoding.DecodeString(res.PNG)
	if err != nil {
		return nil, nil, fmt.Errorf("malformed PNG data from the Windows peer: %w", err)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.ImageContent{Data: png, MIMEType: "image/png"}},
	}, nil, nil
}
