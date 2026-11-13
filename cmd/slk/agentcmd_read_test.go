package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newReadFake is a fake Slack whose answers are computed from the
// request form, for tests that need pagination or per-ID replies.
// auth.test is answered; a path without a handler fails the test.
func newReadFake(t *testing.T, handlers map[string]func(form url.Values) string) *fakeSlack {
	t.Helper()
	f := &fakeSlack{}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.reqs = append(f.reqs, recordedRequest{path: r.URL.Path, form: r.PostForm, query: r.URL.Query(), header: r.Header.Clone()})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/auth.test" {
			_, _ = w.Write([]byte(agentAuthTestOK))
			return
		}
		h, ok := handlers[r.URL.Path]
		if !ok {
			t.Errorf("fake Slack got an unexpected request to %s", r.URL.Path)
			http.Error(w, `{"ok":false,"error":"unexpected_path"}`, http.StatusNotFound)
			return
		}
		body := h(r.PostForm)
		// "429 <seconds>" answers with a rate limit instead of a body.
		if secs, ok := strings.CutPrefix(body, "429 "); ok {
			w.Header().Set("Retry-After", secs)
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.Close)
	return f
}

func readFixed(body string) func(url.Values) string { return func(url.Values) string { return body } }

// requestsTo returns every request made to path, in order.
func (f *fakeSlack) requestsTo(path string) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedRequest
	for _, r := range f.reqs {
		if r.path == path {
			out = append(out, r)
		}
	}
	return out
}

// runRead runs one agent command against srv and returns the exit code
// and what it wrote.
func runRead(t *testing.T, srv *fakeSlack, args ...string) (int, string, string) {
	t.Helper()
	var calls []openCall
	env, stdout, stderr := agentTestEnv(nil, fakeAgentOpener(t, srv, &calls))
	code := runAgent(context.Background(), env, args)
	return code, stdout.String(), stderr.String()
}

func wantOut(t *testing.T, code int, stdout, stderr, want string) {
	t.Helper()
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	if got := strings.TrimSuffix(stdout, "\n"); got != want {
		t.Errorf("stdout\n got  %s\n want %s", got, want)
	}
}

func wantFail(t *testing.T, code int, stdout, stderr string, wantCode int, wantStderr string) {
	t.Helper()
	if code != wantCode {
		t.Errorf("exit %d, want %d (stderr %s)", code, wantCode, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout %q, want empty", stdout)
	}
	if !strings.Contains(stderr, wantStderr) {
		t.Errorf("stderr %q lacks %q", stderr, wantStderr)
	}
}

// ---------------------------------------------------------------------
// message list
// ---------------------------------------------------------------------

const agentReadHistoryBody = `{"ok":true,"has_more":false,"messages":[
 {"type":"message","user":"U2","text":"second <@U3>","ts":"1700000002.000200","reactions":[{"name":"eyes","users":["U1","U3"],"count":2}]},
 {"type":"message","user":"U1","text":"first message","ts":"1700000001.000100","thread_ts":"1700000001.000100","reply_count":2}
]}`

func TestMessageListHistoryOldestFirstFlagsAfterTarget(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.history": readFixed(agentReadHistoryBody)})
	code, out, errOut := runRead(t, srv, "message", "list", "C123", "--limit", "5", "--oldest", "1700000000.000000", "--latest", "1700000009.000000")
	wantOut(t, code, out, errOut, `{"messages":[`+
		`{"ts":"1700000001.000100","thread_ts":"1700000001.000100","reply_count":2,"channel_id":"C123","author":{"user_id":"U1"},"content":"first message"},`+
		`{"ts":"1700000002.000200","channel_id":"C123","author":{"user_id":"U2"},"content":"second @U3"}]}`)
	form := srv.requestTo(t, "/api/conversations.history").form
	if form.Get("channel") != "C123" || form.Get("limit") != "5" || form.Get("oldest") != "1700000000.000000" || form.Get("latest") != "1700000009.000000" {
		t.Errorf("history form = %v", form)
	}
	if form.Get("inclusive") == "1" || form.Get("inclusive") == "true" {
		t.Errorf("bounds must be exclusive, form = %v", form)
	}
}

func TestMessageListDefaultLimit25(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.history": readFixed(`{"ok":true,"messages":[]}`)})
	code, out, errOut := runRead(t, srv, "message", "list", "--workspace", "acme", "C123")
	wantOut(t, code, out, errOut, `{"messages":[]}`)
	if got := srv.requestTo(t, "/api/conversations.history").form.Get("limit"); got != "25" {
		t.Errorf("limit = %q, want 25", got)
	}
}

func TestMessageListByChannelName(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/users.conversations":   readFixed(`{"ok":true,"channels":[{"id":"C9","name":"general","is_channel":true}],"response_metadata":{"next_cursor":""}}`),
		"/api/conversations.history": readFixed(agentReadHistoryBody),
	})
	code, _, errOut := runRead(t, srv, "message", "list", "#general")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got := srv.requestTo(t, "/api/conversations.history").form.Get("channel"); got != "C9" {
		t.Errorf("channel = %q, want C9", got)
	}
}

func replyPages(t *testing.T) func(url.Values) string {
	return func(form url.Values) string {
		if form.Get("cursor") == "" {
			return `{"ok":true,"has_more":true,"messages":[
			 {"type":"message","user":"U1","text":"root","ts":"1700000001.000100","thread_ts":"1700000001.000100","reply_count":2},
			 {"type":"message","user":"U2","text":"reply 1","ts":"1700000002.000100","thread_ts":"1700000001.000100"}],
			 "response_metadata":{"next_cursor":"PAGE2"}}`
		}
		if form.Get("cursor") != "PAGE2" {
			t.Errorf("cursor = %q", form.Get("cursor"))
		}
		return `{"ok":true,"has_more":false,"messages":[
		 {"type":"message","user":"U1","text":"reply 2","ts":"1700000003.000100","thread_ts":"1700000001.000100"}]}`
	}
}

func TestMessageListThreadFollowsCursor(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.replies": replyPages(t)})
	code, out, errOut := runRead(t, srv, "message", "list", "C123", "--thread-ts", "1700000001.000100")
	wantOut(t, code, out, errOut, `{"messages":[`+
		`{"ts":"1700000001.000100","thread_ts":"1700000001.000100","reply_count":2,"channel_id":"C123","author":{"user_id":"U1"},"content":"root"},`+
		`{"ts":"1700000002.000100","thread_ts":"1700000001.000100","channel_id":"C123","author":{"user_id":"U2"},"content":"reply 1"},`+
		`{"ts":"1700000003.000100","thread_ts":"1700000001.000100","channel_id":"C123","author":{"user_id":"U1"},"content":"reply 2"}]}`)
	reqs := srv.requestsTo("/api/conversations.replies")
	if len(reqs) != 2 {
		t.Fatalf("%d replies requests, want 2", len(reqs))
	}
	if reqs[0].form.Get("channel") != "C123" || reqs[0].form.Get("ts") != "1700000001.000100" {
		t.Errorf("first form = %v", reqs[0].form)
	}
	if len(srv.requestsTo("/api/conversations.history")) != 0 {
		t.Error("thread mode must not read history")
	}
}

