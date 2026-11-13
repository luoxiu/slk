package main

import (
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

const userInfoAnn = `{"ok":true,"user":{"id":"U1","name":"ann.lee","real_name":"Ann Lee","profile":{"display_name":"Ann","email":"ann@example.com"}}}`

// user get takes an email or a handle with or without @ (users.list:
// email, or name then display name), and prints the same object as for
// an ID: the match is read back with users.info. users.lookupByEmail is
// never used: Slack refuses it for a browser session token.
func TestUserGetByEmailAndHandle(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/users.lookupByEmail": readFixed(`{"ok":false,"error":"not_allowed_token_type"}`),
		"/api/users.list": func(form url.Values) string {
			if form.Get("cursor") == "" {
				return `{"ok":true,"members":[{"id":"U2","name":"bob","profile":{"display_name":"Bobby"}}],"response_metadata":{"next_cursor":"p2"}}`
			}
			return `{"ok":true,"members":[{"id":"U1","name":"ann.lee","profile":{"display_name":"Ann","email":"Ann@Example.com"}}],"response_metadata":{"next_cursor":""}}`
		},
		"/api/users.info": func(form url.Values) string {
			switch form.Get("user") {
			case "U1":
				return userInfoAnn
			case "U2":
				return `{"ok":true,"user":{"id":"U2","name":"bob","profile":{"display_name":"Bobby"}}}`
			}
			return `{"ok":false,"error":"user_not_found"}`
		},
	})
	for _, arg := range []string{"ann@example.com", "@ann.lee", "ann.lee", "ANN.LEE", "Ann"} {
		code, out, errOut := runRead(t, srv, "user", "get", arg)
		if code != 0 || !strings.Contains(out, `"id":"U1"`) || !strings.Contains(out, `"email":"ann@example.com"`) {
			t.Errorf("user get %s: exit %d, out %s, err %s", arg, code, out, errOut)
		}
	}
	code, out, _ := runRead(t, srv, "user", "get", "@bobby")
	if code != 0 || !strings.Contains(out, `"id":"U2"`) {
		t.Errorf("display name match: exit %d, out %s", code, out)
	}
	if n := len(srv.requestsTo("/api/users.lookupByEmail")); n != 0 {
		t.Errorf("users.lookupByEmail called %d times; want 0", n)
	}
	for _, arg := range []string{"nobody@example.com", "@nobody"} {
		code, out, errOut := runRead(t, srv, "user", "get", arg)
		if code != 1 || out != "" || !strings.Contains(errOut, "user_not_found") {
			t.Errorf("user get %s: exit %d, out %q, err %q; want exit 1 with user_not_found", arg, code, out, errOut)
		}
	}
}

// A handle that two people share as a display name is ambiguous: say so
// instead of picking one.
func TestUserGetAmbiguousDisplayName(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/users.list": readFixed(`{"ok":true,"members":[{"id":"U1","name":"a1","profile":{"display_name":"Sam"}},{"id":"U2","name":"a2","profile":{"display_name":"sam"}}]}`),
	})
	code, _, errOut := runRead(t, srv, "user", "get", "sam")
	if code != 1 || !strings.Contains(errOut, "U1") || !strings.Contains(errOut, "U2") {
		t.Errorf("exit %d, err %q; want exit 1 naming both", code, errOut)
	}
}

// A user ID is U or W and at least eight more capitals or digits, as
// every real one is: an all-caps handle such as WANGLI is a handle, looked
// up in users.list, not sent to users.info as an ID.
func TestUserGetTellsIDsFromHandles(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/users.list": readFixed(`{"ok":true,"members":[{"id":"U0WANGLI01","name":"wangli","profile":{}}],"response_metadata":{"next_cursor":""}}`),
		"/api/users.info": func(form url.Values) string {
			id := form.Get("user")
			return `{"ok":true,"user":{"id":"` + id + `","name":"x","profile":{}}}`
		},
	})
	for _, tc := range []struct {
		arg, wantInfo string
		wantList      bool
	}{
		{"WANGLI", "U0WANGLI01", true},
		{"UNKNOWN", "", true}, // a handle nobody has: user_not_found from the list
		{"U0123ABCD", "U0123ABCD", false},
		{"W0123ABCD", "W0123ABCD", false},
	} {
		lists, infos := srv.count("/api/users.list"), srv.count("/api/users.info")
		code, out, errOut := runRead(t, srv, "user", "get", tc.arg)
		gotList := srv.count("/api/users.list") > lists
		if gotList != tc.wantList {
			t.Errorf("%s: users.list called %v, want %v", tc.arg, gotList, tc.wantList)
		}
		if tc.wantInfo == "" {
			wantFail(t, code, out, errOut, 1, "user_not_found")
			if srv.count("/api/users.info") != infos {
				t.Errorf("%s: users.info called", tc.arg)
			}
			continue
		}
		reqs := srv.requestsTo("/api/users.info")
		if code != 0 || len(reqs) != infos+1 || reqs[len(reqs)-1].form.Get("user") != tc.wantInfo {
			t.Errorf("%s: exit %d, stderr %s; want users.info for %s", tc.arg, code, errOut, tc.wantInfo)
		}
	}
}

// users.list is read page by page for a handle or an email; a rate limit
// on a page is waited out and that page asked again, as the other list
// commands do.
func TestUserGetWaitsOutUsersListRateLimit(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/users.list": onceLimited(`{"ok":true,"members":[{"id":"U0ANNLEE01","name":"ann.lee","profile":{}}],"response_metadata":{"next_cursor":""}}`),
		"/api/users.info": readFixed(`{"ok":true,"user":{"id":"U0ANNLEE01","name":"ann.lee","profile":{}}}`),
	})
	code, out, errOut := runRead(t, srv, "user", "get", "@ann.lee")
	if code != 0 || !strings.Contains(out, `"id":"U0ANNLEE01"`) {
		t.Fatalf("exit %d, out %s, stderr %s", code, out, errOut)
	}
	if n := srv.count("/api/users.list"); n != 2 {
		t.Errorf("%d users.list requests, want 2 (the page again after the 429)", n)
	}
}

// A users.list cursor Slack has already given would read the same pages
// forever: the lookup stops and fails, rather than report a user it did
// not finish looking for as not found.
func TestUserGetStopsOnRepeatedCursor(t *testing.T) {
	var calls atomic.Int32
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/users.list": func(form url.Values) string {
			if calls.Add(1) > 5 { // keeps a lookup without the guard from spinning forever
				return `{"ok":false,"error":"test_gave_up"}`
			}
			return `{"ok":true,"members":[{"id":"U0BOB00001","name":"bob","profile":{}}],"response_metadata":{"next_cursor":"same"}}`
		},
	})
	code, out, errOut := runRead(t, srv, "user", "get", "@ann")
	wantFail(t, code, out, errOut, 1, "cursor")
	if n := srv.count("/api/users.list"); n != 2 {
		t.Errorf("%d users.list requests, want 2 (stop when the cursor repeats)", n)
	}
}
