package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"testing"

	"github.com/gammons/slk/internal/config"
	slackclient "github.com/gammons/slk/internal/slack"
	"github.com/slack-go/slack"
)

// agentTestEnv is an agentEnv writing to buffers, with environment
// variables from vars and openClient from open.
func agentTestEnv(vars map[string]string, open func(ctx context.Context, workspace, linkDomain string) (*slackclient.Client, error)) (*agentEnv, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	env := &agentEnv{
		stdout:     &stdout,
		stderr:     &stderr,
		getenv:     func(k string) string { return vars[k] },
		openClient: open,
	}
	return env, &stdout, &stderr
}

// openCall is one agentEnv.openClient call a test recorded.
type openCall struct{ workspace, linkDomain string }

// fakeAgentOpener returns an openClient that connects a real client to
// srv, as openAgentClient connects one to Slack, and records its
// arguments.
func fakeAgentOpener(t *testing.T, srv *fakeSlack, calls *[]openCall) func(context.Context, string, string) (*slackclient.Client, error) {
	return func(ctx context.Context, workspace, linkDomain string) (*slackclient.Client, error) {
		*calls = append(*calls, openCall{workspace, linkDomain})
		c := newTestClient(t, srv.Server)
		if err := c.Connect(ctx); err != nil {
			return nil, err
		}
		return c, nil
	}
}

// noOpener is an openClient for tests that must not reach Slack.
func noOpener(t *testing.T) func(context.Context, string, string) (*slackclient.Client, error) {
	return func(context.Context, string, string) (*slackclient.Client, error) {
		t.Fatal("openClient called; the command should have stopped before connecting")
		return nil, nil
	}
}

const agentAuthTestOK = `{"ok":true,"url":"https://acme.slack.com/","team":"Acme","user":"grant","team_id":"T1","user_id":"U1"}`

// ---------------------------------------------------------------------
// Dispatch and flags
// ---------------------------------------------------------------------

func TestAgentUnknownCommandPrintsUsageAndExits1(t *testing.T) {
	for _, args := range [][]string{
		{"message", "lst", "C1"},
		{"message"},
		{"message", "react", "C1", "eyes"},
		{"search", "files", "q"},
	} {
		env, stdout, stderr := agentTestEnv(nil, noOpener(t))
		if code := runAgent(context.Background(), env, args); code != 1 {
			t.Errorf("%q: exit %d, want 1", args, code)
		}
		if stdout.Len() != 0 {
			t.Errorf("%q: stdout %q, want empty", args, stdout)
		}
		if !strings.Contains(stderr.String(), "unknown command") || !strings.Contains(stderr.String(), "slk message list <target>") {
			t.Errorf("%q: stderr lacks the unknown-command line or the usage:\n%s", args, stderr)
		}
	}
}

// A help flag after a command group's words (slk message --help) is a
// request for the usage, not an unknown command: usage on stdout, exit 0.
// A full command's own --help still goes to its flag set.
func TestAgentGroupHelpPrintsUsageAndExits0(t *testing.T) {
	for _, args := range [][]string{
		{"message", "--help"},
		{"message", "-h"},
		{"message", "react", "--help"},
		{"search", "help"},
		{"auth", "--help"},
	} {
		env, stdout, stderr := agentTestEnv(nil, noOpener(t))
		if code := runAgent(context.Background(), env, args); code != 0 {
			t.Errorf("%q: exit %d, want 0", args, code)
		}
		if !strings.Contains(stdout.String(), "slk message list <target>") {
			t.Errorf("%q: stdout lacks the usage:\n%s", args, stdout)
		}
		if strings.Contains(stderr.String(), "unknown command") {
			t.Errorf("%q: stderr says unknown command:\n%s", args, stderr)
		}
	}
}