func TestMessageListLinkTargets(t *testing.T) {
	for name, tc := range map[string]struct{ link, wantTS string }{
		"reply link uses its thread_ts": {"https://acme.slack.com/archives/C123/p1700000002000100?thread_ts=1700000001.000100&cid=C123", "1700000001.000100"},
		"plain link uses its own ts":    {"https://acme.slack.com/archives/C123/p1700000001000100", "1700000001.000100"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.replies": replyPages(t)})
			var calls []openCall
			env, _, stderr := agentTestEnv(nil, fakeAgentOpener(t, srv, &calls))
			if code := runAgent(context.Background(), env, []string{"message", "list", tc.link}); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if got := srv.requestsTo("/api/conversations.replies")[0].form.Get("ts"); got != tc.wantTS {
				t.Errorf("ts = %q, want %q", got, tc.wantTS)
			}
			if len(calls) != 1 || calls[0].linkDomain != "acme" {
				t.Errorf("openClient calls = %+v", calls)
			}
		})
	}
}

func TestMessageListThreadNotFound(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.replies": readFixed(`{"ok":false,"error":"thread_not_found"}`)})
	code, out, errOut := runRead(t, srv, "message", "list", "C123", "--thread-ts", "1700000001.000100")
	wantFail(t, code, out, errOut, 1, "thread_not_found")
}

func TestMessageListChannelNotFound(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.history": readFixed(`{"ok":false,"error":"channel_not_found"}`)})
	code, out, errOut := runRead(t, srv, "message", "list", "C404")
	wantFail(t, code, out, errOut, 1, "channel_not_found")
}

func TestMessageListAuthFailureExits2(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.history": readFixed(`{"ok":false,"error":"token_revoked"}`)})
	code, out, errOut := runRead(t, srv, "message", "list", "C123")
	wantFail(t, code, out, errOut, 2, "token_revoked")
}

func TestMessageListMaxBodyChars(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.history": readFixed(
		`{"ok":true,"messages":[{"type":"message","user":"U1","text":"héllo wörld","ts":"1700000001.000100"}]}`)})
	for arg, want := range map[string]string{"5": `"content":"héllo\n…"`, "-1": `"content":"héllo wörld"`, "0": `"content":"\n…"`} {
		code, out, errOut := runRead(t, srv, "message", "list", "C123", "--max-body-chars", arg)
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("--max-body-chars %s: exit %d, out %s, err %s; want %s", arg, code, out, errOut, want)
		}
	}
	code, out, _ := runRead(t, srv, "message", "list", "C123")
	if code != 0 || !strings.Contains(out, `"content":"héllo wörld"`) {
		t.Errorf("default must keep a short body: %s", out)
	}
}

func TestMessageListIncludeReactionsOnlyWhenAsked(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.history": readFixed(agentReadHistoryBody)})
	_, plain, _ := runRead(t, srv, "message", "list", "C123")
	if strings.Contains(plain, "reactions") {
		t.Errorf("reactions leaked without --include-reactions: %s", plain)
	}
	code, with, errOut := runRead(t, srv, "message", "list", "C123", "--include-reactions")
	if code != 0 || !strings.Contains(with, `"reactions":[{"name":"eyes","users":["U1","U3"],"count":2}]`) {
		t.Errorf("exit %d: %s %s", code, with, errOut)
	}
}

func TestMessageListResolveUsers(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/conversations.history": readFixed(agentReadHistoryBody),
		"/api/users.info": func(form url.Values) string {
			switch form.Get("user") {
			case "U1":
				return `{"ok":true,"user":{"id":"U1","name":"ann","real_name":"Ann A","profile":{}}}`
			case "U2":
				return `{"ok":true,"user":{"id":"U2","name":"bob","profile":{"real_name":"Bob B"}}}`
			}
			return `{"ok":false,"error":"user_not_found"}`
		},
	})
	code, out, errOut := runRead(t, srv, "message", "list", "C123", "--resolve-users")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	want := `"referenced_users":{"U1":{"id":"U1","name":"ann","real_name":"Ann A"},"U2":{"id":"U2","name":"bob","real_name":"Bob B"}}}`
	if !strings.Contains(out, want) {
		t.Errorf("stdout %s\n lacks %s", out, want)
	}
	// U3 is mentioned but unknown: looked up, left out.
	asked := map[string]bool{}
	for _, r := range srv.requestsTo("/api/users.info") {
		asked[r.form.Get("user")] = true
	}
	if len(asked) != 3 || !asked["U3"] {
		t.Errorf("users.info asked for %v, want U1 U2 U3", asked)
	}
	_, plain, _ := runRead(t, srv, "message", "list", "C123")
	if strings.Contains(plain, "referenced_users") {
		t.Errorf("referenced_users without --resolve-users: %s", plain)
	}
}

