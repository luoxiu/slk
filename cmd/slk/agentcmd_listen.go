// This file implements `slk listen`: one JSON line per WebSocket event
// on stdout until SIGINT, SIGTERM or an auth failure, designed in
// docs/superpowers/specs/2026-11-12-agent-cli-design.md ("slk listen").
//
// The connection loop follows ConnectionManager's (internal/slack/
// connection.go): StartWebSocket, wait on WsDone, back off from 1s
// doubling to 30s, start again on the same client. It is written out
// here instead of reused because ConnectionManager cannot do what the
// caller relies on:
//
//   - it never stops: a dead token is retried forever and Run returns
//     nothing, where listen must exit 2;
//   - it drops why a connection ended: StartWebSocket's error is
//     swallowed and OnDisconnect takes none, where `disconnected`
//     carries an `error`;
//   - it resets the backoff on every completed handshake, so a server
//     that accepts and hangs up at once is redialled every second;
//     listen resets only after a connection that stayed up
//     (listenStableAfter).
//
// Like the TUI, listen watches for the machine waking from sleep
// (internal/wake). The TUI runs a catch-up then; listen's caller does
// its own catch-up after every `disconnected`, so a wake drops the
// connection and dials again.

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/gammons/slk/internal/mention"
	slackclient "github.com/gammons/slk/internal/slack"
	"github.com/gammons/slk/internal/wake"
	"github.com/slack-go/slack"
)

const (
	// listenBackoffMin and listenBackoffMax are ConnectionManager's
	// bounds: the first retry after 1s, doubling up to 30s.
	listenBackoffMin = time.Second
	listenBackoffMax = 30 * time.Second
	// listenStableAfter is how long a connection must have stayed up
	// for the next retry to start again from listenBackoffMin.
	listenStableAfter = time.Minute
	// listenAuthTimeout bounds the auth.test run after an attempt that
	// never reached Slack's hello.
	listenAuthTimeout = 30 * time.Second
	// listenStepTimeout bounds a step that cannot be cancelled: the
	// WebSocket handshake (its dialer has no timeout) and the first
	// Connect (slack-go's auth.test takes no context). The operating
	// system ends a dial to a dead network within about two and a half
	// minutes, so a step still running after three is stuck for good.
	listenStepTimeout = 3 * time.Minute
	// listenHelloTimeout is how long a connection may go without Slack's
	// hello, which normally is the first frame, within a second or two
	// of the handshake. StartWebSocket's 60s read deadline ends a socket
	// that sends nothing, and pings reset it, so a socket Slack keeps
	// pinging but never says hello on would otherwise stay up, and
	// listen silent, for good. Past 60s, a socket that went quiet is
	// still ended by the read deadline first ("connection closed before
	// hello"); under listenStepTimeout, because this one is no hang: the
	// socket can be closed and dialled again, so listen reconnects
	// instead of exiting.
	listenHelloTimeout = 90 * time.Second
	// listenStopWait is how long a signal waits for the socket's event
	// owner to finish before listen exits anyway.
	listenStopWait = 2 * time.Second
	// listenWakeInterval and listenWakeThreshold are the TUI's wake
	// detector settings (cmd/slk/main.go).
	listenWakeInterval  = 10 * time.Second
	listenWakeThreshold = 5 * time.Second
)

// errListenStopped ends listen without an error: a signal arrived.
var errListenStopped = errors.New("listen stopped")

// listenSocket is the part of *slackclient.Client the connection loop
// drives; tests stand in a scripted socket.
type listenSocket interface {
	StartWebSocket(slackclient.EventHandler) error
	StopWebSocket() error
	WsDone() <-chan struct{}
	CheckAuth(context.Context) error
}

var _ listenSocket = (*slackclient.Client)(nil)

