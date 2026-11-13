package slackclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// postMultipart sends the token and the fields, in order, as
// multipart/form-data, through the client's own HTTP client as postForm
// does (the test server is reached only through it).
func TestPostMultipartBody(t *testing.T) {
	var (
		ct     string
		names  []string
		values = map[string]string{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct = r.Header.Get("Content-Type")
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("multipart reader: %v", err)
			return
		}
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			buf := new(strings.Builder)
			_, _ = io.Copy(buf, p)
			names = append(names, p.FormName())
			values[p.FormName()] = buf.String()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := NewClient("xoxc-test", "d-cookie")
	pointClientAtTestServer(t, c, srv)

	if err := c.MarkSaved(context.Background(), "C1", "1700000001.000200", "completed"); err != nil {
		t.Fatalf("MarkSaved: %v", err)
	}
	if !strings.HasPrefix(ct, "multipart/form-data; boundary=") {
		t.Errorf("Content-Type %q", ct)
	}
	if got := strings.Join(names, ","); got != "token,item_id,item_type,ts,mark" {
		t.Errorf("field order %s", got)
	}
	want := map[string]string{"token": "xoxc-test", "item_id": "C1", "item_type": "message", "ts": "1700000001.000200", "mark": "completed"}
	for k, v := range want {
		if values[k] != v {
			t.Errorf("%s = %q, want %q", k, values[k], v)
		}
	}
}

func TestPostMultipartErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header map[string]string
		body   string
		check  func(t *testing.T, err error)
	}{
		{"Slack refusal keeps its code", 200, nil, `{"ok":false,"error":"not_found"}`, func(t *testing.T, err error) {
			var se slack.SlackErrorResponse
			if !errors.As(err, &se) || se.Err != "not_found" || !strings.Contains(err.Error(), "saved.update: not_found") {
				t.Errorf("err = %v", err)
			}
		}},
		{"429 is a RateLimitedError", 429, map[string]string{"Retry-After": "7"}, `rate limited`, func(t *testing.T, err error) {
			var rl *slack.RateLimitedError
			if !errors.As(err, &rl) || rl.RetryAfter != 7*time.Second {
				t.Errorf("err = %v, want a *slack.RateLimitedError of 7s", err)
			}
		}},
		{"HTTP failure names the status", 502, nil, `<html>`, func(t *testing.T, err error) {
			if err == nil || !strings.Contains(err.Error(), "502") {
				t.Errorf("err = %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := NewClient("xoxc-test", "d-cookie")
			pointClientAtTestServer(t, c, srv)
			tc.check(t, c.SetSavedDue(context.Background(), "C1", "1700000001.000200", 1800000000))
		})
	}
}

// agentPost: a 429 from the form-encoded endpoints comes back as a
// *slack.RateLimitedError too, so the commands can wait it out.
func TestAgentPostRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := NewClient("xoxc-test", "d-cookie")
	pointClientAtTestServer(t, c, srv)
	_, _, _, err := c.ListSavedPage(context.Background(), "", 50, "")
	var rl *slack.RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter != 3*time.Second {
		t.Errorf("err = %v, want a *slack.RateLimitedError of 3s", err)
	}
}

func TestPadDraftTS(t *testing.T) {
	for in, want := range map[string]string{
		"1700000000.123":       "1700000000.1230000",
		"1700000000.1234560":   "1700000000.1234560",
		"1700000000.000001":    "1700000000.0000010",
		"1700000000.123456789": "1700000000.123456789",
		"1700000000":           "1700000000",
		"":                     "",
	} {
		if got := padDraftTS(in); got != want {
			t.Errorf("padDraftTS(%q) = %q, want %q", in, got, want)
		}
	}
}
