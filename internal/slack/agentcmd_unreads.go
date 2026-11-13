package slackclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/slack-go/slack"
)

// This file holds the Web API reads `slk unreads` needs beyond
// agentcmd_read.go: client.counts with the fields GetUnreadCounts
// drops (unread_count, unread_count_display, the conversation kind,
// Slack's error code), and a history page that says whether Slack has
// more. The command mirrors agent-slack's unreads, which reads both.

// AgentUnreadEntry is one conversation of a client.counts response.
type AgentUnreadEntry struct {
	ID string
	// Kind is the list the entry came in: "channel" (public and
	// private channels), "mpim" or "dm".
	Kind         string
	HasUnreads   bool
	MentionCount int
	// UnreadCountDisplay and UnreadCount are nil when Slack leaves the
	// field out (or sends null), which is the usual case.
	UnreadCountDisplay *int
	UnreadCount        *int
	LastRead           string
}

// AgentThreadUnreads is client.counts' threads block.
type AgentThreadUnreads struct {
	HasUnreads   bool
	MentionCount int
}

// AgentUnreadCounts calls client.counts as agent-slack does (with
// thread_count_by_channel) and returns its channels, mpims and ims, in
// that order, and the threads block. A refusal fails with Slack's
// error code in the text (invalid_auth, ...); a rate limit fails with a
// *slack.RateLimitedError, so WaitOutRateLimit can wait it out.
func (c *Client) AgentUnreadCounts(ctx context.Context) ([]AgentUnreadEntry, AgentThreadUnreads, error) {
	raw, err := c.postForm(ctx, "client.counts", url.Values{"thread_count_by_channel": {"true"}})
	if err != nil {
		var rl *rateLimitError
		if errors.As(err, &rl) {
			return nil, AgentThreadUnreads{}, fmt.Errorf("client.counts: %w", &slack.RateLimitedError{RetryAfter: rl.retryAfter})
		}
		return nil, AgentThreadUnreads{}, fmt.Errorf("client.counts: %w", err)
	}
	return decodeAgentUnreadCounts(raw)
}

type agentCountsEntry struct {
	ID                 string `json:"id"`
	HasUnreads         bool   `json:"has_unreads"`
	MentionCount       int    `json:"mention_count"`
	UnreadCountDisplay *int   `json:"unread_count_display"`
	UnreadCount        *int   `json:"unread_count"`
	LastRead           string `json:"last_read"`
}

func decodeAgentUnreadCounts(raw []byte) ([]AgentUnreadEntry, AgentThreadUnreads, error) {
	var resp struct {
		OK       bool               `json:"ok"`
		Error    string             `json:"error"`
		Channels []agentCountsEntry `json:"channels"`
		Mpims    []agentCountsEntry `json:"mpims"`
		Ims      []agentCountsEntry `json:"ims"`
		Threads  struct {
			HasUnreads   bool `json:"has_unreads"`
			MentionCount int  `json:"mention_count"`
		} `json:"threads"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, AgentThreadUnreads{}, fmt.Errorf("decoding client.counts response: %w (body=%s)", err, truncateForLog(raw))
	}
	if !resp.OK {
		code := resp.Error
		if code == "" {
			code = "unknown_error"
		}
		return nil, AgentThreadUnreads{}, fmt.Errorf("client.counts: %w", slack.SlackErrorResponse{Err: code})
	}
	var out []AgentUnreadEntry
	for _, list := range []struct {
		kind    string
		entries []agentCountsEntry
	}{{"channel", resp.Channels}, {"mpim", resp.Mpims}, {"dm", resp.Ims}} {
		for _, e := range list.entries {
			out = append(out, AgentUnreadEntry{
				ID: e.ID, Kind: list.kind, HasUnreads: e.HasUnreads, MentionCount: e.MentionCount,
				UnreadCountDisplay: e.UnreadCountDisplay, UnreadCount: e.UnreadCount, LastRead: e.LastRead,
			})
		}
	}
	return out, AgentThreadUnreads{HasUnreads: resp.Threads.HasUnreads, MentionCount: resp.Threads.MentionCount}, nil
}

// ReadHistoryAfter returns up to limit messages (at most 999, Slack's
// page cap) of a channel after oldest (excluded), newest first, and
// whether Slack has more.
func (c *Client) ReadHistoryAfter(channelID, oldest string, limit int) ([]slack.Message, bool, error) {
	if limit <= 0 || limit > 999 {
		limit = 999
	}
	resp, err := c.api.GetConversationHistory(&slack.GetConversationHistoryParameters{
		ChannelID: channelID,
		Oldest:    oldest,
		Limit:     limit,
	})
	if err != nil {
		return nil, false, fmt.Errorf("getting history: %w", err)
	}
	return resp.Messages, resp.HasMore, nil
}
