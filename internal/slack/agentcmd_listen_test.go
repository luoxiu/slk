package slackclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/slack-go/slack"
)

// TestCheckAuth: one auth.test with the session's token in the form;
// Slack's refusal comes back as its code, an HTTP failure as an error
// that is not one, and the client's identity is left alone.
func TestCheckAuth(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantCode string // "" for no SlackErrorResponse
		wantErr  bool
	}{
		{"ok", http.StatusOK, `{"ok":true,"url":"https://acme.slack.com/","team_id":"T9","user_id":"U9"}`, "", false},
		{"dead token", http.StatusOK, `{"ok":false,"error":"invalid_auth"}`, "invalid_auth", true},
		{"revoked", http.StatusOK, `{"ok":false,"error":"token_revoked"}`, "token_revoked", true},
		{"edge failure", http.StatusBadGateway, `<html>bad gateway</html>`, "", true},
		{"not JSON", http.StatusOK, `<html>captive portal</html>`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var gotForm url.Values
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				body, _ := io.ReadAll(r.Body)
				gotForm, _ = url.ParseQuery(string(body))
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := newTestClient(srv)
			c.teamID, c.userID = "T1", "U1"

			err := c.CheckAuth(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckAuth: %v, want error %v", err, tc.wantErr)
			}
			var refused slack.SlackErrorResponse
			gotCode := ""
			if errors.As(err, &refused) {
				gotCode = refused.Err
			}
			if gotCode != tc.wantCode {
				t.Errorf("Slack code %q, want %q (err %v)", gotCode, tc.wantCode, err)
			}
			if gotPath != "/api/auth.test" || gotForm.Get("token") != "xoxc-test" {
				t.Errorf("request %s token=%q, want /api/auth.test with the session token", gotPath, gotForm.Get("token"))
			}
			if c.teamID != "T1" || c.userID != "U1" {
				t.Errorf("identity changed to %s/%s", c.teamID, c.userID)
			}
		})
	}
}

// TestCheckAuthHonoursContext: unlike Connect, a caller can give up on
// it.
func TestCheckAuthHonoursContext(t *testing.T) {
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hold
	}))
	defer srv.Close()
	defer close(hold)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := newTestClient(srv).CheckAuth(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("CheckAuth on a cancelled context: %v, want context.Canceled", err)
	}
}
