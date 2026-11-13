package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	slackclient "github.com/gammons/slk/internal/slack"
	"github.com/gammons/slk/internal/slackhttp"
)

const (
	sessionTestToken  = "xoxc-dummy-token-value"
	sessionTestCookie = "dummy-d-cookie-value"
)

// sessionTestSetup points XDG dirs at temp dirs, stores one dummy token
// for team T1, and routes every client openAgentClient builds to srv.
func sessionTestSetup(t *testing.T, srv *fakeSlack) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	store := slackclient.NewTokenStore(filepath.Join(xdgData(), "tokens"))
	if err := store.Save(slackclient.Token{AccessToken: sessionTestToken, Cookie: sessionTestCookie, TeamID: "T1", TeamName: "Acme"}); err != nil {
		t.Fatal(err)
	}
	addr := srv.Listener.Addr().String()
	agentClientHook = func(c *slackclient.Client) {
		bt := c.HTTPClient().Transport.(*slackhttp.BrowserTransport)
		bt.Inner = &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
			//nolint:gosec // dialing a local httptest server with a self-signed cert
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	t.Cleanup(func() { agentClientHook = nil })
}

// runSessionCommand runs one agent command through the production
// openAgentClient.
func runSessionCommand(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	env := newAgentEnv()
	env.stdout, env.stderr = &out, &errb
	code = runAgent(context.Background(), env, args)
	return code, out.String(), errb.String()
}

func (f *fakeSlack) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.reqs {
		if r.path == path {
			n++
		}
	}
	return n
}

const sessionUserBody = `{"ok":true,"user":{"id":"U07ZEDZED","name":"zed","real_name":"Zed Z","profile":{}}}`

func newSessionFake(t *testing.T) *fakeSlack {
	return newFakeSlack(t, map[string]string{
		"/api/auth.test":  agentAuthTestOK,
		"/api/users.info": sessionUserBody,
	})
}

func TestAgentSessionCacheSkipsAuthTest(t *testing.T) {
	srv := newSessionFake(t)
	sessionTestSetup(t, srv)

	code, out1, errs := runSessionCommand(t, "user", "get", "U07ZEDZED")
	if code != 0 {
		t.Fatalf("first run exit %d: %s", code, errs)
	}
	if srv.count("/api/auth.test") != 1 || srv.count("/api/users.info") != 1 {
		t.Fatalf("first run: auth.test %d, users.info %d; want 1, 1", srv.count("/api/auth.test"), srv.count("/api/users.info"))
	}
	code, out2, errs := runSessionCommand(t, "user", "get", "U07ZEDZED")
	if code != 0 {
		t.Fatalf("second run exit %d: %s", code, errs)
	}
	if srv.count("/api/auth.test") != 1 || srv.count("/api/users.info") != 2 {
		t.Errorf("second run: auth.test %d, users.info %d; want 1, 2", srv.count("/api/auth.test"), srv.count("/api/users.info"))
	}
	if out1 != out2 {
		t.Errorf("outputs differ:\n%s\n%s", out1, out2)
	}
}

