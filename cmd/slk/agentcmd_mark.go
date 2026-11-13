// This file implements `slk channel mark`: move the user's read
// position in a conversation (conversations.mark) or a thread
// (subscriptions.thread.mark), through slk's own MarkChannel and
// MarkThread. It changes only what he sees, but marked wrong he misses
// messages, so safe mode refuses it like any write. The contract is
// "标记已读" in docs/superpowers/specs/2026-11-12-agent-cli-design.md.

package main

import (
	"context"
	"fmt"
	"strings"

	slackclient "github.com/gammons/slk/internal/slack"
	"github.com/slack-go/slack"
)

type agentMarkResult struct {
	OK        bool   `json:"ok"`
	ChannelID string `json:"channel_id"`
	TS        string `json:"ts"`
	ThreadTS  string `json:"thread_ts,omitempty"`
}

// runAgentChannelMark is `slk channel mark`. The read position moves to
// --ts, else to a link target's message, else to the newest message:
// of the conversation, or, in a thread (--thread-ts, or a link into
// one), its latest reply. A given ts is read back first
// (checkAgentMarkTS), so a ts that names no message there is refused
// instead of moving the read position to an arbitrary point.
func runAgentChannelMark(ctx context.Context, env *agentEnv, args []string) error {
	if err := requireWritable(env); err != nil {
		return err
	}
	f := newAgentFlags(env, "channel mark")
	workspace := f.workspace()
	tsFlag := f.String("ts", "", "mark read up to the message with this `ts` (default: the newest)")
	threadFlag := f.String("thread-ts", "", "mark the thread whose root has this `ts` instead of the conversation")
	pos, err := f.parseExact(args, "target")
	if err != nil {
		return err
	}
	for name, v := range map[string]string{"ts": *tsFlag, "thread-ts": *threadFlag} {
		if err := checkAgentTS(f, name, v); err != nil {
			return err
		}
	}
	client, target, err := openAgentTarget(ctx, env, *workspace, pos[0])
	if err != nil {
		return err
	}
	thread := *threadFlag
	if thread == "" {
		thread = target.ThreadTS
	}
	ts := agentMessageTS(*tsFlag, target)

	if ts != "" {
		if err := checkAgentMarkTS(ctx, client, target.ChannelID, thread, ts); err != nil {
			return err
		}
	}

	if thread == "" {
		if ts == "" {
			msgs, err := agentRetryRateLimit(ctx, func() ([]slack.Message, error) {
				return client.ReadHistory(ctx, target.ChannelID, "", "", false, 1)
			})
			if err != nil {
				return err
			}
			if len(msgs) == 0 {
				return fmt.Errorf("%s has no messages to mark read", target.ChannelID)
			}
			ts = msgs[0].Timestamp
		}
		if err := client.MarkChannel(ctx, target.ChannelID, ts); err != nil {
			return err
		}
		return env.writeJSON(agentMarkResult{OK: true, ChannelID: target.ChannelID, TS: ts})
	}

	if ts == "" {
		if ts, err = agentLatestReply(ctx, client, target.ChannelID, thread); err != nil {
			return err
		}
	}
	if err := client.MarkThread(ctx, target.ChannelID, thread, ts); err != nil {
		return err
	}
	return env.writeJSON(agentMarkResult{OK: true, ChannelID: target.ChannelID, TS: ts, ThreadTS: thread})
}

// checkAgentMarkTS checks that ts names a message where it is to be
// marked read: in the thread rooted at thread (ReadThreadMessage), or,
// with no thread, in the conversation's own history
// (conversations.history, oldest = latest = ts, inclusive, limit 1).
// Not there is message_not_found. A conversation is not marked up to a
// reply that lives only in a thread (thread_ts is another message's,
// and it was not also sent to the channel): that ts is the thread's
// read position, which --thread-ts marks.
func checkAgentMarkTS(ctx context.Context, client *slackclient.Client, channelID, thread, ts string) error {
	if thread != "" {
		m, err := agentRetryRateLimit(ctx, func() (*slack.Message, error) {
			return client.ReadThreadMessage(ctx, channelID, thread, ts)
		})
		if err != nil {
			if strings.Contains(err.Error(), "thread_not_found") {
				return fmt.Errorf("message_not_found: %s is not in thread %s of %s (%w)", ts, thread, channelID, err)
			}
			return err
		}
		if m == nil {
			return fmt.Errorf("message_not_found: %s is not in thread %s of %s", ts, thread, channelID)
		}
		return nil
	}
	msgs, err := agentRetryRateLimit(ctx, func() ([]slack.Message, error) {
		return client.ReadHistory(ctx, channelID, ts, ts, true, 1)
	})
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if m.Timestamp != ts {
			continue
		}
		if m.ThreadTimestamp != "" && m.ThreadTimestamp != ts && m.SubType != slack.MsgSubTypeThreadBroadcast {
			return fmt.Errorf("%s is a reply in thread %s, not a message of %s; mark the thread with --thread-ts %s (or a link carrying thread_ts)", ts, m.ThreadTimestamp, channelID, m.ThreadTimestamp)
		}
		return nil
	}
	return fmt.Errorf("message_not_found: no message %s in %s (a reply in a thread is marked with --thread-ts)", ts, channelID)
}

// agentLatestReply is the ts of the newest reply in the thread rooted
// at root, read from the root's latest_reply (one request however long
// the thread), or root itself when it has no replies.
func agentLatestReply(ctx context.Context, client *slackclient.Client, channelID, root string) (string, error) {
	m, err := agentRetryRateLimit(ctx, func() (*slack.Message, error) {
		return client.ReadThreadMessage(ctx, channelID, root, root)
	})
	if err != nil {
		return "", err
	}
	if m == nil {
		return "", fmt.Errorf("thread_not_found: no thread at %s in %s", root, channelID)
	}
	if m.LatestReply != "" {
		return m.LatestReply, nil
	}
	return root, nil
}
