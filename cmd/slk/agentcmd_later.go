// This file implements `slk later`: Slack's Later list ("saved for
// later"), through the web client's saved.* endpoints
// (internal/slack/agentcmd_saved.go). `later list` reads; the other
// subcommands change his to-do list, so safe mode refuses them. The
// contract is "Later" in
// docs/superpowers/specs/2026-11-12-agent-cli-design.md; behaviour
// follows agent-slack 0.10.2's later command unless noted.

package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	slackclient "github.com/gammons/slk/internal/slack"
	"github.com/slack-go/slack"
)

// ---------------------------------------------------------------------
// later list
// ---------------------------------------------------------------------

type agentLaterCounts struct {
	InProgress int `json:"in_progress"`
	Archived   int `json:"archived"`
	Completed  int `json:"completed"`
	Total      int `json:"total"`
}

type agentLaterItem struct {
	ChannelID     string        `json:"channel_id"`
	ChannelName   string        `json:"channel_name,omitempty"`
	TS            string        `json:"ts"`
	State         string        `json:"state"`
	DateSaved     int64         `json:"date_saved"`
	DateCompleted int64         `json:"date_completed,omitempty"`
	DateDue       int64         `json:"date_due,omitempty"`
	Message       *agentMessage `json:"message,omitempty"`
}

// agentLaterListOutput has no next_cursor: the command reads pages until
// --limit items match the state, so Slack's cursor for the last page read
// would not resume where the output stopped.
type agentLaterListOutput struct {
	Counts agentLaterCounts `json:"counts"`
	Items  []agentLaterItem `json:"items"`
}

const (
	// agentSavedPageSize is the saved.list page agent-slack asks for.
	agentSavedPageSize = 50
	// agentSavedPages bounds how many saved.list pages one command reads.
	agentSavedPages = 40
	// agentLaterInFlight bounds the requests `later list` has in flight
	// while it reads each item's conversation and message: the bound
	// unreads uses, for the same reason (agentUnreadsInFlight).
	agentLaterInFlight = agentUnreadsInFlight
)

// agentLaterStates are the values --state takes.
var agentLaterStates = map[string]bool{"in_progress": true, "archived": true, "completed": true, "all": true}

// runAgentLaterList is `slk later list`, after agent-slack's: saved.list
// page by page (50 a page) until --limit messages in --state are found;
// items that are not messages are left out. Then, all at once (at most
// agentLaterInFlight requests in flight), each conversation's name, by
// the rule unreads names them (describeAgentUnread), and each message,
// fetched by its own ts through conversations.replies
// (ReadThreadMessage), which finds a thread reply as well as a
// top-level message; agent-slack reads conversations.history and so
// misses saved replies. A name or message that cannot be read is left
// out with a line on stderr; only a failed sign-in fails the command.
func runAgentLaterList(ctx context.Context, env *agentEnv, args []string) error {
	f := newAgentFlags(env, "later list")
	workspace := f.workspace()
	state := f.String("state", "in_progress", "list the items in this `state`: in_progress, archived, completed or all")
	limit := f.Int("limit", 20, "return at most `N` items")
	maxBody := f.maxBodyChars()
	countsOnly := f.Bool("counts-only", false, "print only the counts per state")
	if _, err := f.parseExact(args); err != nil {
		return err
	}
	if !agentLaterStates[*state] {
		return f.usageError("--state %q is not one of in_progress, archived, completed, all", *state)
	}
	if *limit < 1 {
		return f.usageError("--limit must be at least 1")
	}
	client, err := env.openClient(ctx, *workspace, "")
	if err != nil {
		return err
	}

	type page struct {
		items  []slackclient.SavedItem
		counts slackclient.SavedCounts
		next   string
	}
	// saved.list without a filter has the in-progress and archived items;
	// completed ones need filter=completed (ListSavedPage). An item is
	// listed once even if both lists carried it.
	filters := []string{""}
	switch *state {
	case "completed":
		filters = []string{"completed"}
	case "all":
		filters = []string{"", "completed"}
	}
	var (
		items  []slackclient.SavedItem
		counts slackclient.SavedCounts
		first  = true
		seen   = map[string]bool{}
	)
	for _, filter := range filters {
		cursor := ""
		for range agentSavedPages {
			p, err := agentRetryRateLimit(ctx, func() (page, error) {
				it, c, next, err := client.ListSavedPage(ctx, filter, agentSavedPageSize, cursor)
				return page{it, c, next}, err
			})
			if err != nil {
				return err
			}
			if first {
				counts, first = p.counts, false
			}
			if *countsOnly {
				break
			}
			for _, it := range p.items {
				if it.State == "" {
					it.State = "in_progress"
				}
				key := it.ItemID + " " + it.TS
				if it.ItemType == "message" && (*state == "all" || it.State == *state) && !seen[key] {
					seen[key] = true
					items = append(items, it)
				}
			}
			prev := cursor
			cursor = p.next
			if len(items) >= *limit || p.next == "" || p.next == prev {
				break
			}
		}
		if *countsOnly || len(items) >= *limit {
			break
		}
	}
	outCounts := agentLaterCounts{InProgress: counts.InProgress, Archived: counts.Archived, Completed: counts.Completed, Total: counts.Total}
	if *countsOnly {
		return env.writeJSON(struct {
			Counts agentLaterCounts `json:"counts"`
		}{outCounts})
	}
	if len(items) > *limit {
		items = items[:*limit]
	}

	opts := agentMessageDefaults()
	opts.MaxBodyChars = *maxBody
	out, err := describeAgentLaterItems(ctx, env, client, items, opts)
	if err != nil {
		return err
	}
	return env.writeJSON(agentLaterListOutput{Counts: outCounts, Items: out})
}

