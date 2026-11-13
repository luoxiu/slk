package slackclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/gammons/slk/internal/slack/mrkdwn"
	"github.com/slack-go/slack"
)

// This file holds Slack's native message drafts for `slk message
// draft` (cmd/slk/agentcmd_drafts.go): the web client's undocumented
// drafts.list, drafts.create, drafts.update and drafts.delete. A draft
// made here shows in the user's own Slack client, in its drafts list
// and in the composer it belongs to.
//
// Request shapes follow agent-slack 0.10.2 (src/slack/drafts.ts),
// which sends them form-encoded with blocks, destinations and file_ids
// as JSON strings, with one exception: drafts.create sends
// is_from_composer=false, the value every draft the desktop app made
// carries (100 drafts read from a real account on 2026-11-13, channel
// and thread ones alike). agent-slack sends true, and its thread drafts
// showed up in the channel's composer instead of the thread's.

// DraftDestination is where a draft will be sent: a conversation, and
// a thread in it when ThreadTS is set (Broadcast: also to the channel).
type DraftDestination struct {
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts,omitempty"`
	Broadcast bool   `json:"broadcast"`
}

// Draft is one drafts.list entry, or the draft drafts.create and
// drafts.update answer with.
type Draft struct {
	ID            string
	Blocks        slack.Blocks
	Destinations  []DraftDestination
	LastUpdatedTS string
	DateCreated   int64
	DateScheduled int64 // 0 when the draft is not scheduled
	FileIDs       []string
	IsDeleted     bool
	IsSent        bool
}

type draftWire struct {
	ID            string          `json:"id"`
	Blocks        slack.Blocks    `json:"blocks"`
	Destinations  json.RawMessage `json:"destinations"`
	LastUpdatedTS string          `json:"last_updated_ts"`
	DateCreated   flexInt         `json:"date_created"`
	DateScheduled flexInt         `json:"date_scheduled"`
	FileIDs       []string        `json:"file_ids"`
	IsDeleted     bool            `json:"is_deleted"`
	IsSent        bool            `json:"is_sent"`
}

func (w draftWire) draft() (Draft, error) {
	d := Draft{
		ID:            w.ID,
		Blocks:        w.Blocks,
		LastUpdatedTS: w.LastUpdatedTS,
		DateCreated:   int64(w.DateCreated),
		DateScheduled: int64(w.DateScheduled),
		FileIDs:       w.FileIDs,
		IsDeleted:     w.IsDeleted,
		IsSent:        w.IsSent,
	}
	if len(w.Destinations) > 0 && string(w.Destinations) != "null" {
		if err := json.Unmarshal(w.Destinations, &d.Destinations); err != nil {
			return Draft{}, fmt.Errorf("decoding draft %s destinations: %w", w.ID, err)
		}
	}
	return d, nil
}

// ListDrafts returns one drafts.list page and the cursor of the next
// ("" at the end): the drafts not yet sent or deleted when activeOnly,
// else all of them. limit 0 leaves Slack's default.
func (c *Client) ListDrafts(ctx context.Context, activeOnly bool, limit int, cursor string) ([]Draft, string, error) {
	form := url.Values{}
	if activeOnly {
		form.Set("is_active", "true")
	}
	if limit > 0 {
		form.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		form.Set("cursor", cursor)
	}
	var resp struct {
		Drafts   []draftWire `json:"drafts"`
		Metadata struct {
			NextCursor string `json:"next_cursor"`
		} `json:"response_metadata"`
	}
	if err := c.agentPost(ctx, "drafts.list", form, &resp); err != nil {
		return nil, "", err
	}
	drafts := make([]Draft, 0, len(resp.Drafts))
	for _, w := range resp.Drafts {
		d, err := w.draft()
		if err != nil {
			return nil, "", err
		}
		drafts = append(drafts, d)
	}
	return drafts, resp.Metadata.NextCursor, nil
}