// runAgentListen is `slk listen`. The context runAgentCommand gives it
// ends on SIGINT or SIGTERM; listen then closes the socket and exits 0.
func runAgentListen(ctx context.Context, env *agentEnv, args []string) error {
	f := newAgentFlags(env, "listen")
	workspace := f.workspace()
	if _, err := f.parseExact(args); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	wakes := make(chan time.Duration, 1)
	go wake.New(listenWakeInterval, listenWakeThreshold, func(slept time.Duration) {
		select {
		case wakes <- slept:
		default:
		}
	}).Run(ctx)

	l := newAgentListener(env, wakes)
	client, err := l.open(ctx, func(ctx context.Context) (*slackclient.Client, error) {
		return env.openClient(withFreshAuth(ctx), *workspace, "")
	})
	if err == nil {
		l.teamID, l.userID = client.TeamID(), client.UserID()
		err = l.run(ctx, client)
	}
	if err == nil || errors.Is(err, errListenStopped) {
		return nil
	}
	return listenRedactedError{err}
}

// agentListener is one `slk listen` run: its output, its timings, and
// the channels its connections report on.
type agentListener struct {
	env *agentEnv
	// teamID and userID are the signed-in workspace and user: hello's
	// fields, and userID is the "me" of mentions_me.
	teamID, userID string
	// now stamps each line's `at`.
	now func() time.Time
	// wakes delivers how long the machine slept, once per wake.
	wakes <-chan time.Duration
	// failed takes the first error a connection's handler got writing
	// stdout. Buffered, so a handler never blocks on it.
	failed chan error

	backoffMin, backoffMax, stableAfter              time.Duration
	authTimeout, stepTimeout, helloTimeout, stopWait time.Duration
}

func newAgentListener(env *agentEnv, wakes <-chan time.Duration) *agentListener {
	return &agentListener{
		env:          env,
		now:          time.Now,
		wakes:        wakes,
		failed:       make(chan error, 1),
		backoffMin:   listenBackoffMin,
		backoffMax:   listenBackoffMax,
		stableAfter:  listenStableAfter,
		authTimeout:  listenAuthTimeout,
		stepTimeout:  listenStepTimeout,
		helloTimeout: listenHelloTimeout,
		stopWait:     listenStopWait,
	}
}

// open returns the connected client, retrying while the failure is the
// network's or Slack's (listenRetryable) and writing a `disconnected`
// line for each failed try. An auth refusal or a local problem (no
// signed-in workspace matches) is returned as it is.
func (l *agentListener) open(ctx context.Context, open func(context.Context) (*slackclient.Client, error)) (*slackclient.Client, error) {
	backoff := l.backoffMin
	for {
		client, err, stop := listenStep(ctx, l.stepTimeout, "connecting to Slack", func() (*slackclient.Client, error) {
			return open(ctx)
		})
		switch {
		case stop != nil:
			return nil, stop
		case err == nil:
			return client, nil
		case agentExitCode(err) == 2 || !listenRetryable(err):
			return nil, err
		}
		if err := l.writeDisconnected(err.Error()); err != nil {
			return nil, err
		}
		if err := l.sleep(ctx, backoff); err != nil {
			return nil, err
		}
		backoff = min(2*backoff, l.backoffMax)
	}
}

// run connects sock and reconnects it until ctx ends (errListenStopped),
// Slack refuses the session (*agentAuthError), stdout fails, or a
// handshake hangs past stepTimeout.
//
// Every connection attempt ends in exactly one `disconnected` line,
// written by this goroutine once the attempt is over: after
// StartWebSocket failed, or after WsDone closed, which happens only
// once the attempt's handler has returned for the last time. The next
// attempt, and so its `hello`, starts after that line. A connection
// still without hello after helloTimeout is closed and handled like any
// other attempt that never reached hello: auth.test, then backoff.
func (l *agentListener) run(ctx context.Context, sock listenSocket) error {
	backoff := l.backoffMin
	for {
		l.drainWakes()
		s := &listenSession{l: l}
		started := time.Now()
		_, startErr, stop := listenStep(ctx, l.stepTimeout, "the WebSocket handshake", func() (struct{}, error) {
			return struct{}{}, sock.StartWebSocket(s)
		})
		if stop != nil {
			return stop
		}
		var reason string
		if startErr != nil {
			reason = startErr.Error()
		} else {
			var err error
			if reason, err = l.await(ctx, sock, s); err != nil {
				return err
			}
		}
		if err := l.writeDisconnected(reason); err != nil {
			return err
		}
		switch {
		case !s.hello.Load():
			// The attempt never reached Slack's hello: the network, or
			// Slack turning the session away. Only auth.test tells which;
			// the handshake's own error does not carry the HTTP status.
			if err := l.checkAuth(ctx, sock); err != nil {
				return err
			}
		case time.Since(started) >= l.stableAfter:
			backoff = l.backoffMin
		}
		if err := l.sleep(ctx, backoff); err != nil {
			return err
		}
		backoff = min(2*backoff, l.backoffMax)
	}
}

