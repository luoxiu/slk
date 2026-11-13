// This file implements `slk message draft list|create|update|delete`:
// Slack's native drafts, which show in the user's own Slack client and
// only to him. Listing and creating drafts are what safe mode allows
// (no requireWritable): drafting is the path safe mode leaves an
// unattended agent. Updating and deleting are refused like any write,
// since the draft may be one the user wrote himself. The contract is "草稿" in
// docs/superpowers/specs/2026-11-12-agent-cli-design.md; the requests
// are slackclient's (internal/slack/agentcmd_drafts.go).

package main

import (
	"context"
	"fmt"
	"strings"

	slackclient "github.com/gammons/slk/internal/slack"
	"github.com/gammons/slk/internal/ui/messages/blockkit"
	"github.com/slack-go/slack"
)

// agentDraft is a draft as the draft commands print it. Empty fields
// are left out.
type agentDraft struct {
	ID            string                  `json:"id"`
	Text          string                  `json:"text,omitempty"`
	Destinations  []agentDraftDestination `json:"destinations,omitempty"`
	LastUpdatedTS string                  `json:"last_updated_ts,omitempty"`
	DateCreated   int64                   `json:"date_created,omitempty"`
	DateScheduled int64                   `json:"date_scheduled,omitempty"`
	FileIDs       []string                `json:"file_ids,omitempty"`
}

type agentDraftDestination struct {
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts,omitempty"`
	Broadcast bool   `json:"broadcast,omitempty"`
}

type agentDraftResult struct {
	OK    bool        `json:"ok"`
	Draft *agentDraft `json:"draft,omitempty"`
}

// encodeAgentDraft converts a draft for output. Its text is its
// rich_text blocks as the markdown a person would type
// (blockkit.RichTextToMarkdown, which seeds the composer when a sent
// message is edited), blocks joined by a blank line.
func encodeAgentDraft(d slackclient.Draft) agentDraft {
	var parts []string
	for _, b := range d.Blocks.BlockSet {
		if rt, ok := b.(*slack.RichTextBlock); ok {
			if md := strings.TrimSpace(blockkit.RichTextToMarkdown(blockkit.RichTextBlock{Elements: rt.Elements})); md != "" {
				parts = append(parts, md)
			}
		}
	}
	out := agentDraft{
		ID:            d.ID,
		Text:          strings.Join(parts, "\n\n"),
		LastUpdatedTS: d.LastUpdatedTS,
		DateCreated:   d.DateCreated,
		DateScheduled: d.DateScheduled,
		FileIDs:       d.FileIDs,
	}
	for _, dest := range d.Destinations {
		out.Destinations = append(out.Destinations, agentDraftDestination(dest))
	}
	return out
}

func draftResult(d *slackclient.Draft) agentDraftResult {
	r := agentDraftResult{OK: true}
	if d != nil {
		e := encodeAgentDraft(*d)
		r.Draft = &e
	}
	return r
}

// runAgentDraftList is `slk message draft list`: drafts.list, the
// active drafts (not sent, not deleted) unless --all, following the
// cursor until --limit drafts are read.
func runAgentDraftList(ctx context.Context, env *agentEnv, args []string) error {
	f := newAgentFlags(env, "message draft list")
	workspace := f.workspace()
	limit := f.Int("limit", 20, "return at most `N` drafts")
	all := f.Bool("all", false, "include sent and deleted drafts")
	if _, err := f.parseExact(args); err != nil {
		return err
	}
	if *limit < 1 {
		return f.usageError("--limit must be at least 1")
	}
	client, err := env.openClient(ctx, *workspace, "")
	if err != nil {
		return err
	}
	drafts, err := listAgentDrafts(ctx, client, !*all, *limit)
	if err != nil {
		return err
	}
	out := struct {
		Drafts []agentDraft `json:"drafts"`
	}{Drafts: make([]agentDraft, 0, len(drafts))}
	for _, d := range drafts {
		out.Drafts = append(out.Drafts, encodeAgentDraft(d))
	}
	return env.writeJSON(out)
}

// agentDraftPages bounds how many drafts.list pages one command reads.
const agentDraftPages = 20

