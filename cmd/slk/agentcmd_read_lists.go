// This file implements the read-only agent commands that list things:
// unreads, search messages, channel list and user get. See
// docs/superpowers/specs/2026-11-12-agent-cli-design.md.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	slackclient "github.com/gammons/slk/internal/slack"
	"github.com/slack-go/slack"
)

// agentUserIDRe matches a Slack user ID: U or W and eight or more
// capitals or digits (every real one has at least nine characters), so
// an all-caps handle such as WANGLI is not taken for one.
var agentUserIDRe = regexp.MustCompile(`^[UW][A-Z0-9]{8,}$`)

// ---------------------------------------------------------------------
// unreads
// ---------------------------------------------------------------------

type agentUnreadChannel struct {
	ChannelID    string         `json:"channel_id"`
	ChannelName  string         `json:"channel_name"`
	ChannelType  string         `json:"channel_type"`
	UnreadCount  int            `json:"unread_count"`
	MentionCount int            `json:"mention_count"`
	Messages     []agentMessage `json:"messages"`
}

type agentUnreadThreads struct {
	HasUnreads   bool `json:"has_unreads"`
	MentionCount int  `json:"mention_count"`
}

type agentUnreadsOutput struct {
	Channels []agentUnreadChannel `json:"channels"`
	// Threads is there only when Slack says some thread has unread
	// replies, as in agent-slack.
	Threads *agentUnreadThreads `json:"threads,omitempty"`
}

// agentUnreadsInFlight bounds the Slack requests `slk unreads` has in
// flight at once. agent-slack sends them all at once (Promise.all), 2–3
// per unread conversation, which on a long inbox is a burst of hundreds
// that Slack's per-method rate limits answer with 429s and waits, and
// that looks nothing like the browser slk presents itself as. 16 keeps
// a usual inbox (a few unread conversations) fully parallel and a long
// one at a steady pace.
const agentUnreadsInFlight = 16

// agentSystemSubtypes are the message subtypes unreads leaves out, as
// agent-slack does by default: joins, leaves, renames, topic and
// purpose changes, (un)archiving.
var agentSystemSubtypes = map[string]bool{
	"channel_join": true, "channel_leave": true, "channel_topic": true, "channel_purpose": true,
	"channel_name": true, "channel_archive": true, "channel_unarchive": true,
	"group_join": true, "group_leave": true, "group_topic": true, "group_purpose": true,
	"group_name": true, "group_archive": true, "group_unarchive": true,
}

// agentChannelType maps a conversation to the spec's channel_type:
// dm, mpim, group (private channel) or channel.
func agentChannelType(ch slack.Channel) string {
	switch {
	case ch.IsMpIM:
		return "mpim"
	case ch.IsIM:
		return "dm"
	case ch.IsPrivate || ch.IsGroup:
		return "group"
	}
	return "channel"
}

// agentLimited runs one Slack request while holding a slot of lim,
// waiting out rate limits (agentRetryRateLimit) without holding it.
func agentLimited[T any](ctx context.Context, lim chan struct{}, fn func() (T, error)) (T, error) {
	return agentRetryRateLimit(ctx, func() (T, error) {
		select {
		case lim <- struct{}{}:
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		}
		defer func() { <-lim }()
		return fn()
	})
}

// agentWorseError is the one of a and b to report: a failed sign-in
// (exit 2) first, then a failure that is not "this item is gone"
// (agentItemGone), then a.
func agentWorseError(a, b error) error {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case agentExitCode(b) == 2 && agentExitCode(a) != 2:
		return b
	case agentItemGone(a) && !agentItemGone(b):
		return b
	}
	return a
}

