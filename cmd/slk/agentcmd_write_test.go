package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

const (
	writeMsgLink    = "https://acme.slack.com/archives/C123/p1700000001000200"
	writeThreadLink = "https://acme.slack.com/archives/C123/p1700000005000300?thread_ts=1700000001.000200&cid=C123"
	// writeBadThreadLink's thread_ts is not a Slack timestamp.
	writeBadThreadLink = "https://acme.slack.com/archives/C123/p1700000005000300?thread_ts=latest&cid=C123"
	writeSentTS        = "1700000100.000400"
)

// writeFake is a fake Slack that answers every write endpoint; tests
// that need another answer override it in bodies.
func writeFake(t *testing.T, override map[string]string) *fakeSlack {
	bodies := map[string]string{
		"/api/auth.test":        agentAuthTestOK,
		"/api/chat.postMessage": `{"ok":true,"channel":"C123","ts":"` + writeSentTS + `"}`,
		"/api/chat.update":      `{"ok":true,"channel":"C123","ts":"1700000001.000200","text":"x"}`,
		"/api/chat.delete":      `{"ok":true,"channel":"C123","ts":"1700000001.000200"}`,
		"/api/reactions.add":    `{"ok":true}`,
		"/api/reactions.remove": `{"ok":true}`,
	}
	for k, v := range override {
		bodies[k] = v
	}
	return newFakeSlack(t, bodies)
}

func runWrite(t *testing.T, srv *fakeSlack, vars map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var calls []openCall
	env, stdout, stderr := agentTestEnv(vars, fakeAgentOpener(t, srv, &calls))
	code := runAgent(context.Background(), env, args)
	return code, stdout.String(), stderr.String()
}

func TestAgentSendToChannel(t *testing.T) {
	srv := writeFake(t, nil)
	code, out, errOut := runWrite(t, srv, nil, "message", "send", "C123", "hello *world*")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	want := `{"ok":true,"channel_id":"C123","ts":"` + writeSentTS + `","permalink":"https://acme.slack.com/archives/C123/p1700000100000400"}` + "\n"
	if out != want {
		t.Errorf("stdout %q\nwant   %q", out, want)
	}
	req := srv.requestTo(t, "/api/chat.postMessage")
	if req.form.Get("channel") != "C123" || req.form.Get("thread_ts") != "" {
		t.Errorf("form channel=%q thread_ts=%q", req.form.Get("channel"), req.form.Get("thread_ts"))
	}
	if !strings.Contains(req.form.Get("text"), "hello") {
		t.Errorf("text %q", req.form.Get("text"))
	}
}

func TestAgentSendReplyTargets(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantThread string
		wantLink   string
	}{
		{"link to a top-level message replies under it", []string{"message", "send", writeMsgLink, "hi"},
			"1700000001.000200", "https://acme.slack.com/archives/C123/p1700000100000400?thread_ts=1700000001.000200&cid=C123"},
		{"link into a thread replies in that thread", []string{"message", "send", writeThreadLink, "hi"},
			"1700000001.000200", "https://acme.slack.com/archives/C123/p1700000100000400?thread_ts=1700000001.000200&cid=C123"},
		{"--thread-ts on a channel", []string{"message", "send", "C123", "hi", "--thread-ts", "1700000002.000900"},
			"1700000002.000900", "https://acme.slack.com/archives/C123/p1700000100000400?thread_ts=1700000002.000900&cid=C123"},
		{"--thread-ts overrides the link", []string{"message", "send", "--thread-ts", "1700000002.000900", writeThreadLink, "hi"},
			"1700000002.000900", "https://acme.slack.com/archives/C123/p1700000100000400?thread_ts=1700000002.000900&cid=C123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := writeFake(t, nil)
			code, out, errOut := runWrite(t, srv, nil, tc.args...)
			if code != 0 {
				t.Fatalf("exit %d, stderr %s", code, errOut)
			}
			req := srv.requestTo(t, "/api/chat.postMessage")
			if req.form.Get("channel") != "C123" || req.form.Get("thread_ts") != tc.wantThread {
				t.Errorf("channel=%q thread_ts=%q, want C123 / %s", req.form.Get("channel"), req.form.Get("thread_ts"), tc.wantThread)
			}
			if req.form.Get("reply_broadcast") != "" {
				t.Errorf("reply_broadcast set")
			}
			want := `{"ok":true,"channel_id":"C123","ts":"` + writeSentTS + `","permalink":"` + tc.wantLink + `"}` + "\n"
			if out != want {
				t.Errorf("stdout %q\nwant   %q", out, want)
			}
		})
	}
}

func TestAgentSendDashLeadingText(t *testing.T) {
	srv := writeFake(t, nil)
	code, _, errOut := runWrite(t, srv, nil, "message", "send", "C123", "--", "- first item")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	if got := srv.requestTo(t, "/api/chat.postMessage").form.Get("text"); !strings.Contains(got, "first item") {
		t.Errorf("text %q", got)
	}
}

// A link in the text must not cost a chat.unfurlLink call or the 3s
// linkChipWait: the CLI has no WebSocket, so lookupLinkChips returns at
// once. The fake fails the test on any request it has no answer for.
func TestAgentSendWithURLDoesNotStall(t *testing.T) {
	srv := writeFake(t, nil)
	start := time.Now()
	code, _, errOut := runWrite(t, srv, nil, "message", "send", "C123", "see https://linear.app/acme/issue/ABC-1 and https://example.com/x")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("send took %s, want well under linkChipWait (3s)", d)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, r := range srv.reqs {
		if strings.Contains(r.path, "unfurl") {
			t.Errorf("unexpected request to %s", r.path)
		}
	}
}