// await waits for the running connection to end and returns why, for
// the `disconnected` line. An error ends listen: errListenStopped on a
// signal, or stdout failing.
//
// A dead connection that sends nothing ends by itself:
// StartWebSocket's reader gives up after 60s without a frame or a
// ping, and WsDone closes. One that is kept alive but has not said
// hello after helloTimeout is closed here.
func (l *agentListener) await(ctx context.Context, sock listenSocket, s *listenSession) (string, error) {
	helloTimer := time.NewTimer(l.helloTimeout)
	defer helloTimer.Stop()
	noHello := helloTimer.C
	for {
		select {
		case <-sock.WsDone():
			// The reader stopped but left the connection open; close it
			// now rather than at the next garbage collection.
			_ = sock.StopWebSocket()
			if !s.hello.Load() {
				return "connection closed before hello", nil
			}
			return "connection lost", nil
		case <-noHello:
			if s.hello.Load() {
				noHello = nil
				continue
			}
			return l.drop(ctx, sock, fmt.Sprintf("no hello within %s", l.helloTimeout))
		case slept := <-l.wakes:
			return l.drop(ctx, sock, fmt.Sprintf("reconnecting after the system slept (%s)", slept.Round(time.Second)))
		case <-ctx.Done():
			l.stop(sock)
			return "", errListenStopped
		case err := <-l.failed:
			l.stop(sock)
			return "", err
		}
	}
}

// drop closes a connection listen gives up on and returns reason once
// its event owner has finished, so the `disconnected` line comes after
// the connection's last event.
func (l *agentListener) drop(ctx context.Context, sock listenSocket, reason string) (string, error) {
	_ = sock.StopWebSocket()
	select {
	case <-sock.WsDone():
		return reason, nil
	case <-ctx.Done():
		return "", errListenStopped
	case err := <-l.failed:
		return "", err
	}
}

// stop closes the socket on the way out and waits a moment for its
// event owner, so events already read are written before listen exits.
func (l *agentListener) stop(sock listenSocket) {
	_ = sock.StopWebSocket()
	t := time.NewTimer(l.stopWait)
	defer t.Stop()
	select {
	case <-sock.WsDone():
	case <-t.C:
	}
}

// checkAuth runs auth.test and returns an *agentAuthError (exit 2) when
// Slack refuses the session. Any other outcome, a network failure
// included, is nil: the next attempt finds out more.
func (l *agentListener) checkAuth(ctx context.Context, sock listenSocket) error {
	ctx, cancel := context.WithTimeout(ctx, l.authTimeout)
	defer cancel()
	err := sock.CheckAuth(ctx)
	var refused slack.SlackErrorResponse
	if errors.As(err, &refused) && agentAuthCodes[refused.Err] {
		return &agentAuthError{msg: "listen: auth.test: " + refused.Err}
	}
	return nil
}

// sleep waits d, or returns errListenStopped when ctx ends first.
func (l *agentListener) sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return errListenStopped
	case <-t.C:
		return nil
	}
}

// drainWakes forgets a wake reported while no connection was up: the
// connection about to start is already a fresh one.
func (l *agentListener) drainWakes() {
	select {
	case <-l.wakes:
	default:
	}
}

// listenStep runs step, which cannot be cancelled, and waits for it.
// stop is errListenStopped when ctx ends first (listen exits and leaves
// step behind), or an error when step is still running after timeout:
// the client is busy inside it, so listen can neither retry on it nor
// wait any longer, and exits 1 for its supervisor to restart it.
func listenStep[T any](ctx context.Context, timeout time.Duration, what string, step func() (T, error)) (v T, err, stop error) {
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := step()
		done <- result{v, err}
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r := <-done:
		return r.v, r.err, nil
	case <-ctx.Done():
		return v, nil, errListenStopped
	case <-t.C:
		return v, nil, fmt.Errorf("listen: %s did not finish within %s", what, timeout)
	}
}