// runAgentUnreads is `slk unreads`, after agent-slack's: one
// client.counts call lists the conversations with unread messages, each
// one's counts and last_read. For every unread conversation, all at
// once (at most agentUnreadsInFlight requests in flight), it asks
// conversations.info for the name and type (users.info for a DM's
// peer) and conversations.history for the newest --max-messages
// messages after last_read, system messages then dropped. Conversations
// are sorted by mention_count, then unread_count, both descending.
//
// mention_count is Slack's number from client.counts; for DMs and group
// DMs that reading is UNVERIFIED (see slackclient.UnreadInfo). Threads
// with new replies show only as the top-level "threads" summary.
func runAgentUnreads(ctx context.Context, env *agentEnv, args []string) error {
	f := newAgentFlags(env, "unreads")
	workspace := f.workspace()
	maxMessages := f.Int("max-messages", 10, "newest `N` unread messages per conversation")
	maxBody := f.maxBodyChars()
	if _, err := f.parseExact(args); err != nil {
		return err
	}
	if *maxMessages < 1 {
		return f.usageError("--max-messages must be at least 1")
	}
	client, err := env.openClient(ctx, *workspace, "")
	if err != nil {
		return err
	}
	type countsResult struct {
		entries []slackclient.AgentUnreadEntry
		threads slackclient.AgentThreadUnreads
	}
	counts, err := agentRetryRateLimit(ctx, func() (countsResult, error) {
		e, t, err := client.AgentUnreadCounts(ctx)
		return countsResult{e, t}, err
	})
	if err != nil {
		return err
	}
	var unread []slackclient.AgentUnreadEntry
	for _, u := range counts.entries {
		if u.HasUnreads {
			unread = append(unread, u)
		}
	}

	opts := agentMessageDefaults()
	opts.MaxBodyChars = *maxBody
	opts.CutMarker = agentUnreadsCutMarker
	lim := make(chan struct{}, agentUnreadsInFlight)
	results := make([]*agentUnreadChannel, len(unread))
	errs := make([]error, len(unread))
	var wg sync.WaitGroup
	for i, u := range unread {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = readAgentUnread(ctx, client, lim, u, *maxMessages, opts)
		}()
	}
	wg.Wait()

	out := agentUnreadsOutput{Channels: []agentUnreadChannel{}}
	var failErr error
	for i, r := range results {
		if errs[i] == nil {
			out.Channels = append(out.Channels, *r)
			continue
		}
		if agentItemGone(errs[i]) {
			// One conversation Slack says is gone (archived, left)
			// must not hide the others.
			fmt.Fprintf(env.stderr, "slk unreads: skipping %s: %v\n", unread[i].ID, errs[i])
			continue
		}
		// Anything else (a network error, a Slack outage, a failed
		// sign-in) must not turn into a shorter, successful list.
		failErr = agentWorseError(failErr, fmt.Errorf("unreads %s: %w", unread[i].ID, errs[i]))
	}
	if failErr != nil {
		return failErr
	}
	sort.SliceStable(out.Channels, func(i, j int) bool {
		a, b := out.Channels[i], out.Channels[j]
		if a.MentionCount != b.MentionCount {
			return a.MentionCount > b.MentionCount
		}
		return a.UnreadCount > b.UnreadCount
	})
	if counts.threads.HasUnreads {
		out.Threads = &agentUnreadThreads{HasUnreads: true, MentionCount: counts.threads.MentionCount}
	}
	return env.writeJSON(out)
}

// readAgentUnread reads one unread conversation: its name and type,
// and its messages after last_read, side by side.
func readAgentUnread(ctx context.Context, client *slackclient.Client, lim chan struct{}, u slackclient.AgentUnreadEntry, maxMessages int, opts agentMessageOptions) (*agentUnreadChannel, error) {
	var (
		wg              sync.WaitGroup
		name, chType    string
		infoErr         error
		page            agentHistoryPage
		histErr         error
		readTheMessages = u.LastRead != "" // as agent-slack: no last_read, no messages
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		name, chType, infoErr = describeAgentUnread(ctx, client, lim, u.ID)
	}()
	if readTheMessages {
		wg.Add(1)
		go func() {
			defer wg.Done()
			page, histErr = agentLimited(ctx, lim, func() (agentHistoryPage, error) {
				msgs, more, err := client.ReadHistoryAfter(u.ID, u.LastRead, maxMessages)
				return agentHistoryPage{msgs, more}, err
			})
		}()
	}
	wg.Wait()
	if err := agentWorseError(infoErr, histErr); err != nil {
		return nil, err
	}

	var msgs []slack.Message
	for _, m := range page.msgs {
		if !agentSystemSubtypes[m.SubType] {
			msgs = append(msgs, m)
		}
	}
	sortAgentOldestFirst(msgs)
	ch := &agentUnreadChannel{
		ChannelID:    u.ID,
		ChannelName:  name,
		ChannelType:  chType,
		UnreadCount:  agentUnreadCount(u, readTheMessages, len(msgs), page.hasMore),
		MentionCount: u.MentionCount,
		Messages:     make([]agentMessage, 0, len(msgs)),
	}
	for _, m := range msgs {
		ch.Messages = append(ch.Messages, encodeAgentMessage(m, u.ID, opts))
	}
	return ch, nil
}

