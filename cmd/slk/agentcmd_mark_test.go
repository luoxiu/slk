package main

import (
	"context"
	"net/url"
	"testing"
)

func markFake(t *testing.T, override map[string]func(url.Values) string) *fakeSlack {
	handlers := map[string]func(url.Values) string{
		"/api/conversations.mark":        readFixed(`{"ok":true}`),
		"/api/subscriptions.thread.mark": readFixed(`{"ok":true}`),
		// Asked for one ts (oldest = latest = ts), history answers with a
		// top-level message at it; asked for none, with the newest.
		"/api/conversations.history": func(form url.Values) string {
			if ts := form.Get("latest"); ts != "" {
				return `{"ok":true,"has_more":false,"messages":[{"type":"message","user":"U1","text":"that one","ts":"` + ts + `"}]}`
			}
			return `{"ok":true,"has_more":true,"messages":[{"type":"message","user":"U1","text":"newest","ts":"1700000099.000100"}]}`
		},
		// The thread rooted at ts: its root, and asked for one later ts,
		// a reply at it.
		"/api/conversations.replies": func(form url.Values) string {
			root := form.Get("ts")
			msgs := `{"type":"message","user":"U1","text":"root","ts":"` + root + `","thread_ts":"` + root + `",` +
				`"reply_count":4,"latest_reply":"1700000080.000700"}`
			if ts := form.Get("latest"); ts != "" && ts != root {
				msgs += `,{"type":"message","user":"U1","text":"reply","ts":"` + ts + `","thread_ts":"` + root + `"}`
			}
			return `{"ok":true,"has_more":false,"messages":[` + msgs + `]}`
		},
		"/api/users.conversations": readFixed(`{"ok":true,"channels":[{"id":"C123","name":"general","is_channel":true}],` +
			`"response_metadata":{"next_cursor":""}}`),
	}
	for k, v := range override {
		handlers[k] = v
	}
	return newFormFake(t, handlers)
}

func TestChannelMarkConversation(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		wantTS string
	}{
		{"channel ID, --ts after", []string{"C123", "--ts", "1700000009.000900"}, "1700000009.000900"},
		{"#name, --ts first", []string{"--ts", "1700000009.000900", "#general"}, "1700000009.000900"},
		{"link: up to the linked message", []string{writeMsgLink}, "1700000001.000200"},
		{"--ts wins over the link", []string{writeMsgLink, "--ts", "1700000009.000900"}, "1700000009.000900"},
		{"no ts: up to the newest message", []string{"C123"}, "1700000099.000100"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := markFake(t, nil)
			code, out, errOut := runRead(t, srv, append([]string{"channel", "mark"}, tc.args...)...)
			wantOut(t, code, out, errOut, `{"ok":true,"channel_id":"C123","ts":"`+tc.wantTS+`"}`)
			form := srv.requestTo(t, "/api/conversations.mark").form
			if form.Get("channel") != "C123" || form.Get("ts") != tc.wantTS {
				t.Errorf("conversations.mark form = %v", form)
			}
			if n := srv.count("/api/subscriptions.thread.mark"); n != 0 {
				t.Errorf("%d thread marks, want none", n)
			}
			// One history read: the newest message, or the given one to
			// check it is there (oldest = latest = ts, inclusive).
			if n := srv.count("/api/conversations.history"); n != 1 {
				t.Errorf("%d history calls, want 1", n)
			}
			h := srv.requestTo(t, "/api/conversations.history").form
			wantRange := [2]string{tc.wantTS, tc.wantTS}
			if tc.wantTS == "1700000099.000100" {
				wantRange = [2]string{"", ""}
			}
			if h.Get("limit") != "1" || h.Get("oldest") != wantRange[0] || h.Get("latest") != wantRange[1] ||
				(wantRange[0] != "" && h.Get("inclusive") != "1") {
				t.Errorf("history form = %v, want limit 1, oldest = latest = %q", h, wantRange[0])
			}
		})
	}
}

