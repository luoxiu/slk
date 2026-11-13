package main

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// savedListBody is a saved.list page: a top-level message and a thread
// reply in progress (the reply with a due date), a saved file (not a
// message, so never listed), a completed DM message and an archived
// message that can no longer be read.
const savedListBody = `{"ok":true,"saved_items":[
 {"item_id":"C123","item_type":"message","ts":"1700000001.000200","state":"in_progress","date_created":1700001000,"date_due":0,"date_completed":0},
 {"item_id":"C123","item_type":"message","ts":"1700000005.000300","state":"in_progress","date_created":1700002000,"date_due":1700050000,"date_completed":0},
 {"item_id":"F1","item_type":"file","state":"in_progress","date_created":1700003000},
 {"item_id":"D9","item_type":"message","ts":"1700000009.000900","state":"completed","date_created":1700004000,"date_completed":1700005000},
 {"item_id":"C123","item_type":"message","ts":"1700000010.000100","state":"archived","date_created":1700006000}
],"counts":{"uncompleted_count":2,"archived_count":1,"completed_count":1,"total_count":4},
"response_metadata":{"next_cursor":""}}`

const savedCountsOut = `{"in_progress":2,"archived":1,"completed":1,"total":4}`

// laterReadHandlers answer what `later list` asks about each item: the
// conversation (a DM's peer too) and the message, by its own ts, from
// conversations.replies, which finds a thread reply as well.
func laterReadHandlers(list func(url.Values) string) map[string]func(url.Values) string {
	return map[string]func(url.Values) string{
		"/api/saved.list": list,
		"/api/conversations.info": func(form url.Values) string {
			switch form.Get("channel") {
			case "C123":
				return `{"ok":true,"channel":{"id":"C123","name":"general","is_channel":true}}`
			case "D9":
				return `{"ok":true,"channel":{"id":"D9","is_im":true,"user":"U2"}}`
			}
			return `{"ok":false,"error":"channel_not_found"}`
		},
		"/api/users.info": readFixed(`{"ok":true,"user":{"id":"U2","name":"ann","real_name":"Ann A","profile":{"display_name":"annie"}}}`),
		"/api/conversations.replies": func(form url.Values) string {
			switch form.Get("ts") {
			case "1700000001.000200":
				return `{"ok":true,"has_more":false,"messages":[{"type":"message","user":"U1","text":"top","ts":"1700000001.000200"}]}`
			case "1700000005.000300":
				return `{"ok":true,"has_more":false,"messages":[` +
					`{"type":"message","user":"U1","text":"root","ts":"1700000001.000200","thread_ts":"1700000001.000200","reply_count":1},` +
					`{"type":"message","user":"U2","text":"a reply","ts":"1700000005.000300","thread_ts":"1700000001.000200"}]}`
			case "1700000009.000900":
				return `{"ok":true,"has_more":false,"messages":[{"type":"message","user":"U2","text":"dm *text*","ts":"1700000009.000900"}]}`
			}
			return `{"ok":false,"error":"thread_not_found"}`
		},
	}
}

const (
	laterTopOut = `{"channel_id":"C123","channel_name":"general","ts":"1700000001.000200","state":"in_progress","date_saved":1700001000,` +
		`"message":{"ts":"1700000001.000200","channel_id":"C123","author":{"user_id":"U1"},"content":"top"}}`
	laterReplyOut = `{"channel_id":"C123","channel_name":"general","ts":"1700000005.000300","state":"in_progress","date_saved":1700002000,"date_due":1700050000,` +
		`"message":{"ts":"1700000005.000300","thread_ts":"1700000001.000200","channel_id":"C123","author":{"user_id":"U2"},"content":"a reply"}}`
	laterDMOut = `{"channel_id":"D9","channel_name":"annie","ts":"1700000009.000900","state":"completed","date_saved":1700004000,"date_completed":1700005000,` +
		`"message":{"ts":"1700000009.000900","channel_id":"D9","author":{"user_id":"U2"},"content":"dm *text*"}}`
	laterGoneOut = `{"channel_id":"C123","channel_name":"general","ts":"1700000010.000100","state":"archived","date_saved":1700006000}`
)