type agentHistoryPage struct {
	msgs    []slack.Message
	hasMore bool
}

// agentUnreadCount is agent-slack's unread_count: Slack's
// unread_count_display, else its unread_count; when client.counts has
// neither (the usual case), the number of messages read after
// last_read (system messages left out), at least 2 when Slack has
// more; 1 when there was no last_read to read after.
func agentUnreadCount(u slackclient.AgentUnreadEntry, read bool, n int, hasMore bool) int {
	switch {
	case u.UnreadCountDisplay != nil:
		return *u.UnreadCountDisplay
	case u.UnreadCount != nil:
		return *u.UnreadCount
	case !read:
		if u.HasUnreads {
			return 1
		}
		return 0
	case hasMore:
		return max(n, 2)
	}
	return n
}

// describeAgentUnread returns a conversation's name and channel_type.
// A DM has no name of its own: it is named after the peer
// (display_name, else real_name, else name), or the peer's ID when
// Slack cannot find them.
func describeAgentUnread(ctx context.Context, client *slackclient.Client, lim chan struct{}, channelID string) (string, string, error) {
	info, err := agentLimited(ctx, lim, func() (*slack.Channel, error) {
		return client.GetConversationInfo(ctx, channelID)
	})
	if err != nil {
		return "", "", err
	}
	name := info.Name
	if name == "" {
		name = info.NameNormalized
	}
	if info.IsIM && name == "" && info.User != "" {
		p, err := agentLimited(ctx, lim, func() (*slackclient.UserProfile, error) {
			return client.GetUserProfileContext(ctx, info.User)
		})
		switch {
		case err == nil:
			name = agentPersonName(p)
		case agentItemGone(err):
			// The peer is gone: the DM is still worth reading.
		default:
			return "", "", err
		}
		if name == "" {
			name = info.User
		}
	}
	return name, agentChannelType(*info), nil
}

// agentPersonName is what agent-slack calls a DM's peer: their display
// name, else their real name, else their handle.
func agentPersonName(p *slackclient.UserProfile) string {
	if p == nil || p.User == nil {
		return ""
	}
	for _, n := range []string{p.Profile.DisplayName, p.RealName, p.Name} {
		if n != "" {
			return n
		}
	}
	return ""
}

// ---------------------------------------------------------------------
// search messages
// ---------------------------------------------------------------------

// agentSearchMaxCount is the most results one search.messages page
// returns.
const agentSearchMaxCount = 100

// agentSearchQuery is the query text sent to Slack: the user's query
// followed by the modifiers the flags ask for.
func agentSearchQuery(query, after, before, user string) string {
	parts := []string{query}
	if after != "" {
		parts = append(parts, "after:"+after)
	}
	if before != "" {
		parts = append(parts, "before:"+before)
	}
	if user != "" {
		parts = append(parts, "from:<@"+user+">")
	}
	return strings.Join(parts, " ")
}

// runAgentSearchMessages is `slk search messages`: one search.messages
// call sorted by time, newest first. Slack's `after:` and `before:`
// both exclude the date given.
func runAgentSearchMessages(ctx context.Context, env *agentEnv, args []string) error {
	f := newAgentFlags(env, "search messages")
	workspace := f.workspace()
	after := f.String("after", "", "only messages after this `date` (YYYY-MM-DD)")
	before := f.String("before", "", "only messages before this `date` (YYYY-MM-DD)")
	user := f.String("user", "", "only messages from this user `ID`")
	limit := f.Int("limit", 20, fmt.Sprintf("return at most `N` messages (up to %d)", agentSearchMaxCount))
	maxBody := f.maxBodyChars()
	pos, err := f.parseExact(args, "query")
	if err != nil {
		return err
	}
	for name, v := range map[string]string{"after": *after, "before": *before} {
		if v == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", v); err != nil {
			return f.usageError("--%s %q is not a date like 2026-11-12", name, v)
		}
	}
	if *user != "" && !agentUserIDRe.MatchString(*user) {
		return f.usageError("--user %q is not a user ID (like U0123ABC)", *user)
	}
	if *limit < 1 {
		return f.usageError("--limit must be at least 1")
	}
	count := *limit
	if count > agentSearchMaxCount {
		count = agentSearchMaxCount
	}
	client, err := env.openClient(ctx, *workspace, "")
	if err != nil {
		return err
	}
	res, err := client.SearchMessagesNewestFirst(ctx, agentSearchQuery(pos[0], *after, *before, *user), count)
	if err != nil {
		return err
	}
	out := struct {
		Messages []agentMessage `json:"messages"`
	}{Messages: []agentMessage{}}
	for _, hit := range res {
		if len(out.Messages) == *limit {
			break
		}
		m := slack.Message{Msg: slack.Msg{
			Timestamp:   hit.Timestamp,
			User:        hit.User,
			Username:    hit.Username,
			BotID:       hit.BotID,
			Text:        hit.Text,
			Blocks:      hit.Blocks,
			Attachments: hit.Attachments,
		}}
		m.ThreadTimestamp = agentPermalinkThreadTS(hit.Permalink)
		opts := agentMessageDefaults()
		opts.MaxBodyChars = *maxBody
		opts.Permalink = hit.Permalink
		out.Messages = append(out.Messages, encodeAgentMessage(m, hit.Channel.ID, opts))
	}
	return env.writeJSON(out)
}

