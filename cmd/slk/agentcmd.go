// This file is the shared plumbing of the headless agent commands
// (`slk message list`, `slk listen`, ...), designed in
// docs/superpowers/specs/2026-11-12-agent-cli-design.md: the command
// table, the environment a command runs in, flag parsing, workspace
// selection, targets, the message encoder, JSON output, safe mode and
// exit codes. Each group of commands lives in its own agentcmd_*.go
// file and only registers here.
//
// Agent commands use only the tokens `slk --add-workspace` stored. They
// never re-mint from the desktop app (remintTokens, slackdesktop), so
// they never touch the keychain: callers run them unattended, where a
// keychain prompt would hang the caller.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/gammons/slk/internal/config"
	slackclient "github.com/gammons/slk/internal/slack"
	"github.com/gammons/slk/internal/slackhttp"
	"github.com/gammons/slk/internal/slackurl"
	"github.com/slack-go/slack"
)

// agentEnv is everything an agent command reads or writes besides its
// arguments. newAgentEnv builds the real one; tests build one around
// buffers and a fake Slack, so no test reaches the network or the
// user's token directory.
type agentEnv struct {
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
	// openClient returns a client for the chosen workspace that has
	// already run Connect (one auth.test). workspace is the --workspace
	// value. linkDomain is the subdomain of a message link given as the
	// target (agentTargetDomain), which picks the workspace when
	// workspace is empty; "" when the target is not a link.
	openClient func(ctx context.Context, workspace, linkDomain string) (*slackclient.Client, error)

	// restoredSession is the cache file the opened client was restored
	// from instead of calling auth.test ("" when it was connected). If
	// the command then fails with an auth error, runAgent deletes it so
	// the next run re-checks.
	restoredSession string

	// fileAuth is the token and cookie of the workspace openAgentClient
	// picked, for downloading that workspace's files (--download-files)
	// the way the TUI's downloader does. Zero until a client is opened.
	// Never printed.
	fileAuth slackhttp.TeamAuth

	// outMu keeps writeJSON's lines whole when listen writes from more
	// than one goroutine.
	outMu sync.Mutex
}

func newAgentEnv() *agentEnv {
	env := &agentEnv{
		stdout: os.Stdout,
		stderr: os.Stderr,
		getenv: os.Getenv,
	}
	env.openClient = func(ctx context.Context, workspace, linkDomain string) (*slackclient.Client, error) {
		return openAgentClient(ctx, env, workspace, linkDomain)
	}
	return env
}

// agentHandler runs one agent command. args are what follows the
// command's words (`slk message list C1 --limit 5` gives
// ["C1", "--limit", "5"]). Success is JSON on env.stdout and a nil
// error; the error decides the exit code (agentExitCode) and is printed
// on stderr as one line.
type agentHandler func(ctx context.Context, env *agentEnv, args []string) error

// agentCommand is one entry of the command table.
type agentCommand struct {
	path     string // the command's words, e.g. "message react add"
	synopsis string // its arguments, for usage text
	run      agentHandler
}