func TestAgentReact(t *testing.T) {
	cases := []struct {
		args []string
		path string
	}{
		{[]string{"message", "react", "add", writeMsgLink, "eyes"}, "/api/reactions.add"},
		{[]string{"message", "react", "add", "C123", ":eyes:", "--ts", "1700000001.000200"}, "/api/reactions.add"},
		{[]string{"message", "react", "add", "C123", "--ts", "1700000001.000200", "--", "-1"}, "/api/reactions.add"},
		{[]string{"message", "react", "remove", writeThreadLink, ":eyes:"}, "/api/reactions.remove"},
	}
	for _, tc := range cases {
		srv := writeFake(t, nil)
		code, out, errOut := runWrite(t, srv, nil, tc.args...)
		if code != 0 {
			t.Fatalf("%q: exit %d, stderr %s", tc.args, code, errOut)
		}
		if out != `{"ok":true}`+"\n" {
			t.Errorf("%q: stdout %q", tc.args, out)
		}
		req := srv.requestTo(t, tc.path)
		wantName := "eyes"
		if tc.args[len(tc.args)-1] == "-1" {
			wantName = "-1"
		}
		wantTS := "1700000001.000200"
		if strings.Contains(tc.args[len(tc.args)-1], "thread_ts") || tc.args[3] == writeThreadLink {
			wantTS = "1700000005.000300"
		}
		if req.form.Get("name") != wantName || req.form.Get("channel") != "C123" || req.form.Get("timestamp") != wantTS {
			t.Errorf("%q: name=%q channel=%q timestamp=%q", tc.args, req.form.Get("name"), req.form.Get("channel"), req.form.Get("timestamp"))
		}
	}
}

func TestAgentReactAlreadyReacted(t *testing.T) {
	srv := writeFake(t, map[string]string{"/api/reactions.add": `{"ok":false,"error":"already_reacted"}`})
	code, out, errOut := runWrite(t, srv, nil, "message", "react", "add", writeMsgLink, "eyes")
	if code != 1 || out != "" || !strings.Contains(errOut, "already_reacted") {
		t.Errorf("exit %d stdout %q stderr %q", code, out, errOut)
	}
}

func TestAgentEditAndDelete(t *testing.T) {
	srv := writeFake(t, nil)
	code, out, errOut := runWrite(t, srv, nil, "message", "edit", writeMsgLink, "new text")
	if code != 0 {
		t.Fatalf("edit exit %d, stderr %s", code, errOut)
	}
	if want := `{"ok":true,"channel_id":"C123","ts":"1700000001.000200"}` + "\n"; out != want {
		t.Errorf("edit stdout %q want %q", out, want)
	}
	req := srv.requestTo(t, "/api/chat.update")
	if req.form.Get("channel") != "C123" || req.form.Get("ts") != "1700000001.000200" || !strings.Contains(req.form.Get("text"), "new text") {
		t.Errorf("update form %v", req.form)
	}

	srv = writeFake(t, nil)
	code, out, errOut = runWrite(t, srv, nil, "message", "delete", "C123", "--ts", "1700000009.000100")
	if code != 0 {
		t.Fatalf("delete exit %d, stderr %s", code, errOut)
	}
	if want := `{"ok":true,"channel_id":"C123","ts":"1700000009.000100"}` + "\n"; out != want {
		t.Errorf("delete stdout %q want %q", out, want)
	}
	req = srv.requestTo(t, "/api/chat.delete")
	if req.form.Get("channel") != "C123" || req.form.Get("ts") != "1700000009.000100" {
		t.Errorf("delete form %v", req.form)
	}
}

func TestAgentWriteSlackErrorsExit1(t *testing.T) {
	srv := writeFake(t, map[string]string{"/api/chat.delete": `{"ok":false,"error":"message_not_found"}`})
	code, out, errOut := runWrite(t, srv, nil, "message", "delete", writeMsgLink)
	if code != 1 || out != "" || !strings.Contains(errOut, "message_not_found") {
		t.Errorf("exit %d stdout %q stderr %q", code, out, errOut)
	}
}

func TestAgentWriteUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"message", "edit", "C123", "text"},
		{"message", "delete", "C123"},
		{"message", "react", "add", "C123", "eyes"},
		{"message", "react", "remove", "C123", "eyes"},
		{"message", "send", "C123"},
		{"message", "send", "C123", "a", "b"},
		{"message", "send", "C123", ""},
		{"message", "react", "add", writeMsgLink, "::"},
		{"message", "delete", "https://example.com/not-slack"},
		{"message", "send", "C123", "hi", "--thread-ts", "yesterday"},
		{"message", "send", "C123", "hi", "--thread-ts=1700000001.000200&x=1"},
		{"message", "send", writeBadThreadLink, "hi"},
		{"message", "draft", "create", writeBadThreadLink, "hi"},
		{"channel", "mark", writeBadThreadLink},
		{"message", "list", writeBadThreadLink},
	} {
		env, stdout, stderr := agentTestEnv(nil, noOpener(t))
		code := runAgent(context.Background(), env, args)
		if code != 1 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Errorf("%q: exit %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
}

func TestAgentSafeModeRefusesBeforeAnyClient(t *testing.T) {
	for _, args := range [][]string{
		{"message", "send", "C123", "hi"},
		{"message", "react", "add", writeMsgLink, "eyes"},
		{"message", "react", "remove", writeMsgLink, "eyes"},
		{"message", "edit", writeMsgLink, "x"},
		{"message", "delete", writeMsgLink},
		{"message", "send"}, // refused even with bad arguments
	} {
		env, stdout, stderr := agentTestEnv(map[string]string{"SLK_SAFE_MODE": "1"}, noOpener(t))
		code := runAgent(context.Background(), env, args)
		if code != 1 || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "safe mode:") {
			t.Errorf("%q: exit %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
}