// agentPermalinkThreadTS is the ?thread_ts= of a Slack permalink, ""
// when it has none.
func agentPermalinkThreadTS(permalink string) string {
	u, err := url.Parse(permalink)
	if err != nil {
		return ""
	}
	return u.Query().Get("thread_ts")
}

// ---------------------------------------------------------------------
// channel list
// ---------------------------------------------------------------------

type agentChannelEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsChannel bool   `json:"is_channel"`
	IsGroup   bool   `json:"is_group"`
	IsIM      bool   `json:"is_im"`
	IsMpIM    bool   `json:"is_mpim"`
	IsPrivate bool   `json:"is_private"`
	User      string `json:"user,omitempty"`
}

// runAgentChannelList is `slk channel list`: one users.conversations
// page and the cursor for the next ("" at the end).
func runAgentChannelList(ctx context.Context, env *agentEnv, args []string) error {
	f := newAgentFlags(env, "channel list")
	workspace := f.workspace()
	limit := f.Int("limit", 200, "return at most `N` conversations")
	cursor := f.String("cursor", "", "the next_cursor of the previous page")
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
	channels, next, err := client.ListConversationsPage(ctx, *limit, *cursor)
	if err != nil {
		return err
	}
	out := struct {
		Channels   []agentChannelEntry `json:"channels"`
		NextCursor string              `json:"next_cursor"`
	}{Channels: make([]agentChannelEntry, 0, len(channels)), NextCursor: next}
	for _, ch := range channels {
		e := agentChannelEntry{
			ID: ch.ID, Name: ch.Name,
			IsChannel: ch.IsChannel, IsGroup: ch.IsGroup, IsIM: ch.IsIM,
			IsMpIM: ch.IsMpIM, IsPrivate: ch.IsPrivate,
		}
		if ch.IsIM {
			e.User = ch.User
		}
		out.Channels = append(out.Channels, e)
	}
	return env.writeJSON(out)
}

// ---------------------------------------------------------------------
// user get
// ---------------------------------------------------------------------

type agentUserOutput struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	RealName          string `json:"real_name"`
	DisplayName       string `json:"display_name"`
	Email             string `json:"email"`
	IsBot             bool   `json:"is_bot"`
	Deleted           bool   `json:"deleted"`
	IsRestricted      bool   `json:"is_restricted"`
	IsUltraRestricted bool   `json:"is_ultra_restricted"`
	IsStranger        bool   `json:"is_stranger"`
	TZ                string `json:"tz"`
	Image72           string `json:"image_72"`
}

