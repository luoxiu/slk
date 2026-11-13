package slackclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/slack-go/slack"
)

// FileInfo returns one file's metadata (files.info). `slk message send
// --attach` asks for it after an upload, for the permalink: UploadFile
// returns only what slack-go keeps of files.completeUploadExternal's
// answer, the file's id and title. A refusal fails with Slack's error
// code in the text (file_not_found, ...); a rate limit fails with a
// *slack.RateLimitedError, so WaitOutRateLimit can wait it out.
func (c *Client) FileInfo(ctx context.Context, fileID string) (*slack.File, error) {
	raw, err := c.postForm(ctx, "files.info", url.Values{"file": {fileID}})
	if err != nil {
		var rl *rateLimitError
		if errors.As(err, &rl) {
			return nil, fmt.Errorf("files.info: %w", &slack.RateLimitedError{RetryAfter: rl.retryAfter})
		}
		return nil, fmt.Errorf("files.info: %w", err)
	}
	var resp struct {
		OK    bool       `json:"ok"`
		Error string     `json:"error"`
		File  slack.File `json:"file"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decoding files.info response: %w (body=%s)", err, truncateForLog(raw))
	}
	if !resp.OK {
		return nil, fmt.Errorf("files.info: %s", resp.Error)
	}
	return &resp.File, nil
}