// agentCommands is the command table. It is a function rather than a
// package variable because handlers read it back (newAgentFlags looks
// up their synopsis), which a variable would make an initialization
// cycle. The handlers live in agentcmd_auth.go, agentcmd_read.go,
// agentcmd_read_lists.go, agentcmd_write.go, agentcmd_drafts.go,
// agentcmd_mark.go, agentcmd_later.go and agentcmd_listen.go.
func agentCommands() []agentCommand {
	return []agentCommand{
		{"message list", "<target> [--thread-ts TS] [--oldest TS] [--latest TS] [--limit N] [--max-body-chars N] [--resolve-users] [--include-reactions] [--download-files]", runAgentMessageList},
		{"message get", "<target> [--ts TS] [--thread-ts TS] [--max-body-chars N] [--download-files]", runAgentMessageGet},
		{"message send", "<target> <text> [--thread-ts TS] [--attach PATH]... (<text> may be left out with --attach)", runAgentMessageSend},
		{"message react add", "<target> <emoji> [--ts TS]", runAgentReactAdd},
		{"message react remove", "<target> <emoji> [--ts TS]", runAgentReactRemove},
		{"message edit", "<target> <text> [--ts TS]", runAgentMessageEdit},
		{"message delete", "<target> [--ts TS]", runAgentMessageDelete},
		{"message draft list", "[--limit N] [--all]", runAgentDraftList},
		{"message draft create", "<target> <text> [--thread-ts TS] [--broadcast]", runAgentDraftCreate},
		{"message draft update", "<draft-id> <text>", runAgentDraftUpdate},
		{"message draft delete", "<draft-id>", runAgentDraftDelete},
		{"unreads", "[--max-messages N] [--max-body-chars N]", runAgentUnreads},
		{"search messages", "<query> [--after YYYY-MM-DD] [--before YYYY-MM-DD] [--user U] [--limit N]", runAgentSearchMessages},
		{"channel list", "[--limit N] [--cursor C]", runAgentChannelList},
		{"channel mark", "<target> [--ts TS] [--thread-ts TS]", runAgentChannelMark},
		{"user get", "<user-id|email|@handle>", runAgentUserGet},
		{"later list", "[--state in_progress|archived|completed|all] [--limit N] [--max-body-chars N] [--counts-only]", runAgentLaterList},
		{"later save", "<target> [--ts TS]", runAgentLaterSave},
		{"later complete", "<target> [--ts TS]", runAgentLaterComplete},
		{"later archive", "<target> [--ts TS]", runAgentLaterArchive},
		{"later reopen", "<target> [--ts TS]", runAgentLaterReopen},
		{"later remove", "<target> [--ts TS]", runAgentLaterRemove},
		{"later remind", "<target> [--ts TS] (--in 30m|2h|3d | --at RFC3339|UNIX)", runAgentLaterRemind},
		{"auth test", "", runAgentAuthTest},
		{"listen", "", runAgentListen},
	}
}

// runAgentCommand runs the agent command named by args (os.Args[1:])
// and returns the process exit code. SIGINT and SIGTERM cancel the
// context; after the first one the default handling is back, so a
// second Ctrl-C kills a step that does not watch the context.
func runAgentCommand(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	context.AfterFunc(ctx, stop)
	return runAgent(ctx, newAgentEnv(), args)
}

// runAgent dispatches args to the command whose words they start with
// (the longest match) and maps the outcome to an exit code. An unknown
// command prints the usage on stderr and exits 1.
func runAgent(ctx context.Context, env *agentEnv, args []string) int {
	var match agentCommand
	matched := 0
	for _, c := range agentCommands() {
		words := strings.Fields(c.path)
		if len(words) > len(args) || len(words) <= matched {
			continue
		}
		if strings.Join(args[:len(words)], " ") == c.path {
			match, matched = c, len(words)
		}
	}
	if matched == 0 && asksForHelp(args) {
		printAgentUsage(env.stdout)
		return 0
	}
	if matched == 0 {
		fmt.Fprintf(env.stderr, "slk: unknown command %q\n", agentCommandWords(args))
		printAgentUsage(env.stderr)
		return 1
	}
	err := match.run(ctx, env, args[matched:])
	if agentExitCode(err) == 2 {
		// The session the cache vouched for no longer works.
		dropAgentSession(env.restoredSession)
	}
	return reportAgentError(env, err)
}

// asksForHelp reports whether args are command words followed by a help
// request (slk message --help, slk search help): what an agent types to
// learn a command group, which deserves the usage rather than an error.
func asksForHelp(args []string) bool {
	words := strings.Fields(agentCommandWords(args))
	n := len(words)
	if n == 0 {
		return false
	}
	if words[n-1] == "help" {
		return true
	}
	if n < len(args) {
		switch args[n] {
		case "-h", "-help", "--help":
			return true
		}
	}
	return false
}

// agentCommandWords is the command a user typed, for the unknown-command
// message: the leading arguments up to the first flag, at most three.
func agentCommandWords(args []string) string {
	var words []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") || len(words) == 3 {
			break
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}

func printAgentUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: slk <command> [arguments] [flags]")
	fmt.Fprintln(w, "\nCommands (JSON on stdout; exit 0 ok, 1 failure, 2 not signed in):")
	for _, c := range agentCommands() {
		fmt.Fprintf(w, "  slk %s\n", strings.TrimSpace(c.path+" "+c.synopsis))
	}
	fmt.Fprintln(w, "\nEvery command takes --workspace <slug|team id|name|domain>, needed when more than one is signed in; SLK_WORKSPACE, then [general] default_workspace in config.toml, set its default.")
	fmt.Fprintln(w, "<target> is a channel ID (C…, D…, G…), #channel-name, or a message link.")
	fmt.Fprintln(w, "Flags may come before or after arguments; everything after -- is an argument.")
	fmt.Fprintln(w, "SLK_SAFE_MODE=1 refuses send, edit, delete, react, channel mark, later's writes (all but later list) and draft update and delete; draft list and create are allowed.")
}

// ---------------------------------------------------------------------
// Errors and exit codes
// ---------------------------------------------------------------------

// errAgentUsage is returned for bad arguments once the usage has been
// printed, so reportAgentError adds nothing. Exit code 1.
var errAgentUsage = errors.New("invalid arguments")

// agentAuthError is a failure the user fixes by signing in (again):
// exit code 2. Slack's own auth refusals are recognized by their code
// instead (agentAuthCodes), so they need not be wrapped in one.
type agentAuthError struct{ msg string }

func (e *agentAuthError) Error() string { return e.msg }

// agentAuthCodes are the Slack error codes that mean the stored session
// no longer works.
var agentAuthCodes = map[string]bool{
	"invalid_auth":     true,
	"not_authed":       true,
	"token_revoked":    true,
	"token_expired":    true,
	"account_inactive": true,
}

// agentAuthCodeRe finds one of agentAuthCodes in an error's text, for
// the hand-rolled endpoints (postForm callers) whose errors are plain
// strings like "conversations.mark: invalid_auth (body=...)".
var agentAuthCodeRe = regexp.MustCompile(`(?:^|[^a-z_])(?:invalid_auth|not_authed|token_revoked|token_expired|account_inactive)(?:$|[^a-z_])`)

// agentExitCode maps a handler's error to the process exit code: 0 for
// none (or a help request), 2 for an auth failure, 1 for anything else.
func agentExitCode(err error) int {
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return 0
	}
	var authErr *agentAuthError
	if errors.As(err, &authErr) {
		return 2
	}
	var slackErr slack.SlackErrorResponse
	if errors.As(err, &slackErr) && agentAuthCodes[slackErr.Err] {
		return 2
	}
	if agentAuthCodeRe.MatchString(err.Error()) {
		return 2
	}
	return 1
}

// reportAgentError prints err on env.stderr as one line and returns the
// exit code. The line is err's own text, so Slack's error code (which
// slack-go errors carry as their text) survives any %w wrapping, and a
// "safe mode: ..." refusal starts the line. An auth failure gets the
// hint to run `slk --add-workspace`.
func reportAgentError(env *agentEnv, err error) int {
	code := agentExitCode(err)
	if code == 0 || errors.Is(err, errAgentUsage) {
		return code
	}
	msg := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(err.Error())
	if code == 2 && !strings.Contains(msg, "slk --add-workspace") {
		msg += "; run 'slk --add-workspace' to sign in again"
	}
	fmt.Fprintln(env.stderr, msg)
	return code
}

// requireWritable refuses a write (send, edit, delete, react, channel
// mark, later's writes, draft update and delete; listing and creating
// drafts are exempt) while SLK_SAFE_MODE is set:
// Callers set it in unattended sessions and unset it only for the one
// command the user approved. Call it first, before opening a
// client, so a refused write makes no request at all. Any value but ""
// and "0" turns safe mode on, so a typo errs on the side of refusing.
func requireWritable(env *agentEnv) error {
	if v := env.getenv("SLK_SAFE_MODE"); v != "" && v != "0" {
		return errors.New("safe mode: write commands are disabled while SLK_SAFE_MODE is set")
	}
	return nil
}

// ---------------------------------------------------------------------
// Flags
// ---------------------------------------------------------------------

// agentFlags is a stdlib flag set for one command whose parse accepts
// flags before and after the positional arguments. Register flags on
// it as on any *flag.FlagSet (f.Int, f.String, f.Bool, ...).
type agentFlags struct {
	*flag.FlagSet
	path string
	env  *agentEnv
}