// decodeAgentUser reads a users.info response. It decodes the raw body
// itself because slack-go's User has no is_stranger.
func decodeAgentUser(raw []byte) (agentUserOutput, error) {
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		User  struct {
			ID                string `json:"id"`
			Name              string `json:"name"`
			RealName          string `json:"real_name"`
			Deleted           bool   `json:"deleted"`
			IsBot             bool   `json:"is_bot"`
			IsRestricted      bool   `json:"is_restricted"`
			IsUltraRestricted bool   `json:"is_ultra_restricted"`
			IsStranger        bool   `json:"is_stranger"`
			TZ                string `json:"tz"`
			Profile           struct {
				RealName    string `json:"real_name"`
				DisplayName string `json:"display_name"`
				Email       string `json:"email"`
				Image72     string `json:"image_72"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return agentUserOutput{}, fmt.Errorf("decoding users.info response: %w", err)
	}
	if !resp.OK {
		code := resp.Error
		if code == "" {
			code = "unknown_error"
		}
		return agentUserOutput{}, fmt.Errorf("users.info: %s", code)
	}
	u := resp.User
	real := u.RealName
	if real == "" {
		real = u.Profile.RealName
	}
	return agentUserOutput{
		ID: u.ID, Name: u.Name, RealName: real,
		DisplayName: u.Profile.DisplayName, Email: u.Profile.Email,
		IsBot: u.IsBot, Deleted: u.Deleted,
		IsRestricted: u.IsRestricted, IsUltraRestricted: u.IsUltraRestricted,
		IsStranger: u.IsStranger, TZ: u.TZ, Image72: u.Profile.Image72,
	}, nil
}

// runAgentUserGet is `slk user get`: a user by ID (users.info), or by
// email or handle (with or without @) found in users.list
// (findAgentUser). Every form prints the same object.
func runAgentUserGet(ctx context.Context, env *agentEnv, args []string) error {
	f := newAgentFlags(env, "user get")
	workspace := f.workspace()
	pos, err := f.parseExact(args, "user")
	if err != nil {
		return err
	}
	who := strings.TrimSpace(pos[0])
	if who == "" || who == "@" {
		return f.usageError("give a user ID (U0123ABC), an email, or a handle")
	}
	client, err := env.openClient(ctx, *workspace, "")
	if err != nil {
		return err
	}
	var raw []byte
	switch {
	case agentUserIDRe.MatchString(who):
		raw, err = client.PostForm(ctx, "users.info", url.Values{"user": {who}})
	default:
		var id string
		if id, err = findAgentUser(ctx, client, who); err != nil {
			return err
		}
		raw, err = client.PostForm(ctx, "users.info", url.Values{"user": {id}})
	}
	if err != nil {
		return err
	}
	u, err := decodeAgentUser(raw)
	if err != nil {
		return err
	}
	return env.writeJSON(u)
}

var agentEmailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// agentUsersPageSize is users.list's page size for a handle lookup.
const agentUsersPageSize = 500

// findAgentUser walks users.list for who, an email or a handle, waiting
// out rate limits as the other list commands do. users.lookupByEmail
// would be one call, but Slack refuses it for a browser session token
// (not_allowed_token_type, 2026-11-13), so an email is matched against
// each profile's email too. For a handle the account name (what Slack
// shows as the handle, e.g. ann.lee) matched case-insensitively wins at
// once; failing that, a unique display name match is used, and several
// are an error naming them, not a guess. A cursor Slack gives a second
// time would loop over the same pages forever: the lookup fails then,
// rather than call a user it did not finish looking for not found.
func findAgentUser(ctx context.Context, client *slackclient.Client, who string) (string, error) {
	email := agentEmailRe.MatchString(who)
	handle := strings.TrimPrefix(who, "@")
	var byDisplay []string
	type page struct {
		users []slackclient.UserListEntry
		next  string
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		p, err := agentRetryRateLimit(ctx, func() (page, error) {
			u, next, err := client.ListUsersPage(ctx, agentUsersPageSize, cursor)
			return page{u, next}, err
		})
		if err != nil {
			return "", err
		}
		for _, m := range p.users {
			if email {
				if strings.EqualFold(m.Email, who) {
					return m.ID, nil
				}
				continue
			}
			if strings.EqualFold(m.Name, handle) {
				return m.ID, nil
			}
			if m.DisplayName != "" && strings.EqualFold(m.DisplayName, handle) {
				byDisplay = append(byDisplay, m.ID)
			}
		}
		if p.next == "" {
			break
		}
		if seen[p.next] {
			return "", fmt.Errorf("users.list gave the cursor %q twice, so the lookup of %q stopped before the end of the list", p.next, who)
		}
		seen[p.next] = true
		cursor = p.next
	}
	switch len(byDisplay) {
	case 1:
		return byDisplay[0], nil
	case 0:
		if email {
			return "", fmt.Errorf("user_not_found: no user with the email %q", who)
		}
		return "", fmt.Errorf("user_not_found: no user with the handle or display name %q", handle)
	}
	return "", fmt.Errorf("%q is the display name of several users (%s); use one of the IDs", handle, strings.Join(byDisplay, ", "))
}