// listAgentDrafts reads drafts.list, following the cursor until limit
// drafts are read (limit 0: all of them) or Slack has no more.
func listAgentDrafts(ctx context.Context, client *slackclient.Client, activeOnly bool, limit int) ([]slackclient.Draft, error) {
	type page struct {
		drafts []slackclient.Draft
		next   string
	}
	var all []slackclient.Draft
	cursor := ""
	for range agentDraftPages {
		p, err := agentRetryRateLimit(ctx, func() (page, error) {
			want := 0
			if limit > 0 {
				want = limit - len(all)
			}
			d, next, err := client.ListDrafts(ctx, activeOnly, want, cursor)
			return page{d, next}, err
		})
		if err != nil {
			return nil, err
		}
		all = append(all, p.drafts...)
		if (limit > 0 && len(all) >= limit) || p.next == "" || p.next == cursor {
			break
		}
		cursor = p.next
	}
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// findAgentDraft returns the draft id names, for update and delete: an
// entry not yet sent or deleted. It is looked for among the active
// drafts (is_active=true) first, so a long history of sent drafts
// cannot push it out of the pages listAgentDrafts reads. Only when that
// fails are all drafts read: to tell a draft that was sent or deleted
// (draft_not_found saying so) from an id there is no draft for
// (draft_not_found), and to take a live entry the active list left out.
func findAgentDraft(ctx context.Context, client *slackclient.Client, id string) (slackclient.Draft, error) {
	active, err := listAgentDrafts(ctx, client, true, 0)
	if err != nil {
		return slackclient.Draft{}, err
	}
	if d, ok := pickAgentDraft(active, id); ok && liveDraft(d) {
		return d, nil
	}
	all, err := listAgentDrafts(ctx, client, false, 0)
	if err != nil {
		return slackclient.Draft{}, err
	}
	d, ok := pickAgentDraft(all, id)
	switch {
	case !ok:
		return slackclient.Draft{}, fmt.Errorf("draft_not_found: no draft %s (see 'slk message draft list --all')", id)
	case !liveDraft(d):
		return slackclient.Draft{}, fmt.Errorf("draft_not_found: draft %s was already sent or deleted", id)
	}
	return d, nil
}

// liveDraft reports whether d is a draft still: not sent, not deleted.
func liveDraft(d slackclient.Draft) bool { return !d.IsDeleted && !d.IsSent }

// pickAgentDraft finds draft id in drafts. Slack can list one id more
// than once; a live entry with a last_updated_ts (which update and
// delete must send back) wins.
func pickAgentDraft(drafts []slackclient.Draft, id string) (slackclient.Draft, bool) {
	rank := func(d slackclient.Draft) int {
		r := 0
		if d.LastUpdatedTS != "" {
			r++
		}
		if liveDraft(d) {
			r += 2
		}
		return r
	}
	best := -1
	for i, d := range drafts {
		if d.ID == id && (best < 0 || rank(d) > rank(drafts[best])) {
			best = i
		}
	}
	if best < 0 {
		return slackclient.Draft{}, false
	}
	return drafts[best], true
}

// runAgentDraftCreate is `slk message draft create`: a new draft for a
// conversation, or for a thread (--thread-ts, or a link target, whose
// thread the draft joins, as `message send` replies in it).
func runAgentDraftCreate(ctx context.Context, env *agentEnv, args []string) error {
	f := newAgentFlags(env, "message draft create")
	workspace := f.workspace()
	threadFlag := f.String("thread-ts", "", "draft a reply in the thread whose root has this `ts`")
	broadcast := f.Bool("broadcast", false, "the reply is also sent to the channel (needs a thread)")
	pos, err := f.parseExact(args, "target", "text")
	if err != nil {
		return err
	}
	if strings.TrimSpace(pos[1]) == "" {
		return f.usageError("<text> is empty")
	}
	if err := checkAgentTS(f, "thread-ts", *threadFlag); err != nil {
		return err
	}
	link, _, _, err := parseAgentLink(pos[0])
	if err != nil {
		return err
	}
	thread := *threadFlag
	if thread == "" {
		thread = link.ThreadRoot()
	}
	if *broadcast && thread == "" {
		return f.usageError("--broadcast needs a thread: pass --thread-ts or a message link")
	}
	client, target, err := openAgentTarget(ctx, env, *workspace, pos[0])
	if err != nil {
		return err
	}
	d, err := client.CreateDraft(ctx, slackclient.DraftDestination{
		ChannelID: target.ChannelID,
		ThreadTS:  thread,
		Broadcast: *broadcast,
	}, pos[1])
	if err != nil {
		return err
	}
	return env.writeJSON(draftResult(d))
}

// runAgentDraftUpdate is `slk message draft update`: new text for a
// draft, its destinations and files unchanged. Safe mode refuses it:
// the draft may be one the user wrote himself.
func runAgentDraftUpdate(ctx context.Context, env *agentEnv, args []string) error {
	if err := requireWritable(env); err != nil {
		return err
	}
	f := newAgentFlags(env, "message draft update")
	workspace := f.workspace()
	pos, err := f.parseExact(args, "draft-id", "text")
	if err != nil {
		return err
	}
	if strings.TrimSpace(pos[0]) == "" {
		return f.usageError("<draft-id> is empty")
	}
	if strings.TrimSpace(pos[1]) == "" {
		return f.usageError("<text> is empty")
	}
	client, err := env.openClient(ctx, *workspace, "")
	if err != nil {
		return err
	}
	d, err := findAgentDraft(ctx, client, pos[0])
	if err != nil {
		return err
	}
	if d.DateScheduled > 0 {
		// drafts.update carries no schedule, so it could drop it and the
		// message would never go out (agent-slack refuses for the same
		// reason).
		return fmt.Errorf("draft %s is scheduled to send at %d; updating it here could drop the schedule, so edit it in Slack", d.ID, d.DateScheduled)
	}
	updated, err := client.UpdateDraft(ctx, d, pos[1])
	if err != nil {
		return err
	}
	return env.writeJSON(draftResult(updated))
}

// runAgentDraftDelete is `slk message draft delete`. Safe mode refuses
// it, as it refuses update.
func runAgentDraftDelete(ctx context.Context, env *agentEnv, args []string) error {
	if err := requireWritable(env); err != nil {
		return err
	}
	f := newAgentFlags(env, "message draft delete")
	workspace := f.workspace()
	pos, err := f.parseExact(args, "draft-id")
	if err != nil {
		return err
	}
	if strings.TrimSpace(pos[0]) == "" {
		return f.usageError("<draft-id> is empty")
	}
	client, err := env.openClient(ctx, *workspace, "")
	if err != nil {
		return err
	}
	d, err := findAgentDraft(ctx, client, pos[0])
	if err != nil {
		return err
	}
	if d.DateScheduled > 0 {
		// A scheduled draft is a message set to go out: deleting it
		// cancels the send, which is the user's call, made in Slack.
		return fmt.Errorf("draft %s is a scheduled message (sends at %d); deleting it would cancel the send, so handle it in Slack", d.ID, d.DateScheduled)
	}
	if err := client.DeleteDraft(ctx, d.ID, d.LastUpdatedTS); err != nil {
		return err
	}
	return env.writeJSON(struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}{true, d.ID})
}