// listenRetryable reports whether a failed connect came from the
// network or from Slack: a timeout, a refused or reset connection, an
// HTTP error status, a rate limit, or an API error. Auth refusals are
// caught before this. Anything else is local (no stored token matches
// --workspace, the token store cannot be read), and retrying would not
// change it.
func listenRetryable(err error) bool {
	var netErr net.Error
	var rateLimited *slack.RateLimitedError
	var status slack.StatusCodeError
	var refused slack.SlackErrorResponse
	return errors.As(err, &netErr) || errors.As(err, &rateLimited) ||
		errors.As(err, &status) || errors.As(err, &refused)
}

// ---------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------

// listenEvent is the start of every line.
type listenEvent struct {
	Type string `json:"type"`
	// At is when listen received the event, local time, RFC 3339.
	At string `json:"at"`
}

func (l *agentListener) event(typ string) listenEvent {
	return listenEvent{Type: typ, At: l.now().Format(time.RFC3339)}
}

type listenHello struct {
	listenEvent
	TeamID string `json:"team_id"`
	UserID string `json:"user_id"`
}

type listenDisconnected struct {
	listenEvent
	Error string `json:"error"`
}

// listenMessage is a new or changed message: the agent message object
// with the event's own two facts. changed is a message_changed (an
// edit, an unfurl added, a preview removed); the object's own `edited`
// says whether a person edited it.
type listenMessage struct {
	listenEvent
	agentMessage
	Changed    bool `json:"changed"`
	MentionsMe bool `json:"mentions_me"`
}

// listenMessageRef is message_deleted and channel_marked.
type listenMessageRef struct {
	listenEvent
	ChannelID string `json:"channel_id"`
	TS        string `json:"ts"`
}

type listenReaction struct {
	listenEvent
	ChannelID string `json:"channel_id"`
	TS        string `json:"ts"`
	UserID    string `json:"user_id"`
	Emoji     string `json:"emoji"`
}

type listenThreadMarked struct {
	listenEvent
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts"`
	TS        string `json:"ts"`
}

func (l *agentListener) writeDisconnected(reason string) error {
	return l.env.writeJSON(listenDisconnected{l.event("disconnected"), listenRedact(reason)})
}

var (
	// listenQueryRe is a URL's query string, up to the space or quote
	// that ends the URL in an error's text. StartWebSocket's URL carries
	// the session token in its query, and gorilla/websocket quotes the
	// whole URL in the error for one it cannot parse.
	listenQueryRe = regexp.MustCompile(`\?[^\s"']+`)
	// listenTokenRe is a Slack token or d cookie wherever it appears.
	listenTokenRe = regexp.MustCompile(`xox[a-z]-[A-Za-z0-9-]+`)
)

// listenRedact strips URL query strings and Slack tokens from an error's
// text. Every error text listen writes goes through it: the
// `disconnected` lines (writeDisconnected) and the line on stderr
// (listenRedactedError).
func listenRedact(s string) string {
	s = listenQueryRe.ReplaceAllString(s, "?[redacted]")
	return listenTokenRe.ReplaceAllString(s, "[redacted]")
}

// listenRedactedError is listen's error for stderr: its text redacted,
// the error itself kept for the exit code (agentExitCode).
type listenRedactedError struct{ err error }

func (e listenRedactedError) Error() string { return listenRedact(e.err.Error()) }
func (e listenRedactedError) Unwrap() error { return e.err }

// ---------------------------------------------------------------------
// One connection's events
// ---------------------------------------------------------------------

// listenSession is the EventHandler of one connection attempt. Its
// methods run on that connection's event owner, one at a time.
type listenSession struct {
	l *agentListener
	// hello is set by the connection's first hello. The loop reads it
	// once WsDone has closed.
	hello atomic.Bool
}

var _ slackclient.EventHandler = (*listenSession)(nil)

// emit writes one line. A write error goes to the loop, which stops.
func (s *listenSession) emit(v any) {
	if err := s.l.env.writeJSON(v); err != nil {
		select {
		case s.l.failed <- err:
		default:
		}
	}
}