func TestAgentFlagsInterspersed(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantPos   []string
		limit     int
		maxBody   int
		reactions bool
		workspace string
	}{
		{"flag after", []string{"C1", "--limit", "5"}, []string{"C1"}, 5, 4000, false, ""},
		{"flag before", []string{"--limit", "5", "C1"}, []string{"C1"}, 5, 4000, false, ""},
		{"equals form and bool after", []string{"--limit=7", "C1", "--include-reactions"}, []string{"C1"}, 7, 4000, true, ""},
		{"between positionals", []string{"C1", "--workspace", "acme", "hello world"}, []string{"C1", "hello world"}, 25, 4000, false, "acme"},
		{"negative value", []string{"C1", "--max-body-chars", "-1"}, []string{"C1"}, 25, -1, false, ""},
		{"double dash", []string{"C1", "--limit", "3", "--", "-1", "--limit"}, []string{"C1", "-1", "--limit"}, 3, 4000, false, ""},
		{"lone dash", []string{"-", "C1"}, []string{"-", "C1"}, 25, 4000, false, ""},
		{"single-dash flag", []string{"-limit", "9", "C1"}, []string{"C1"}, 9, 4000, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, _, stderr := agentTestEnv(nil, nil)
			f := newAgentFlags(env, "message list")
			limit := f.Int("limit", 25, "")
			reactions := f.Bool("include-reactions", false, "")
			maxBody := f.maxBodyChars()
			workspace := f.workspace()
			pos, err := f.parse(tc.args)
			if err != nil {
				t.Fatalf("parse(%q): %v; stderr %s", tc.args, err, stderr)
			}
			if strings.Join(pos, "|") != strings.Join(tc.wantPos, "|") {
				t.Errorf("positionals = %q, want %q", pos, tc.wantPos)
			}
			if *limit != tc.limit || *maxBody != tc.maxBody || *reactions != tc.reactions || *workspace != tc.workspace {
				t.Errorf("limit=%d max-body-chars=%d include-reactions=%v workspace=%q, want %d %d %v %q",
					*limit, *maxBody, *reactions, *workspace, tc.limit, tc.maxBody, tc.reactions, tc.workspace)
			}
		})
	}
}

func TestAgentBadArgumentsPrintUsageAndExit1(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"auth", "test", "--bogus"}, "flag provided but not defined: -bogus"},
		{[]string{"auth", "test", "extra"}, `unexpected argument "extra"`},
		{[]string{"auth", "test", "--workspace"}, "flag needs an argument"},
	}
	for _, tc := range cases {
		env, stdout, stderr := agentTestEnv(nil, noOpener(t))
		if code := runAgent(context.Background(), env, tc.args); code != 1 {
			t.Errorf("%q: exit %d, want 1", tc.args, code)
		}
		if stdout.Len() != 0 {
			t.Errorf("%q: stdout %q, want empty", tc.args, stdout)
		}
		if !strings.Contains(stderr.String(), tc.want) || !strings.Contains(stderr.String(), "usage: slk auth test") {
			t.Errorf("%q: stderr lacks %q or the usage:\n%s", tc.args, tc.want, stderr)
		}
	}
}

func TestAgentHelpExits0(t *testing.T) {
	env, _, stderr := agentTestEnv(nil, noOpener(t))
	if code := runAgent(context.Background(), env, []string{"auth", "test", "--help"}); code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "usage: slk auth test") {
		t.Errorf("stderr lacks the usage:\n%s", stderr)
	}
}

func TestAgentParseExactNamesTheMissingArgument(t *testing.T) {
	env, _, stderr := agentTestEnv(nil, nil)
	f := newAgentFlags(env, "message send")
	_, err := f.parseExact([]string{"C1"}, "target", "text")
	if !errors.Is(err, errAgentUsage) {
		t.Fatalf("err = %v, want errAgentUsage", err)
	}
	if !strings.Contains(stderr.String(), "missing <text>") || !strings.Contains(stderr.String(), "usage: slk message send <target> <text>") {
		t.Errorf("stderr:\n%s", stderr)
	}
}

// ---------------------------------------------------------------------
// Safe mode
// ---------------------------------------------------------------------