// A bot message's mentions live in its blocks and attachments, and its
// content shows them as @U…: referenced_users must still cover them.
func TestMessageListResolveUsersFromBlocksAndAttachments(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/conversations.history": readFixed(`{"ok":true,"messages":[{"type":"message","subtype":"bot_message","bot_id":"B1","text":"","ts":"1700000001.000100",
		 "blocks":[{"type":"rich_text","block_id":"b","elements":[{"type":"rich_text_section","elements":[{"type":"user","user_id":"U4"},{"type":"text","text":" on call"}]}]}],
		 "attachments":[{"id":1,"fields":[{"title":"Assignee","value":"<@U5>","short":true}]}]}]}`),
		"/api/users.info": func(form url.Values) string {
			id := form.Get("user")
			return `{"ok":true,"user":{"id":"` + id + `","name":"n` + id + `","real_name":"R` + id + `","profile":{}}}`
		},
	})
	code, out, errOut := runRead(t, srv, "message", "list", "C123", "--resolve-users")
	wantOut(t, code, out, errOut, `{"messages":[{"ts":"1700000001.000100","channel_id":"C123","author":{"bot_id":"B1"},"content":"@U4 on call\n\nAssignee\n@U5","subtype":"bot_message"}],`+
		`"referenced_users":{"U4":{"id":"U4","name":"nU4","real_name":"RU4"},"U5":{"id":"U5","name":"nU5","real_name":"RU5"}}}`)
}

func TestMessageListUsageErrorsDoNotConnect(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"message", "list"}, "missing <target>"},
		{[]string{"message", "list", "C123", "--oldest", "yesterday"}, "not a Slack timestamp"},
		{[]string{"message", "list", "C123", "--limit", "0"}, "--limit must be at least 1"},
		{[]string{"message", "list", "https://acme.slack.com/archives/C1"}, "not a Slack message link"},
	} {
		env, stdout, stderr := agentTestEnv(nil, noOpener(t))
		code := runAgent(context.Background(), env, tc.args)
		wantFail(t, code, stdout.String(), stderr.String(), 1, tc.want)
	}
}

// ---------------------------------------------------------------------
// message get
// ---------------------------------------------------------------------

func TestMessageGetByHistory(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.history": readFixed(
		`{"ok":true,"messages":[{"type":"message","user":"U1","text":"hello","ts":"1700000001.000100"}]}`)})
	code, out, errOut := runRead(t, srv, "message", "get", "C123", "--ts", "1700000001.000100")
	wantOut(t, code, out, errOut, `{"message":{"ts":"1700000001.000100","channel_id":"C123","author":{"user_id":"U1"},"content":"hello"}}`)
	form := srv.requestTo(t, "/api/conversations.history").form
	if form.Get("oldest") != "1700000001.000100" || form.Get("latest") != "1700000001.000100" || form.Get("limit") != "1" {
		t.Errorf("form = %v", form)
	}
	if v := form.Get("inclusive"); v != "1" && v != "true" {
		t.Errorf("inclusive = %q, want set", v)
	}
}

func TestMessageGetLinkTakesTSFromLink(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.history": readFixed(
		`{"ok":true,"messages":[{"type":"message","user":"U1","text":"hello","ts":"1700000001.000100"}]}`)})
	code, out, errOut := runRead(t, srv, "message", "get", "https://acme.slack.com/archives/C123/p1700000001000100", "--max-body-chars", "2")
	wantOut(t, code, out, errOut, `{"message":{"ts":"1700000001.000100","channel_id":"C123","author":{"user_id":"U1"},"content":"he\n…"}}`)
}

func TestMessageGetNotFound(t *testing.T) {
	// Slack answers with the neighbouring message when the ts does not
	// exist; the ts must match exactly.
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.history": readFixed(
		`{"ok":true,"messages":[{"type":"message","user":"U1","text":"other","ts":"1700000009.000900"}]}`)})
	code, out, errOut := runRead(t, srv, "message", "get", "C123", "--ts", "1700000001.000100")
	wantFail(t, code, out, errOut, 1, "message_not_found")

	srv = newReadFake(t, map[string]func(url.Values) string{"/api/conversations.history": readFixed(`{"ok":true,"messages":[]}`)})
	code, out, errOut = runRead(t, srv, "message", "get", "C123", "--ts", "1700000001.000100")
	wantFail(t, code, out, errOut, 1, "message_not_found")
}

func TestMessageGetInThread(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.replies": replyPages(t)})
	code, out, errOut := runRead(t, srv, "message", "get",
		"https://acme.slack.com/archives/C123/p1700000003000100?thread_ts=1700000001.000100&cid=C123")
	wantOut(t, code, out, errOut, `{"message":{"ts":"1700000003.000100","thread_ts":"1700000001.000100","channel_id":"C123","author":{"user_id":"U1"},"content":"reply 2"}}`)
	if len(srv.requestsTo("/api/conversations.history")) != 0 {
		t.Error("a message in a thread is read from the thread")
	}

	code, out, errOut = runRead(t, srv, "message", "get", "C123", "--ts", "1700000099.000100", "--thread-ts", "1700000001.000100")
	wantFail(t, code, out, errOut, 1, "message_not_found")
}

func TestMessageGetThreadGoneIsMessageNotFound(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.replies": readFixed(`{"ok":false,"error":"thread_not_found"}`)})
	code, out, errOut := runRead(t, srv, "message", "get", "C123", "--ts", "1700000002.000100", "--thread-ts", "1700000001.000100")
	wantFail(t, code, out, errOut, 1, "message_not_found")
}

func TestMessageGetNeedsATS(t *testing.T) {
	env, stdout, stderr := agentTestEnv(nil, noOpener(t))
	code := runAgent(context.Background(), env, []string{"message", "get", "C123"})
	wantFail(t, code, stdout.String(), stderr.String(), 1, "needs --ts")
}

// ---------------------------------------------------------------------
// unreads
// ---------------------------------------------------------------------

const agentUnreadCountsBody = `{"ok":true,
 "channels":[
  {"id":"C123","has_unreads":true,"mention_count":2,"last_read":"1700000000.000000"},
  {"id":"C2","has_unreads":false,"mention_count":0,"last_read":"1700000000.000000"},
  {"id":"G1","has_unreads":true,"mention_count":0,"last_read":"1700000000.000001"}],
 "mpims":[{"id":"G2","has_unreads":true,"mention_count":0,"last_read":"1700000000.000002"}],
 "ims":[{"id":"D1","has_unreads":true,"mention_count":1,"last_read":"1700000000.000003"}]}`

func unreadFake(t *testing.T) *fakeSlack {
	return newReadFake(t, map[string]func(url.Values) string{
		"/api/client.counts": readFixed(agentUnreadCountsBody),
		"/api/conversations.info": func(form url.Values) string {
			switch form.Get("channel") {
			case "C123":
				return `{"ok":true,"channel":{"id":"C123","name":"general","is_channel":true}}`
			case "G1":
				return `{"ok":true,"channel":{"id":"G1","name":"secret","is_group":true,"is_private":true}}`
			case "G2":
				return `{"ok":true,"channel":{"id":"G2","name":"mpdm-a--b-1","is_mpim":true,"is_group":true,"is_private":true}}`
			case "D1":
				return `{"ok":true,"channel":{"id":"D1","is_im":true,"user":"U7"}}`
			}
			return `{"ok":false,"error":"channel_not_found"}`
		},
		"/api/users.info": readFixed(`{"ok":true,"user":{"id":"U7","name":"zed","real_name":"Zed Z","profile":{}}}`),
		"/api/conversations.history": func(form url.Values) string {
			return `{"ok":true,"messages":[
			 {"type":"message","user":"U9","text":"new in ` + form.Get("channel") + `","ts":"1700000020.000200"},
			 {"type":"message","user":"U9","text":"older","ts":"1700000010.000100"}]}`
		},
	})
}

func TestUnreadsShape(t *testing.T) {
	srv := unreadFake(t)
	code, out, errOut := runRead(t, srv, "unreads", "--max-messages", "2")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	msgs := func(ch string) string {
		return `[{"ts":"1700000010.000100","channel_id":"` + ch + `","author":{"user_id":"U9"},"content":"older"},` +
			`{"ts":"1700000020.000200","channel_id":"` + ch + `","author":{"user_id":"U9"},"content":"new in ` + ch + `"}]`
	}
	// Most mentions first; the DM is named after its peer's real name
	// (no display name). client.counts gives no unread counts, so each
	// is the number of messages read.
	want := `{"channels":[` +
		`{"channel_id":"C123","channel_name":"general","channel_type":"channel","unread_count":2,"mention_count":2,"messages":` + msgs("C123") + `},` +
		`{"channel_id":"D1","channel_name":"Zed Z","channel_type":"dm","unread_count":2,"mention_count":1,"messages":` + msgs("D1") + `},` +
		`{"channel_id":"G1","channel_name":"secret","channel_type":"group","unread_count":2,"mention_count":0,"messages":` + msgs("G1") + `},` +
		`{"channel_id":"G2","channel_name":"mpdm-a--b-1","channel_type":"mpim","unread_count":2,"mention_count":0,"messages":` + msgs("G2") + `}]}`
	if got := strings.TrimSuffix(out, "\n"); got != want {
		t.Errorf("stdout\n got  %s\n want %s", got, want)
	}
	if got := srv.requestTo(t, "/api/client.counts").form.Get("thread_count_by_channel"); got != "true" {
		t.Errorf("client.counts thread_count_by_channel = %q, want true (as agent-slack asks)", got)
	}
	// Only conversations with unreads are asked about, after their last_read.
	for _, r := range srv.requestsTo("/api/conversations.history") {
		if r.form.Get("channel") == "C2" {
			t.Error("read history of a conversation with no unreads")
		}
		if r.form.Get("limit") != "2" {
			t.Errorf("limit = %q, want 2", r.form.Get("limit"))
		}
		if r.form.Get("channel") == "D1" && r.form.Get("oldest") != "1700000000.000003" {
			t.Errorf("D1 oldest = %q, want its last_read", r.form.Get("oldest"))
		}
		if v := r.form.Get("inclusive"); v == "1" || v == "true" {
			t.Errorf("history must exclude last_read itself, form = %v", r.form)
		}
	}
	if n := len(srv.requestsTo("/api/conversations.history")); n != 4 {
		t.Errorf("%d history requests, want 4", n)
	}
}

// unreadsOne is a fake Slack with one unread channel C1 whose
// client.counts entry, history and users.info answers are given.
func unreadsOne(t *testing.T, countsEntry, history string) *fakeSlack {
	return newReadFake(t, map[string]func(url.Values) string{
		"/api/client.counts":         readFixed(`{"ok":true,"channels":[` + countsEntry + `]}`),
		"/api/conversations.info":    readFixed(`{"ok":true,"channel":{"id":"C1","name":"ops","is_channel":true}}`),
		"/api/conversations.history": readFixed(history),
	})
}

func TestUnreadsDropsSystemMessagesAndMarksCuts(t *testing.T) {
	srv := unreadsOne(t, `{"id":"C1","has_unreads":true,"last_read":"1.0"}`, `{"ok":true,"has_more":false,"messages":[
	 {"type":"message","subtype":"channel_topic","user":"U2","text":"set the channel topic: x","topic":"x","ts":"1700000006.000100"},
	 {"type":"message","subtype":"thread_broadcast","user":"U2","text":"also","ts":"1700000005.000100","thread_ts":"1700000001.000100","root":{"ts":"1700000001.000100"}},
	 {"type":"message","subtype":"bot_message","bot_id":"B1","text":"build green","ts":"1700000004.000100"},
	 {"type":"message","subtype":"group_join","user":"U3","text":"<@U3> has joined the channel","ts":"1700000003.000100"},
	 {"type":"message","user":"U2","text":"a real message","ts":"1700000002.000100"},
	 {"type":"message","subtype":"channel_join","user":"U4","text":"<@U4> has joined the channel","ts":"1700000001.500000"}]}`)
	code, out, errOut := runRead(t, srv, "unreads", "--max-body-chars", "5")
	wantOut(t, code, out, errOut, `{"channels":[{"channel_id":"C1","channel_name":"ops","channel_type":"channel","unread_count":3,"mention_count":0,"messages":[`+
		`{"ts":"1700000002.000100","channel_id":"C1","author":{"user_id":"U2"},"content":"a rea\n..."},`+
		`{"ts":"1700000004.000100","channel_id":"C1","author":{"bot_id":"B1"},"content":"build\n...","subtype":"bot_message"},`+
		`{"ts":"1700000005.000100","thread_ts":"1700000001.000100","channel_id":"C1","author":{"user_id":"U2"},"content":"also","subtype":"thread_broadcast"}]}]}`)
}

func TestUnreadsUnreadCount(t *testing.T) {
	twoMsgs := `{"ok":true,"has_more":%s,"messages":[
	 {"type":"message","user":"U2","text":"b","ts":"1700000002.000100"},
	 {"type":"message","user":"U2","text":"a","ts":"1700000001.000100"}]}`
	for _, tc := range []struct {
		name, entry, history string
		want                 int
	}{
		{"Slack's display count wins", `{"id":"C1","has_unreads":true,"unread_count_display":5,"unread_count":7,"last_read":"1.0"}`, fmt.Sprintf(twoMsgs, "false"), 5},
		{"then Slack's unread_count", `{"id":"C1","has_unreads":true,"unread_count":7,"last_read":"1.0"}`, fmt.Sprintf(twoMsgs, "false"), 7},
		{"else the messages read", `{"id":"C1","has_unreads":true,"last_read":"1.0"}`, fmt.Sprintf(twoMsgs, "false"), 2},
		{"at least 2 when Slack has more", `{"id":"C1","has_unreads":true,"last_read":"1.0"}`,
			`{"ok":true,"has_more":true,"messages":[{"type":"message","user":"U2","text":"b","ts":"1700000002.000100"}]}`, 2},
		{"more than 2 kept when Slack has more", `{"id":"C1","has_unreads":true,"last_read":"1.0"}`, fmt.Sprintf(twoMsgs, "true"), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := runRead(t, unreadsOne(t, tc.entry, tc.history), "unreads", "--max-messages", "2")
			if want := fmt.Sprintf(`"unread_count":%d,`, tc.want); code != 0 || !strings.Contains(out, want) {
				t.Errorf("exit %d, out %s, stderr %s; want %s", code, out, errOut, want)
			}
		})
	}

	// No last_read: nothing to read after, so no history call, no
	// messages, and a count of 1 (as agent-slack).
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/client.counts":      readFixed(`{"ok":true,"ims":[{"id":"D1","has_unreads":true,"mention_count":1}]}`),
		"/api/conversations.info": readFixed(`{"ok":true,"channel":{"id":"D1","is_im":true,"user":"U7"}}`),
		"/api/users.info":         readFixed(`{"ok":true,"user":{"id":"U7","name":"zed","profile":{}}}`),
	})
	code, out, errOut := runRead(t, srv, "unreads")
	wantOut(t, code, out, errOut, `{"channels":[{"channel_id":"D1","channel_name":"zed","channel_type":"dm","unread_count":1,"mention_count":1,"messages":[]}]}`)
}

func TestUnreadsSortsByMentionsThenUnreadCount(t *testing.T) {
	entry := func(id string, mentions, unread int) string {
		return fmt.Sprintf(`{"id":"%s","has_unreads":true,"mention_count":%d,"unread_count_display":%d,"last_read":"1.0"}`, id, mentions, unread)
	}
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/client.counts": readFixed(`{"ok":true,"channels":[` + entry("C1", 0, 5) + `,` + entry("C2", 2, 1) + `,` + entry("C3", 0, 9) + `,` +
			entry("C4", 2, 4) + `],"mpims":[` + entry("G1", 1, 0) + `],"ims":[` + entry("D1", 0, 5) + `]}`),
		"/api/conversations.info": func(form url.Values) string {
			return `{"ok":true,"channel":{"id":"` + form.Get("channel") + `","name":"n","is_channel":true}}`
		},
		"/api/conversations.history": readFixed(`{"ok":true,"messages":[]}`),
	})
	code, out, errOut := runRead(t, srv, "unreads")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var got struct {
		Channels []struct {
			ChannelID string `json:"channel_id"`
		} `json:"channels"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, c := range got.Channels {
		order = append(order, c.ChannelID)
	}
	// Ties keep client.counts' order: channels, then group DMs, then DMs.
	if want := "C4 C2 G1 C3 C1 D1"; strings.Join(order, " ") != want {
		t.Errorf("order %v, want %s", order, want)
	}
}