// ---------------------------------------------------------------------
// later list
// ---------------------------------------------------------------------

func TestLaterListInProgressWithThreadReply(t *testing.T) {
	srv := newFormFake(t, laterReadHandlers(readFixed(savedListBody)))
	code, out, errOut := runRead(t, srv, "later", "list")
	wantOut(t, code, out, errOut, `{"counts":`+savedCountsOut+`,"items":[`+laterTopOut+`,`+laterReplyOut+`]}`)

	if got := srv.requestTo(t, "/api/saved.list").form.Get("limit"); got != "50" {
		t.Errorf("saved.list limit = %q, want 50", got)
	}
	// One conversations.info per conversation, however many items.
	if n := srv.count("/api/conversations.info"); n != 1 {
		t.Errorf("%d conversations.info calls, want 1", n)
	}
	// The message is asked for by its own ts, exactly: the reply is found
	// although its thread is not known.
	for _, r := range srv.requestsTo("/api/conversations.replies") {
		ts := r.form.Get("ts")
		if r.form.Get("channel") != "C123" || r.form.Get("oldest") != ts || r.form.Get("latest") != ts || r.form.Get("inclusive") != "1" {
			t.Errorf("conversations.replies form = %v", r.form)
		}
	}
}

func TestLaterListAllStatesDMAndUnreadableMessage(t *testing.T) {
	srv := newFormFake(t, laterReadHandlers(readFixed(savedListBody)))
	code, out, errOut := runRead(t, srv, "later", "list", "--state", "all", "--limit", "10")
	wantOut(t, code, out, errOut, `{"counts":`+savedCountsOut+`,"items":[`+laterTopOut+`,`+laterReplyOut+`,`+laterDMOut+`,`+laterGoneOut+`]}`)
	// The message that could not be read is left out with a line on
	// stderr; the command still succeeds.
	if !strings.Contains(errOut, "1700000010.000100") || !strings.Contains(errOut, "thread_not_found") {
		t.Errorf("stderr %q does not name the unreadable message", errOut)
	}
}

func TestLaterListStateFilterAndMaxBody(t *testing.T) {
	srv := newFormFake(t, laterReadHandlers(readFixed(savedListBody)))
	code, out, errOut := runRead(t, srv, "later", "list", "--max-body-chars", "2", "--state", "completed")
	cut := strings.Replace(laterDMOut, `"content":"dm *text*"`, `"content":"dm\n…"`, 1)
	wantOut(t, code, out, errOut, `{"counts":`+savedCountsOut+`,"items":[`+cut+`]}`)
}

func TestLaterListPagesUntilLimit(t *testing.T) {
	page := func(cursor string) string {
		if cursor == "" {
			return `{"ok":true,"saved_items":[{"item_id":"C123","item_type":"message","ts":"1700000001.000200","state":"in_progress","date_created":1700001000}],` +
				`"counts":{"uncompleted_count":2,"archived_count":1,"completed_count":1,"total_count":4},"response_metadata":{"next_cursor":"p2"}}`
		}
		return `{"ok":true,"saved_items":[{"item_id":"C123","item_type":"message","ts":"1700000005.000300","state":"in_progress","date_created":1700002000,"date_due":1700050000}],` +
			`"counts":{"uncompleted_count":9,"archived_count":9,"completed_count":9,"total_count":9},"response_metadata":{"next_cursor":"p3"}}`
	}
	srv := newFormFake(t, laterReadHandlers(func(form url.Values) string { return page(form.Get("cursor")) }))
	code, out, errOut := runRead(t, srv, "later", "list", "--limit", "2")
	// counts come from the first page; the cursor is the last page's.
	wantOut(t, code, out, errOut, `{"counts":`+savedCountsOut+`,"items":[`+laterTopOut+`,`+laterReplyOut+`]}`)
	reqs := srv.requestsTo("/api/saved.list")
	if len(reqs) != 2 || reqs[1].form.Get("cursor") != "p2" {
		t.Errorf("%d saved.list calls, want 2 with the second at cursor p2", len(reqs))
	}

	// One page already holds --limit 1: no second page.
	srv = newFormFake(t, laterReadHandlers(func(form url.Values) string { return page(form.Get("cursor")) }))
	code, _, errOut = runRead(t, srv, "later", "list", "--limit", "1")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	if n := srv.count("/api/saved.list"); n != 1 {
		t.Errorf("%d saved.list calls, want 1", n)
	}
}