// newAgentFlags returns the flag set for the command at path (its words
// in the command table). Errors and usage go to env.stderr.
func newAgentFlags(env *agentEnv, path string) *agentFlags {
	fs := flag.NewFlagSet("slk "+path, flag.ContinueOnError)
	fs.SetOutput(env.stderr)
	synopsis := path
	for _, c := range agentCommands() {
		if c.path == path {
			synopsis = strings.TrimSpace(path + " " + c.synopsis)
		}
	}
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: slk %s\n", synopsis)
		fs.PrintDefaults()
	}
	return &agentFlags{FlagSet: fs, path: path, env: env}
}

// workspace registers the --workspace flag every command takes. It
// defaults to SLK_WORKSPACE, so a caller signed in to several
// workspaces need not pass it each time.
func (f *agentFlags) workspace() *string {
	return f.String("workspace", f.env.getenv("SLK_WORKSPACE"), "workspace slug, team ID, name or domain (needed when more than one is signed in; default $SLK_WORKSPACE, then config default_workspace)")
}

// agentDefaultMaxBodyChars is --max-body-chars' default.
const agentDefaultMaxBodyChars = 4000

// maxBodyChars registers --max-body-chars (default 4000, -1 for no
// limit), for agentMessageOptions.MaxBodyChars.
func (f *agentFlags) maxBodyChars() *int {
	return f.Int("max-body-chars", agentDefaultMaxBodyChars, "cut each message's content to `N` characters; -1 keeps it whole")
}

// parse parses args and returns the positional arguments in order.
// Flags may come before, between or after them; everything after a
// "--" is positional, so text or an emoji name starting with "-" can be
// passed. A lone "-" is positional. An unknown flag prints the error
// and the usage and returns errAgentUsage; -h/--help prints the usage
// and returns flag.ErrHelp (exit 0).
func (f *agentFlags) parse(args []string) ([]string, error) {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		if strings.Contains(a, "=") {
			continue
		}
		// A flag that takes a value takes the next argument with it,
		// whatever it looks like ("--max-body-chars -1").
		if fl := f.Lookup(strings.TrimLeft(a, "-")); fl != nil && !isBoolFlag(fl) && i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	if err := f.FlagSet.Parse(flags); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, errAgentUsage
	}
	return positional, nil
}

// parseExact is parse for a command that takes exactly the positional
// arguments named, in order (names are for the error message only).
func (f *agentFlags) parseExact(args []string, names ...string) ([]string, error) {
	positional, err := f.parse(args)
	if err != nil {
		return nil, err
	}
	if err := f.wantPositional(positional, names...); err != nil {
		return nil, err
	}
	return positional, nil
}

// wantPositional is parseExact's check on its own, for a command that
// decides after parsing which arguments it takes (message send's text
// is optional with --attach).
func (f *agentFlags) wantPositional(positional []string, names ...string) error {
	switch {
	case len(positional) < len(names):
		return f.usageError("missing <%s>", names[len(positional)])
	case len(positional) > len(names):
		return f.usageError("unexpected argument %q", positional[len(names)])
	}
	return nil
}

// usageError prints a problem with the arguments and the usage, and
// returns errAgentUsage for the handler to return.
func (f *agentFlags) usageError(format string, a ...any) error {
	fmt.Fprintf(f.Output(), "slk %s: %s\n", f.path, fmt.Sprintf(format, a...))
	f.Usage()
	return errAgentUsage
}