func TestAgentSafeModeRefusesWritesBeforeConnecting(t *testing.T) {
	writes := [][]string{
		{"message", "send", "C1", "hi"},
		{"message", "react", "add", "C1", "eyes", "--ts", "1700000000.000100"},
		{"message", "react", "remove", "C1", "eyes", "--ts", "1700000000.000100"},
		{"message", "edit", "C1", "hi", "--ts", "1700000000.000100"},
		{"message", "delete", "C1", "--ts", "1700000000.000100"},
	}
	for _, args := range writes {
		env, stdout, stderr := agentTestEnv(map[string]string{"SLK_SAFE_MODE": "1"}, noOpener(t))
		if code := runAgent(context.Background(), env, args); code != 1 {
			t.Errorf("%q: exit %d, want 1", args, code)
		}
		if stdout.Len() != 0 {
			t.Errorf("%q: stdout %q, want empty", args, stdout)
		}
		if !strings.HasPrefix(stderr.String(), "safe mode:") {
			t.Errorf("%q: stderr %q, want it to start with %q", args, stderr, "safe mode:")
		}
	}
}

func TestRequireWritable(t *testing.T) {
	for value, wantRefused := range map[string]bool{"1": true, "true": true, "": false, "0": false} {
		env, _, _ := agentTestEnv(map[string]string{"SLK_SAFE_MODE": value}, nil)
		err := requireWritable(env)
		if (err != nil) != wantRefused {
			t.Errorf("SLK_SAFE_MODE=%q: err = %v, want refused=%v", value, err, wantRefused)
		}
		if err != nil && !strings.HasPrefix(err.Error(), "safe mode:") {
			t.Errorf("SLK_SAFE_MODE=%q: %q does not start with %q", value, err, "safe mode:")
		}
	}
}

// ---------------------------------------------------------------------
// Errors and exit codes
// ---------------------------------------------------------------------

func TestReportAgentErrorExitCodesAndStderr(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantCode   int
		wantStderr []string // substrings; nil means stderr must be empty
	}{
		{"success", nil, 0, nil},
		{"help", flag.ErrHelp, 0, nil},
		{"usage already printed", errAgentUsage, 1, nil},
		{"slack refusal keeps its code", fmt.Errorf("adding reaction: %w", slack.SlackErrorResponse{Err: "already_reacted"}), 1, []string{"adding reaction: already_reacted"}},
		{"plain failure", errors.New("getting conversation C9: channel_not_found"), 1, []string{"channel_not_found"}},
		{"invalid_auth", fmt.Errorf("connecting to Acme: %w", fmt.Errorf("auth test failed: %w", slack.SlackErrorResponse{Err: "invalid_auth"})), 2, []string{"invalid_auth", "slk --add-workspace"}},
		{"not_authed", slack.SlackErrorResponse{Err: "not_authed"}, 2, []string{"not_authed", "slk --add-workspace"}},
		{"account_inactive", slack.SlackErrorResponse{Err: "account_inactive"}, 2, []string{"account_inactive"}},
		{"hand-rolled token_revoked", errors.New(`conversations.mark: token_revoked (body={"ok":false,"error":"token_revoked"})`), 2, []string{"token_revoked", "slk --add-workspace"}},
		{"auth code only as part of a word", errors.New("my_invalid_auth_rule failed"), 1, []string{"my_invalid_auth_rule"}},
		{"no token", &agentAuthError{msg: "no Slack workspace is signed in; run 'slk --add-workspace' first"}, 2, []string{"no Slack workspace is signed in"}},
		{"safe mode", errors.New("safe mode: write commands are disabled"), 1, []string{"safe mode: write commands are disabled"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, _, stderr := agentTestEnv(nil, nil)
			if code := reportAgentError(env, tc.err); code != tc.wantCode {
				t.Errorf("exit %d, want %d", code, tc.wantCode)
			}
			if tc.wantStderr == nil {
				if stderr.Len() != 0 {
					t.Errorf("stderr %q, want empty", stderr)
				}
				return
			}
			if strings.Count(stderr.String(), "\n") != 1 || !strings.HasSuffix(stderr.String(), "\n") {
				t.Errorf("stderr %q is not exactly one line", stderr)
			}
			for _, want := range tc.wantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr %q lacks %q", stderr, want)
				}
			}
			if strings.Count(stderr.String(), "slk --add-workspace") > 1 {
				t.Errorf("stderr %q repeats the sign-in hint", stderr)
			}
		})
	}
}