func TestUnreadsDMName(t *testing.T) {
	for _, tc := range []struct {
		name, info, user, want string
		asksUser               bool
	}{
		{"display name first", `{"id":"D1","is_im":true,"user":"U7"}`,
			`{"ok":true,"user":{"id":"U7","name":"zed","real_name":"Zed Z","profile":{"display_name":"zeddy","real_name":"Zed Z"}}}`, "zeddy", true},
		{"then real name", `{"id":"D1","is_im":true,"user":"U7"}`,
			`{"ok":true,"user":{"id":"U7","name":"zed","real_name":"Zed Z","profile":{"display_name":""}}}`, "Zed Z", true},
		{"then handle", `{"id":"D1","is_im":true,"user":"U7"}`, `{"ok":true,"user":{"id":"U7","name":"zed","profile":{}}}`, "zed", true},
		{"peer gone: their ID", `{"id":"D1","is_im":true,"user":"U7"}`, `{"ok":false,"error":"user_not_found"}`, "U7", true},
		{"a name of its own wins", `{"id":"D1","name":"zed-dm","is_im":true,"user":"U7"}`, `{"ok":false,"error":"unexpected"}`, "zed-dm", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newReadFake(t, map[string]func(url.Values) string{
				"/api/client.counts":         readFixed(`{"ok":true,"ims":[{"id":"D1","has_unreads":true,"mention_count":1,"last_read":"1.0"}]}`),
				"/api/conversations.info":    readFixed(`{"ok":true,"channel":` + tc.info + `}`),
				"/api/users.info":            readFixed(tc.user),
				"/api/conversations.history": readFixed(`{"ok":true,"messages":[]}`),
			})
			code, out, errOut := runRead(t, srv, "unreads")
			if want := `"channel_name":"` + tc.want + `","channel_type":"dm"`; code != 0 || !strings.Contains(out, want) {
				t.Errorf("exit %d, out %s, stderr %s; want %s", code, out, errOut, want)
			}
			if asked := len(srv.requestsTo("/api/users.info")) > 0; asked != tc.asksUser {
				t.Errorf("users.info asked: %v, want %v", asked, tc.asksUser)
			}
		})
	}
}

