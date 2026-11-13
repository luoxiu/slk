package slackclient

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/slack-go/slack"
)

// This file holds what `slk listen` (cmd/slk/agentcmd_listen.go) needs
// from the client beyond StartWebSocket, StopWebSocket and WsDone: an
// auth check it can bound and abandon, and the two seams its tests use
// to reach the WebSocket path from outside this package.

// CheckAuth asks Slack whether the session still works: one auth.test.
// Slack's refusal comes back as slack.SlackErrorResponse with its code
// ("invalid_auth", "token_revoked", ...); a transport failure as the
// transport's error.
//
// It is not Connect: Connect ignores ctx (slack-go's AuthTest takes
// none) and rewrites the client's identity and API client, so a caller
// can neither cancel it nor walk away from it while the client is in
// use. listen runs this after a connection attempt that never reached
// Slack's hello, to tell a dead token from a network that is down.
func (c *Client) CheckAuth(ctx context.Context) error {
	raw, err := c.postForm(ctx, "auth.test", nil)
	if err != nil {
		return err
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("auth.test: decoding response: %w", err)
	}
	if !resp.OK {
		return slack.SlackErrorResponse{Err: resp.Error}
	}
	return nil
}

// DispatchWebSocketEventForTest routes one raw WebSocket frame to
// handler exactly as a live socket's event owner does
// (dispatchWebSocketEvent). For tests outside this package that feed
// captured frames to their own EventHandler; live frames only ever
// arrive through StartWebSocket.
func DispatchWebSocketEventForTest(frame []byte, handler EventHandler) {
	dispatchWebSocketEvent(frame, handler)
}

// SetWebSocketBaseURLForTest points StartWebSocket at base (scheme and
// host, e.g. "ws://127.0.0.1:4321") instead of Slack, so a test outside
// this package can run a real Client against a local WebSocket server,
// as tests inside it do by setting wsBaseURL. Call it before
// StartWebSocket.
func (c *Client) SetWebSocketBaseURLForTest(base string) {
	c.wsBaseURL = base
}
