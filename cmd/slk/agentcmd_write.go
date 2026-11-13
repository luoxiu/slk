// This file implements the agent commands that write to Slack: message
// send, message react add/remove, message edit and message delete. A
// send with --attach is an upload, in agentcmd_files.go.
// Each starts with requireWritable, before opening a client, so safe
// mode refuses without a request. See
// docs/superpowers/specs/2026-11-12-agent-cli-design.md.
//
// A send goes through slk's SendMessage / SendReply, so its formatting
// is what the UI sends. Those try link chips (chat.unfurlLink plus up to
// linkChipWait for a WebSocket frame) only while the client's socket is
// live (lookupLinkChips checks wsLive); these commands never start one,
// so a link in the text costs no extra request and no wait.

package main

import (
	"context"
	"strings"

	"github.com/gammons/slk/internal/ids"
	"github.com/gammons/slk/internal/slackurl"
)

type agentSendResult struct {
	OK        bool   `json:"ok"`
	ChannelID string `json:"channel_id"`
	TS        string `json:"ts"`
	Permalink string `json:"permalink"`
}

type agentEditResult struct {
	OK        bool   `json:"ok"`
	ChannelID string `json:"channel_id"`
	TS        string `json:"ts"`
}

// agentMessageTS is the ts of the message a react, edit or delete acts
// on: --ts, else the one in a message-link target.
func agentMessageTS(flagTS string, t agentTarget) string {
	if flagTS != "" {
		return flagTS
	}
	return t.TS
}

// requireMessageTS is the usage check for those commands, run before a
// client is opened: a target that is not a link needs --ts.
func requireMessageTS(f *agentFlags, flagTS, rawTarget string) error {
	if flagTS != "" {
		return nil
	}
	if _, _, isLink, _ := parseAgentLink(rawTarget); isLink {
		return nil
	}
	return f.usageError("target %q is not a message link; pass --ts", rawTarget)
}

func runAgentMessageSend(ctx context.Context, env *agentEnv, args []string) error {
	if err := requireWritable(env); err != nil {
		return err
	}
	f := newAgentFlags(env, "message send")
	workspace := f.workspace()
	threadFlag := f.String("thread-ts", "", "reply in the thread whose root has this `ts`")
	var attach agentPathList
	f.Var(&attach, "attach", "upload the file at `path` (repeat for more); <text>, optional then, is the comment on the last file")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(attach) > 0 && len(pos) == 1 {
		pos = append(pos, "")
	}
	if err := f.wantPositional(pos, "target", "text"); err != nil {
		return err
	}
	if len(attach) == 0 && strings.TrimSpace(pos[1]) == "" {
		return f.usageError("<text> is empty")
	}
	if err := checkAgentTS(f, "thread-ts", *threadFlag); err != nil {
		return err
	}
	// Every file is checked and opened before the client, so a bad
	// path stops the command before any request.
	var files []agentAttachment
	if len(attach) > 0 {
		if files, err = openAgentAttachments(attach); err != nil {
			return err
		}
		defer closeAgentAttachments(files)
	}
	client, target, err := openAgentTarget(ctx, env, *workspace, pos[0])
	if err != nil {
		return err
	}
	thread := target.ThreadRoot()
	if *threadFlag != "" {
		thread = *threadFlag
	}
	if len(files) > 0 {
		return uploadAgentFiles(ctx, env, client, target.ChannelID, thread, pos[1], files)
	}
	var ts string
	if thread == "" {
		ts, _, err = client.SendMessage(ctx, target.ChannelID, pos[1])
	} else {
		ts, _, err = client.SendReply(ctx, target.ChannelID, thread, pos[1], false)
	}
	if err != nil {
		return err
	}
	permalink, _ := slackurl.Build(client.TeamURL(), ids.ChannelID(target.ChannelID), ids.MessageTS(ts), ids.ThreadTS(thread))
	return env.writeJSON(agentSendResult{OK: true, ChannelID: target.ChannelID, TS: ts, Permalink: permalink})
}

func runAgentReactAdd(ctx context.Context, env *agentEnv, args []string) error {
	return runAgentReact(ctx, env, args, "message react add", true)
}

func runAgentReactRemove(ctx context.Context, env *agentEnv, args []string) error {
	return runAgentReact(ctx, env, args, "message react remove", false)
}

func runAgentReact(ctx context.Context, env *agentEnv, args []string, path string, add bool) error {
	if err := requireWritable(env); err != nil {
		return err
	}
	f := newAgentFlags(env, path)
	workspace := f.workspace()
	tsFlag := f.String("ts", "", "react to the message with this `ts`")
	pos, err := f.parseExact(args, "target", "emoji")
	if err != nil {
		return err
	}
	name := strings.Trim(strings.TrimSpace(pos[1]), ":")
	if name == "" {
		return f.usageError("<emoji> is empty")
	}
	if err := requireMessageTS(f, *tsFlag, pos[0]); err != nil {
		return err
	}
	client, target, err := openAgentTarget(ctx, env, *workspace, pos[0])
	if err != nil {
		return err
	}
	ts := agentMessageTS(*tsFlag, target)
	if add {
		err = client.AddReaction(ctx, target.ChannelID, ts, name)
	} else {
		err = client.RemoveReaction(ctx, target.ChannelID, ts, name)
	}
	if err != nil {
		return err
	}
	return env.writeJSON(struct {
		OK bool `json:"ok"`
	}{true})
}

func runAgentMessageEdit(ctx context.Context, env *agentEnv, args []string) error {
	if err := requireWritable(env); err != nil {
		return err
	}
	f := newAgentFlags(env, "message edit")
	workspace := f.workspace()
	tsFlag := f.String("ts", "", "edit the message with this `ts`")
	pos, err := f.parseExact(args, "target", "text")
	if err != nil {
		return err
	}
	if strings.TrimSpace(pos[1]) == "" {
		return f.usageError("<text> is empty")
	}
	if err := requireMessageTS(f, *tsFlag, pos[0]); err != nil {
		return err
	}
	client, target, err := openAgentTarget(ctx, env, *workspace, pos[0])
	if err != nil {
		return err
	}
	ts := agentMessageTS(*tsFlag, target)
	if _, err := client.EditMessage(ctx, target.ChannelID, ts, pos[1]); err != nil {
		return err
	}
	return env.writeJSON(agentEditResult{OK: true, ChannelID: target.ChannelID, TS: ts})
}

func runAgentMessageDelete(ctx context.Context, env *agentEnv, args []string) error {
	if err := requireWritable(env); err != nil {
		return err
	}
	f := newAgentFlags(env, "message delete")
	workspace := f.workspace()
	tsFlag := f.String("ts", "", "delete the message with this `ts`")
	pos, err := f.parseExact(args, "target")
	if err != nil {
		return err
	}
	if err := requireMessageTS(f, *tsFlag, pos[0]); err != nil {
		return err
	}
	client, target, err := openAgentTarget(ctx, env, *workspace, pos[0])
	if err != nil {
		return err
	}
	ts := agentMessageTS(*tsFlag, target)
	if err := client.RemoveMessage(ctx, target.ChannelID, ts); err != nil {
		return err
	}
	return env.writeJSON(agentEditResult{OK: true, ChannelID: target.ChannelID, TS: ts})
}