func TestReportAgentErrorKeepsOneLine(t *testing.T) {
	env, _, stderr := agentTestEnv(nil, nil)
	reportAgentError(env, errors.New("first\nsecond\r\nthird"))
	if got := stderr.String(); got != "first second third\n" {
		t.Errorf("stderr = %q", got)
	}
}

// ---------------------------------------------------------------------
// Workspace selection
// ---------------------------------------------------------------------

func TestSelectAgentToken(t *testing.T) {
	two := []config.OrderedToken{
		{Token: slackclient.Token{TeamID: "T1", TeamName: "Acme", Domain: "acme"}, Slug: "work"},
		{Token: slackclient.Token{TeamID: "T2", TeamName: "Other", Domain: "other"}},
	}
	cases := []struct {
		name                   string
		tokens                 []config.OrderedToken
		workspace, link, deflt string
		wantTeam, wantErrs     string
	}{
		{"link domain picks", two, "", "acme", "", "T1", ""},
		{"link domain case-insensitive", two, "", "OTHER", "", "T2", ""},
		{"explicit workspace wins over link", two, "T2", "acme", "", "T2", ""},
		{"slug", two, "work", "", "", "T1", ""},
		{"unmatched link with several", two, "", "nowhere", "", "", "--workspace"},
		{"no workspace with several", two, "", "", "", "", "--workspace is required"},
		{"unmatched link with one", two[1:], "", "nowhere", "", "T2", ""},
		{"one without anything", two[:1], "", "", "", "T1", ""},
		// [general] default_workspace in config.toml: used when nothing
		// more specific names a workspace.
		{"config default with several", two, "", "", "work", "T1", ""},
		{"config default for unmatched link", two, "", "nowhere", "other", "T2", ""},
		{"explicit workspace wins over config default", two, "T2", "", "work", "T2", ""},
		{"link wins over config default", two, "", "other", "work", "T2", ""},
		{"unknown config default", two, "", "", "ghost", "", "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok, err := selectAgentToken(tc.tokens, tc.workspace, tc.link, tc.deflt)
			if tc.wantErrs != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrs) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErrs)
				}
				if agentExitCode(err) != 1 {
					t.Errorf("exit code %d, want 1", agentExitCode(err))
				}
				return
			}
			if err != nil || tok.TeamID != tc.wantTeam {
				t.Errorf("got %q, %v; want %q", tok.TeamID, err, tc.wantTeam)
			}
		})
	}
}

func TestAgentNoStoredTokenExits2(t *testing.T) {
	// The real environment and openAgentClient, pointed at empty
	// directories: no token is found, so nothing reaches the network.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	env := newAgentEnv()
	env.stdout, env.stderr = &stdout, &stderr

	if code := runAgent(context.Background(), env, []string{"auth", "test"}); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "slk --add-workspace") {
		t.Errorf("stderr %q does not say to run slk --add-workspace", stderr.String())
	}
}

// ---------------------------------------------------------------------
// Targets
// ---------------------------------------------------------------------

func TestParseAgentTargetChannelID(t *testing.T) {
	for _, id := range []string{"C123ABC", "D0123456789", "G0456"} {
		got, err := parseAgentTarget(context.Background(), nil, id)
		if err != nil || got != (agentTarget{ChannelID: id}) {
			t.Errorf("parseAgentTarget(%q) = %+v, %v", id, got, err)
		}
		if d := agentTargetDomain(id); d != "" {
			t.Errorf("agentTargetDomain(%q) = %q, want empty", id, d)
		}
	}
}

func TestParseAgentTargetPermalink(t *testing.T) {
	cases := []struct {
		link     string
		want     agentTarget
		wantRoot string
	}{
		{
			"https://acme.slack.com/archives/C999/p1700000050000400?thread_ts=1700000000.000100&cid=C999",
			agentTarget{ChannelID: "C999", TS: "1700000050.000400", ThreadTS: "1700000000.000100"},
			"1700000000.000100",
		},
		{
			"https://acme.slack.com/archives/C999/p1700000050000400",
			agentTarget{ChannelID: "C999", TS: "1700000050.000400"},
			"1700000050.000400",
		},
	}
	for _, tc := range cases {
		got, err := parseAgentTarget(context.Background(), nil, tc.link)
		if err != nil || got != tc.want {
			t.Errorf("parseAgentTarget(%q) = %+v, %v; want %+v", tc.link, got, err, tc.want)
		}
		if got.ThreadRoot() != tc.wantRoot {
			t.Errorf("ThreadRoot() = %q, want %q", got.ThreadRoot(), tc.wantRoot)
		}
		if d := agentTargetDomain(tc.link); d != "acme" {
			t.Errorf("agentTargetDomain = %q, want acme", d)
		}
	}
}