// describeAgentLaterItems builds the output items: each conversation's
// name, asked once however many items it holds, and each message.
func describeAgentLaterItems(ctx context.Context, env *agentEnv, client *slackclient.Client, items []slackclient.SavedItem, opts agentMessageOptions) ([]agentLaterItem, error) {
	var channels []string
	nameAt := map[string]int{}
	for _, it := range items {
		if _, seen := nameAt[it.ItemID]; !seen {
			nameAt[it.ItemID] = len(channels)
			channels = append(channels, it.ItemID)
		}
	}
	lim := make(chan struct{}, agentLaterInFlight)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		authErr error
		// Each goroutine writes only its own index of these.
		names = make([]string, len(channels))
		msgs  = make([]*slack.Message, len(items))
	)
	// skip records a lookup that failed: a failed sign-in fails the
	// command, anything else only loses that field.
	skip := func(what string, err error) {
		mu.Lock()
		defer mu.Unlock()
		if agentExitCode(err) == 2 || errors.Is(err, context.Canceled) {
			if authErr == nil {
				authErr = err
			}
			return
		}
		fmt.Fprintf(env.stderr, "slk later list: no %s: %v\n", what, err)
	}
	for j, ch := range channels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name, _, err := describeAgentUnread(ctx, client, lim, ch)
			if err != nil {
				skip("name for "+ch, err)
				return
			}
			names[j] = name
		}()
	}
	for i, it := range items {
		if it.TS == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := agentLimited(ctx, lim, func() (*slack.Message, error) {
				return client.ReadThreadMessage(ctx, it.ItemID, it.TS, it.TS)
			})
			if err == nil && m == nil {
				err = fmt.Errorf("message_not_found: %s is gone", it.TS)
			}
			if err != nil {
				skip("message for "+it.ItemID+" "+it.TS, err)
				return
			}
			msgs[i] = m
		}()
	}
	wg.Wait()
	if authErr != nil {
		return nil, authErr
	}

	out := make([]agentLaterItem, 0, len(items))
	for i, it := range items {
		o := agentLaterItem{
			ChannelID:     it.ItemID,
			ChannelName:   names[nameAt[it.ItemID]],
			TS:            it.TS,
			State:         it.State,
			DateSaved:     it.DateCreated,
			DateCompleted: it.DateCompleted,
			DateDue:       it.DateDue,
		}
		if msgs[i] != nil {
			m := encodeAgentMessage(*msgs[i], it.ItemID, opts)
			o.Message = &m
		}
		out = append(out, o)
	}
	return out, nil
}

// ---------------------------------------------------------------------
// later save|complete|archive|reopen|remove|remind
// ---------------------------------------------------------------------

type agentLaterResult struct {
	OK        bool   `json:"ok"`
	ChannelID string `json:"channel_id"`
	TS        string `json:"ts"`
	DateDue   int64  `json:"date_due,omitempty"`
}

// laterWrite is the part every later write shares: safe mode first,
// then <target> [--ts TS] resolved to the message, then act. extra
// registers the command's own flags and checks them before any
// request; act does the write and may set the result's date_due.
func laterWrite(ctx context.Context, env *agentEnv, args []string, path string,
	extra func(f *agentFlags) func() error,
	act func(client *slackclient.Client, channelID, ts string, res *agentLaterResult) error,
) error {
	if err := requireWritable(env); err != nil {
		return err
	}
	f := newAgentFlags(env, path)
	workspace := f.workspace()
	tsFlag := f.String("ts", "", "the saved message's `ts` (taken from a message link target)")
	check := func() error { return nil }
	if extra != nil {
		check = extra(f)
	}
	pos, err := f.parseExact(args, "target")
	if err != nil {
		return err
	}
	if err := checkAgentTS(f, "ts", *tsFlag); err != nil {
		return err
	}
	if err := requireMessageTS(f, *tsFlag, pos[0]); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	client, target, err := openAgentTarget(ctx, env, *workspace, pos[0])
	if err != nil {
		return err
	}
	res := agentLaterResult{OK: true, ChannelID: target.ChannelID, TS: agentMessageTS(*tsFlag, target)}
	if err := act(client, res.ChannelID, res.TS, &res); err != nil {
		return err
	}
	return env.writeJSON(res)
}