func isBoolFlag(fl *flag.Flag) bool {
	b, ok := fl.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// ---------------------------------------------------------------------
// Workspace and client
// ---------------------------------------------------------------------

// openAgentClient is agentEnv.openClient for real use: the token comes
// from slk's token store (never the keychain), the workspace is picked
// by selectAgentToken, and the client is connected: either by Connect
// (one auth.test, which then refreshes the session cache) or, when the
// cache is valid for the token file as it is now, restored from it with
// no request. ctx from withFreshAuth always takes the Connect path.
// env.restoredSession records a restore, for runAgent's cleanup.
func openAgentClient(ctx context.Context, env *agentEnv, workspace, linkDomain string) (*slackclient.Client, error) {
	store := slackclient.NewTokenStore(filepath.Join(xdgData(), "tokens"))
	tokens, err := store.List()
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	cfg, _ := config.Load(filepath.Join(xdgConfig(), "config.toml")) // best-effort, as in exportChannel
	tok, err := selectAgentToken(config.OrderTokens(tokens, cfg), workspace, linkDomain, cfg.General.DefaultWorkspace)
	if err != nil {
		return nil, err
	}
	env.fileAuth = slackhttp.TeamAuth{TeamID: tok.TeamID, Token: tok.AccessToken, DCookie: tok.Cookie}
	client := slackclient.NewClient(tok.AccessToken, tok.Cookie)
	if agentClientHook != nil {
		agentClientHook(client)
	}
	if !wantsFreshAuth(ctx) {
		if sess, ok := loadAgentSession(tok.TeamID); ok {
			client.RestoreAgentSession(sess)
			env.restoredSession = agentSessionPath(tok.TeamID)
			return client, nil
		}
	}
	if err := client.Connect(ctx); err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", tok.TeamName, err)
	}
	saveAgentSession(tok.TeamID, client.AgentSession())
	return client, nil
}

// agentClientHook, when set, sees each client openAgentClient builds,
// before it connects. Tests use it to point the client at a fake Slack.
var agentClientHook func(*slackclient.Client)

// selectAgentToken picks the stored token to use. An explicit workspace
// (--workspace or SLK_WORKSPACE) wins and follows selectExportToken's
// rules. Otherwise a message link's subdomain picks the workspace whose
// slug, team ID, name or domain it equals; failing that, the config's
// [general] default_workspace (defaultWorkspace) is used, the same
// setting that picks the TUI's first workspace, so a machine signed in
// to several workspaces needs no per-caller setting. With none of
// those and only one workspace signed in, that one is used. No stored
// token at all is an auth error.
func selectAgentToken(tokens []config.OrderedToken, workspace, linkDomain, defaultWorkspace string) (slackclient.Token, error) {
	if len(tokens) == 0 {
		return slackclient.Token{}, &agentAuthError{msg: "no Slack workspace is signed in; run 'slk --add-workspace' first"}
	}
	if workspace != "" {
		return selectExportToken(tokens, workspace)
	}
	if linkDomain != "" {
		if tok, err := selectExportToken(tokens, linkDomain); err == nil {
			return tok, nil
		}
	}
	if defaultWorkspace != "" {
		tok, err := selectExportToken(tokens, defaultWorkspace)
		if err != nil {
			return slackclient.Token{}, fmt.Errorf("config default_workspace: %w", err)
		}
		return tok, nil
	}
	if linkDomain == "" {
		return selectExportToken(tokens, "")
	}
	if len(tokens) == 1 {
		return tokens[0].Token, nil
	}
	return slackclient.Token{}, fmt.Errorf("no signed-in workspace matches the link's host %s.slack.com; pass --workspace (see 'slk --list-workspaces')", linkDomain)
}

// ---------------------------------------------------------------------
// Targets
// ---------------------------------------------------------------------

// agentTarget is a resolved <target>: a conversation, and for a message
// link the message and the thread it is in.
type agentTarget struct {
	ChannelID string
	TS        string // the linked message's ts; "" unless the target is a link
	ThreadTS  string // the link's ?thread_ts=; "" unless the link points into a thread
}

// ThreadRoot is the ts of the thread a link points into: its thread_ts,
// else the linked message itself. "" when the target is not a link.
func (t agentTarget) ThreadRoot() string {
	if t.ThreadTS != "" {
		return t.ThreadTS
	}
	return t.TS
}

// agentChannelIDRe matches the conversation IDs a target may be given
// as: channels (C…), DMs (D…) and private or group conversations (G…).
var agentChannelIDRe = regexp.MustCompile(`^[CDG][A-Z0-9]{2,}$`)