func (s *listenSession) OnConnect() {
	if s.hello.CompareAndSwap(false, true) {
		s.emit(listenHello{s.l.event("hello"), s.l.teamID, s.l.userID})
	}
}

func (s *listenSession) OnMessage(channelID, userID, ts, text, threadTS, subtype string, changed, edited bool, files []slack.File, blocks slack.Blocks, attachments []slack.Attachment, botID, username string) {
	m := slack.Message{Msg: slack.Msg{
		Channel:         channelID,
		User:            userID,
		BotID:           botID,
		Username:        username,
		Timestamp:       ts,
		ThreadTimestamp: threadTS,
		SubType:         subtype,
		Text:            text,
		Files:           files,
		Blocks:          blocks,
		Attachments:     attachments,
	}}
	if edited {
		m.Edited = &slack.Edited{}
	}
	s.emit(listenMessage{
		listenEvent:  s.l.event("message"),
		agentMessage: encodeAgentMessage(m, channelID, agentMessageDefaults()),
		Changed:      changed,
		MentionsMe:   mention.InText(text, s.l.userID),
	})
}

func (s *listenSession) OnMessageDeleted(channelID, ts string) {
	s.emit(listenMessageRef{s.l.event("message_deleted"), channelID, ts})
}

func (s *listenSession) OnReactionAdded(channelID, ts, userID, emoji string) {
	s.emit(listenReaction{s.l.event("reaction_added"), channelID, ts, userID, emoji})
}

func (s *listenSession) OnReactionRemoved(channelID, ts, userID, emoji string) {
	s.emit(listenReaction{s.l.event("reaction_removed"), channelID, ts, userID, emoji})
}

// OnChannelMarked covers channel_marked, im_marked, group_marked and
// mpim_marked alike.
func (s *listenSession) OnChannelMarked(channelID, ts string, _, _ int) {
	s.emit(listenMessageRef{s.l.event("channel_marked"), channelID, ts})
}

func (s *listenSession) OnThreadMarked(channelID, threadTS, lastRead string, _ slackclient.Subscribed) {
	s.emit(listenThreadMarked{s.l.event("thread_marked"), channelID, threadTS, lastRead})
}

// OnDisconnect does nothing: the loop writes `disconnected` once
// WsDone has closed, with the reason it knows.
func (s *listenSession) OnDisconnect() {}

// The rest of Slack's events are not part of listen's output.

func (s *listenSession) OnEphemeralMessage(slackclient.EphemeralMessage)             {}
func (s *listenSession) OnViewOpened(string)                                         {}
func (s *listenSession) OnPresenceChange(string, string)                             {}
func (s *listenSession) OnUserTyping(string, string)                                 {}
func (s *listenSession) OnAssistantStatus(slackclient.AssistantStatus)               {}
func (s *listenSession) OnSelfPresenceChange(string)                                 {}
func (s *listenSession) OnDNDChange(bool, int64)                                     {}
func (s *listenSession) OnBadgeCountsUpdated(int)                                    {}
func (s *listenSession) OnThreadSubscriptionChanged(string, string, string, bool)    {}
func (s *listenSession) OnConversationOpened(slack.Channel)                          {}
func (s *listenSession) OnChannelSectionUpserted(slackclient.ChannelSectionUpserted) {}
func (s *listenSession) OnChannelSectionDeleted(string)                              {}
func (s *listenSession) OnChannelSectionChannelsUpserted(string, []string)           {}
func (s *listenSession) OnChannelSectionChannelsRemoved(string, []string)            {}
func (s *listenSession) OnPrefChange(string, string)                                 {}
func (s *listenSession) OnMemberJoined(string, string)                               {}
func (s *listenSession) OnMemberLeft(string, string)                                 {}
func (s *listenSession) OnUserStatusChange(string, slackclient.UserStatus)           {}
func (s *listenSession) OnUserInvalidated(string)                                    {}
func (s *listenSession) OnDNDInvalidated(string)                                     {}
func (s *listenSession) OnUserDNDChange(string, bool, int64)                         {}
func (s *listenSession) OnEmojiChanged(string, string, []string, string)             {}
