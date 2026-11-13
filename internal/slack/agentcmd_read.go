package slackclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/slack-go/slack"
)

// This file holds the Web API reads the headless `slk` commands need
// that the TUI's methods do not cover: a bounded history window, a
// paginated thread, one page of conversations, one page of users, and
// a time-sorted search. Errors keep Slack's error code in their text
// (`thread_not_found`, `channel_not_found`), which callers match on.

// agentRepliesPageSize is conversations.replies' per-page limit.
const agentRepliesPageSize = 200

// ReadHistory returns the messages of a channel between oldest and
// latest (either may be empty; both bounds are excluded unless
// inclusive), newest first, at most limit of them (Slack caps one page
// at 999). With no oldest it is the newest limit messages before latest.
func (c *Client) ReadHistory(ctx context.Context, channelID, oldest, latest string, inclusive bool, limit int) ([]slack.Message, error) {
	if limit <= 0 || limit > 999 {
		limit = 999
	}
	resp, err := c.api.GetConversationHistory(&slack.GetConversationHistoryParameters{
		ChannelID: channelID,
		Oldest:    oldest,
		Latest:    latest,
		Inclusive: inclusive,
		Limit:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("getting history: %w", err)
	}
	return resp.Messages, nil
}

// ReadThread returns the thread that contains ts (the root, or any
// reply in it), root first, following the cursor until the thread ends
// or max messages are read. A thread that does not exist fails with
// Slack's thread_not_found in the error text. Slack may repeat the
// root at the top of every page; each ts is kept once and repeats do
// not count toward max.
func (c *Client) ReadThread(ctx context.Context, channelID, ts string, max int) ([]slack.Message, error) {
	var all []slack.Message
	seen := map[string]bool{}
	cursor := ""
	for {
		limit := agentRepliesPageSize
		// +1 leaves room for a repeated root, so a page always carries
		// at least one new message.
		if max > 0 && max-len(all)+1 < limit {
			limit = max - len(all) + 1
		}
		msgs, hasMore, next, err := c.api.GetConversationReplies(&slack.GetConversationRepliesParameters{
			ChannelID: channelID,
			Timestamp: ts,
			Cursor:    cursor,
			Limit:     limit,
		})
		if err != nil {
			return nil, fmt.Errorf("getting thread replies: %w", err)
		}
		for _, m := range msgs {
			if seen[m.Timestamp] || (max > 0 && len(all) >= max) {
				continue
			}
			seen[m.Timestamp] = true
			all = append(all, m)
		}
		if !hasMore || next == "" || (max > 0 && len(all) >= max) {
			break
		}
		cursor = next
	}
	return all, nil
}

// ReadThreadMessage returns the message ts of the thread rooted at
// root, or nil when the thread has no such message. It asks Slack for
// just that ts (oldest = latest = ts, inclusive) instead of reading the
// thread, so a late reply is found however long the thread is. Slack
// may put the root first, so the page limit is 2 and the entry is
// picked by its ts; a thread that does not exist fails with
// thread_not_found in the error text.
func (c *Client) ReadThreadMessage(ctx context.Context, channelID, root, ts string) (*slack.Message, error) {
	cursor := ""
	for {
		msgs, hasMore, next, err := c.api.GetConversationReplies(&slack.GetConversationRepliesParameters{
			ChannelID: channelID,
			Timestamp: root,
			Oldest:    ts,
			Latest:    ts,
			Inclusive: true,
			Cursor:    cursor,
			Limit:     2,
		})
		if err != nil {
			return nil, fmt.Errorf("getting thread message: %w", err)
		}
		for i := range msgs {
			if msgs[i].Timestamp == ts {
				return &msgs[i], nil
			}
		}
		if !hasMore || next == "" {
			return nil, nil
		}
		cursor = next
	}
}

// ListConversationsPage returns one page of the conversations the user
// is in (users.conversations: public and private channels, group DMs
// and DMs, archived ones left out) and the cursor of the next page,
// "" at the end.
func (c *Client) ListConversationsPage(ctx context.Context, limit int, cursor string) ([]slack.Channel, string, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 999 {
		limit = 999
	}
	channels, next, err := c.api.GetConversationsForUser(&slack.GetConversationsForUserParameters{
		Types:           []string{"public_channel", "private_channel", "mpim", "im"},
		Limit:           limit,
		Cursor:          cursor,
		ExcludeArchived: true,
	})
	if err != nil {
		var rl *slack.RateLimitedError
		if errors.As(err, &rl) {
			return nil, "", fmt.Errorf("listing conversations: rate limited, retry in %s", rl.RetryAfter.Round(time.Second))
		}
		return nil, "", fmt.Errorf("listing conversations: %w", err)
	}
	return channels, next, nil
}

// UserListEntry is one users.list member: the fields `slk user get`
// matches an email or a handle on.
type UserListEntry struct {
	ID          string
	Name        string // the account name, which Slack shows as the handle
	DisplayName string
	Email       string
}

// ListUsersPage returns one users.list page of at most limit members
// (Slack's default when 0) from cursor, and the cursor of the next page
// ("" at the end). A rate limit fails with a *slack.RateLimitedError,
// so WaitOutRateLimit can wait it out; a refusal with Slack's error code
// in the text.
func (c *Client) ListUsersPage(ctx context.Context, limit int, cursor string) ([]UserListEntry, string, error) {
	form := url.Values{}
	if limit > 0 {
		form.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		form.Set("cursor", cursor)
	}
	var resp struct {
		Members []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Profile struct {
				DisplayName string `json:"display_name"`
				Email       string `json:"email"`
			} `json:"profile"`
		} `json:"members"`
		Meta struct {
			NextCursor string `json:"next_cursor"`
		} `json:"response_metadata"`
	}
	if err := c.agentPost(ctx, "users.list", form, &resp); err != nil {
		return nil, "", err
	}
	users := make([]UserListEntry, 0, len(resp.Members))
	for _, m := range resp.Members {
		users = append(users, UserListEntry{ID: m.ID, Name: m.Name, DisplayName: m.Profile.DisplayName, Email: m.Profile.Email})
	}
	return users, resp.Meta.NextCursor, nil
}

// SearchHit is one search.messages match: slack-go's SearchMessage plus
// bot_id, which slack-go does not decode.
type SearchHit struct {
	slack.SearchMessage
	BotID string `json:"bot_id"`
}

// SearchMessagesNewestFirst is a search.messages call sorted by
// timestamp, newest first, instead of by relevance. query is passed
// through as is, so modifiers (from:, after:, ...) work. It decodes the
// response itself so bot_id survives.
func (c *Client) SearchMessagesNewestFirst(ctx context.Context, query string, count int) ([]SearchHit, error) {
	form := url.Values{"query": {query}, "sort": {"timestamp"}}
	if count > 0 {
		form.Set("count", strconv.Itoa(count))
	}
	raw, err := c.postForm(ctx, "search.messages", form)
	if err != nil {
		var rl *rateLimitError
		if errors.As(err, &rl) {
			return nil, fmt.Errorf("searching messages: %w", &slack.RateLimitedError{RetryAfter: rl.retryAfter})
		}
		return nil, fmt.Errorf("searching messages: %w", err)
	}
	var resp struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Messages struct {
			Matches []SearchHit `json:"matches"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decoding search.messages response: %w", err)
	}
	if !resp.OK {
		code := resp.Error
		if code == "" {
			code = "unknown_error"
		}
		return nil, fmt.Errorf("searching messages: %w", slack.SlackErrorResponse{Err: code})
	}
	return resp.Messages.Matches, nil
}