func TestParseAgentTargetChannelName(t *testing.T) {
	srv := newFakeSlack(t, map[string]string{
		"/api/users.conversations": `{"ok":true,"channels":[
			{"id":"C8","name":"random","is_channel":true},
			{"id":"C9","name":"general","is_channel":true}
		],"response_metadata":{"next_cursor":""}}`,
		"/api/conversations.info": `{"ok":false,"error":"channel_not_found"}`,
	})
	client := newTestClient(t, srv.Server)

	for raw, want := range map[string]string{"#general": "C9", "#Random": "C8"} {
		got, err := parseAgentTarget(context.Background(), client, raw)
		if err != nil || got != (agentTarget{ChannelID: want}) {
			t.Errorf("parseAgentTarget(%q) = %+v, %v; want %s", raw, got, err, want)
		}
	}

	_, err := parseAgentTarget(context.Background(), client, "#missing")
	if err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Fatalf("err = %v, want one carrying channel_not_found", err)
	}
	if agentExitCode(err) != 1 {
		t.Errorf("exit code %d, want 1", agentExitCode(err))
	}
}

func TestOpenAgentTargetPicksWorkspaceFromLink(t *testing.T) {
	srv := newFakeSlack(t, map[string]string{"/api/auth.test": agentAuthTestOK})
	var calls []openCall
	env, _, _ := agentTestEnv(nil, fakeAgentOpener(t, srv, &calls))

	_, target, err := openAgentTarget(context.Background(), env, "", "https://acme.slack.com/archives/C1/p1700000000000100")
	if err != nil {
		t.Fatalf("openAgentTarget: %v", err)
	}
	if target.ChannelID != "C1" || target.TS != "1700000000.000100" {
		t.Errorf("target = %+v", target)
	}
	if len(calls) != 1 || calls[0] != (openCall{"", "acme"}) {
		t.Errorf("openClient calls = %+v, want one with linkDomain acme", calls)
	}
}

func TestOpenAgentTargetRejectsBadLinkBeforeConnecting(t *testing.T) {
	env, _, _ := agentTestEnv(nil, noOpener(t))
	_, _, err := openAgentTarget(context.Background(), env, "", "https://acme.slack.com/archives/C1")
	if err == nil || !strings.Contains(err.Error(), "not a Slack message link") {
		t.Errorf("err = %v", err)
	}
}

// A link's thread_ts goes out as a request parameter (the thread to
// read, reply in or mark), so one that is not a Slack timestamp is
// refused like a malformed link: before any request.
func TestParseAgentTargetRejectsBadThreadTS(t *testing.T) {
	for _, link := range []string{
		"https://acme.slack.com/archives/C1/p1700000050000400?thread_ts=yesterday&cid=C1",
		"https://acme.slack.com/archives/C1/p1700000050000400?thread_ts=1700000000.000100%26oldest%3D0",
	} {
		_, err := parseAgentTarget(context.Background(), nil, link)
		if err == nil || !strings.Contains(err.Error(), "not a Slack timestamp") {
			t.Errorf("parseAgentTarget(%q) err = %v; want a timestamp error", link, err)
		}
		env, _, _ := agentTestEnv(nil, noOpener(t))
		if _, _, err := openAgentTarget(context.Background(), env, "", link); err == nil {
			t.Errorf("openAgentTarget(%q) accepted it", link)
		}
	}
	// An empty thread_ts is none: the link is to a top-level message.
	link := "https://acme.slack.com/archives/C1/p1700000050000400?thread_ts="
	if got, err := parseAgentTarget(context.Background(), nil, link); err != nil || got.ThreadTS != "" {
		t.Errorf("parseAgentTarget(%q) = %+v, %v; want the link accepted, no thread", link, got, err)
	}
}

