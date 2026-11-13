package slackclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/slack-go/slack"
)

// This file is the request plumbing the headless agent commands'
// hand-rolled endpoints share (drafts.*, saved.*): a form-encoded call
// and a multipart one, both answering errors the way the commands read
// them. SearchMessagesNewestFirst and AgentUnreadCounts predate it and
// still map their errors inline.

// agentPost calls a form-encoded web-client endpoint and decodes an ok
// response into out (nil to skip). Errors keep what the commands match
// on: a 429 is a *slack.RateLimitedError (WaitOutRateLimit waits it
// out), and a refusal is a slack.SlackErrorResponse whose code is in
// the text ("drafts.update: draft_not_found"), so an auth code maps to
// exit 2.
func (c *Client) agentPost(ctx context.Context, method string, form url.Values, out any) error {
	raw, err := c.postForm(ctx, method, form)
	return decodeAgentResponse(method, raw, err, out)
}

// formField is one multipart field, kept in order.
type formField struct{ name, value string }

// agentPostMultipart is agentPost sent as multipart/form-data, for the
// endpoints that ignore some fields when they arrive form-encoded.
func (c *Client) agentPostMultipart(ctx context.Context, method string, fields []formField, out any) error {
	raw, err := c.postMultipart(ctx, method, fields)
	return decodeAgentResponse(method, raw, err, out)
}

func decodeAgentResponse(method string, raw []byte, err error, out any) error {
	if err != nil {
		var rl *rateLimitError
		if errors.As(err, &rl) {
			return fmt.Errorf("%s: %w", method, &slack.RateLimitedError{RetryAfter: rl.retryAfter})
		}
		return err
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("decoding %s response: %w (body=%s)", method, err, truncateForLog(raw))
	}
	if !resp.OK {
		code := resp.Error
		if code == "" {
			code = "unknown_error"
		}
		return fmt.Errorf("%s: %w", method, slack.SlackErrorResponse{Err: code})
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding %s response: %w (body=%s)", method, err, truncateForLog(raw))
	}
	return nil
}

// postMultipart is postForm with a multipart/form-data body: the token
// first, as the web client and agent-slack put it, then fields in the
// order given. It goes through the same browser transport (cookie,
// headers, query-string envelope); the transport adds its body fields
// (_x_reason, _x_mode, _x_sonic, _x_app_name) to form-encoded bodies
// only, so this body has none. Its response handling is postForm's,
// copied: client.go is upstream's and stays untouched.
func (c *Client) postMultipart(ctx context.Context, method string, fields []formField) ([]byte, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, f := range append([]formField{{"token", c.token}}, fields...) {
		if err := w.WriteField(f.name, f.value); err != nil {
			return nil, fmt.Errorf("building %s request: %w", method, err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("building %s request: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.apiBaseURL+method, bytes.NewReader(body.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("creating %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.apiHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", method, err)
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &rateLimitError{
			method:     method,
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), 5*time.Second),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("calling %s: HTTP %s: %s", method, resp.Status, truncateForLog(raw))
	}
	if readErr != nil {
		return nil, fmt.Errorf("reading %s response: %w", method, readErr)
	}
	return raw, nil
}

// flexInt is a JSON number field of these undocumented endpoints
// (dates, counts) that decodes whether Slack sends it as an integer, a
// float, a numeric string or null, so one odd value does not fail a
// whole list. Anything else is 0.
type flexInt int64

func (n *flexInt) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case float64:
		*n = flexInt(x)
	case string:
		if f, err := strconv.ParseFloat(x, 64); err == nil {
			*n = flexInt(f)
		}
	}
	return nil
}