// parseAgentLink parses raw when it is a URL. isLink is false when raw
// is not a URL at all; err is set when it is one but not a Slack message
// link, or when its thread_ts is not a Slack timestamp: that value goes
// out as a request parameter (the thread to read, reply in or mark), so
// it is checked as --thread-ts is (checkAgentTS). slackurl.Parse passes
// thread_ts through as given.
func parseAgentLink(raw string) (target agentTarget, domain string, isLink bool, err error) {
	if !strings.Contains(raw, "://") {
		return agentTarget{}, "", false, nil
	}
	p, ok := slackurl.Parse(raw)
	if !ok {
		return agentTarget{}, "", true, fmt.Errorf("target %q is not a Slack message link (https://<workspace>.slack.com/archives/<channel>/p<ts>)", raw)
	}
	if p.ThreadTS != "" && !agentTSRe.MatchString(string(p.ThreadTS)) {
		return agentTarget{}, "", true, fmt.Errorf("target %q: thread_ts %q is not a Slack timestamp (like 1793932809.791579)", raw, p.ThreadTS)
	}
	return agentTarget{
		ChannelID: string(p.ChannelID),
		TS:        string(p.MessageTS),
		ThreadTS:  string(p.ThreadTS),
	}, p.Subdomain, true, nil
}

// agentTargetDomain is the workspace subdomain of raw when it is a
// message link, "" otherwise. It needs no client, so it can pick the
// workspace before one is opened (agentEnv.openClient's linkDomain).
func agentTargetDomain(raw string) string {
	_, domain, _, _ := parseAgentLink(raw)
	return domain
}

// parseAgentTarget resolves a <target>: a conversation ID as given, a
// message link through slackurl.Parse, and anything else (#name, or a
// bare name) through resolveExportChannel, the only form that calls
// Slack. client may be nil for the first two forms.
func parseAgentTarget(ctx context.Context, client *slackclient.Client, raw string) (agentTarget, error) {
	if raw == "" {
		return agentTarget{}, errors.New("empty target")
	}
	if t, _, isLink, err := parseAgentLink(raw); isLink {
		return t, err
	}
	if agentChannelIDRe.MatchString(raw) {
		return agentTarget{ChannelID: raw}, nil
	}
	if client == nil {
		return agentTarget{}, fmt.Errorf("target %q needs a Slack lookup and no client was given", raw)
	}
	ch, _, err := resolveExportChannel(ctx, client, raw)
	if err != nil {
		return agentTarget{}, err
	}
	return agentTarget{ChannelID: ch.ID}, nil
}

// openAgentTarget is the usual start of a command that takes a
// <target>: it rejects a malformed link before any request, opens the
// client for the target's workspace, and resolves the target.
func openAgentTarget(ctx context.Context, env *agentEnv, workspace, raw string) (*slackclient.Client, agentTarget, error) {
	_, domain, _, err := parseAgentLink(raw)
	if err != nil {
		return nil, agentTarget{}, err
	}
	client, err := env.openClient(ctx, workspace, domain)
	if err != nil {
		return nil, agentTarget{}, err
	}
	target, err := parseAgentTarget(ctx, client, raw)
	if err != nil {
		return nil, agentTarget{}, err
	}
	return client, target, nil
}

// ---------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------

