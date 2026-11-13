package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	slackclient "github.com/gammons/slk/internal/slack"
	"github.com/gorilla/websocket"
	"github.com/slack-go/slack"
)

// listenOutput is stdout for a listen test: safe to read while the
// listener writes, and able to wait for lines.
type listenOutput struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	changed chan struct{}
}

func newListenOutput() *listenOutput {
	return &listenOutput{changed: make(chan struct{}, 1)}
}

func (o *listenOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.buf.Write(p)
	select {
	case o.changed <- struct{}{}:
	default:
	}
	return n, err
}

func (o *listenOutput) lines() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := strings.TrimSuffix(o.buf.String(), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// waitLines waits until at least n lines have been written and returns
// all of them.
func (o *listenOutput) waitLines(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if got := o.lines(); len(got) >= n {
			return got
		}
		select {
		case <-o.changed:
		case <-deadline:
			t.Fatalf("waited for %d lines, got:\n%s", n, strings.Join(o.lines(), "\n"))
		}
	}
}

// listenAt is the fixed receive time unit tests stamp lines with.
var listenAt = time.Date(2026, 11, 12, 9, 30, 0, 0, time.FixedZone("CST", 8*3600))

const listenAtJSON = `"at":"2026-11-12T09:30:00+08:00"`

// testListener is a listener writing to out, signed in as
// U0EXAMPLE01 on T0EXAMPLE01, with a fixed clock and millisecond
// backoffs.
func testListener(out *listenOutput, wakes <-chan time.Duration) (*agentListener, *bytes.Buffer) {
	env, _, stderr := agentTestEnv(nil, nil)
	env.stdout = out
	l := newAgentListener(env, wakes)
	l.teamID, l.userID = "T0EXAMPLE01", "U0EXAMPLE01"
	l.now = func() time.Time { return listenAt }
	l.backoffMin, l.backoffMax = time.Millisecond, 4*time.Millisecond
	l.stopWait = time.Second
	return l, stderr
}

func readWSFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "slack", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// ---------------------------------------------------------------------
// Event lines
// ---------------------------------------------------------------------