func TestUnreadsThreadsSummary(t *testing.T) {
	for _, tc := range []struct{ threads, want string }{
		{`{"has_unreads":true,"mention_count":3,"unread_count":5}`, `{"channels":[],"threads":{"has_unreads":true,"mention_count":3}}`},
		{`{"has_unreads":true}`, `{"channels":[],"threads":{"has_unreads":true,"mention_count":0}}`},
		{`{"has_unreads":false,"mention_count":0}`, `{"channels":[]}`},
	} {
		srv := newReadFake(t, map[string]func(url.Values) string{"/api/client.counts": readFixed(`{"ok":true,"channels":[],"threads":` + tc.threads + `}`)})
		code, out, errOut := runRead(t, srv, "unreads")
		wantOut(t, code, out, errOut, tc.want)
	}
}

func TestUnreadsBoundsRequestsInFlight(t *testing.T) {
	const conversations = 40
	var inFlight, peak atomic.Int32
	track := func(body func(url.Values) string) func(url.Values) string {
		return func(form url.Values) string {
			n := inFlight.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			time.Sleep(20 * time.Millisecond)
			inFlight.Add(-1)
			return body(form)
		}
	}
	var entries []string
	for i := range conversations {
		entries = append(entries, fmt.Sprintf(`{"id":"C%03d","has_unreads":true,"last_read":"1.0"}`, i))
	}
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/client.counts": readFixed(`{"ok":true,"channels":[` + strings.Join(entries, ",") + `]}`),
		"/api/conversations.info": track(func(form url.Values) string {
			return `{"ok":true,"channel":{"id":"` + form.Get("channel") + `","name":"n","is_channel":true}}`
		}),
		"/api/conversations.history": track(readFixed(`{"ok":true,"messages":[]}`)),
	})
	code, out, errOut := runRead(t, srv, "unreads")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if n := strings.Count(out, `"channel_id"`); n != conversations {
		t.Errorf("%d conversations listed, want %d", n, conversations)
	}
	if p := peak.Load(); p > agentUnreadsInFlight || p < 2 {
		t.Errorf("at most %d requests were in flight at once; want concurrent and at most %d", p, agentUnreadsInFlight)
	}
}

func TestUnreadsCountsRefusalAndRateLimit(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/client.counts": readFixed(`{"ok":false,"error":"invalid_auth"}`)})
	code, out, errOut := runRead(t, srv, "unreads")
	wantFail(t, code, out, errOut, 2, "invalid_auth")

	srv = newReadFake(t, map[string]func(url.Values) string{"/api/client.counts": onceLimited(`{"ok":true,"channels":[]}`)})
	code, out, errOut = runRead(t, srv, "unreads")
	wantOut(t, code, out, errOut, `{"channels":[]}`)
	if n := len(srv.requestsTo("/api/client.counts")); n != 2 {
		t.Errorf("%d client.counts requests, want 2 (retry after the 429)", n)
	}
}

func TestUnreadsEmptyIsAnEmptyArray(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/client.counts": readFixed(`{"ok":true,"channels":[],"ims":[],"mpims":[]}`)})
	code, out, errOut := runRead(t, srv, "unreads")
	wantOut(t, code, out, errOut, `{"channels":[]}`)
}

func TestUnreadsSkipsAConversationSlackWillNotDescribe(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/client.counts": readFixed(`{"ok":true,"channels":[{"id":"C123","has_unreads":true,"last_read":"1.0"},{"id":"C9","has_unreads":true,"last_read":"1.0"}]}`),
		"/api/conversations.info": func(form url.Values) string {
			if form.Get("channel") == "C9" {
				return `{"ok":false,"error":"channel_not_found"}`
			}
			return `{"ok":true,"channel":{"id":"C123","name":"general","is_channel":true}}`
		},
		"/api/conversations.history": readFixed(`{"ok":true,"messages":[]}`),
	})
	code, out, errOut := runRead(t, srv, "unreads")
	wantOut(t, code, out, "", `{"channels":[{"channel_id":"C123","channel_name":"general","channel_type":"channel","unread_count":0,"mention_count":0,"messages":[]}]}`)
	if !strings.Contains(errOut, "C9") || !strings.Contains(errOut, "channel_not_found") {
		t.Errorf("stderr %q should say what was skipped", errOut)
	}
}

func TestUnreadsAuthFailureExits2(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/client.counts":         readFixed(`{"ok":true,"channels":[{"id":"C123","has_unreads":true,"last_read":"1.0"}]}`),
		"/api/conversations.info":    readFixed(`{"ok":false,"error":"invalid_auth"}`),
		"/api/conversations.history": readFixed(`{"ok":false,"error":"invalid_auth"}`),
	})
	code, out, errOut := runRead(t, srv, "unreads")
	wantFail(t, code, out, errOut, 2, "invalid_auth")
}

// ---------------------------------------------------------------------
// search messages
// ---------------------------------------------------------------------

const agentReadSearchBody = `{"ok":true,"messages":{"total":2,"matches":[
 {"type":"message","channel":{"id":"C5","name":"ops"},"user":"U1","username":"ann","ts":"1700000002.000100","text":"reply hit",
  "permalink":"https://acme.slack.com/archives/C5/p1700000002000100?thread_ts=1700000001.000100&cid=C5"},
 {"type":"message","channel":{"id":"C5","name":"ops"},"user":"U2","username":"bob","ts":"1700000001.000100","text":"root hit",
  "permalink":"https://acme.slack.com/archives/C5/p1700000001000100"}]}}`

