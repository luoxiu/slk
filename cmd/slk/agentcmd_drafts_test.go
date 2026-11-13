package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// newFormFake is newReadFake that also reads multipart/form-data
// bodies into the recorded form (saved.update is sent multipart), so a
// test sees the fields whichever encoding a request used and tells the
// two apart by the recorded Content-Type. auth.test is answered.
func newFormFake(t *testing.T, handlers map[string]func(form url.Values) string) *fakeSlack {
	t.Helper()
	f := &fakeSlack{}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("%s: reading multipart body: %v", r.URL.Path, err)
			}
		} else {
			_ = r.ParseForm()
		}
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
		_, _ = w.Write([]byte(h(r.PostForm)))
	}))
	t.Cleanup(f.Close)
	return f
}

// runAgentWith runs one agent command against srv with the given
// environment variables.
func runAgentWith(t *testing.T, srv *fakeSlack, vars map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var calls []openCall
	env, stdout, stderr := agentTestEnv(vars, fakeAgentOpener(t, srv, &calls))
	code := runAgent(context.Background(), env, args)
	return code, stdout.String(), stderr.String()
}

func contentType(r recordedRequest) string {
	ct, _, _ := strings.Cut(r.header.Get("Content-Type"), ";")
	return ct
}

// A draft as drafts.list returns it, with the fields the desktop app's
// own drafts carry (read from a real account on 2026-11-13).
const (
	draftChannelJSON = `{"id":"Dr01","client_msg_id":"6c1f0b7e-0000-4000-8000-000000000001","user_id":"U1","team_id":"T1",` +
		`"date_created":1700000000,"date_scheduled":0,"last_updated_ts":"1700000100.123","last_updated_client":"desktop",` +
		`"is_from_composer":false,"is_deleted":false,"is_sent":false,"file_ids":[],` +
		`"destinations":[{"channel_id":"C123","broadcast":false}],` +
		`"blocks":[{"type":"rich_text","block_id":"b1","elements":[{"type":"rich_text_section","elements":[` +
		`{"type":"text","text":"hello "},{"type":"text","text":"world","style":{"bold":true}}]}]}]}`
	draftThreadJSON = `{"id":"Dr02","client_msg_id":"6c1f0b7e-0000-4000-8000-000000000002","user_id":"U1","team_id":"T1",` +
		`"date_created":1700000200,"date_scheduled":0,"last_updated_ts":"1700000300.1234560","last_updated_client":"desktop",` +
		`"is_from_composer":false,"is_deleted":false,"is_sent":false,"file_ids":["F1"],` +
		`"destinations":[{"channel_id":"C123","thread_ts":"1700000001.000200","broadcast":false}],` +
		`"blocks":[{"type":"rich_text","block_id":"b2","elements":[{"type":"rich_text_list","style":"bullet","indent":0,"elements":[` +
		`{"type":"rich_text_section","elements":[{"type":"text","text":"one"}]},` +
		`{"type":"rich_text_section","elements":[{"type":"text","text":"two"}]}]}]}]}`
	draftScheduledJSON = `{"id":"Dr03","date_created":1700000400,"date_scheduled":1700090000,"last_updated_ts":"1700000500.000001",` +
		`"is_deleted":false,"is_sent":false,"file_ids":[],"destinations":[{"channel_id":"D9","broadcast":false}],` +
		`"blocks":[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"later"}]}]}]}`
	draftSentJSON = `{"id":"Dr04","date_created":1700000600,"last_updated_ts":"1700000700.5","is_deleted":false,"is_sent":true,` +
		`"destinations":[{"channel_id":"C123","broadcast":false}],"blocks":[]}`
	draftDeletedJSON = `{"id":"Dr05","date_created":1700000800,"last_updated_ts":"1700000900.5","is_deleted":true,"is_sent":false,` +
		`"destinations":[{"channel_id":"C123","broadcast":false}],"blocks":[]}`

	draftChannelOut = `{"id":"Dr01","text":"hello **world**","destinations":[{"channel_id":"C123"}],` +
		`"last_updated_ts":"1700000100.123","date_created":1700000000}`
	draftThreadOut = `{"id":"Dr02","text":"- one\n- two","destinations":[{"channel_id":"C123","thread_ts":"1700000001.000200"}],` +
		`"last_updated_ts":"1700000300.1234560","date_created":1700000200,"file_ids":["F1"]}`
	draftScheduledOut = `{"id":"Dr03","text":"later","destinations":[{"channel_id":"D9"}],` +
		`"last_updated_ts":"1700000500.000001","date_created":1700000400,"date_scheduled":1700090000}`
)

