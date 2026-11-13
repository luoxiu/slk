package slackclient

import (
	"context"
	"net/url"
	"strconv"
)

// This file holds Slack's Later list ("saved for later") for `slk
// later` (cmd/slk/agentcmd_later.go): the web client's undocumented
// saved.list, saved.add, saved.update and saved.delete, shaped as
// agent-slack 0.10.2 sends them (src/slack/later.ts).
//
// saved.update goes multipart/form-data: sent form-encoded, Slack
// answers ok and ignores mark and date_due (agent-slack PR #76, whose
// author checked each transition in the Slack UI). saved.list, .add and
// .delete are form-encoded, as agent-slack sends them.

// SavedItem is one saved.list entry. For a message, ItemID is its
// conversation and TS its ts.
type SavedItem struct {
	ItemID   string
	ItemType string // "message" for a message; files and others are listed too
	TS       string
	State    string // "in_progress", "completed" or "archived"
	// Unix seconds; 0 when Slack leaves the date out.
	DateCreated   int64
	DateCompleted int64
	DateDue       int64
}

// SavedCounts is saved.list's counts block.
type SavedCounts struct {
	InProgress int // Slack's uncompleted_count
	Archived   int
	Completed  int
	Total      int
}

// ListSavedPage returns one saved.list page, the counts Slack sends with
// it, and the cursor of the next page ("" at the end). Without a filter
// Slack lists in-progress and archived items; completed ones come only
// with filter "completed" (seen live 2026-11-13).
func (c *Client) ListSavedPage(ctx context.Context, filter string, limit int, cursor string) ([]SavedItem, SavedCounts, string, error) {
	form := url.Values{}
	if filter != "" {
		form.Set("filter", filter)
	}
	if limit > 0 {
		form.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		form.Set("cursor", cursor)
	}
	var resp struct {
		Items []struct {
			ItemID        string  `json:"item_id"`
			ItemType      string  `json:"item_type"`
			TS            string  `json:"ts"`
			State         string  `json:"state"`
			DateCreated   flexInt `json:"date_created"`
			DateCompleted flexInt `json:"date_completed"`
			DateDue       flexInt `json:"date_due"`
		} `json:"saved_items"`
		Counts struct {
			Uncompleted flexInt `json:"uncompleted_count"`
			Archived    flexInt `json:"archived_count"`
			Completed   flexInt `json:"completed_count"`
			Total       flexInt `json:"total_count"`
		} `json:"counts"`
		Metadata struct {
			NextCursor string `json:"next_cursor"`
		} `json:"response_metadata"`
	}
	if err := c.agentPost(ctx, "saved.list", form, &resp); err != nil {
		return nil, SavedCounts{}, "", err
	}
	items := make([]SavedItem, 0, len(resp.Items))
	for _, it := range resp.Items {
		items = append(items, SavedItem{
			ItemID: it.ItemID, ItemType: it.ItemType, TS: it.TS, State: it.State,
			DateCreated: int64(it.DateCreated), DateCompleted: int64(it.DateCompleted), DateDue: int64(it.DateDue),
		})
	}
	counts := SavedCounts{
		InProgress: int(resp.Counts.Uncompleted),
		Archived:   int(resp.Counts.Archived),
		Completed:  int(resp.Counts.Completed),
		Total:      int(resp.Counts.Total),
	}
	return items, counts, resp.Metadata.NextCursor, nil
}

func savedMessageForm(channelID, ts string) url.Values {
	return url.Values{"item_id": {channelID}, "item_type": {"message"}, "ts": {ts}}
}

// AddSaved saves message ts of channelID for later (saved.add).
func (c *Client) AddSaved(ctx context.Context, channelID, ts string) error {
	return c.agentPost(ctx, "saved.add", savedMessageForm(channelID, ts), nil)
}

// DeleteSaved takes message ts of channelID off the Later list
// altogether (saved.delete).
func (c *Client) DeleteSaved(ctx context.Context, channelID, ts string) error {
	return c.agentPost(ctx, "saved.delete", savedMessageForm(channelID, ts), nil)
}

// The saved.update marks: Later's state transitions.
const (
	SavedMarkCompleted   = "completed"
	SavedMarkUncompleted = "uncompleted"
	SavedMarkArchived    = "archived"
	SavedMarkUnarchived  = "unarchived"
)

// MarkSaved moves a saved message to another state (saved.update with
// mark, one of the SavedMark* values).
func (c *Client) MarkSaved(ctx context.Context, channelID, ts, mark string) error {
	return c.agentPostMultipart(ctx, "saved.update", savedMessageFields(channelID, ts, formField{"mark", mark}), nil)
}

// SetSavedDue sets a saved message's reminder to due, in Unix seconds
// (saved.update with date_due).
func (c *Client) SetSavedDue(ctx context.Context, channelID, ts string, due int64) error {
	return c.agentPostMultipart(ctx, "saved.update", savedMessageFields(channelID, ts, formField{"date_due", strconv.FormatInt(due, 10)}), nil)
}

func savedMessageFields(channelID, ts string, extra formField) []formField {
	return []formField{{"item_id", channelID}, {"item_type", "message"}, {"ts", ts}, extra}
}