// writeJSON writes v to stdout as one line of JSON and flushes it, so
// listen's NDJSON reaches a pipe line by line. HTML characters stay as
// they are: message text is full of <@U…> and <url|label>.
func (env *agentEnv) writeJSON(v any) error {
	env.outMu.Lock()
	defer env.outMu.Unlock()
	enc := json.NewEncoder(env.stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	if f, ok := env.stdout.(interface{ Flush() error }); ok {
		if err := f.Flush(); err != nil {
			return fmt.Errorf("writing output: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// The message object
// ---------------------------------------------------------------------

// agentMessage is the one shape every command prints a message in
// (message list/get, search messages, unreads, listen). Empty fields
// are left out, except ts, author and content.
type agentMessage struct {
	TS         string          `json:"ts"`
	ThreadTS   string          `json:"thread_ts,omitempty"`
	ReplyCount int             `json:"reply_count,omitempty"`
	ChannelID  string          `json:"channel_id,omitempty"`
	Author     agentAuthor     `json:"author"`
	Content    string          `json:"content"`
	Subtype    string          `json:"subtype,omitempty"`
	Edited     bool            `json:"edited,omitempty"`
	Files      []agentFile     `json:"files,omitempty"`
	Reactions  []agentReaction `json:"reactions,omitempty"`
	Permalink  string          `json:"permalink,omitempty"`
}

type agentAuthor struct {
	UserID   string `json:"user_id,omitempty"`
	BotID    string `json:"bot_id,omitempty"`
	Username string `json:"username,omitempty"`
}

type agentFile struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Title    string `json:"title,omitempty"`
	Mimetype string `json:"mimetype,omitempty"`
	// Size, Path and DownloadError are set only with --download-files
	// (agentMessageOptions.Downloads), so the object keeps its shape
	// without it. Size is Slack's, in bytes; Path is the downloaded
	// copy's absolute path; DownloadError says why there is none.
	Size          *int   `json:"size,omitempty"`
	Path          string `json:"path,omitempty"`
	DownloadError string `json:"download_error,omitempty"`
}

type agentReaction struct {
	Name  string   `json:"name"`
	Users []string `json:"users,omitempty"`
	Count int      `json:"count"`
}

// agentMessageOptions are encodeAgentMessage's knobs. Start from
// agentMessageDefaults(): the zero value cuts content to nothing.
type agentMessageOptions struct {
	// MaxBodyChars cuts content to this many characters (runes); -1
	// keeps it whole. --max-body-chars, default 4000.
	MaxBodyChars int
	// CutMarker is appended to content that MaxBodyChars cut. It is
	// agent-slack's: "\n…" (agentCutMarker), except in unreads, where
	// agent-slack writes "\n..." (agentUnreadsCutMarker).
	CutMarker string
	// IncludeReactions adds "reactions" (--include-reactions).
	IncludeReactions bool
	// Permalink is printed when not empty: search results pass Slack's
	// own link; elsewhere it stays "".
	Permalink string
	// Downloads, when not nil, is --download-files' outcome by file ID
	// (downloadAgentFiles): every file then gets its size and its path
	// or download error.
	Downloads map[string]agentFileDownload
}

// The marks agent-slack appends to a body it cut.
const (
	agentCutMarker        = "\n…"
	agentUnreadsCutMarker = "\n..."
)

func agentMessageDefaults() agentMessageOptions {
	return agentMessageOptions{MaxBodyChars: agentDefaultMaxBodyChars, CutMarker: agentCutMarker}
}

// encodeAgentMessage converts a Slack message to the agent message
// object. channelID is the conversation the message is in; every
// command passes it. content is the message rendered as agent-slack
// renders it (renderAgentContent, agentcmd_content.go), then cut to
// opts.MaxBodyChars.
func encodeAgentMessage(m slack.Message, channelID string, opts agentMessageOptions) agentMessage {
	content := renderAgentContent(m)
	if cut := truncateRunes(content, opts.MaxBodyChars); cut != content {
		content = cut + opts.CutMarker
	}
	out := agentMessage{
		TS:         m.Timestamp,
		ThreadTS:   m.ThreadTimestamp,
		ReplyCount: m.ReplyCount,
		ChannelID:  channelID,
		Author:     agentAuthor{UserID: m.User, BotID: m.BotID, Username: m.Username},
		Content:    content,
		Subtype:    m.SubType,
		Edited:     m.Edited != nil,
		Permalink:  opts.Permalink,
	}
	for _, f := range m.Files {
		af := agentFile{ID: f.ID, Name: f.Name, Title: f.Title, Mimetype: f.Mimetype}
		if opts.Downloads != nil {
			size := f.Size
			af.Size = &size
			d, ok := opts.Downloads[f.ID]
			if !ok {
				d.Err = "not downloaded"
			}
			af.Path, af.DownloadError = d.Path, d.Err
		}
		out.Files = append(out.Files, af)
	}
	if opts.IncludeReactions {
		for _, r := range m.Reactions {
			out.Reactions = append(out.Reactions, agentReaction{Name: r.Name, Users: r.Users, Count: r.Count})
		}
	}
	return out
}

// truncateRunes returns the first n runes of s, or s whole when n < 0.
func truncateRunes(s string, n int) string {
	if n < 0 {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