func draftsListBody(drafts ...string) string {
	return `{"ok":true,"drafts":[` + strings.Join(drafts, ",") + `],"response_metadata":{"next_cursor":""}}`
}

var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// ---------------------------------------------------------------------
// draft list
// ---------------------------------------------------------------------

func TestDraftListOutputShape(t *testing.T) {
	srv := newFormFake(t, map[string]func(url.Values) string{
		"/api/drafts.list": readFixed(draftsListBody(draftChannelJSON, draftThreadJSON, draftScheduledJSON)),
	})
	code, out, errOut := runRead(t, srv, "message", "draft", "list")
	wantOut(t, code, out, errOut, `{"drafts":[`+draftChannelOut+`,`+draftThreadOut+`,`+draftScheduledOut+`]}`)
	form := srv.requestTo(t, "/api/drafts.list").form
	if form.Get("is_active") != "true" || form.Get("limit") != "20" {
		t.Errorf("drafts.list form = %v, want is_active=true limit=20", form)
	}
}

func TestDraftListAllAndLimit(t *testing.T) {
	for _, args := range [][]string{
		{"message", "draft", "list", "--all", "--limit", "2"},
		{"message", "draft", "list", "--limit=2", "--all"},
	} {
		srv := newFormFake(t, map[string]func(url.Values) string{
			"/api/drafts.list": readFixed(draftsListBody(draftChannelJSON, draftThreadJSON, draftScheduledJSON)),
		})
		code, out, errOut := runRead(t, srv, args...)
		// Slack sent three; --limit cuts the output to two.
		wantOut(t, code, out, errOut, `{"drafts":[`+draftChannelOut+`,`+draftThreadOut+`]}`)
		form := srv.requestTo(t, "/api/drafts.list").form
		if _, ok := form["is_active"]; ok || form.Get("limit") != "2" {
			t.Errorf("%q: drafts.list form = %v, want no is_active and limit=2", args, form)
		}
	}
}

func TestDraftListFollowsCursorUpToLimit(t *testing.T) {
	srv := newFormFake(t, map[string]func(url.Values) string{
		"/api/drafts.list": func(form url.Values) string {
			if form.Get("cursor") == "" {
				return `{"ok":true,"drafts":[` + draftChannelJSON + `],"response_metadata":{"next_cursor":"page2"}}`
			}
			return draftsListBody(draftThreadJSON)
		},
	})
	code, out, errOut := runRead(t, srv, "message", "draft", "list", "--limit", "5")
	wantOut(t, code, out, errOut, `{"drafts":[`+draftChannelOut+`,`+draftThreadOut+`]}`)
	reqs := srv.requestsTo("/api/drafts.list")
	if len(reqs) != 2 || reqs[1].form.Get("cursor") != "page2" {
		t.Errorf("drafts.list requests = %d, second cursor %q", len(reqs), reqs[len(reqs)-1].form.Get("cursor"))
	}
}

func TestDraftListEmptyPrintsEmptyArray(t *testing.T) {
	srv := newFormFake(t, map[string]func(url.Values) string{"/api/drafts.list": readFixed(draftsListBody())})
	code, out, errOut := runRead(t, srv, "message", "draft", "list")
	wantOut(t, code, out, errOut, `{"drafts":[]}`)
}

// ---------------------------------------------------------------------
// draft create
// ---------------------------------------------------------------------

func draftCreateFake(t *testing.T) *fakeSlack {
	return newFormFake(t, map[string]func(url.Values) string{
		"/api/drafts.create": readFixed(`{"ok":true,"draft":` + draftThreadJSON + `}`),
		"/api/users.conversations": readFixed(`{"ok":true,"channels":[{"id":"C123","name":"general","is_channel":true}],` +
			`"response_metadata":{"next_cursor":""}}`),
	})
}