func TestChannelMarkThread(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantTS   string
		wantRoot string
	}{
		{"link into a thread: up to the linked reply", []string{writeThreadLink}, "1700000005.000300", "1700000001.000200"},
		{"--thread-ts and --ts", []string{"C123", "--thread-ts", "1700000001.000200", "--ts", "1700000005.000300"}, "1700000005.000300", "1700000001.000200"},
		{"--thread-ts alone: up to the latest reply", []string{"--thread-ts", "1700000001.000200", "C123"}, "1700000080.000700", "1700000001.000200"},
		{"--thread-ts and its root as --ts", []string{"C123", "--thread-ts", "1700000001.000200", "--ts", "1700000001.000200"}, "1700000001.000200", "1700000001.000200"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := markFake(t, nil)
			code, out, errOut := runRead(t, srv, append([]string{"channel", "mark"}, tc.args...)...)
			wantOut(t, code, out, errOut, `{"ok":true,"channel_id":"C123","ts":"`+tc.wantTS+`","thread_ts":"`+tc.wantRoot+`"}`)
			form := srv.requestTo(t, "/api/subscriptions.thread.mark").form
			if form.Get("channel") != "C123" || form.Get("thread_ts") != tc.wantRoot || form.Get("ts") != tc.wantTS || form.Get("read") != "1" {
				t.Errorf("subscriptions.thread.mark form = %v", form)
			}
			if n := srv.count("/api/conversations.mark"); n != 0 {
				t.Errorf("%d conversation marks, want none", n)
			}
			// One replies read: the root's latest_reply, or the given
			// message, to check it is in the thread.
			if n := srv.count("/api/conversations.replies"); n != 1 {
				t.Errorf("%d replies calls, want 1", n)
			}
			r := srv.requestTo(t, "/api/conversations.replies").form
			wantOldest := tc.wantTS
			if tc.wantTS == "1700000080.000700" {
				wantOldest = tc.wantRoot // the root, for its latest_reply
			}
			if r.Get("ts") != tc.wantRoot || r.Get("oldest") != wantOldest || r.Get("latest") != wantOldest {
				t.Errorf("replies form = %v, want ts %s, oldest = latest = %s", r, tc.wantRoot, wantOldest)
			}
		})
	}
}

// A thread with no replies is marked up to its root.
func TestChannelMarkThreadWithoutReplies(t *testing.T) {
	srv := markFake(t, map[string]func(url.Values) string{
		"/api/conversations.replies": readFixed(`{"ok":true,"has_more":false,"messages":[` +
			`{"type":"message","user":"U1","text":"root","ts":"1700000001.000200"}]}`),
	})
	code, out, errOut := runRead(t, srv, "channel", "mark", "C123", "--thread-ts", "1700000001.000200")
	wantOut(t, code, out, errOut, `{"ok":true,"channel_id":"C123","ts":"1700000001.000200","thread_ts":"1700000001.000200"}`)
}