// TestListenEventLines feeds frames shaped like the web client's
// through the real dispatcher (dispatchWebSocketEvent) into one
// connection's handler and checks every line it writes.
func TestListenEventLines(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  []string // lines without the "type" and "at" prefix
	}{
		{
			"message that mentions me",
			`{"client_msg_id":"0b7a6c1e-0000-4000-8000-000000000001","type":"message","text":"<@U0EXAMPLE01> can you look?","user":"U0EXAMPLE02","ts":"1793932809.791579","blocks":[{"type":"rich_text","block_id":"a1","elements":[{"type":"rich_text_section","elements":[{"type":"user","user_id":"U0EXAMPLE01"},{"type":"text","text":" can you look?"}]}]}],"team":"T0EXAMPLE01","channel":"C0EXAMPLE01","event_ts":"1793932809.791579","channel_type":"channel"}`,
			[]string{`"type":"message",AT,"ts":"1793932809.791579","channel_id":"C0EXAMPLE01","author":{"user_id":"U0EXAMPLE02"},"content":"@U0EXAMPLE01 can you look?","changed":false,"mentions_me":true}`},
		},
		{
			"message that mentions someone else",
			`{"type":"message","text":"<@U0EXAMPLE03> ship it","user":"U0EXAMPLE02","ts":"1793932810.000100","team":"T0EXAMPLE01","channel":"C0EXAMPLE01","event_ts":"1793932810.000100"}`,
			[]string{`"type":"message",AT,"ts":"1793932810.000100","channel_id":"C0EXAMPLE01","author":{"user_id":"U0EXAMPLE02"},"content":"@U0EXAMPLE03 ship it","changed":false,"mentions_me":false}`},
		},
		{
			"@here counts as mentioning me",
			`{"type":"message","text":"<!here> deploy at 5","user":"U0EXAMPLE02","ts":"1793932811.000100","channel":"C0EXAMPLE01","event_ts":"1793932811.000100"}`,
			[]string{`"type":"message",AT,"ts":"1793932811.000100","channel_id":"C0EXAMPLE01","author":{"user_id":"U0EXAMPLE02"},"content":"@here deploy at 5","changed":false,"mentions_me":true}`},
		},
		{
			"edited message",
			`{"type":"message","subtype":"message_changed","message":{"client_msg_id":"0b7a6c1e-0000-4000-8000-000000000001","type":"message","text":"<@U0EXAMPLE01> can you look now?","user":"U0EXAMPLE02","team":"T0EXAMPLE01","edited":{"user":"U0EXAMPLE02","ts":"1793932820.000000"},"ts":"1793932809.791579","source_team":"T0EXAMPLE01","user_team":"T0EXAMPLE01"},"previous_message":{"type":"message","text":"<@U0EXAMPLE01> can you look?","user":"U0EXAMPLE02","ts":"1793932809.791579"},"channel":"C0EXAMPLE01","hidden":true,"ts":"1793932820.000200","event_ts":"1793932820.000200","channel_type":"channel"}`,
			[]string{`"type":"message",AT,"ts":"1793932809.791579","channel_id":"C0EXAMPLE01","author":{"user_id":"U0EXAMPLE02"},"content":"@U0EXAMPLE01 can you look now?","edited":true,"changed":true,"mentions_me":true}`},
		},
		{
			// The unfurl's title and text, as agent-slack shows a message
			// whose attachments have text: its own text (normally also in
			// its rich_text blocks, absent from this frame) is not repeated.
			"changed without an edit (an unfurl landed)",
			`{"type":"message","subtype":"message_changed","message":{"type":"message","text":"see https://example.com","user":"U0EXAMPLE02","attachments":[{"id":1,"from_url":"https://example.com","title":"Example","text":"Example Domain"}],"ts":"1793932812.000100"},"previous_message":{"type":"message","text":"see https://example.com","user":"U0EXAMPLE02","ts":"1793932812.000100"},"channel":"C0EXAMPLE01","hidden":true,"ts":"1793932813.000100","event_ts":"1793932813.000100"}`,
			[]string{`"type":"message",AT,"ts":"1793932812.000100","channel_id":"C0EXAMPLE01","author":{"user_id":"U0EXAMPLE02"},"content":"Example\nExample Domain","changed":true,"mentions_me":false}`},
		},
		{
			"thread reply",
			`{"client_msg_id":"0b7a6c1e-0000-4000-8000-000000000002","type":"message","text":"on it","user":"U0EXAMPLE02","ts":"1793932900.000200","team":"T0EXAMPLE01","thread_ts":"1793932000.000100","parent_user_id":"U0EXAMPLE01","channel":"C0EXAMPLE01","event_ts":"1793932900.000200","channel_type":"channel"}`,
			[]string{`"type":"message",AT,"ts":"1793932900.000200","thread_ts":"1793932000.000100","channel_id":"C0EXAMPLE01","author":{"user_id":"U0EXAMPLE02"},"content":"on it","changed":false,"mentions_me":false}`},
		},
		{
			"bot message whose content is in an attachment",
			`{"type":"message","subtype":"bot_message","text":"","username":"deploy-bot","bot_id":"B0EXAMPLE01","attachments":[{"id":1,"color":"d00000","fallback":"Deploy failed","text":"Deploy of api failed on prod"}],"ts":"1793932950.000300","channel":"C0EXAMPLE02","event_ts":"1793932950.000300"}`,
			[]string{`"type":"message",AT,"ts":"1793932950.000300","channel_id":"C0EXAMPLE02","author":{"bot_id":"B0EXAMPLE01","username":"deploy-bot"},"content":"Deploy of api failed on prod","subtype":"bot_message","changed":false,"mentions_me":false}`},
		},
		{
			"message with a file",
			`{"type":"message","subtype":"file_share","text":"logs","user":"U0EXAMPLE02","files":[{"id":"F0EXAMPLE01","name":"app.log","title":"app.log","mimetype":"text/plain","filetype":"text"}],"upload":false,"ts":"1793932960.000100","channel":"D0EXAMPLE01","event_ts":"1793932960.000100","channel_type":"im"}`,
			[]string{`"type":"message",AT,"ts":"1793932960.000100","channel_id":"D0EXAMPLE01","author":{"user_id":"U0EXAMPLE02"},"content":"logs","subtype":"file_share","files":[{"id":"F0EXAMPLE01","name":"app.log","title":"app.log","mimetype":"text/plain"}],"changed":false,"mentions_me":false}`},
		},
		{
			"me_message (captured frame)",
			readWSFixture(t, "ws_me_message.json"),
			[]string{`"type":"message",AT,"ts":"1791803261.761229","channel_id":"D0EXAMPLE01","author":{"user_id":"U0EXAMPLE01"},"content":"waves","subtype":"me_message","changed":false,"mentions_me":false}`},
		},
		{
			"deleted message",
			`{"type":"message","subtype":"message_deleted","hidden":true,"deleted_ts":"1793932809.791579","channel":"C0EXAMPLE01","previous_message":{"type":"message","text":"oops","user":"U0EXAMPLE02","ts":"1793932809.791579"},"event_ts":"1793932970.000100","ts":"1793932970.000100","channel_type":"channel"}`,
			[]string{`"type":"message_deleted",AT,"channel_id":"C0EXAMPLE01","ts":"1793932809.791579"}`},
		},
		{
			"reaction added",
			`{"type":"reaction_added","user":"U0EXAMPLE02","item":{"type":"message","channel":"C0EXAMPLE01","ts":"1793932809.791579"},"reaction":"eyes","item_user":"U0EXAMPLE01","event_ts":"1793932980.000100","ts":"1793932980.000100"}`,
			[]string{`"type":"reaction_added",AT,"channel_id":"C0EXAMPLE01","ts":"1793932809.791579","user_id":"U0EXAMPLE02","emoji":"eyes"}`},
		},
		{
			"reaction removed",
			`{"type":"reaction_removed","user":"U0EXAMPLE02","item":{"type":"message","channel":"C0EXAMPLE01","ts":"1793932809.791579"},"reaction":"white_check_mark","item_user":"U0EXAMPLE01","event_ts":"1793932990.000100","ts":"1793932990.000100"}`,
			[]string{`"type":"reaction_removed",AT,"channel_id":"C0EXAMPLE01","ts":"1793932809.791579","user_id":"U0EXAMPLE02","emoji":"white_check_mark"}`},
		},
		{
			"channel marked read elsewhere",
			`{"type":"channel_marked","channel":"C0EXAMPLE01","ts":"1793932809.791579","unread_count":0,"unread_count_display":0,"num_mentions":0,"num_mentions_display":0,"mention_count":0,"mention_count_display":0,"event_ts":"1793933000.000100"}`,
			[]string{`"type":"channel_marked",AT,"channel_id":"C0EXAMPLE01","ts":"1793932809.791579"}`},
		},
		{
			"DM marked read elsewhere",
			`{"type":"im_marked","channel":"D0EXAMPLE01","ts":"1793932960.000100","unread_count":0,"unread_count_display":0,"dm_count":0,"event_ts":"1793933001.000100"}`,
			[]string{`"type":"channel_marked",AT,"channel_id":"D0EXAMPLE01","ts":"1793932960.000100"}`},
		},
		{
			"thread marked read elsewhere",
			`{"type":"thread_marked","subscription":{"type":"thread","channel":"C0EXAMPLE01","thread_ts":"1793932000.000100","date_create":1793932000,"active":true,"last_read":"1793932900.000200"},"event_ts":"1793933010.000100"}`,
			[]string{`"type":"thread_marked",AT,"channel_id":"C0EXAMPLE01","thread_ts":"1793932000.000100","ts":"1793932900.000200"}`},
		},
		{"typing", `{"type":"user_typing","channel":"C0EXAMPLE01","user":"U0EXAMPLE02"}`, nil},
		{"presence", `{"type":"presence_change","user":"U0EXAMPLE02","presence":"active"}`, nil},
		{"batched presence", `{"type":"presence_change","users":["U0EXAMPLE02","U0EXAMPLE03"],"presence":"away"}`, nil},
		{"own presence", `{"type":"manual_presence_change","presence":"away"}`, nil},
		{"DND", `{"type":"dnd_updated","dnd_status":{"dnd_enabled":true,"snooze_enabled":true,"snooze_endtime":4102444800,"next_dnd_start_ts":0,"next_dnd_end_ts":0}}`, nil},
		{"badge counts", `{"type":"badge_counts_updated","activity_v2":{"at_user":2,"thread_v2":1},"event_ts":"1793933020.000100"}`, nil},
		{"assistant status", `{"type":"ai_assistant_status","channel_id":"C0EXAMPLE01","status":"Thinking...","bot_user_id":"U0EXAMPLE09","thread_ts":"1793932000.000100","status_type":"banner","event_ts":"1793933030.000100"}`, nil},
		{"ephemeral", `{"type":"message","channel":"C0EXAMPLE01","user":"USLACKBOT","text":"Only visible to you","ts":"1793933040.000100","is_ephemeral":true}`, nil},
		{"view opened (captured frame)", readWSFixture(t, "ws_view_opened.json"), nil},
		{"section change (captured frame)", readWSFixture(t, "ws_section_upserted.json"), nil},
		{"member joined", `{"type":"member_joined_channel","channel":"C0EXAMPLE01","user":"U0EXAMPLE03","team":"T0EXAMPLE01"}`, nil},
		{"pref change", `{"type":"pref_change","name":"muted_channels","value":"C0EXAMPLE02"}`, nil},
		{"thread subscribed", `{"type":"thread_subscribed","subscription":{"channel":"C0EXAMPLE01","thread_ts":"1793932000.000100","last_read":"1793932900.000200","active":true}}`, nil},
		{"goodbye", `{"type":"goodbye","source":"gateway_server"}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := newListenOutput()
			l, _ := testListener(out, nil)
			slackclient.DispatchWebSocketEventForTest([]byte(tc.frame), &listenSession{l: l})
			var want []string
			for _, w := range tc.want {
				want = append(want, "{"+strings.Replace(w, "AT", listenAtJSON, 1))
			}
			if got := out.lines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// TestListenHelloOncePerConnection: hello carries who listen is signed
// in as, and a second hello on the same connection writes nothing, so
// a `disconnected` always separates two hello lines.
func TestListenHelloOncePerConnection(t *testing.T) {
	out := newListenOutput()
	l, _ := testListener(out, nil)
	s := &listenSession{l: l}
	slackclient.DispatchWebSocketEventForTest([]byte(`{"type":"hello","start":true,"host_id":"gs-iad-1"}`), s)
	slackclient.DispatchWebSocketEventForTest([]byte(`{"type":"hello"}`), s)
	want := `{"type":"hello",` + listenAtJSON + `,"team_id":"T0EXAMPLE01","user_id":"U0EXAMPLE01"}`
	if got := strings.Join(out.lines(), "\n"); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// ---------------------------------------------------------------------
// The connection loop, against a scripted socket
// ---------------------------------------------------------------------

// fakeAttempt is what one StartWebSocket call does.
type fakeAttempt struct {
	err    error    // returned by StartWebSocket; nothing else happens
	frames []string // dispatched in order through the real dispatcher
	hold   bool     // after the frames, stay connected until StopWebSocket
}

// fakeSocket plays StartWebSocket's contract from a script: frames go
// through dispatchWebSocketEvent on a goroutine of their own, then
// OnDisconnect runs, then WsDone closes.
type fakeSocket struct {
	t        *testing.T
	mu       sync.Mutex
	attempts []fakeAttempt
	started  int
	done     chan struct{}
	stop     chan struct{}
	stops    int
	authErr  error
	auths    int
}

func (f *fakeSocket) StartWebSocket(h slackclient.EventHandler) error {
	f.mu.Lock()
	if f.started == len(f.attempts) {
		f.mu.Unlock()
		f.t.Errorf("StartWebSocket called %d times; the script has %d attempts", f.started+1, len(f.attempts))
		return errors.New("script exhausted")
	}
	a := f.attempts[f.started]
	f.started++
	if a.err != nil {
		f.mu.Unlock()
		return a.err
	}
	done, stop := make(chan struct{}), make(chan struct{})
	f.done, f.stop = done, stop
	f.mu.Unlock()
	go func() {
		defer close(done)
		defer h.OnDisconnect()
		for _, frame := range a.frames {
			slackclient.DispatchWebSocketEventForTest([]byte(frame), h)
		}
		if a.hold {
			<-stop
		}
	}()
	return nil
}

func (f *fakeSocket) StopWebSocket() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	if f.stop != nil {
		select {
		case <-f.stop:
		default:
			close(f.stop)
		}
	}
	return nil
}

func (f *fakeSocket) WsDone() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.done
}

func (f *fakeSocket) CheckAuth(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auths++
	return f.authErr
}

func (f *fakeSocket) counts() (started, stops, auths int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started, f.stops, f.auths
}

// runListener runs l on sock in the background; the returned function
// cancels it and returns run's error.
func runListener(t *testing.T, l *agentListener, sock listenSocket) (stop func() error, result <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- l.run(ctx, sock) }()
	t.Cleanup(cancel)
	return func() error {
		cancel()
		select {
		case err := <-errc:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("listen did not stop after its context ended")
			return nil
		}
	}, errc
}

const (
	helloFrame = `{"type":"hello","start":true}`
	msgFrameA  = `{"type":"message","text":"first","user":"U0EXAMPLE02","ts":"1793932809.000100","channel":"C0EXAMPLE01"}`
	msgFrameB  = `{"type":"message","text":"second","user":"U0EXAMPLE02","ts":"1793932999.000100","channel":"C0EXAMPLE01"}`
)

func helloLine() string {
	return `{"type":"hello",` + listenAtJSON + `,"team_id":"T0EXAMPLE01","user_id":"U0EXAMPLE01"}`
}

func disconnectedLine(reason string) string {
	b, _ := json.Marshal(reason)
	return `{"type":"disconnected",` + listenAtJSON + `,"error":` + string(b) + `}`
}

func messageLine(ts, text string) string {
	return `{"type":"message",` + listenAtJSON + `,"ts":"` + ts + `","channel_id":"C0EXAMPLE01","author":{"user_id":"U0EXAMPLE02"},"content":"` + text + `","changed":false,"mentions_me":false}`
}

func assertLines(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestListenDisconnectedPrecedesNextHello drops a connection, fails a
// dial, has one connection close before Slack's hello, then connects:
// each attempt ends in one `disconnected`, written after the attempt's
// last event and before the next `hello`. Only the attempts that never
// reached hello run auth.test, and a network failure there is not an
// auth failure.
func TestListenDisconnectedPrecedesNextHello(t *testing.T) {
	out := newListenOutput()
	l, _ := testListener(out, nil)
	dialErr := errors.New("websocket connect failed: dial tcp 1.2.3.4:443: connect: connection refused")
	sock := &fakeSocket{t: t, attempts: []fakeAttempt{
		{frames: []string{helloFrame, msgFrameA}},
		{err: dialErr},
		{frames: nil},
		{frames: []string{helloFrame, msgFrameB}, hold: true},
	}, authErr: &url.Error{Op: "Post", URL: "https://slack.com/api/auth.test", Err: errors.New("dial tcp: connection refused")}}
	stop, _ := runListener(t, l, sock)

	got := out.waitLines(t, 7)
	if err := stop(); !errors.Is(err, errListenStopped) {
		t.Errorf("run returned %v, want errListenStopped", err)
	}
	assertLines(t, got, []string{
		helloLine(),
		messageLine("1793932809.000100", "first"),
		disconnectedLine("connection lost"),
		disconnectedLine(dialErr.Error()),
		disconnectedLine("connection closed before hello"),
		helloLine(),
		messageLine("1793932999.000100", "second"),
	})
	if started, _, auths := sock.counts(); started != 4 || auths != 2 {
		t.Errorf("StartWebSocket %d times, auth.test %d times; want 4 and 2", started, auths)
	}
	if got := out.lines(); len(got) != 7 {
		t.Errorf("stopping wrote more lines: %q", got[7:])
	}
}

// TestListenAuthFailureExits2: when a reconnect fails and auth.test
// says the session is dead, listen stops and exits 2 with Slack's code
// on stderr.
func TestListenAuthFailureExits2(t *testing.T) {
	for _, code := range []string{"invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive"} {
		t.Run(code, func(t *testing.T) {
			out := newListenOutput()
			l, stderr := testListener(out, nil)
			handshake := errors.New("websocket connect failed: websocket: bad handshake")
			sock := &fakeSocket{t: t, attempts: []fakeAttempt{
				{frames: []string{helloFrame}},
				{err: handshake},
			}, authErr: slack.SlackErrorResponse{Err: code}}

			_, result := runListener(t, l, sock)
			var err error
			select {
			case err = <-result:
			case <-time.After(5 * time.Second):
				t.Fatal("listen kept running on a dead token")
			}
			if exit := reportAgentError(l.env, err); exit != 2 {
				t.Errorf("exit %d, want 2 (err %v)", exit, err)
			}
			if !strings.Contains(stderr.String(), code) || !strings.Contains(stderr.String(), "slk --add-workspace") {
				t.Errorf("stderr %q lacks %s or the sign-in hint", stderr, code)
			}
			assertLines(t, out.lines(), []string{
				helloLine(),
				disconnectedLine("connection lost"),
				disconnectedLine(handshake.Error()),
			})
			if started, _, auths := sock.counts(); started != 2 || auths != 1 {
				t.Errorf("StartWebSocket %d times, auth.test %d times; want 2 and 1", started, auths)
			}
		})
	}
}

// TestListenNonAuthRefusalKeepsRetrying: auth.test failing for any
// other reason (rate limit, Slack's own trouble) is not a dead token.
func TestListenNonAuthRefusalKeepsRetrying(t *testing.T) {
	out := newListenOutput()
	l, _ := testListener(out, nil)
	sock := &fakeSocket{t: t, attempts: []fakeAttempt{
		{err: errors.New("websocket connect failed: websocket: bad handshake")},
		{frames: []string{helloFrame}, hold: true},
	}, authErr: slack.SlackErrorResponse{Err: "ratelimited"}}
	stop, _ := runListener(t, l, sock)
	out.waitLines(t, 2)
	if err := stop(); !errors.Is(err, errListenStopped) {
		t.Errorf("run returned %v, want errListenStopped", err)
	}
}

// TestListenSignalClosesSocket: the context ending (SIGINT, SIGTERM)
// closes the socket and returns without another line.
func TestListenSignalClosesSocket(t *testing.T) {
	out := newListenOutput()
	l, _ := testListener(out, nil)
	sock := &fakeSocket{t: t, attempts: []fakeAttempt{{frames: []string{helloFrame}, hold: true}}}
	stop, _ := runListener(t, l, sock)
	out.waitLines(t, 1)
	if err := stop(); !errors.Is(err, errListenStopped) {
		t.Errorf("run returned %v, want errListenStopped", err)
	}
	if _, stops, _ := sock.counts(); stops == 0 {
		t.Error("the socket was not closed")
	}
	assertLines(t, out.lines(), []string{helloLine()})
}

// TestListenWakeReconnects: after the machine wakes, listen drops the
// connection and dials again, so the caller sees `disconnected` and
// catches up on what the sleep may have cost.
func TestListenWakeReconnects(t *testing.T) {
	out := newListenOutput()
	wakes := make(chan time.Duration, 1)
	l, _ := testListener(out, wakes)
	sock := &fakeSocket{t: t, attempts: []fakeAttempt{
		{frames: []string{helloFrame}, hold: true},
		{frames: []string{helloFrame}, hold: true},
	}}
	stop, _ := runListener(t, l, sock)
	out.waitLines(t, 1)
	wakes <- 5*time.Minute + 12*time.Second
	got := out.waitLines(t, 3)
	if err := stop(); !errors.Is(err, errListenStopped) {
		t.Errorf("run returned %v, want errListenStopped", err)
	}
	assertLines(t, got, []string{
		helloLine(),
		disconnectedLine("reconnecting after the system slept (5m12s)"),
		helloLine(),
	})
}

// failingWriter is stdout after the reader went away.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write |1: broken pipe") }

// TestListenStdoutFailureStops: a listen nobody can read from exits 1
// instead of running on.
func TestListenStdoutFailureStops(t *testing.T) {
	l, _ := testListener(newListenOutput(), nil)
	l.env.stdout = failingWriter{}
	sock := &fakeSocket{t: t, attempts: []fakeAttempt{{frames: []string{helloFrame}, hold: true}}}
	_, result := runListener(t, l, sock)
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "broken pipe") {
			t.Errorf("run returned %v, want the write error", err)
		}
		if exit := agentExitCode(err); exit != 1 {
			t.Errorf("exit %d, want 1", exit)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listen kept running with stdout gone")
	}
}

// TestListenStuckHandshakeExits: a handshake that never returns cannot
// be retried on the same client, so listen exits 1 for its supervisor
// to restart it rather than wait forever.
func TestListenStuckHandshakeExits(t *testing.T) {
	l, _ := testListener(newListenOutput(), nil)
	l.stepTimeout = 20 * time.Millisecond
	release := make(chan struct{})
	defer close(release)
	_, result := runListener(t, l, stuckSocket{release})
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "did not finish") || agentExitCode(err) != 1 {
			t.Errorf("run returned %v, want a stall error (exit 1)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listen waited on a stuck handshake")
	}
}

type stuckSocket struct{ release chan struct{} }

func (s stuckSocket) StartWebSocket(slackclient.EventHandler) error {
	<-s.release
	return errors.New("released")
}
func (stuckSocket) StopWebSocket() error            { return nil }
func (stuckSocket) WsDone() <-chan struct{}         { return nil }
func (stuckSocket) CheckAuth(context.Context) error { return nil }

// ---------------------------------------------------------------------
// Opening the client
// ---------------------------------------------------------------------

// TestListenOpenRetriesNetworkFailures: at startup a network failure is
// retried (one `disconnected` each); an auth refusal or a local problem
// is returned at once.
func TestListenOpenRetriesNetworkFailures(t *testing.T) {
	netErr := &url.Error{Op: "Post", URL: "https://slack.com/api/auth.test", Err: errors.New("dial tcp: lookup slack.com: no such host")}
	cases := []struct {
		name      string
		errs      []error // what the opener returns before it succeeds
		wantExit  int     // 0: open returns a client
		wantLines int
	}{
		{"network down, then up", []error{netErr, slack.StatusCodeError{Code: 503, Status: "503 Service Unavailable"}}, 0, 2},
		{"dead token", []error{slack.SlackErrorResponse{Err: "invalid_auth"}}, 2, 0},
		{"no token stored", []error{&agentAuthError{msg: "no Slack workspace is signed in"}}, 2, 0},
		{"no workspace matches", []error{errors.New(`no workspace matches "acme"`)}, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := newListenOutput()
			l, _ := testListener(out, nil)
			calls := 0
			client, err := l.open(context.Background(), func(context.Context) (*slackclient.Client, error) {
				calls++
				if calls <= len(tc.errs) {
					return nil, tc.errs[calls-1]
				}
				return slackclient.NewClient("xoxc-test", "d-cookie"), nil
			})
			if tc.wantExit == 0 {
				if err != nil || client == nil {
					t.Fatalf("open: %v, %v; want a client", client, err)
				}
			} else if err == nil || agentExitCode(err) != tc.wantExit {
				t.Fatalf("open returned %v (exit %d), want exit %d", err, agentExitCode(err), tc.wantExit)
			}
			lines := out.lines()
			if len(lines) != tc.wantLines {
				t.Fatalf("lines %q, want %d disconnected lines", lines, tc.wantLines)
			}
			if tc.wantLines > 0 && lines[0] != disconnectedLine(netErr.Error()) {
				t.Errorf("line %s, want %s", lines[0], disconnectedLine(netErr.Error()))
			}
		})
	}
}

// ---------------------------------------------------------------------
// End to end: `slk listen` against a local WebSocket server
// ---------------------------------------------------------------------

// fakeSlackSocket is a local WebSocket server; serve runs per
// connection with its 1-based number.
func fakeSlackSocket(t *testing.T, serve func(n int, conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		mu.Lock()
		n++
		num := n
		mu.Unlock()
		serve(num, conn)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestListenEndToEndReconnects runs `slk listen` with a real client:
// the server sends hello and a message, hangs up, and on the second
// connection says hello again.
func TestListenEndToEndReconnects(t *testing.T) {
	api := newFakeSlack(t, map[string]string{"/api/auth.test": agentAuthTestOK})
	release := make(chan struct{})
	ws := fakeSlackSocket(t, func(n int, conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"hello","start":true}`))
		if n == 1 {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"message","text":"<@U1> ping","user":"U2","ts":"1793932809.000100","channel":"C1","event_ts":"1793932809.000100"}`))
			return // hang up
		}
		<-release
	})
	defer close(release)

	out := newListenOutput()
	env, _, stderr := agentTestEnv(nil, func(ctx context.Context, workspace, linkDomain string) (*slackclient.Client, error) {
		c := newTestClient(t, api.Server)
		if err := c.Connect(ctx); err != nil {
			return nil, err
		}
		c.SetWebSocketBaseURLForTest("ws://" + ws.Listener.Addr().String())
		return c, nil
	})
	env.stdout = out
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exit := make(chan int, 1)
	go func() { exit <- runAgent(ctx, env, []string{"listen"}) }()

	got := out.waitLines(t, 4)
	cancel() // what SIGINT or SIGTERM does
	select {
	case code := <-exit:
		if code != 0 {
			t.Errorf("exit %d, want 0; stderr %q", code, stderr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listen did not exit after the signal")
	}

	type line struct {
		Type       string `json:"type"`
		At         string `json:"at"`
		TeamID     string `json:"team_id"`
		UserID     string `json:"user_id"`
		ChannelID  string `json:"channel_id"`
		Content    string `json:"content"`
		MentionsMe bool   `json:"mentions_me"`
		Error      string `json:"error"`
	}
	var types []string
	for i, raw := range got {
		var l line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("line %d is not JSON: %s", i, raw)
		}
		if _, err := time.Parse(time.RFC3339, l.At); err != nil {
			t.Errorf("line %d: at %q is not RFC 3339", i, l.At)
		}
		types = append(types, l.Type)
		switch l.Type {
		case "hello":
			if l.TeamID != "T1" || l.UserID != "U1" {
				t.Errorf("hello %s, want team T1 user U1", raw)
			}
		case "message":
			if l.ChannelID != "C1" || l.Content != "@U1 ping" || !l.MentionsMe {
				t.Errorf("message %s", raw)
			}
		case "disconnected":
			if l.Error != "connection lost" {
				t.Errorf("disconnected %s", raw)
			}
		}
	}
	if want := "hello message disconnected hello"; strings.Join(types, " ") != want {
		t.Errorf("events %q, want %q", strings.Join(types, " "), want)
	}
}

// TestListenEndToEndDeadTokenExits2: Slack refuses the WebSocket and
// auth.test says invalid_auth: listen exits 2 instead of retrying.
func TestListenEndToEndDeadTokenExits2(t *testing.T) {
	api := newFakeSlack(t, map[string]string{"/api/auth.test": `{"ok":false,"error":"invalid_auth"}`})
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer refuse.Close()

	out := newListenOutput()
	env, _, stderr := agentTestEnv(nil, func(ctx context.Context, workspace, linkDomain string) (*slackclient.Client, error) {
		// Not connected: the stored session went bad after startup.
		c := newTestClient(t, api.Server)
		c.SetWebSocketBaseURLForTest("ws://" + refuse.Listener.Addr().String())
		return c, nil
	})
	env.stdout = out
	exit := make(chan int, 1)
	go func() { exit <- runAgent(context.Background(), env, []string{"listen"}) }()
	select {
	case code := <-exit:
		if code != 2 {
			t.Errorf("exit %d, want 2; stderr %q", code, stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("listen kept running on a dead token")
	}
	if !strings.Contains(stderr.String(), "invalid_auth") {
		t.Errorf("stderr %q lacks invalid_auth", stderr)
	}
	if req := api.requestTo(t, "/api/auth.test"); req.form.Get("token") != "xoxc-test" {
		t.Errorf("auth.test sent token %q", req.form.Get("token"))
	}
	lines := out.lines()
	if len(lines) != 1 || !strings.Contains(lines[0], `"type":"disconnected"`) || !strings.Contains(lines[0], "bad handshake") {
		t.Errorf("lines %q, want one disconnected line for the refused handshake", lines)
	}
}

// TestListenNoHelloReconnects: a socket kept alive by pings that never
// delivers Slack's hello is dropped after helloTimeout with one
// `disconnected`; auth.test runs, and the next dial says hello.
func TestListenNoHelloReconnects(t *testing.T) {
	api := newFakeSlack(t, map[string]string{"/api/auth.test": agentAuthTestOK})
	release := make(chan struct{})
	var pongs atomic.Int32
	ws := fakeSlackSocket(t, func(n int, conn *websocket.Conn) {
		if n > 1 {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(helloFrame))
			<-release
			return
		}
		// Ping like Slack does and read so the client's pongs (and its
		// close) arrive, but never say hello.
		conn.SetPongHandler(func(string) error { pongs.Add(1); return nil })
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)); err != nil {
				return
			}
			select {
			case <-closed:
				return
			case <-release:
				return
			case <-tick.C:
			}
		}
	})
	defer close(release)

	c := newTestClient(t, api.Server)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.SetWebSocketBaseURLForTest("ws://" + ws.Listener.Addr().String())
	out := newListenOutput()
	l, _ := testListener(out, nil)
	l.helloTimeout = 300 * time.Millisecond
	stop, _ := runListener(t, l, c)

	got := out.waitLines(t, 2)
	if err := stop(); !errors.Is(err, errListenStopped) {
		t.Errorf("run returned %v, want errListenStopped", err)
	}
	assertLines(t, got, []string{disconnectedLine("no hello within 300ms"), helloLine()})
	if got := out.lines(); len(got) != 2 {
		t.Errorf("stopping wrote more lines: %q", got[2:])
	}
	if pongs.Load() == 0 {
		t.Error("the client answered no ping; the first connection was not kept alive")
	}
	api.mu.Lock()
	auths := 0
	for _, r := range api.reqs {
		if r.path == "/api/auth.test" {
			auths++
		}
	}
	api.mu.Unlock()
	if auths != 2 {
		t.Errorf("auth.test %d times, want 2 (Connect, then after the attempt without hello)", auths)
	}
}

// TestListenDialErrorHidesToken: gorilla/websocket quotes a URL it
// cannot parse in its error, and StartWebSocket's URL carries the
// session token in its query. The `disconnected` line keeps the error
// but neither the query nor the token.
func TestListenDialErrorHidesToken(t *testing.T) {
	api := newFakeSlack(t, map[string]string{"/api/auth.test": agentAuthTestOK})
	c := newTestClient(t, api.Server)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Not a port: url.Parse fails before any dial.
	c.SetWebSocketBaseURLForTest("ws://127.0.0.1:notaport")
	out := newListenOutput()
	l, _ := testListener(out, nil)
	stop, _ := runListener(t, l, c)
	got := out.waitLines(t, 1)
	if err := stop(); !errors.Is(err, errListenStopped) {
		t.Errorf("run returned %v, want errListenStopped", err)
	}
	line := got[0]
	if strings.Contains(line, "xoxc-") || strings.Contains(line, "token=") {
		t.Errorf("disconnected line leaks the token: %s", line)
	}
	if !strings.Contains(line, `"type":"disconnected"`) || !strings.Contains(line, "websocket connect failed") || !strings.Contains(line, "invalid port") {
		t.Errorf("line %s, want a disconnected line that still says why", line)
	}
}

// TestListenStderrHidesToken: a token in an error listen gives up on
// reaches neither the `disconnected` lines nor stderr.
func TestListenStderrHidesToken(t *testing.T) {
	calls := 0
	env, _, stderr := agentTestEnv(nil, func(context.Context, string, string) (*slackclient.Client, error) {
		calls++
		if calls == 1 { // retried: a *url.Error is a net.Error
			return nil, &url.Error{Op: "Post", URL: "https://slack.com/api/auth.test?token=xoxc-111-222-abc", Err: errors.New("i/o timeout")}
		}
		return nil, errors.New("reading the session for wss://example.test/?token=xoxc-111-222-abc failed; d=xoxd-ZZZ")
	})
	out := newListenOutput()
	env.stdout = out
	if code := runAgent(context.Background(), env, []string{"listen"}); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	for name, text := range map[string]string{"stdout": strings.Join(out.lines(), "\n"), "stderr": stderr.String()} {
		if strings.Contains(text, "xox") || strings.Contains(text, "token=") {
			t.Errorf("%s leaks a token: %s", name, text)
		}
	}
	if !strings.Contains(stderr.String(), "reading the session for wss://example.test/") {
		t.Errorf("stderr %q lost the error", stderr)
	}
	if lines := out.lines(); len(lines) != 1 || !strings.Contains(lines[0], `"type":"disconnected"`) || !strings.Contains(lines[0], "i/o timeout") {
		t.Errorf("stdout %q, want one disconnected line for the retried failure", lines)
	}
}

// TestListenRedact: queries and tokens go, the rest of the error stays.
func TestListenRedact(t *testing.T) {
	cases := []struct{ in, want string }{
		{`websocket connect failed: parse "ws://h:x/?token=xoxc-1-2-ab&flannel=3": invalid port ":x" after host`,
			`websocket connect failed: parse "ws://h:x/?[redacted]": invalid port ":x" after host`},
		{`GET /?token=xoxc-1-2-ab&a=b HTTP/1.1`, `GET /?[redacted] HTTP/1.1`},
		{`cookie d=xoxd-AbC-def rejected, token xoxc-1-2-ab`, `cookie d=[redacted] rejected, token [redacted]`},
		{`websocket connect failed: dial tcp 1.2.3.4:443: connect: connection refused`, `websocket connect failed: dial tcp 1.2.3.4:443: connect: connection refused`},
		{`Post "https://slack.com/api/auth.test": dial tcp: lookup slack.com: no such host`, `Post "https://slack.com/api/auth.test": dial tcp: lookup slack.com: no such host`},
		{`connection closed before hello`, `connection closed before hello`},
	}
	for _, tc := range cases {
		if got := listenRedact(tc.in); got != tc.want {
			t.Errorf("listenRedact(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}