func TestDraftCreateTargets(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantDest string
	}{
		{"channel ID", []string{"C123", "hi"}, `[{"channel_id":"C123","broadcast":false}]`},
		{"#name, flag first", []string{"--workspace", "acme", "#general", "hi"}, `[{"channel_id":"C123","broadcast":false}]`},
		{"link to a top-level message drafts in its thread", []string{writeMsgLink, "hi"},
			`[{"channel_id":"C123","thread_ts":"1700000001.000200","broadcast":false}]`},
		{"link into a thread drafts in that thread", []string{writeThreadLink, "hi"},
			`[{"channel_id":"C123","thread_ts":"1700000001.000200","broadcast":false}]`},
		{"--thread-ts after the text", []string{"C123", "hi", "--thread-ts", "1700000002.000900"},
			`[{"channel_id":"C123","thread_ts":"1700000002.000900","broadcast":false}]`},
		{"--broadcast with a thread", []string{writeThreadLink, "hi", "--broadcast"},
			`[{"channel_id":"C123","thread_ts":"1700000001.000200","broadcast":true}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := draftCreateFake(t)
			code, out, errOut := runRead(t, srv, append([]string{"message", "draft", "create"}, tc.args...)...)
			wantOut(t, code, out, errOut, `{"ok":true,"draft":`+draftThreadOut+`}`)
			req := srv.requestTo(t, "/api/drafts.create")
			if ct := contentType(req); ct != "application/x-www-form-urlencoded" {
				t.Errorf("Content-Type %q", ct)
			}
			form := req.form
			if got := form.Get("destinations"); got != tc.wantDest {
				t.Errorf("destinations = %s\nwant          %s", got, tc.wantDest)
			}
			if form.Get("is_from_composer") != "false" {
				t.Errorf("is_from_composer = %q, want false (the desktop app's value)", form.Get("is_from_composer"))
			}
			if form.Get("file_ids") != "[]" {
				t.Errorf("file_ids = %q, want []", form.Get("file_ids"))
			}
			if !uuidV4Re.MatchString(form.Get("client_msg_id")) {
				t.Errorf("client_msg_id = %q, want a v4 UUID", form.Get("client_msg_id"))
			}
			var blocks []map[string]any
			if err := json.Unmarshal([]byte(form.Get("blocks")), &blocks); err != nil || len(blocks) != 1 || blocks[0]["type"] != "rich_text" {
				t.Errorf("blocks = %s (%v), want one rich_text block", form.Get("blocks"), err)
			}
			if !strings.Contains(form.Get("blocks"), `"text":"hi"`) {
				t.Errorf("blocks = %s, want the text in it", form.Get("blocks"))
			}
		})
	}
}

// The text goes through mrkdwn.Convert, as message send does: markdown
// becomes rich_text styles, not literal asterisks.
func TestDraftCreateConvertsMarkdown(t *testing.T) {
	srv := draftCreateFake(t)
	code, _, errOut := runRead(t, srv, "message", "draft", "create", "C123", "--", "- **bold** item")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	blocks := srv.requestTo(t, "/api/drafts.create").form.Get("blocks")
	if !strings.Contains(blocks, `"rich_text_list"`) || !strings.Contains(blocks, `"bold":true`) || strings.Contains(blocks, "**") {
		t.Errorf("blocks = %s, want a bullet list with a bold run", blocks)
	}
}

func TestDraftCreateUsageErrorsBeforeConnecting(t *testing.T) {
	for _, args := range [][]string{
		{"message", "draft", "create", "C123"},
		{"message", "draft", "create", "C123", "  "},
		{"message", "draft", "create", "C123", "a", "b"},
		{"message", "draft", "create", "C123", "hi", "--broadcast"}, // no thread to broadcast from
		{"message", "draft", "create", "C123", "hi", "--thread-ts", "yesterday"},
		{"message", "draft", "create", "https://example.com/x", "hi"},
	} {
		env, stdout, stderr := agentTestEnv(nil, noOpener(t))
		code := runAgent(context.Background(), env, args)
		if code != 1 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Errorf("%q: exit %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
}

// ---------------------------------------------------------------------
// draft update and delete
// ---------------------------------------------------------------------

func TestDraftUpdateKeepsDestinationAndFiles(t *testing.T) {
	srv := newFormFake(t, map[string]func(url.Values) string{
		"/api/drafts.list":   readFixed(draftsListBody(draftChannelJSON, draftThreadJSON)),
		"/api/drafts.update": readFixed(`{"ok":true,"draft":` + draftThreadJSON + `}`),
	})
	code, out, errOut := runRead(t, srv, "message", "draft", "update", "Dr02", "new text")
	wantOut(t, code, out, errOut, `{"ok":true,"draft":`+draftThreadOut+`}`)

	// The draft is looked up among the active drafts, and found there,
	// so all drafts are not read.
	if reqs := srv.requestsTo("/api/drafts.list"); len(reqs) != 1 || reqs[0].form.Get("is_active") != "true" {
		t.Errorf("drafts.list for the lookup: %d requests, first form %v; want one, is_active=true", len(reqs), reqs[0].form)
	}
	form := srv.requestTo(t, "/api/drafts.update").form
	want := map[string]string{
		"draft_id":               "Dr02",
		"destinations":           `[{"channel_id":"C123","thread_ts":"1700000001.000200","broadcast":false}]`,
		"file_ids":               `["F1"]`,
		"client_last_updated_ts": "1700000300.1234560",
	}
	for k, v := range want {
		if form.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, form.Get(k), v)
		}
	}
	// Not needed: an update without it leaves the draft's
	// is_from_composer=false (seen live 2026-11-13, see UpdateDraft).
	if _, ok := form["is_from_composer"]; ok {
		t.Errorf("is_from_composer sent on update; only create sends it")
	}
	if !strings.Contains(form.Get("blocks"), `"text":"new text"`) {
		t.Errorf("blocks = %s", form.Get("blocks"))
	}
	if !uuidV4Re.MatchString(form.Get("client_msg_id")) {
		t.Errorf("client_msg_id = %q", form.Get("client_msg_id"))
	}
}

// client_last_updated_ts goes back padded to 7 decimal places, as
// agent-slack's padDraftTs does: Slack refuses "1700000100.123".
func TestDraftUpdatePadsLastUpdatedTS(t *testing.T) {
	srv := newFormFake(t, map[string]func(url.Values) string{
		"/api/drafts.list":   readFixed(draftsListBody(draftChannelJSON)),
		"/api/drafts.update": readFixed(`{"ok":true,"draft":` + draftChannelJSON + `}`),
	})
	code, _, errOut := runRead(t, srv, "message", "draft", "update", "--", "Dr01", "-x")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	if got := srv.requestTo(t, "/api/drafts.update").form.Get("client_last_updated_ts"); got != "1700000100.1230000" {
		t.Errorf("client_last_updated_ts = %q, want 1700000100.1230000", got)
	}
}

func TestDraftUpdateAndDeleteNotFound(t *testing.T) {
	for _, args := range [][]string{
		{"message", "draft", "update", "Dr99", "x"},
		{"message", "draft", "delete", "Dr99"},
		// A sent or deleted draft is no draft to edit or delete any more.
		{"message", "draft", "update", "Dr04", "x"},
		{"message", "draft", "delete", "Dr04"},
		{"message", "draft", "update", "Dr05", "x"},
		{"message", "draft", "delete", "Dr05"},
	} {
		// No drafts.update or drafts.delete handler: the fake fails the
		// test if either is called.
		srv := newFormFake(t, map[string]func(url.Values) string{
			"/api/drafts.list": readFixed(draftsListBody(draftChannelJSON, draftSentJSON, draftDeletedJSON)),
		})
		code, out, errOut := runRead(t, srv, args...)
		wantFail(t, code, out, errOut, 1, "draft_not_found")
	}
}

// The draft to update or delete is looked up among the active drafts
// first (is_active=true). An account's sent and deleted drafts can fill
// every page the lookup reads (agentDraftPages); a live draft must
// still be found.
func TestDraftLookupFindsActiveDraftPastLongHistory(t *testing.T) {
	for _, args := range [][]string{
		{"message", "draft", "update", "Dr02", "x"},
		{"message", "draft", "delete", "Dr02"},
	} {
		srv := newFormFake(t, map[string]func(url.Values) string{
			"/api/drafts.list": func(form url.Values) string {
				if form.Get("is_active") == "true" {
					return draftsListBody(draftThreadJSON)
				}
				// All drafts: page after page of sent ones, Dr02 never reached.
				n := form.Get("cursor") + "x"
				return `{"ok":true,"drafts":[` + draftSentJSON + `],"response_metadata":{"next_cursor":"` + n + `"}}`
			},
			"/api/drafts.update": readFixed(`{"ok":true,"draft":` + draftThreadJSON + `}`),
			"/api/drafts.delete": readFixed(`{"ok":true}`),
		})
		code, _, errOut := runRead(t, srv, args...)
		if code != 0 {
			t.Fatalf("%q: exit %d, stderr %s", args, code, errOut)
		}
		reqs := srv.requestsTo("/api/drafts.list")
		if len(reqs) != 1 || reqs[0].form.Get("is_active") != "true" {
			t.Errorf("%q: %d drafts.list requests; want one, with is_active=true", args, len(reqs))
		}
	}
}

// A draft the active list does not have is looked for among all drafts,
// only to say it was sent or deleted rather than that there is no such
// draft.
func TestDraftLookupFallsBackToAllDrafts(t *testing.T) {
	srv := newFormFake(t, map[string]func(url.Values) string{
		"/api/drafts.list": func(form url.Values) string {
			if form.Get("is_active") == "true" {
				return draftsListBody(draftChannelJSON)
			}
			return draftsListBody(draftChannelJSON, draftSentJSON)
		},
	})
	code, out, errOut := runRead(t, srv, "message", "draft", "delete", "Dr04")
	wantFail(t, code, out, errOut, 1, "draft_not_found")
	if !strings.Contains(errOut, "already sent or deleted") {
		t.Errorf("stderr %q does not say the draft was sent or deleted", errOut)
	}
	reqs := srv.requestsTo("/api/drafts.list")
	if len(reqs) != 2 || reqs[0].form.Get("is_active") != "true" {
		t.Fatalf("%d drafts.list requests; want two, the first with is_active=true", len(reqs))
	}
	if _, ok := reqs[1].form["is_active"]; ok {
		t.Errorf("the second drafts.list filtered by is_active: %v", reqs[1].form)
	}
}

// Updating a scheduled draft is refused: drafts.update does not carry
// the schedule, so it could drop it and the message would never go out.
func TestDraftUpdateRefusesScheduledDraft(t *testing.T) {
	srv := newFormFake(t, map[string]func(url.Values) string{
		"/api/drafts.list": readFixed(draftsListBody(draftScheduledJSON)),
	})
	code, out, errOut := runRead(t, srv, "message", "draft", "update", "Dr03", "x")
	wantFail(t, code, out, errOut, 1, "scheduled")
}

// Deleting a scheduled draft is refused: it is a message set to go out,
// and deleting it would cancel the send.
func TestDraftDeleteRefusesScheduledDraft(t *testing.T) {
	srv := newFormFake(t, map[string]func(url.Values) string{
		"/api/drafts.list": readFixed(draftsListBody(draftScheduledJSON)),
	})
	code, out, errOut := runRead(t, srv, "message", "draft", "delete", "Dr03")
	wantFail(t, code, out, errOut, 1, "scheduled")
	for _, want := range []string{"cancel", "in Slack"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr %q lacks %q", errOut, want)
		}
	}
}

func TestDraftDelete(t *testing.T) {
	srv := newFormFake(t, map[string]func(url.Values) string{
		"/api/drafts.list":   readFixed(draftsListBody(draftChannelJSON, draftThreadJSON)),
		"/api/drafts.delete": readFixed(`{"ok":true}`),
	})
	code, out, errOut := runRead(t, srv, "message", "draft", "delete", "Dr01")
	wantOut(t, code, out, errOut, `{"ok":true,"id":"Dr01"}`)
	form := srv.requestTo(t, "/api/drafts.delete").form
	if form.Get("draft_id") != "Dr01" || form.Get("client_last_updated_ts") != "1700000100.1230000" {
		t.Errorf("drafts.delete form = %v", form)
	}
}

func TestDraftSlackErrors(t *testing.T) {
	cases := []struct {
		slackErr string
		wantCode int
	}{
		{"invalid_auth", 2},
		{"channel_not_found", 1},
	}
	for _, tc := range cases {
		srv := newFormFake(t, map[string]func(url.Values) string{
			"/api/drafts.create": readFixed(`{"ok":false,"error":"` + tc.slackErr + `"}`),
		})
		code, out, errOut := runRead(t, srv, "message", "draft", "create", "C123", "hi")
		wantFail(t, code, out, errOut, tc.wantCode, tc.slackErr)
	}
}

// Safe mode leaves the agent drafting: listing drafts and creating new
// ones are not refused.
func TestSafeModeAllowsDraftListAndCreate(t *testing.T) {
	safe := map[string]string{"SLK_SAFE_MODE": "1"}
	for _, args := range [][]string{
		{"message", "draft", "list"},
		{"message", "draft", "create", "C123", "hi"},
	} {
		srv := newFormFake(t, map[string]func(url.Values) string{
			"/api/drafts.list":   readFixed(draftsListBody(draftChannelJSON)),
			"/api/drafts.create": readFixed(`{"ok":true,"draft":` + draftChannelJSON + `}`),
		})
		code, out, errOut := runAgentWith(t, srv, safe, args...)
		if code != 0 || out == "" {
			t.Errorf("%q in safe mode: exit %d stdout %q stderr %q", args, code, out, errOut)
		}
	}
}

// Safe mode refuses changing or deleting a draft, before any request:
// the drafts there may be ones the user wrote himself, and an
// unattended agent may add drafts but not touch his.
func TestSafeModeRefusesDraftUpdateAndDelete(t *testing.T) {
	for _, args := range [][]string{
		{"message", "draft", "update", "Dr01", "hi"},
		{"message", "draft", "delete", "Dr01"},
		{"message", "draft", "delete", "--workspace", "acme", "Dr01"},
	} {
		env, stdout, stderr := agentTestEnv(map[string]string{"SLK_SAFE_MODE": "1"}, noOpener(t))
		code := runAgent(context.Background(), env, args)
		if code != 1 || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "safe mode:") {
			t.Errorf("%q: exit %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
}

func TestDraftHelp(t *testing.T) {
	env, stdout, _ := agentTestEnv(nil, noOpener(t))
	if code := runAgent(context.Background(), env, []string{"message", "draft", "--help"}); code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	for _, want := range []string{
		"slk message draft list [--limit N] [--all]",
		"slk message draft create <target> <text> [--thread-ts TS] [--broadcast]",
		"slk message draft update <draft-id> <text>",
		"slk message draft delete <draft-id>",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("usage lacks %q:\n%s", want, stdout)
		}
	}
}

// drafts.list gives a DM draft's destination with the peer's user_ids
// as well as the channel_id; Slack refuses both in drafts.update
// (both_user_ids_and_channel_id_provided_in_destination, seen live
// 2026-11-13), so an update sends only channel_id, thread_ts, broadcast.
func TestDraftUpdateDMDestinationDropsUserIDs(t *testing.T) {
	dm := `{"id":"Dr09","blocks":[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"hi"}]}]}],` +
		`"destinations":[{"channel_id":"D1","user_ids":["U9"],"broadcast":false}],"last_updated_ts":"1700000300.123456","date_created":1700000000,"date_scheduled":0,"file_ids":[]}`
	srv := newFormFake(t, map[string]func(url.Values) string{
		"/api/drafts.list":   readFixed(draftsListBody(dm)),
		"/api/drafts.update": readFixed(`{"ok":true,"draft":` + dm + `}`),
	})
	if code, _, errOut := runRead(t, srv, "message", "draft", "update", "Dr09", "new"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	if got, want := srv.requestTo(t, "/api/drafts.update").form.Get("destinations"), `[{"channel_id":"D1","broadcast":false}]`; got != want {
		t.Errorf("destinations = %s, want %s", got, want)
	}
}