// CreateDraft saves text as a new draft for dest. The text is converted
// as SendMessage converts it (mrkdwn.Convert, one rich_text block).
func (c *Client) CreateDraft(ctx context.Context, dest DraftDestination, text string) (*Draft, error) {
	form, err := draftForm(text)
	if err != nil {
		return nil, err
	}
	dests, err := json.Marshal([]DraftDestination{dest})
	if err != nil {
		return nil, err
	}
	form.Set("destinations", string(dests))
	form.Set("file_ids", "[]")
	form.Set("is_from_composer", "false")
	return c.draftCall(ctx, "drafts.create", form)
}

// UpdateDraft replaces the text of draft d (as ListDrafts returned it),
// keeping its destinations and its files. The destinations go back as
// channel_id, thread_ts and broadcast only: drafts.list adds a DM's
// user_ids, and drafts.update refuses a destination with both
// (both_user_ids_and_channel_id_provided_in_destination, 2026-11-13). Slack
// checks client_last_updated_ts against the draft's own, so a draft
// edited elsewhere since it was listed is refused rather than
// overwritten. It sends no is_from_composer: on 2026-11-13 a thread
// draft and a DM draft created here were each updated once, and
// drafts.list still gave both is_from_composer=false, so an update
// without it does not reset the value CreateDraft set.
func (c *Client) UpdateDraft(ctx context.Context, d Draft, text string) (*Draft, error) {
	form, err := draftForm(text)
	if err != nil {
		return nil, err
	}
	dests, err := json.Marshal(d.Destinations)
	if err != nil {
		return nil, err
	}
	files := d.FileIDs
	if files == nil {
		files = []string{}
	}
	fileJSON, err := json.Marshal(files)
	if err != nil {
		return nil, err
	}
	form.Set("destinations", string(dests))
	form.Set("file_ids", string(fileJSON))
	form.Set("draft_id", d.ID)
	form.Set("client_last_updated_ts", padDraftTS(d.LastUpdatedTS))
	return c.draftCall(ctx, "drafts.update", form)
}

// DeleteDraft deletes draft id; lastUpdatedTS is its last_updated_ts
// from ListDrafts.
func (c *Client) DeleteDraft(ctx context.Context, id, lastUpdatedTS string) error {
	return c.agentPost(ctx, "drafts.delete", url.Values{
		"draft_id":               {id},
		"client_last_updated_ts": {padDraftTS(lastUpdatedTS)},
	}, nil)
}

// draftForm is the part of a create or update body that carries the
// text: the rich_text block and a fresh client_msg_id (agent-slack
// makes a new one for every create and update).
func draftForm(text string) (url.Values, error) {
	_, block := mrkdwn.Convert(text)
	if block == nil {
		return nil, errors.New("draft text is empty")
	}
	blocks, err := json.Marshal([]slack.Block{block})
	if err != nil {
		return nil, fmt.Errorf("encoding draft blocks: %w", err)
	}
	return url.Values{
		"blocks":        {string(blocks)},
		"client_msg_id": {newClientMsgID()},
	}, nil
}

func (c *Client) draftCall(ctx context.Context, method string, form url.Values) (*Draft, error) {
	var resp struct {
		Draft *draftWire `json:"draft"`
	}
	if err := c.agentPost(ctx, method, form, &resp); err != nil {
		return nil, err
	}
	if resp.Draft == nil {
		return nil, nil
	}
	d, err := resp.Draft.draft()
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// padDraftTS pads a draft timestamp's fraction to 7 digits, as
// agent-slack's padDraftTs does: Slack refuses a
// client_last_updated_ts with fewer ("1700000000.123" must go back as
// "1700000000.1230000"). Longer fractions are left alone.
func padDraftTS(ts string) string {
	sec, frac, ok := strings.Cut(ts, ".")
	if !ok {
		return ts
	}
	if len(frac) < 7 {
		frac += strings.Repeat("0", 7-len(frac))
	}
	return sec + "." + frac
}