// ---------------------------------------------------------------------
// The message object
// ---------------------------------------------------------------------

// decodeMessage decodes a Slack API message object, so fixtures go
// through slack-go's own decoding (blocks, attachments) as a response
// would.
func decodeMessage(t *testing.T, raw string) slack.Message {
	t.Helper()
	var m slack.Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("decoding fixture: %v", err)
	}
	return m
}

func marshalAgentMessage(t *testing.T, m agentMessage) string {
	t.Helper()
	var buf bytes.Buffer
	env, _, _ := agentTestEnv(nil, nil)
	env.stdout = &buf
	if err := env.writeJSON(m); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

func TestEncodeAgentMessageMinimalKeepsOnlyTSAuthorContent(t *testing.T) {
	m := decodeMessage(t, `{"type":"message","ts":"1700000000.000100","text":""}`)
	got := marshalAgentMessage(t, encodeAgentMessage(m, "", agentMessageDefaults()))
	if want := `{"ts":"1700000000.000100","author":{},"content":""}`; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

const fullMessageFixture = `{
	"type":"message","subtype":"bot_message",
	"ts":"1700000050.000400","thread_ts":"1700000000.000100","reply_count":3,
	"user":"U1","bot_id":"B1","username":"deploy-bot",
	"text":"deployed <@U2> & <https://x.example|x>",
	"edited":{"user":"U1","ts":"1700000060.000000"},
	"files":[{"id":"F1","name":"a.png","title":"A","mimetype":"image/png"}],
	"reactions":[{"name":"eyes","users":["U1","U2"],"count":2}]
}`

func TestEncodeAgentMessageAllFields(t *testing.T) {
	m := decodeMessage(t, fullMessageFixture)
	opts := agentMessageDefaults()
	opts.IncludeReactions = true
	opts.Permalink = "https://acme.slack.com/archives/C1/p1700000050000400?thread_ts=1700000000.000100&cid=C1"
	got := marshalAgentMessage(t, encodeAgentMessage(m, "C1", opts))
	want := `{"ts":"1700000050.000400","thread_ts":"1700000000.000100","reply_count":3,"channel_id":"C1",` +
		`"author":{"user_id":"U1","bot_id":"B1","username":"deploy-bot"},` +
		`"content":"deployed @U2 & [x](https://x.example)","subtype":"bot_message","edited":true,` +
		`"files":[{"id":"F1","name":"a.png","title":"A","mimetype":"image/png"}],` +
		`"reactions":[{"name":"eyes","users":["U1","U2"],"count":2}],` +
		`"permalink":"https://acme.slack.com/archives/C1/p1700000050000400?thread_ts=1700000000.000100&cid=C1"}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestEncodeAgentMessageReactionsOnlyWhenAsked(t *testing.T) {
	m := decodeMessage(t, fullMessageFixture)
	got := marshalAgentMessage(t, encodeAgentMessage(m, "C1", agentMessageDefaults()))
	if strings.Contains(got, `"reactions"`) || strings.Contains(got, `"permalink"`) {
		t.Errorf("reactions or permalink printed without being asked for: %s", got)
	}
}

func TestEncodeAgentMessageTruncatesByCharacters(t *testing.T) {
	m := decodeMessage(t, `{"ts":"1.000001","text":"日本語のテキスト"}`)
	cases := []struct {
		max  int
		want string
	}{
		{3, "日本語\n…"},
		{8, "日本語のテキスト"},
		{100, "日本語のテキスト"},
		{-1, "日本語のテキスト"},
		{0, "\n…"},
	}
	for _, tc := range cases {
		opts := agentMessageDefaults()
		opts.MaxBodyChars = tc.max
		got := encodeAgentMessage(m, "", opts).Content
		if got != tc.want {
			t.Errorf("MaxBodyChars %d: content %q, want %q", tc.max, got, tc.want)
		}
	}

	long := decodeMessage(t, `{"ts":"1.000001","text":"`+strings.Repeat("é", 5000)+`"}`)
	if got, want := encodeAgentMessage(long, "", agentMessageDefaults()).Content, strings.Repeat("é", 4000)+"\n…"; got != want {
		t.Errorf("default cut to %d characters, want 4000 and the mark", len([]rune(got)))
	}
	// The cut counts the rendered content, not the raw text.
	link := decodeMessage(t, `{"ts":"1.000001","text":"<https://example.com/a-long-path|ab>"}`)
	opts := agentMessageDefaults()
	opts.MaxBodyChars = 4
	if got := encodeAgentMessage(link, "", opts).Content; got != "[ab]\n…" {
		t.Errorf("content %q, want the rendered link cut", got)
	}
}

// ---------------------------------------------------------------------
// auth test, end to end
// ---------------------------------------------------------------------

func TestAgentAuthTestPrintsIdentity(t *testing.T) {
	srv := newFakeSlack(t, map[string]string{"/api/auth.test": agentAuthTestOK})
	var calls []openCall
	env, stdout, stderr := agentTestEnv(nil, fakeAgentOpener(t, srv, &calls))

	if code := runAgent(context.Background(), env, []string{"auth", "test", "--workspace", "acme"}); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	want := `{"ok":true,"team_id":"T1","team":"Acme","user_id":"U1","user":"grant","url":"https://acme.slack.com/"}` + "\n"
	if stdout.String() != want {
		t.Errorf("stdout %q\nwant   %q", stdout, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr %q, want empty", stderr)
	}
	if len(calls) != 1 || calls[0] != (openCall{"acme", ""}) {
		t.Errorf("openClient calls = %+v, want one for workspace acme", calls)
	}
	srv.mu.Lock()
	n := len(srv.reqs)
	srv.mu.Unlock()
	if n != 1 {
		t.Errorf("%d requests, want exactly one auth.test", n)
	}
	srv.requestTo(t, "/api/auth.test")
	for _, secret := range []string{"xoxc-test", "d-cookie"} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Errorf("output contains the credential %q", secret)
		}
	}
}

func TestAgentWorkspaceDefaultsFromEnv(t *testing.T) {
	cases := []struct {
		name string
		vars map[string]string
		args []string
		want string
	}{
		{"env only", map[string]string{"SLK_WORKSPACE": "acme"}, []string{"auth", "test"}, "acme"},
		{"flag wins", map[string]string{"SLK_WORKSPACE": "acme"}, []string{"auth", "test", "--workspace", "other"}, "other"},
		{"neither", nil, []string{"auth", "test"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeSlack(t, map[string]string{"/api/auth.test": agentAuthTestOK})
			var calls []openCall
			env, _, stderr := agentTestEnv(tc.vars, fakeAgentOpener(t, srv, &calls))
			if code := runAgent(context.Background(), env, tc.args); code != 0 {
				t.Fatalf("exit %d, stderr %s", code, stderr)
			}
			if len(calls) != 1 || calls[0].workspace != tc.want {
				t.Errorf("openClient calls = %+v, want workspace %q", calls, tc.want)
			}
		})
	}
}

func TestAgentAuthTestExitCodes(t *testing.T) {
	cases := []struct {
		slackErr string
		wantCode int
	}{
		{"invalid_auth", 2},
		{"token_revoked", 2},
		{"fatal_error", 1},
	}
	for _, tc := range cases {
		t.Run(tc.slackErr, func(t *testing.T) {
			srv := newFakeSlack(t, map[string]string{"/api/auth.test": `{"ok":false,"error":"` + tc.slackErr + `"}`})
			var calls []openCall
			env, stdout, stderr := agentTestEnv(nil, fakeAgentOpener(t, srv, &calls))
			if code := runAgent(context.Background(), env, []string{"auth", "test"}); code != tc.wantCode {
				t.Errorf("exit %d, want %d", code, tc.wantCode)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout %q, want empty", stdout)
			}
			if !strings.Contains(stderr.String(), tc.slackErr) {
				t.Errorf("stderr %q lacks %q", stderr, tc.slackErr)
			}
			if tc.wantCode == 2 && !strings.Contains(stderr.String(), "slk --add-workspace") {
				t.Errorf("stderr %q lacks the sign-in hint", stderr)
			}
		})
	}
}