func TestLaterListCountsOnly(t *testing.T) {
	srv := newFormFake(t, map[string]func(url.Values) string{"/api/saved.list": readFixed(savedListBody)})
	code, out, errOut := runRead(t, srv, "later", "list", "--counts-only")
	wantOut(t, code, out, errOut, `{"counts":`+savedCountsOut+`}`)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.reqs) != 2 { // auth.test and one saved.list
		t.Errorf("%d requests, want auth.test and one saved.list", len(srv.reqs))
	}
}

func TestLaterListErrors(t *testing.T) {
	srv := newFormFake(t, map[string]func(url.Values) string{"/api/saved.list": readFixed(`{"ok":false,"error":"invalid_auth"}`)})
	code, out, errOut := runRead(t, srv, "later", "list")
	wantFail(t, code, out, errOut, 2, "invalid_auth")

	for _, args := range [][]string{
		{"later", "list", "--state", "done"},
		{"later", "list", "--limit", "0"},
		{"later", "list", "C123"},
	} {
		env, stdout, stderr := agentTestEnv(nil, noOpener(t))
		code := runAgent(context.Background(), env, args)
		if code != 1 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Errorf("%q: exit %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
}

// ---------------------------------------------------------------------
// later writes
// ---------------------------------------------------------------------

func laterWriteFake(t *testing.T, update func(url.Values) string) *fakeSlack {
	if update == nil {
		update = readFixed(`{"ok":true}`)
	}
	return newFormFake(t, map[string]func(url.Values) string{
		"/api/saved.add":    readFixed(`{"ok":true}`),
		"/api/saved.delete": readFixed(`{"ok":true}`),
		"/api/saved.update": update,
		"/api/users.conversations": readFixed(`{"ok":true,"channels":[{"id":"C123","name":"general","is_channel":true}],` +
			`"response_metadata":{"next_cursor":""}}`),
	})
}

// The target forms every later write takes, and the ts each gives.
var laterTargets = []struct {
	name   string
	args   []string
	wantTS string
}{
	{"link", []string{writeMsgLink}, "1700000001.000200"},
	{"link into a thread", []string{writeThreadLink}, "1700000005.000300"},
	{"channel ID, --ts first", []string{"--ts", "1700000009.000900", "C123"}, "1700000009.000900"},
	{"#name, --ts after", []string{"#general", "--ts", "1700000009.000900"}, "1700000009.000900"},
}

func TestLaterWrites(t *testing.T) {
	cases := []struct {
		verb     string
		path     string
		wantCT   string
		wantMark string
	}{
		{"save", "/api/saved.add", "application/x-www-form-urlencoded", ""},
		{"remove", "/api/saved.delete", "application/x-www-form-urlencoded", ""},
		// saved.update ignores mark and date_due sent form-encoded
		// (agent-slack PR #76), so it goes multipart.
		{"complete", "/api/saved.update", "multipart/form-data", "completed"},
		{"archive", "/api/saved.update", "multipart/form-data", "archived"},
	}
	for _, tc := range cases {
		for _, target := range laterTargets {
			t.Run(tc.verb+" "+target.name, func(t *testing.T) {
				srv := laterWriteFake(t, nil)
				code, out, errOut := runRead(t, srv, append([]string{"later", tc.verb}, target.args...)...)
				wantOut(t, code, out, errOut, `{"ok":true,"channel_id":"C123","ts":"`+target.wantTS+`"}`)
				req := srv.requestTo(t, tc.path)
				if ct := contentType(req); ct != tc.wantCT {
					t.Errorf("Content-Type %q, want %q", ct, tc.wantCT)
				}
				f := req.form
				if f.Get("item_id") != "C123" || f.Get("item_type") != "message" || f.Get("ts") != target.wantTS || f.Get("mark") != tc.wantMark {
					t.Errorf("form = %v", f)
				}
				if f.Get("token") != "xoxc-test" {
					t.Errorf("token not in the body")
				}
				if n := srv.count(tc.path); n != 1 {
					t.Errorf("%d calls to %s, want 1", n, tc.path)
				}
			})
		}
	}
}

// reopen does not know whether the item is completed or archived, so it
// sends both marks, as agent-slack does; one taking is success.
func TestLaterReopen(t *testing.T) {
	cases := []struct {
		name       string
		answers    map[string]string // mark -> response
		wantCode   int
		wantStderr string
	}{
		{"both ok", map[string]string{"uncompleted": `{"ok":true}`, "unarchived": `{"ok":true}`}, 0, ""},
		{"only one applies", map[string]string{"uncompleted": `{"ok":false,"error":"invalid_state"}`, "unarchived": `{"ok":true}`}, 0, ""},
		{"neither", map[string]string{"uncompleted": `{"ok":false,"error":"not_found"}`, "unarchived": `{"ok":false,"error":"not_found"}`}, 1, "not_found"},
		{"auth", map[string]string{"uncompleted": `{"ok":false,"error":"invalid_auth"}`, "unarchived": `{"ok":false,"error":"invalid_auth"}`}, 2, "invalid_auth"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := laterWriteFake(t, func(form url.Values) string { return tc.answers[form.Get("mark")] })
			code, out, errOut := runRead(t, srv, "later", "reopen", writeMsgLink)
			if tc.wantCode == 0 {
				wantOut(t, code, out, errOut, `{"ok":true,"channel_id":"C123","ts":"1700000001.000200"}`)
			} else {
				wantFail(t, code, out, errOut, tc.wantCode, tc.wantStderr)
			}
			reqs := srv.requestsTo("/api/saved.update")
			if tc.wantCode == 2 {
				return
			}
			if len(reqs) != 2 || reqs[0].form.Get("mark") != "uncompleted" || reqs[1].form.Get("mark") != "unarchived" {
				t.Errorf("saved.update calls = %d, want uncompleted then unarchived", len(reqs))
			}
		})
	}
}

func TestLaterRemind(t *testing.T) {
	at := "2026-11-14T09:00:00+08:00"
	atTime, _ := time.Parse(time.RFC3339, at)
	cases := []struct {
		name   string
		args   []string
		wantAt int64 // 0: --in 30m, checked against the clock
	}{
		{"--in", []string{writeMsgLink, "--in", "30m"}, 0},
		{"--at RFC 3339, flag first", []string{"--at", at, writeMsgLink}, atTime.Unix()},
		{"--at Unix seconds", []string{writeMsgLink, "--at", "1800000000"}, 1800000000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := laterWriteFake(t, nil)
			before := time.Now().Unix()
			code, out, errOut := runRead(t, srv, append([]string{"later", "remind"}, tc.args...)...)
			after := time.Now().Unix()
			if code != 0 {
				t.Fatalf("exit %d, stderr %s", code, errOut)
			}
			req := srv.requestTo(t, "/api/saved.update")
			if contentType(req) != "multipart/form-data" {
				t.Errorf("Content-Type %q, want multipart/form-data", contentType(req))
			}
			due, err := strconv.ParseInt(req.form.Get("date_due"), 10, 64)
			if err != nil {
				t.Fatalf("date_due %q: %v", req.form.Get("date_due"), err)
			}
			if tc.wantAt == 0 {
				if due < before+1800 || due > after+1800 {
					t.Errorf("date_due %d, want now+1800 (%d..%d)", due, before+1800, after+1800)
				}
			} else if due != tc.wantAt {
				t.Errorf("date_due %d, want %d", due, tc.wantAt)
			}
			if _, ok := req.form["mark"]; ok || req.form.Get("item_id") != "C123" || req.form.Get("ts") != "1700000001.000200" {
				t.Errorf("form = %v", req.form)
			}
			var got struct {
				OK        bool   `json:"ok"`
				ChannelID string `json:"channel_id"`
				TS        string `json:"ts"`
				DateDue   int64  `json:"date_due"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil || !got.OK || got.ChannelID != "C123" || got.TS != "1700000001.000200" || got.DateDue != due {
				t.Errorf("stdout %q (%v)", out, err)
			}
		})
	}
}

func TestParseLaterDue(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cases := []struct {
		in, at  string
		want    int64
		wantErr bool
	}{
		{"30m", "", 1700000000 + 30*60, false},
		{"2h", "", 1700000000 + 2*3600, false},
		{"3d", "", 1700000000 + 3*86400, false},
		{"", "1800000000", 1800000000, false},
		{"", "2026-11-14T01:00:00Z", 1794618000, false},
		{"", "", 0, true},
		{"30m", "1800000000", 0, true},
		{"30", "", 0, true},
		{"0m", "", 0, true},
		{"1w", "", 0, true},
		{"-5m", "", 0, true},
		{"", "tomorrow", 0, true},
		{"", "0", 0, true},
	}
	for _, tc := range cases {
		got, err := parseLaterDue(tc.in, tc.at, now)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("parseLaterDue(%q, %q) = %d, %v; want %d, err=%v", tc.in, tc.at, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestLaterWriteUsageErrorsBeforeConnecting(t *testing.T) {
	for _, args := range [][]string{
		{"later", "save", "C123"}, // a channel needs --ts
		{"later", "complete"},
		{"later", "archive", "C123", "--ts", "noon"},
		{"later", "remove", writeMsgLink, "extra"},
		{"later", "remind", writeMsgLink},
		{"later", "remind", writeMsgLink, "--in", "30m", "--at", "1800000000"},
		{"later", "remind", writeMsgLink, "--in", "soon"},
		{"later", "reopen", "https://example.com/x"},
	} {
		env, stdout, stderr := agentTestEnv(nil, noOpener(t))
		code := runAgent(context.Background(), env, args)
		if code != 1 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Errorf("%q: exit %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
}

func TestLaterWriteSlackErrors(t *testing.T) {
	cases := []struct {
		body     string
		wantCode int
		want     string
	}{
		{`{"ok":false,"error":"not_found"}`, 1, "not_found"},
		{`{"ok":false,"error":"token_revoked"}`, 2, "token_revoked"},
	}
	for _, tc := range cases {
		srv := newFormFake(t, map[string]func(url.Values) string{"/api/saved.update": readFixed(tc.body)})
		code, out, errOut := runRead(t, srv, "later", "complete", writeMsgLink)
		wantFail(t, code, out, errOut, tc.wantCode, tc.want)
	}
}

// ---------------------------------------------------------------------
// Safe mode and help
// ---------------------------------------------------------------------

// Later's writes and channel mark change only what he sees, but an
// unattended agent getting them wrong hides messages from him, so safe
// mode refuses them like any write, before connecting.
func TestLaterAndMarkRefusedBySafeMode(t *testing.T) {
	for _, args := range [][]string{
		{"later", "save", writeMsgLink},
		{"later", "complete", writeMsgLink},
		{"later", "archive", writeMsgLink},
		{"later", "reopen", writeMsgLink},
		{"later", "remove", writeMsgLink},
		{"later", "remind", writeMsgLink, "--in", "1h"},
		{"later", "remind"}, // refused even with bad arguments
		{"channel", "mark", writeMsgLink},
		{"channel", "mark", "C123", "--thread-ts", "1700000001.000200"},
	} {
		env, stdout, stderr := agentTestEnv(map[string]string{"SLK_SAFE_MODE": "1"}, noOpener(t))
		code := runAgent(context.Background(), env, args)
		if code != 1 || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "safe mode:") {
			t.Errorf("%q: exit %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
}

func TestLaterListAllowedInSafeMode(t *testing.T) {
	srv := newFormFake(t, map[string]func(url.Values) string{"/api/saved.list": readFixed(savedListBody)})
	code, out, errOut := runAgentWith(t, srv, map[string]string{"SLK_SAFE_MODE": "1"}, "later", "list", "--counts-only")
	wantOut(t, code, out, errOut, `{"counts":`+savedCountsOut+`}`)
}

func TestLaterHelp(t *testing.T) {
	for _, args := range [][]string{{"later", "--help"}, {"later", "help"}} {
		env, stdout, _ := agentTestEnv(nil, noOpener(t))
		if code := runAgent(context.Background(), env, args); code != 0 {
			t.Errorf("%q: exit %d, want 0", args, code)
		}
		for _, want := range []string{
			"slk later list [--state in_progress|archived|completed|all] [--limit N] [--max-body-chars N] [--counts-only]",
			"slk later save <target> [--ts TS]",
			"slk later complete <target> [--ts TS]",
			"slk later archive <target> [--ts TS]",
			"slk later reopen <target> [--ts TS]",
			"slk later remove <target> [--ts TS]",
			"slk later remind <target> [--ts TS] (--in 30m|2h|3d | --at RFC3339|UNIX)",
			"slk channel mark <target> [--ts TS] [--thread-ts TS]",
		} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("%q: usage lacks %q", args, want)
			}
		}
	}
	// A subcommand's own --help prints its usage and flags.
	env, _, stderr := agentTestEnv(nil, noOpener(t))
	if code := runAgent(context.Background(), env, []string{"later", "remind", "--help"}); code != 0 {
		t.Errorf("later remind --help: exit %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: slk later remind <target>") || !strings.Contains(stderr.String(), "-in") {
		t.Errorf("later remind --help:\n%s", stderr)
	}
}

// saved.list leaves completed items out unless asked with
// filter=completed (seen live 2026-11-13: a completed item vanished from
// the default list and came back on reopen). --state completed asks for
// them; --state all reads both lists, each item once.
func TestLaterListCompletedNeedsFilter(t *testing.T) {
	open := `{"ok":true,"saved_items":[
 {"item_id":"C123","item_type":"message","ts":"1700000001.000200","state":"in_progress","date_created":1700001000},
 {"item_id":"C123","item_type":"message","ts":"1700000010.000100","state":"archived","date_created":1700006000}
],"counts":{"uncompleted_count":1,"archived_count":1,"completed_count":1,"total_count":3},"response_metadata":{"next_cursor":""}}`
	done := `{"ok":true,"saved_items":[
 {"item_id":"D9","item_type":"message","ts":"1700000009.000900","state":"completed","date_created":1700004000,"date_completed":1700005000}
],"counts":{"uncompleted_count":1,"archived_count":1,"completed_count":1,"total_count":3},"response_metadata":{"next_cursor":""}}`
	list := func(form url.Values) string {
		if form.Get("filter") == "completed" {
			return done
		}
		return open
	}
	tsOf := func(out string) []string {
		var got struct {
			Items []struct {
				TS    string `json:"ts"`
				State string `json:"state"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decoding %s: %v", out, err)
		}
		var ts []string
		for _, it := range got.Items {
			ts = append(ts, it.State+"/"+it.TS)
		}
		return ts
	}
	for _, tc := range []struct {
		state   string
		want    string
		filters string
	}{
		{"in_progress", "in_progress/1700000001.000200", ""},
		{"completed", "completed/1700000009.000900", "completed"},
		{"all", "in_progress/1700000001.000200 archived/1700000010.000100 completed/1700000009.000900", ",completed"},
	} {
		srv := newFormFake(t, laterReadHandlers(list))
		code, out, errOut := runRead(t, srv, "later", "list", "--state", tc.state)
		if code != 0 {
			t.Fatalf("--state %s: exit %d, stderr %s", tc.state, code, errOut)
		}
		if got := strings.Join(tsOf(out), " "); got != tc.want {
			t.Errorf("--state %s: items %s, want %s", tc.state, got, tc.want)
		}
		var filters []string
		for _, r := range srv.requestsTo("/api/saved.list") {
			filters = append(filters, r.form.Get("filter"))
		}
		if got := strings.Join(filters, ","); got != tc.filters {
			t.Errorf("--state %s: saved.list filters %q, want %q", tc.state, got, tc.filters)
		}
	}
}