// A given ts is checked before anything is marked: a ts that names no
// message (a typo, a message from another conversation) is refused with
// message_not_found, and so is marking a conversation up to a reply
// that lives only in a thread (that is the thread's read position, not
// the conversation's). A reply also sent to the channel is in the
// conversation, so it may be.
func TestChannelMarkChecksTheMessage(t *testing.T) {
	const reply = "1700000005.000300"
	historyWith := func(msg string) func(url.Values) string {
		return readFixed(`{"ok":true,"has_more":false,"messages":[` + msg + `]}`)
	}
	cases := []struct {
		name     string
		override map[string]func(url.Values) string
		args     []string
		want     string // "" when the mark goes ahead
	}{
		{"no message at the ts", map[string]func(url.Values) string{
			"/api/conversations.history": historyWith(""),
		}, []string{"C123", "--ts", "1700000009.000900"}, "message_not_found"},
		{"history answers with another message", map[string]func(url.Values) string{
			"/api/conversations.history": historyWith(`{"type":"message","user":"U1","text":"x","ts":"1700000008.000100"}`),
		}, []string{"C123", "--ts", "1700000009.000900"}, "message_not_found"},
		{"a thread reply marks the conversation", map[string]func(url.Values) string{
			"/api/conversations.history": historyWith(`{"type":"message","user":"U1","text":"r","ts":"` + reply + `","thread_ts":"1700000001.000200"}`),
		}, []string{"C123", "--ts", reply}, "--thread-ts"},
		{"a reply also sent to the channel", map[string]func(url.Values) string{
			"/api/conversations.history": historyWith(`{"type":"message","subtype":"thread_broadcast","user":"U1","text":"r","ts":"` + reply + `","thread_ts":"1700000001.000200"}`),
		}, []string{"C123", "--ts", reply}, ""},
		{"a thread root", map[string]func(url.Values) string{
			"/api/conversations.history": historyWith(`{"type":"message","user":"U1","text":"r","ts":"1700000001.000200","thread_ts":"1700000001.000200","reply_count":2}`),
		}, []string{"C123", "--ts", "1700000001.000200"}, ""},
		{"no such message in the thread", map[string]func(url.Values) string{
			"/api/conversations.replies": readFixed(`{"ok":true,"has_more":false,"messages":[` +
				`{"type":"message","user":"U1","text":"root","ts":"1700000001.000200","thread_ts":"1700000001.000200"}]}`),
		}, []string{writeThreadLink}, "message_not_found"},
		{"no such thread", map[string]func(url.Values) string{
			"/api/conversations.replies": readFixed(`{"ok":false,"error":"thread_not_found"}`),
		}, []string{"C123", "--thread-ts", "1700000001.000200", "--ts", reply}, "message_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := markFake(t, tc.override)
			code, out, errOut := runRead(t, srv, append([]string{"channel", "mark"}, tc.args...)...)
			marks := srv.count("/api/conversations.mark") + srv.count("/api/subscriptions.thread.mark")
			if tc.want == "" {
				if code != 0 || marks != 1 {
					t.Errorf("exit %d, %d marks, stderr %s; want the mark made", code, marks, errOut)
				}
				return
			}
			wantFail(t, code, out, errOut, 1, tc.want)
			if marks != 0 {
				t.Errorf("%d marks made; want none", marks)
			}
		})
	}
}

func TestChannelMarkErrors(t *testing.T) {
	cases := []struct {
		name     string
		override map[string]func(url.Values) string
		args     []string
		wantCode int
		want     string
	}{
		{"Slack refuses the mark", map[string]func(url.Values) string{
			"/api/conversations.mark": readFixed(`{"ok":false,"error":"channel_not_found"}`),
		}, []string{"C999", "--ts", "1700000009.000900"}, 1, "channel_not_found"},
		{"auth", map[string]func(url.Values) string{
			"/api/subscriptions.thread.mark": readFixed(`{"ok":false,"error":"invalid_auth"}`),
		}, []string{writeThreadLink}, 2, "invalid_auth"},
		{"empty conversation", map[string]func(url.Values) string{
			"/api/conversations.history": readFixed(`{"ok":true,"has_more":false,"messages":[]}`),
		}, []string{"C123"}, 1, "no messages"},
		{"no such thread", map[string]func(url.Values) string{
			"/api/conversations.replies": readFixed(`{"ok":false,"error":"thread_not_found"}`),
		}, []string{"C123", "--thread-ts", "1700000001.000200"}, 1, "thread_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := markFake(t, tc.override)
			code, out, errOut := runRead(t, srv, append([]string{"channel", "mark"}, tc.args...)...)
			wantFail(t, code, out, errOut, tc.wantCode, tc.want)
		})
	}
}

func TestChannelMarkUsageErrorsBeforeConnecting(t *testing.T) {
	for _, args := range [][]string{
		{"channel", "mark"},
		{"channel", "mark", "C123", "C456"},
		{"channel", "mark", "C123", "--ts", "now"},
		{"channel", "mark", "C123", "--thread-ts", "x"},
		{"channel", "mark", "https://example.com/x"},
	} {
		env, stdout, stderr := agentTestEnv(nil, noOpener(t))
		code := runAgent(context.Background(), env, args)
		if code != 1 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Errorf("%q: exit %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
}