func TestSearchMessagesShapeAndQuery(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/search.messages": readFixed(agentReadSearchBody)})
	code, out, errOut := runRead(t, srv, "search", "messages", "deploy failed", "--after", "2026-11-01", "--before", "2026-11-12", "--user", "U0123ABCD", "--limit", "7")
	wantOut(t, code, out, errOut, `{"messages":[`+
		`{"ts":"1700000002.000100","thread_ts":"1700000001.000100","channel_id":"C5","author":{"user_id":"U1","username":"ann"},"content":"reply hit","permalink":"https://acme.slack.com/archives/C5/p1700000002000100?thread_ts=1700000001.000100&cid=C5"},`+
		`{"ts":"1700000001.000100","channel_id":"C5","author":{"user_id":"U2","username":"bob"},"content":"root hit","permalink":"https://acme.slack.com/archives/C5/p1700000001000100"}]}`)
	form := srv.requestTo(t, "/api/search.messages").form
	if got, want := form.Get("query"), "deploy failed after:2026-11-01 before:2026-11-12 from:<@U0123ABCD>"; got != want {
		t.Errorf("query = %q, want %q", got, want)
	}
	// slack-go leaves sort_dir out when it is "desc", Slack's default.
	if form.Get("sort") != "timestamp" || (form.Get("sort_dir") != "" && form.Get("sort_dir") != "desc") || form.Get("count") != "7" {
		t.Errorf("form = %v, want sort=timestamp, newest first, count=7", form)
	}
}

func TestSearchMessagesDefaultsAndLimitCut(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/search.messages": readFixed(agentReadSearchBody)})
	code, out, errOut := runRead(t, srv, "search", "messages", "--limit", "1", "--", "-spam deploy")
	_ = out
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.Count(out, `"channel_id"`) != 1 {
		t.Errorf("--limit 1 returned more than one result: %s", out)
	}
	if got := srv.requestTo(t, "/api/search.messages").form.Get("query"); got != "-spam deploy" {
		t.Errorf("query after -- = %q", got)
	}
	code, _, errOut = runRead(t, srv, "search", "messages", "hello")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	reqs := srv.requestsTo("/api/search.messages")
	if got := reqs[len(reqs)-1].form; got.Get("query") != "hello" || (got.Get("count") != "" && got.Get("count") != "20") || got.Get("sort") != "timestamp" {
		t.Errorf("defaults: form = %v", got)
	}
}

func TestSearchMessagesNoHitsIsEmptyArray(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/search.messages": readFixed(`{"ok":true,"messages":{"total":0,"matches":[]}}`)})
	code, out, errOut := runRead(t, srv, "search", "messages", "zzz")
	wantOut(t, code, out, errOut, `{"messages":[]}`)
}

func TestSearchMessagesUsageErrorsDoNotConnect(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"search", "messages"}, "missing <query>"},
		{[]string{"search", "messages", "x", "--after", "11/01/2026"}, "not a date"},
		{[]string{"search", "messages", "x", "--before", "2026-13-40"}, "not a date"},
		{[]string{"search", "messages", "x", "--user", "ann>"}, "not a user ID"},
		{[]string{"search", "messages", "x", "--limit", "0"}, "--limit must be at least 1"},
	} {
		env, stdout, stderr := agentTestEnv(nil, noOpener(t))
		code := runAgent(context.Background(), env, tc.args)
		wantFail(t, code, stdout.String(), stderr.String(), 1, tc.want)
	}
}

func TestSearchMessagesSlackError(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/search.messages": readFixed(`{"ok":false,"error":"not_authed"}`)})
	code, out, errOut := runRead(t, srv, "search", "messages", "x")
	wantFail(t, code, out, errOut, 2, "not_authed")
}

// ---------------------------------------------------------------------
// channel list
// ---------------------------------------------------------------------

func TestChannelListShapeAndCursor(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/users.conversations": func(form url.Values) string {
		if form.Get("cursor") == "" {
			return `{"ok":true,"channels":[
			 {"id":"C123","name":"general","is_channel":true,"is_private":false},
			 {"id":"G1","name":"secret","is_group":true,"is_private":true},
			 {"id":"D1","is_im":true,"user":"U7"}],"response_metadata":{"next_cursor":"NEXT"}}`
		}
		return `{"ok":true,"channels":[{"id":"G2","name":"mpdm-a--b-1","is_mpim":true,"is_group":true,"is_private":true}],"response_metadata":{"next_cursor":""}}`
	}})
	code, out, errOut := runRead(t, srv, "channel", "list", "--limit", "3")
	wantOut(t, code, out, errOut, `{"channels":[`+
		`{"id":"C123","name":"general","is_channel":true,"is_group":false,"is_im":false,"is_mpim":false,"is_private":false},`+
		`{"id":"G1","name":"secret","is_channel":false,"is_group":true,"is_im":false,"is_mpim":false,"is_private":true},`+
		`{"id":"D1","name":"","is_channel":false,"is_group":false,"is_im":true,"is_mpim":false,"is_private":false,"user":"U7"}],`+
		`"next_cursor":"NEXT"}`)
	form := srv.requestTo(t, "/api/users.conversations").form
	if form.Get("limit") != "3" || form.Get("exclude_archived") != "true" {
		t.Errorf("form = %v", form)
	}
	for _, typ := range []string{"public_channel", "private_channel", "mpim", "im"} {
		if !strings.Contains(form.Get("types"), typ) {
			t.Errorf("types = %q lacks %s", form.Get("types"), typ)
		}
	}

	code, out, errOut = runRead(t, srv, "channel", "list", "--cursor", "NEXT")
	wantOut(t, code, out, errOut, `{"channels":[`+
		`{"id":"G2","name":"mpdm-a--b-1","is_channel":false,"is_group":true,"is_im":false,"is_mpim":true,"is_private":true}],`+
		`"next_cursor":""}`)
	reqs := srv.requestsTo("/api/users.conversations")
	if last := reqs[len(reqs)-1].form; last.Get("cursor") != "NEXT" || last.Get("limit") != "200" {
		t.Errorf("second page form = %v, want cursor NEXT, default limit 200", last)
	}
}

func TestChannelListEmpty(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/users.conversations": readFixed(`{"ok":true,"channels":[]}`)})
	code, out, errOut := runRead(t, srv, "channel", "list")
	wantOut(t, code, out, errOut, `{"channels":[],"next_cursor":""}`)
}

// ---------------------------------------------------------------------
// user get
// ---------------------------------------------------------------------

