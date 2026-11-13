// This file implements `slk message list` and `slk message get`; the
// other read-only commands (unreads, search messages, channel list,
// user get) are in agentcmd_read_lists.go. The contract is
// docs/superpowers/specs/2026-11-12-agent-cli-design.md.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"

	slackclient "github.com/gammons/slk/internal/slack"
	"github.com/gammons/slk/internal/slackfmt"
	"github.com/slack-go/slack"
)

const (
	// agentThreadMax is how many messages `message list` reads from one
	// thread before stopping.
	agentThreadMax = 1000
	// agentFetchWorkers bounds the concurrent users.info lookups of
	// --resolve-users. unreads has its own bound, agentUnreadsInFlight.
	agentFetchWorkers = 5
)

var agentTSRe = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,8})?$`)

// checkAgentTS rejects a flag value that is not a Slack timestamp
// before it is sent as a request parameter.
func checkAgentTS(f *agentFlags, flagName, v string) error {
	if v != "" && !agentTSRe.MatchString(v) {
		return f.usageError("--%s %q is not a Slack timestamp (like 1793932809.791579)", flagName, v)
	}
	return nil
}

// agentTSLess orders Slack timestamps ("seconds.micros") numerically
// without floating point: fewer digits first, then lexicographic.
func agentTSLess(a, b string) bool {
	ai, af, _ := strings.Cut(a, ".")
	bi, bf, _ := strings.Cut(b, ".")
	if len(ai) != len(bi) {
		return len(ai) < len(bi)
	}
	if ai != bi {
		return ai < bi
	}
	return af < bf
}

// sortAgentOldestFirst orders messages by ts, oldest first.
func sortAgentOldestFirst(msgs []slack.Message) {
	sort.SliceStable(msgs, func(i, j int) bool {
		return agentTSLess(msgs[i].Timestamp, msgs[j].Timestamp)
	})
}

// agentGoneRe finds the Slack error codes that mean one item (a
// conversation, a user) is legitimately gone or not readable by this
// session, in an error's text. missing_scope is not among them: it is
// a property of the token, not of one item, so skipping it would turn a
// broken token into an empty, successful result.
var agentGoneRe = regexp.MustCompile(`(?:^|[^a-z_])(?:channel_not_found|not_in_channel|is_archived|user_not_found|user_not_visible)(?:$|[^a-z_])`)

// agentItemGone reports whether err says the one item asked about does
// not exist or cannot be read, so a batch command may skip it.
func agentItemGone(err error) bool {
	return err != nil && agentGoneRe.MatchString(err.Error())
}

// agentRetryRateLimit runs fn, waiting out Slack rate limits (honouring
// ctx) and retrying, and returns the first result that is not one.
func agentRetryRateLimit[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	for {
		v, err := fn()
		if err == nil {
			return v, nil
		}
		limited, waitErr := slackclient.WaitOutRateLimit(ctx, err)
		if waitErr != nil {
			return v, waitErr
		}
		if !limited {
			return v, err
		}
	}
}

// agentUserRef is one entry of referenced_users.
type agentUserRef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RealName string `json:"real_name"`
}

type agentMessageListOutput struct {
	Messages []agentMessage `json:"messages"`
	// A pointer so that --resolve-users with nobody to resolve still
	// prints {}, while without the flag the key is absent.
	ReferencedUsers *map[string]agentUserRef `json:"referenced_users,omitempty"`
}

// runAgentMessageList is `slk message list`: a thread (--thread-ts, or
// a link target) through conversations.replies, otherwise a window of
// channel history through conversations.history.
func runAgentMessageList(ctx context.Context, env *agentEnv, args []string) error {
	f := newAgentFlags(env, "message list")
	workspace := f.workspace()
	threadTS := f.String("thread-ts", "", "read the thread whose root or reply has this `ts`")
	oldest := f.String("oldest", "", "only messages after this `ts` (history mode)")
	latest := f.String("latest", "", "only messages before this `ts` (history mode)")
	limit := f.Int("limit", 25, "newest `N` messages to return (history mode)")
	maxBody := f.maxBodyChars()
	resolveUsers := f.Bool("resolve-users", false, "add referenced_users for authors and @-mentioned people")
	includeReactions := f.Bool("include-reactions", false, "add each message's reactions")
	downloadFiles := f.downloadFiles()
	pos, err := f.parseExact(args, "target")
	if err != nil {
		return err
	}
	for name, v := range map[string]string{"thread-ts": *threadTS, "oldest": *oldest, "latest": *latest} {
		if err := checkAgentTS(f, name, v); err != nil {
			return err
		}
	}
	if *limit < 1 {
		return f.usageError("--limit must be at least 1")
	}
	client, target, err := openAgentTarget(ctx, env, *workspace, pos[0])
	if err != nil {
		return err
	}

	root := *threadTS
	if root == "" {
		root = target.ThreadRoot()
	}
	var msgs []slack.Message
	if root != "" {
		msgs, err = client.ReadThread(ctx, target.ChannelID, root, agentThreadMax)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			return fmt.Errorf("thread_not_found: no thread at %s in %s", root, target.ChannelID)
		}
	} else {
		msgs, err = client.ReadHistory(ctx, target.ChannelID, *oldest, *latest, false, *limit)
		if err != nil {
			return err
		}
	}
	sortAgentOldestFirst(msgs)

	opts := agentMessageDefaults()
	opts.MaxBodyChars = *maxBody
	opts.IncludeReactions = *includeReactions
	if *downloadFiles {
		opts.Downloads = downloadAgentFiles(ctx, env, msgs)
	}
	out := agentMessageListOutput{Messages: make([]agentMessage, 0, len(msgs))}
	for _, m := range msgs {
		out.Messages = append(out.Messages, encodeAgentMessage(m, target.ChannelID, opts))
	}
	if *resolveUsers {
		refs, err := resolveAgentUsers(ctx, env.stderr, client, msgs)
		if err != nil {
			return err
		}
		out.ReferencedUsers = &refs
	}
	return env.writeJSON(out)
}

// resolveAgentUsers looks up (users.info, a few at a time) the authors
// of msgs and the people they mention with <@U…>, in their text or in
// the blocks and attachments their content is rendered from (read
// before rendering, which turns <@U…> into @U…). A user Slack
// cannot find (agentItemGone) is left out with a line on stderr; Slack
// rate limits are waited out; any other failure, a failed sign-in
// first, fails the call so no referenced user goes missing silently.
func resolveAgentUsers(ctx context.Context, stderr io.Writer, client *slackclient.Client, msgs []slack.Message) (map[string]agentUserRef, error) {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, m := range msgs {
		add(m.User)
		for _, id := range slackfmt.MentionedUserIDs(m.Text) {
			add(id)
		}
		for _, id := range slackfmt.MentionedUserIDs(agentContentMrkdwn(m)) {
			add(id)
		}
	}
	refs := make(map[string]agentUserRef, len(ids))
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		failErr error
		sem     = make(chan struct{}, agentFetchWorkers)
	)
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			p, err := agentRetryRateLimit(ctx, func() (*slackclient.UserProfile, error) {
				return client.GetUserProfileContext(ctx, id)
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if agentItemGone(err) {
					fmt.Fprintf(stderr, "slk: skipping user %s: %v\n", id, err)
					return
				}
				// An auth failure (exit 2) outranks other failures.
				if failErr == nil || (agentExitCode(err) == 2 && agentExitCode(failErr) != 2) {
					failErr = err
				}
				return
			}
			real := p.RealName
			if real == "" {
				real = p.Profile.RealName
			}
			refs[id] = agentUserRef{ID: id, Name: p.Name, RealName: real}
		}()
	}
	wg.Wait()
	if failErr != nil {
		return nil, failErr
	}
	return refs, nil
}

// runAgentMessageGet is `slk message get`: one message. In a thread
// (--thread-ts, or a link into one) it is fetched from the thread by
// its own ts; otherwise it is the one message conversations.history
// returns for exactly that ts.
func runAgentMessageGet(ctx context.Context, env *agentEnv, args []string) error {
	f := newAgentFlags(env, "message get")
	workspace := f.workspace()
	tsFlag := f.String("ts", "", "the message's `ts` (taken from a message link target)")
	threadFlag := f.String("thread-ts", "", "the `ts` of the thread the message is in")
	maxBody := f.maxBodyChars()
	downloadFiles := f.downloadFiles()
	pos, err := f.parseExact(args, "target")
	if err != nil {
		return err
	}
	for name, v := range map[string]string{"ts": *tsFlag, "thread-ts": *threadFlag} {
		if err := checkAgentTS(f, name, v); err != nil {
			return err
		}
	}
	if *tsFlag == "" && !strings.Contains(pos[0], "://") {
		return f.usageError("needs --ts, or a message link as the target")
	}
	client, target, err := openAgentTarget(ctx, env, *workspace, pos[0])
	if err != nil {
		return err
	}
	ts, threadTS := *tsFlag, *threadFlag
	if ts == "" {
		ts = target.TS
	}
	if threadTS == "" {
		threadTS = target.ThreadTS
	}
	if ts == "" {
		return f.usageError("needs --ts, or a message link as the target")
	}

	var found *slack.Message
	if threadTS != "" && threadTS != ts {
		found, err = client.ReadThreadMessage(ctx, target.ChannelID, threadTS, ts)
		if err != nil {
			if strings.Contains(err.Error(), "thread_not_found") {
				return fmt.Errorf("message_not_found: %s is not in thread %s of %s (%w)", ts, threadTS, target.ChannelID, err)
			}
			return err
		}
	} else {
		msgs, err := client.ReadHistory(ctx, target.ChannelID, ts, ts, true, 1)
		if err != nil {
			return err
		}
		for i := range msgs {
			if msgs[i].Timestamp == ts {
				found = &msgs[i]
				break
			}
		}
	}
	if found == nil {
		return errors.New("message_not_found: no message " + ts + " in " + target.ChannelID)
	}
	opts := agentMessageDefaults()
	opts.MaxBodyChars = *maxBody
	if *downloadFiles {
		opts.Downloads = downloadAgentFiles(ctx, env, []slack.Message{*found})
	}
	return env.writeJSON(struct {
		Message agentMessage `json:"message"`
	}{encodeAgentMessage(*found, target.ChannelID, opts)})
}