func runAgentLaterSave(ctx context.Context, env *agentEnv, args []string) error {
	return laterWrite(ctx, env, args, "later save", nil, func(c *slackclient.Client, ch, ts string, _ *agentLaterResult) error {
		return c.AddSaved(ctx, ch, ts)
	})
}

func runAgentLaterRemove(ctx context.Context, env *agentEnv, args []string) error {
	return laterWrite(ctx, env, args, "later remove", nil, func(c *slackclient.Client, ch, ts string, _ *agentLaterResult) error {
		return c.DeleteSaved(ctx, ch, ts)
	})
}

func runAgentLaterComplete(ctx context.Context, env *agentEnv, args []string) error {
	return laterWrite(ctx, env, args, "later complete", nil, func(c *slackclient.Client, ch, ts string, _ *agentLaterResult) error {
		return c.MarkSaved(ctx, ch, ts, slackclient.SavedMarkCompleted)
	})
}

func runAgentLaterArchive(ctx context.Context, env *agentEnv, args []string) error {
	return laterWrite(ctx, env, args, "later archive", nil, func(c *slackclient.Client, ch, ts string, _ *agentLaterResult) error {
		return c.MarkSaved(ctx, ch, ts, slackclient.SavedMarkArchived)
	})
}

// runAgentLaterReopen is `slk later reopen`: back to in progress from
// completed or archived. It cannot tell which without reading the whole
// list, so it sends both marks, uncompleted then unarchived, as
// agent-slack does (its author checked reopening from both states in
// the Slack UI). Slack refusing one of them is expected and is still
// success; both refused, a failed sign-in, or a request that may not
// have reached Slack (network, HTTP error) is a failure.
func runAgentLaterReopen(ctx context.Context, env *agentEnv, args []string) error {
	return laterWrite(ctx, env, args, "later reopen", nil, func(c *slackclient.Client, ch, ts string, _ *agentLaterResult) error {
		var errs []error
		for _, mark := range []string{slackclient.SavedMarkUncompleted, slackclient.SavedMarkUnarchived} {
			err := c.MarkSaved(ctx, ch, ts, mark)
			if err == nil {
				continue
			}
			var refusal slack.SlackErrorResponse
			if agentExitCode(err) == 2 || !errors.As(err, &refusal) {
				return err
			}
			errs = append(errs, err)
		}
		if len(errs) == 2 {
			return agentWorseError(errs[0], errs[1])
		}
		return nil
	})
}

// runAgentLaterRemind is `slk later remind`: saved.update with date_due,
// from --in (relative) or --at (absolute), exactly one of them.
func runAgentLaterRemind(ctx context.Context, env *agentEnv, args []string) error {
	var in, at *string
	var due int64
	extra := func(f *agentFlags) func() error {
		in = f.String("in", "", "remind after this long: `N`m, Nh or Nd")
		at = f.String("at", "", "remind at this `time`: RFC 3339 (2026-11-14T09:00:00+08:00) or Unix seconds")
		return func() error {
			var err error
			if due, err = parseLaterDue(*in, *at, time.Now()); err != nil {
				return f.usageError("%v", err)
			}
			return nil
		}
	}
	return laterWrite(ctx, env, args, "later remind", extra, func(c *slackclient.Client, ch, ts string, res *agentLaterResult) error {
		if err := c.SetSavedDue(ctx, ch, ts, due); err != nil {
			return err
		}
		res.DateDue = due
		return nil
	})
}

var agentLaterInRe = regexp.MustCompile(`^([0-9]+)([mhd])$`)

// agentLaterMaxIn is the longest --in: ten years, far past any real
// reminder and far short of overflowing a time.Duration.
const agentLaterMaxIn = 10 * 366 * 24 * time.Hour

// parseLaterDue turns --in or --at (exactly one) into Unix seconds.
// --in is a whole number of minutes, hours or days after now; --at is
// RFC 3339 or Unix seconds.
func parseLaterDue(in, at string, now time.Time) (int64, error) {
	switch {
	case in == "" && at == "":
		return 0, errors.New("pass --in or --at")
	case in != "" && at != "":
		return 0, errors.New("pass --in or --at, not both")
	case in != "":
		m := agentLaterInRe.FindStringSubmatch(in)
		if m == nil {
			return 0, fmt.Errorf("--in %q is not like 30m, 2h or 3d", in)
		}
		unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || n <= 0 || n > int64(agentLaterMaxIn/unit) {
			return 0, fmt.Errorf("--in %q must be more than zero and at most %d days", in, int64(agentLaterMaxIn/(24*time.Hour)))
		}
		return now.Add(time.Duration(n) * unit).Unix(), nil
	}
	if secs, err := strconv.ParseInt(at, 10, 64); err == nil {
		if secs <= 0 {
			return 0, fmt.Errorf("--at %q is not a time", at)
		}
		return secs, nil
	}
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return 0, fmt.Errorf("--at %q is neither RFC 3339 (2026-11-14T09:00:00+08:00) nor Unix seconds", at)
	}
	return t.Unix(), nil
}