func TestUserGetShape(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/users.info": readFixed(`{"ok":true,"user":{
	 "id":"U0123ABCD","name":"ann","real_name":"Ann A","tz":"Asia/Shanghai","is_stranger":true,
	 "profile":{"display_name":"annie","email":"ann@example.com","image_72":"https://img/72.png"}}}`)})
	code, out, errOut := runRead(t, srv, "user", "get", "U0123ABCD")
	wantOut(t, code, out, errOut, `{"id":"U0123ABCD","name":"ann","real_name":"Ann A","display_name":"annie","email":"ann@example.com",`+
		`"is_bot":false,"deleted":false,"is_restricted":false,"is_ultra_restricted":false,"is_stranger":true,`+
		`"tz":"Asia/Shanghai","image_72":"https://img/72.png"}`)
	if got := srv.requestTo(t, "/api/users.info").form.Get("user"); got != "U0123ABCD" {
		t.Errorf("user = %q", got)
	}
}

func TestUserGetBotAndRealNameFallback(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/users.info": readFixed(`{"ok":true,"user":{
	 "id":"U0HOOK001","name":"hook","is_bot":true,"deleted":true,"is_restricted":true,"is_ultra_restricted":true,
	 "profile":{"real_name":"Hook Bot"}}}`)})
	code, out, errOut := runRead(t, srv, "user", "get", "U0HOOK001")
	wantOut(t, code, out, errOut, `{"id":"U0HOOK001","name":"hook","real_name":"Hook Bot","display_name":"","email":"",`+
		`"is_bot":true,"deleted":true,"is_restricted":true,"is_ultra_restricted":true,"is_stranger":false,"tz":"","image_72":""}`)
}

func TestUserGetErrors(t *testing.T) {
	for _, tc := range []struct {
		slackErr string
		wantCode int
	}{{"user_not_found", 1}, {"invalid_auth", 2}} {
		srv := newReadFake(t, map[string]func(url.Values) string{"/api/users.info": readFixed(`{"ok":false,"error":"` + tc.slackErr + `"}`)})
		code, out, errOut := runRead(t, srv, "user", "get", "U0NOBODY0")
		wantFail(t, code, out, errOut, tc.wantCode, tc.slackErr)
	}
}

func TestUserGetUsageErrorsDoNotConnect(t *testing.T) {
	// "ann" is a handle now (TestUserGetByEmailAndHandle); a bare @ is not.
	for _, args := range [][]string{{"user", "get"}, {"user", "get", "@"}, {"user", "get", "U1", "U2"}} {
		env, stdout, stderr := agentTestEnv(nil, noOpener(t))
		if code := runAgent(context.Background(), env, args); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "usage: slk user get") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
}

// ---------------------------------------------------------------------
// review fixes: errors must not silently drop data
// ---------------------------------------------------------------------

// onceLimited answers the first call with a 429 (Retry-After 1) and
// every later one with body.
func onceLimited(body string) func(url.Values) string {
	var n atomic.Int32
	return func(url.Values) string {
		if n.Add(1) == 1 {
			return "429 1"
		}
		return body
	}
}

func TestUnreadsWaitsOutRateLimit(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/client.counts":         readFixed(`{"ok":true,"channels":[{"id":"C123","has_unreads":true,"last_read":"1.0"}]}`),
		"/api/conversations.info":    onceLimited(`{"ok":true,"channel":{"id":"C123","name":"general","is_channel":true}}`),
		"/api/conversations.history": readFixed(`{"ok":true,"messages":[]}`),
	})
	code, out, errOut := runRead(t, srv, "unreads")
	wantOut(t, code, out, errOut, `{"channels":[{"channel_id":"C123","channel_name":"general","channel_type":"channel","unread_count":0,"mention_count":0,"messages":[]}]}`)
	if n := len(srv.requestsTo("/api/conversations.info")); n != 2 {
		t.Errorf("%d conversations.info requests, want 2 (retry after the 429)", n)
	}
}

func TestUnreadsUnexpectedErrorFailsTheCommand(t *testing.T) {
	for name, info := range map[string]string{
		"slack error":   `{"ok":false,"error":"internal_error"}`,
		"missing_scope": `{"ok":false,"error":"missing_scope"}`,
	} {
		srv := newReadFake(t, map[string]func(url.Values) string{
			"/api/client.counts": readFixed(`{"ok":true,"channels":[{"id":"C123","has_unreads":true,"last_read":"1.0"},{"id":"C9","has_unreads":true,"last_read":"1.0"}]}`),
			"/api/conversations.info": func(form url.Values) string {
				if form.Get("channel") == "C9" {
					return info
				}
				return `{"ok":true,"channel":{"id":"C123","name":"general","is_channel":true}}`
			},
			"/api/conversations.history": readFixed(`{"ok":true,"messages":[]}`),
		})
		code, out, errOut := runRead(t, srv, "unreads")
		t.Run(name, func(t *testing.T) { wantFail(t, code, out, errOut, 1, "C9") })
		if !strings.Contains(errOut, strings.Split(strings.Split(info, `"error":"`)[1], `"`)[0]) {
			t.Errorf("%s: stderr %q lacks the Slack code", name, errOut)
		}
	}
	// A server failure (not a Slack answer) fails too.
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/client.counts":         readFixed(`{"ok":true,"channels":[{"id":"C9","has_unreads":true,"last_read":"1.0"}]}`),
		"/api/conversations.info":    func(url.Values) string { return "not json at all" },
		"/api/conversations.history": readFixed(`{"ok":true,"messages":[]}`),
	})
	code, out, errOut := runRead(t, srv, "unreads")
	wantFail(t, code, out, errOut, 1, "C9")
}

func TestUnreadsSkipsEveryGoneCode(t *testing.T) {
	for _, gone := range []string{"channel_not_found", "not_in_channel", "is_archived"} {
		srv := newReadFake(t, map[string]func(url.Values) string{
			"/api/client.counts":         readFixed(`{"ok":true,"channels":[{"id":"C9","has_unreads":true,"last_read":"1.0"}]}`),
			"/api/conversations.info":    readFixed(`{"ok":false,"error":"` + gone + `"}`),
			"/api/conversations.history": readFixed(`{"ok":false,"error":"` + gone + `"}`),
		})
		code, out, errOut := runRead(t, srv, "unreads")
		wantOut(t, code, out, "", `{"channels":[]}`)
		if !strings.Contains(errOut, "C9") || !strings.Contains(errOut, gone) {
			t.Errorf("%s: stderr %q should say what was skipped", gone, errOut)
		}
	}
}

func TestUnreadsHistoryRateLimitedAndFailing(t *testing.T) {
	counts := readFixed(`{"ok":true,"channels":[{"id":"C123","has_unreads":true,"last_read":"1.0"}]}`)
	info := readFixed(`{"ok":true,"channel":{"id":"C123","name":"general","is_channel":true}}`)
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/client.counts": counts, "/api/conversations.info": info,
		"/api/conversations.history": onceLimited(`{"ok":true,"messages":[]}`),
	})
	code, out, errOut := runRead(t, srv, "unreads")
	if code != 0 || !strings.Contains(out, `"C123"`) {
		t.Errorf("rate-limited history: exit %d, out %s, stderr %s", code, out, errOut)
	}
	srv = newReadFake(t, map[string]func(url.Values) string{
		"/api/client.counts": counts, "/api/conversations.info": info,
		"/api/conversations.history": readFixed(`{"ok":false,"error":"fatal_error"}`),
	})
	code, out, errOut = runRead(t, srv, "unreads")
	wantFail(t, code, out, errOut, 1, "fatal_error")
}

func TestResolveUsersWaitsOutRateLimit(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/conversations.history": readFixed(`{"ok":true,"messages":[{"type":"message","user":"U1","text":"hi","ts":"1700000001.000100"}]}`),
		"/api/users.info":            onceLimited(`{"ok":true,"user":{"id":"U1","name":"ann","real_name":"Ann A","profile":{}}}`),
	})
	code, out, errOut := runRead(t, srv, "message", "list", "C123", "--resolve-users")
	if code != 0 || !strings.Contains(out, `"referenced_users":{"U1":{"id":"U1","name":"ann","real_name":"Ann A"}}`) {
		t.Errorf("exit %d, out %s, stderr %s", code, out, errOut)
	}
}

func TestResolveUsersErrors(t *testing.T) {
	hist := readFixed(`{"ok":true,"messages":[{"type":"message","user":"U1","text":"hi","ts":"1700000001.000100"}]}`)
	for _, bad := range []string{"internal_error", "missing_scope"} {
		srv := newReadFake(t, map[string]func(url.Values) string{
			"/api/conversations.history": hist,
			"/api/users.info":            readFixed(`{"ok":false,"error":"` + bad + `"}`),
		})
		code, out, errOut := runRead(t, srv, "message", "list", "C123", "--resolve-users")
		wantFail(t, code, out, errOut, 1, bad)
	}
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/conversations.history": hist,
		"/api/users.info":            readFixed(`{"ok":false,"error":"user_not_found"}`),
	})
	code, out, errOut := runRead(t, srv, "message", "list", "C123", "--resolve-users")
	if code != 0 || !strings.Contains(out, `"referenced_users":{}`) {
		t.Errorf("gone user: exit %d, out %s", code, out)
	}
	if !strings.Contains(errOut, "U1") || !strings.Contains(errOut, "user_not_found") {
		t.Errorf("stderr %q should say which user was skipped", errOut)
	}
}

func TestReferencedUsersEmptyObjectOnlyWithFlag(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/conversations.history": readFixed(`{"ok":true,"messages":[{"type":"message","text":"no author","ts":"1700000001.000100"}]}`),
	})
	code, out, errOut := runRead(t, srv, "message", "list", "C123", "--resolve-users")
	if code != 0 || !strings.HasSuffix(strings.TrimSpace(out), `,"referenced_users":{}}`) {
		t.Errorf("exit %d, out %s, stderr %s", code, out, errOut)
	}
	_, plain, _ := runRead(t, srv, "message", "list", "C123")
	if strings.Contains(plain, "referenced_users") {
		t.Errorf("referenced_users without --resolve-users: %s", plain)
	}
}

func TestSearchMessagesPassesBotIDAndUsername(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/search.messages": readFixed(
		`{"ok":true,"messages":{"matches":[{"type":"message","channel":{"id":"C5"},"bot_id":"B0BOT","username":"deploy-bot","ts":"1700000001.000100","text":"failed","permalink":"https://acme.slack.com/archives/C5/p1700000001000100"}]}}`)})
	code, out, errOut := runRead(t, srv, "search", "messages", "failed")
	wantOut(t, code, out, errOut, `{"messages":[{"ts":"1700000001.000100","channel_id":"C5","author":{"bot_id":"B0BOT","username":"deploy-bot"},"content":"failed","permalink":"https://acme.slack.com/archives/C5/p1700000001000100"}]}`)
}

func TestMessageGetLateReplyAsksSlackForThatTS(t *testing.T) {
	const root, reply = "1700000001.000100", "1700000999.000100"
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.replies": func(form url.Values) string {
		if form.Get("oldest") != reply {
			// Reading the whole thread would never reach the reply.
			return `{"ok":true,"has_more":true,"messages":[{"type":"message","user":"U1","text":"root","ts":"` + root + `"}],"response_metadata":{"next_cursor":"X"}}`
		}
		return `{"ok":true,"messages":[{"type":"message","user":"U1","text":"root","ts":"` + root + `"},{"type":"message","user":"U2","text":"late","ts":"` + reply + `","thread_ts":"` + root + `"}]}`
	}})
	code, out, errOut := runRead(t, srv, "message", "get", "C123", "--ts", reply, "--thread-ts", root)
	wantOut(t, code, out, errOut, `{"message":{"ts":"`+reply+`","thread_ts":"`+root+`","channel_id":"C123","author":{"user_id":"U2"},"content":"late"}}`)
	reqs := srv.requestsTo("/api/conversations.replies")
	if len(reqs) != 1 {
		t.Fatalf("%d replies requests, want 1", len(reqs))
	}
	form := reqs[0].form
	if form.Get("ts") != root || form.Get("oldest") != reply || form.Get("latest") != reply || (form.Get("inclusive") != "1" && form.Get("inclusive") != "true") {
		t.Errorf("form = %v", form)
	}
	// Absent: only the root comes back.
	srv = newReadFake(t, map[string]func(url.Values) string{"/api/conversations.replies": readFixed(
		`{"ok":true,"messages":[{"type":"message","user":"U1","text":"root","ts":"` + root + `"}]}`)})
	code, out, errOut = runRead(t, srv, "message", "get", "C123", "--ts", reply, "--thread-ts", root)
	wantFail(t, code, out, errOut, 1, "message_not_found")
}

func TestChannelListClampsLimitTo999(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/users.conversations": readFixed(`{"ok":true,"channels":[]}`)})
	code, out, errOut := runRead(t, srv, "channel", "list", "--limit", "1000")
	wantOut(t, code, out, errOut, `{"channels":[],"next_cursor":""}`)
	if got := srv.requestTo(t, "/api/users.conversations").form.Get("limit"); got != "999" {
		t.Errorf("limit = %q, want 999", got)
	}
}

// Slack may repeat the root at the top of every page of replies.
func TestMessageListThreadDedupesRepeatedRootAndCapsUniqueMessages(t *testing.T) {
	const root = "1700000001.000100"
	msg := func(n int) string {
		return fmt.Sprintf(`{"type":"message","user":"U1","text":"r%d","ts":"17000%05d.000100","thread_ts":"%s"}`, n, 2+n, root)
	}
	// 1300 replies, 199 new ones per page behind a repeated root.
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.replies": func(form url.Values) string {
		page := 0
		if c := form.Get("cursor"); c != "" {
			fmt.Sscanf(c, "P%d", &page)
		}
		parts := []string{`{"type":"message","user":"U1","text":"root","ts":"` + root + `"}`}
		for n := page * 199; n < (page+1)*199 && n < 1300; n++ {
			parts = append(parts, msg(n))
		}
		more := (page+1)*199 < 1300
		return fmt.Sprintf(`{"ok":true,"has_more":%t,"messages":[%s],"response_metadata":{"next_cursor":"P%d"}}`, more, strings.Join(parts, ","), page+1)
	}})
	code, out, errOut := runRead(t, srv, "message", "list", "C123", "--thread-ts", root)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var got struct {
		Messages []struct {
			TS string `json:"ts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 1000 {
		t.Errorf("%d messages, want the 1000 cap of unique ones", len(got.Messages))
	}
	seen := map[string]bool{}
	for _, m := range got.Messages {
		if seen[m.TS] {
			t.Fatalf("ts %s appears twice", m.TS)
		}
		seen[m.TS] = true
	}
	if !seen[root] {
		t.Error("root missing")
	}
}