func TestAgentSessionCacheInvalidatedByTokenFileChange(t *testing.T) {
	srv := newSessionFake(t)
	sessionTestSetup(t, srv)
	runSessionCommand(t, "user", "get", "U07ZEDZED")

	tokenPath := agentTokenPath("T1")
	// mtime change
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(tokenPath, later, later); err != nil {
		t.Fatal(err)
	}
	runSessionCommand(t, "user", "get", "U07ZEDZED")
	if n := srv.count("/api/auth.test"); n != 2 {
		t.Fatalf("after mtime change: auth.test %d, want 2", n)
	}
	// the run above rewrote the cache for the new mtime
	runSessionCommand(t, "user", "get", "U07ZEDZED")
	if n := srv.count("/api/auth.test"); n != 2 {
		t.Fatalf("cache not refreshed: auth.test %d, want 2", n)
	}
	// size change
	data, _ := os.ReadFile(tokenPath)
	if err := os.WriteFile(tokenPath, append(data, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tokenPath, later, later); err != nil { // same mtime, only the size differs
		t.Fatal(err)
	}
	runSessionCommand(t, "user", "get", "U07ZEDZED")
	if n := srv.count("/api/auth.test"); n != 3 {
		t.Fatalf("after size change: auth.test %d, want 3", n)
	}
}

func TestAgentSessionCorruptCacheFallsBack(t *testing.T) {
	srv := newSessionFake(t)
	sessionTestSetup(t, srv)
	runSessionCommand(t, "user", "get", "U07ZEDZED")
	if err := os.WriteFile(agentSessionPath("T1"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runSessionCommand(t, "user", "get", "U07ZEDZED"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if n := srv.count("/api/auth.test"); n != 2 {
		t.Errorf("auth.test %d, want 2", n)
	}
	if _, ok := loadAgentSession("T1"); !ok {
		t.Error("cache was not rewritten after the fallback")
	}
}

func TestAgentSessionAuthErrorDropsCache(t *testing.T) {
	srv := newFakeSlack(t, map[string]string{
		"/api/auth.test":  agentAuthTestOK,
		"/api/users.info": `{"ok":false,"error":"token_revoked"}`,
	})
	sessionTestSetup(t, srv)
	// Seed the cache with a first run that fails after connecting: the
	// failure is on a connected (not restored) client, so the cache stays.
	if code, _, _ := runSessionCommand(t, "user", "get", "U07ZEDZED"); code != 2 {
		t.Fatalf("first run exit %d, want 2", code)
	}
	if _, err := os.Stat(agentSessionPath("T1")); err != nil {
		t.Fatalf("cache missing after a connected run: %v", err)
	}
	code, _, errs := runSessionCommand(t, "user", "get", "U07ZEDZED")
	if code != 2 || !strings.Contains(errs, "token_revoked") {
		t.Fatalf("cached run exit %d, stderr %q; want 2 with token_revoked", code, errs)
	}
	if srv.count("/api/auth.test") != 1 {
		t.Errorf("cached run called auth.test")
	}
	if _, err := os.Stat(agentSessionPath("T1")); !os.IsNotExist(err) {
		t.Errorf("cache file still there after an auth error on a restored client: %v", err)
	}
}

func TestAgentAuthTestAlwaysCallsAuthTest(t *testing.T) {
	srv := newSessionFake(t)
	sessionTestSetup(t, srv)
	for i := 1; i <= 2; i++ {
		if code, _, errs := runSessionCommand(t, "auth", "test"); code != 0 {
			t.Fatalf("run %d exit %d: %s", i, code, errs)
		}
		if n := srv.count("/api/auth.test"); n != i {
			t.Fatalf("after run %d: auth.test %d, want %d", i, n, i)
		}
	}
	// and it refreshed the cache for the next ordinary command
	runSessionCommand(t, "user", "get", "U07ZEDZED")
	if n := srv.count("/api/auth.test"); n != 2 {
		t.Errorf("user get after auth test: auth.test %d, want 2", n)
	}
}

func TestAgentSessionFreshAuthContext(t *testing.T) {
	// listen opens its client through withFreshAuth; it must Connect
	// even when a valid cache exists.
	srv := newSessionFake(t)
	sessionTestSetup(t, srv)
	runSessionCommand(t, "user", "get", "U07ZEDZED")
	env := newAgentEnv()
	if _, err := env.openClient(withFreshAuth(context.Background()), "", ""); err != nil {
		t.Fatal(err)
	}
	if n := srv.count("/api/auth.test"); n != 2 {
		t.Errorf("auth.test %d, want 2", n)
	}
	if env.restoredSession != "" {
		t.Error("fresh open recorded a restore")
	}
}

func TestAgentSessionCacheHoldsNoSecrets(t *testing.T) {
	srv := newSessionFake(t)
	sessionTestSetup(t, srv)
	runSessionCommand(t, "user", "get", "U07ZEDZED")
	data, err := os.ReadFile(agentSessionPath("T1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{sessionTestToken, sessionTestCookie, "xox", "cookie", "token\""} {
		if strings.Contains(string(data), s) {
			t.Errorf("cache contains %q", s)
		}
	}
	fi, _ := os.Stat(agentSessionPath("T1"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("cache mode %v, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(agentSessionPath("T1")))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("cache dir mode %v, want 0700", di.Mode().Perm())
	}
}
