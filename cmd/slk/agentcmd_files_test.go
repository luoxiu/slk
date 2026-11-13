package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gammons/slk/internal/slackhttp"
)

// ---------------------------------------------------------------------
// Upload: message send --attach
// ---------------------------------------------------------------------

// uploadFake is a fake Slack for uploads. files.getUploadURLExternal
// hands out F1, F2, ... with an upload URL on files.slack.com (which
// newTestClient routes here as well); the upload records the bytes it
// carried; files.completeUploadExternal answers for the files it was
// given; files.info describes a file by the name it was uploaded under.
type uploadFake struct {
	*fakeSlack
	failURLAt int    // the getUploadURLExternal call (1-based) to refuse; 0 for none
	infoBody  string // files.info's answer when set

	// completeFailAt is the completeUploadExternal call (1-based) that
	// answers completeStatus and completeBody instead; 0 for none. Set
	// them with failCompleteAt.
	completeFailAt int
	completeStatus int
	completeBody   string

	urlCalls      int
	completeCalls int
	names         map[string]string // file ID -> filename it was asked for
	uploaded      map[string]string // file ID -> the bytes uploaded
}

func newUploadFake(t *testing.T, failURLAt int, infoBody string) *uploadFake {
	t.Helper()
	u := &uploadFake{fakeSlack: &fakeSlack{}, failURLAt: failURLAt, infoBody: infoBody, names: map[string]string{}, uploaded: map[string]string{}}
	u.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recordedRequest{path: r.URL.Path, query: r.URL.Query(), header: r.Header.Clone()}
		var body []byte
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Errorf("upload to %s has no file part: %v", r.URL.Path, err)
			} else {
				body, _ = io.ReadAll(file)
			}
		} else {
			_ = r.ParseForm()
			rec.form = r.PostForm
		}
		u.mu.Lock()
		defer u.mu.Unlock()
		u.reqs = append(u.reqs, rec)
		w.Header().Set("Content-Type", "application/json")
		write := func(s string) { _, _ = w.Write([]byte(s)) }
		switch p := r.URL.Path; {
		case p == "/api/auth.test":
			write(agentAuthTestOK)
		case p == "/api/files.getUploadURLExternal":
			u.urlCalls++
			if u.urlCalls == u.failURLAt {
				write(`{"ok":false,"error":"internal_error"}`)
				return
			}
			id := fmt.Sprintf("F%d", u.urlCalls)
			u.names[id] = r.PostForm.Get("filename")
			write(fmt.Sprintf(`{"ok":true,"upload_url":"https://files.slack.com/upload/v1/%s","file_id":"%s"}`, id, id))
		case strings.HasPrefix(p, "/upload/v1/"):
			u.uploaded[strings.TrimPrefix(p, "/upload/v1/")] = string(body)
			write("OK")
		case p == "/api/files.completeUploadExternal":
			u.completeCalls++
			if u.completeCalls == u.completeFailAt {
				w.WriteHeader(u.completeStatus)
				write(u.completeBody)
				return
			}
			var files []struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(r.PostForm.Get("files")), &files); err != nil || len(files) != 1 {
				t.Errorf("completeUploadExternal files=%q", r.PostForm.Get("files"))
				write(`{"ok":false,"error":"invalid_arguments"}`)
				return
			}
			write(`{"ok":true,"files":[{"id":"` + files[0].ID + `","title":""}]}`)
		case p == "/api/files.info":
			if u.infoBody != "" {
				write(u.infoBody)
				return
			}
			id := r.PostForm.Get("file")
			name := u.names[id]
			write(fmt.Sprintf(`{"ok":true,"file":{"id":"%s","name":"%s","title":"T %s","permalink":"https://acme.slack.com/files/U1/%s/%s"}}`, id, name, name, id, name))
		default:
			t.Errorf("fake Slack got an unexpected request to %s", p)
			http.Error(w, `{"ok":false,"error":"unexpected_path"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

// failCompleteAt makes the n-th files.completeUploadExternal call
// answer HTTP status with body.
func (u *uploadFake) failCompleteAt(n, status int, body string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.completeFailAt, u.completeStatus, u.completeBody = n, status, body
}

// writeTestFile writes content to dir/name and returns the path.
func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runUpload(t *testing.T, srv *uploadFake, args ...string) (int, string, string) {
	t.Helper()
	var calls []openCall
	env, stdout, stderr := agentTestEnv(nil, fakeAgentOpener(t, srv.fakeSlack, &calls))
	code := runAgent(context.Background(), env, args)
	return code, stdout.String(), stderr.String()
}

// Three files go up one at a time, in order, each its own three steps;
// the text is the initial comment of the last one only; files.info
// supplies each permalink; nothing is posted with chat.postMessage.
func TestAgentSendAttachUploadsEachFileCommentOnLast(t *testing.T) {
	dir := t.TempDir()
	a := writeTestFile(t, dir, "a.txt", "alpha")
	b := writeTestFile(t, dir, "b.png", "bravo!")
	c := writeTestFile(t, dir, "c.pdf", "charlie")
	srv := newUploadFake(t, 0, "")

	code, out, errOut := runUpload(t, srv, "message", "send", "C123", "see these", "--attach", a, "--attach", b, "--attach", c)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	want := `{"ok":true,"channel_id":"C123","files":[` +
		`{"id":"F1","name":"a.txt","title":"T a.txt","permalink":"https://acme.slack.com/files/U1/F1/a.txt"},` +
		`{"id":"F2","name":"b.png","title":"T b.png","permalink":"https://acme.slack.com/files/U1/F2/b.png"},` +
		`{"id":"F3","name":"c.pdf","title":"T c.pdf","permalink":"https://acme.slack.com/files/U1/F3/c.pdf"}]}`
	if got := strings.TrimSuffix(out, "\n"); got != want {
		t.Errorf("stdout\n got  %s\n want %s", got, want)
	}

	urls := srv.requestsTo("/api/files.getUploadURLExternal")
	completes := srv.requestsTo("/api/files.completeUploadExternal")
	if len(urls) != 3 || len(completes) != 3 {
		t.Fatalf("getUploadURLExternal %d, completeUploadExternal %d; want 3, 3", len(urls), len(completes))
	}
	for i, w := range []struct{ name, length, content string }{{"a.txt", "5", "alpha"}, {"b.png", "6", "bravo!"}, {"c.pdf", "7", "charlie"}} {
		id := fmt.Sprintf("F%d", i+1)
		if f := urls[i].form; f.Get("filename") != w.name || f.Get("length") != w.length {
			t.Errorf("getUploadURLExternal #%d filename=%q length=%q, want %s %s", i+1, f.Get("filename"), f.Get("length"), w.name, w.length)
		}
		if got := srv.uploaded[id]; got != w.content {
			t.Errorf("%s uploaded %q, want %q", id, got, w.content)
		}
		f := completes[i].form
		if f.Get("channel_id") != "C123" || f.Get("thread_ts") != "" || !strings.Contains(f.Get("files"), `"`+id+`"`) {
			t.Errorf("completeUploadExternal #%d form %v", i+1, f)
		}
		wantComment := ""
		if i == 2 {
			wantComment = "see these"
		}
		if got := f.Get("initial_comment"); got != wantComment {
			t.Errorf("completeUploadExternal #%d initial_comment %q, want %q", i+1, got, wantComment)
		}
	}
	if n := len(srv.requestsTo("/api/files.info")); n != 3 {
		t.Errorf("files.info %d times, want 3", n)
	}
	if n := len(srv.requestsTo("/api/chat.postMessage")); n != 0 {
		t.Errorf("chat.postMessage %d times, want 0", n)
	}
}

// Flags may come before, between or after the arguments; the text may
// be left out; a link target or --thread-ts puts the files in a thread,
// which the output then names.
func TestAgentSendAttachArgumentsAndThreads(t *testing.T) {
	dir := t.TempDir()
	a := writeTestFile(t, dir, "a.txt", "alpha")
	b := writeTestFile(t, dir, "b.txt", "bravo")
	cases := []struct {
		name        string
		args        []string
		wantFiles   int
		wantThread  string
		wantComment string
	}{
		{"flag before the target, no text", []string{"--attach", a, "C123"}, 1, "", ""},
		{"flags around the arguments", []string{"--attach=" + a, "C123", "hi", "--attach", b}, 2, "", "hi"},
		{"text after --", []string{"C123", "--attach", a, "--", "-1 not this"}, 1, "", "-1 not this"},
		{"link replies in its thread", []string{writeThreadLink, "--attach", a, "in thread"}, 1, "1700000001.000200", "in thread"},
		{"--thread-ts on a channel", []string{"C123", "--attach", a, "--thread-ts", "1700000002.000900"}, 1, "1700000002.000900", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newUploadFake(t, 0, "")
			code, out, errOut := runUpload(t, srv, append([]string{"message", "send"}, tc.args...)...)
			if code != 0 {
				t.Fatalf("exit %d, stderr %s", code, errOut)
			}
			var res struct {
				OK        bool   `json:"ok"`
				ChannelID string `json:"channel_id"`
				ThreadTS  string `json:"thread_ts"`
				Files     []struct {
					ID string `json:"id"`
				} `json:"files"`
			}
			if err := json.Unmarshal([]byte(out), &res); err != nil {
				t.Fatalf("stdout %q: %v", out, err)
			}
			if !res.OK || res.ChannelID != "C123" || res.ThreadTS != tc.wantThread || len(res.Files) != tc.wantFiles {
				t.Errorf("result %+v, want channel C123, thread %q, %d files", res, tc.wantThread, tc.wantFiles)
			}
			if tc.wantThread == "" && strings.Contains(out, "thread_ts") {
				t.Errorf("stdout names a thread: %s", out)
			}
			completes := srv.requestsTo("/api/files.completeUploadExternal")
			if len(completes) != tc.wantFiles {
				t.Fatalf("completeUploadExternal %d times, want %d", len(completes), tc.wantFiles)
			}
			for i, c := range completes {
				if c.form.Get("thread_ts") != tc.wantThread {
					t.Errorf("complete #%d thread_ts %q, want %q", i+1, c.form.Get("thread_ts"), tc.wantThread)
				}
				wantComment := ""
				if i == len(completes)-1 {
					wantComment = tc.wantComment
				}
				if c.form.Get("initial_comment") != wantComment {
					t.Errorf("complete #%d initial_comment %q, want %q", i+1, c.form.Get("initial_comment"), wantComment)
				}
			}
		})
	}
}

// A path that is missing, a directory, empty, unreadable or over 1 GB
// stops the whole command before a client is opened, so not even the
// good files go up; stderr names each bad path.
func TestAgentSendAttachBadPathUploadsNothing(t *testing.T) {
	dir := t.TempDir()
	good := writeTestFile(t, dir, "good.txt", "fine")
	empty := writeTestFile(t, dir, "empty.txt", "")
	missing := filepath.Join(dir, "missing.txt")
	big := filepath.Join(dir, "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse: no gigabyte is written.
	if err := f.Truncate(agentMaxUploadBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	unreadable := writeTestFile(t, dir, "secret.txt", "x")
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		args  []string
		wants []string
	}{
		{"missing among good ones", []string{"C123", "hi", "--attach", good, "--attach", missing}, []string{missing + ": no such file or directory"}},
		{"a directory", []string{"C123", "--attach", dir}, []string{dir + ": is a directory"}},
		{"an empty file", []string{"C123", "--attach", empty}, []string{empty + ": is empty"}},
		{"over 1 GB", []string{"C123", "--attach", big}, []string{big + ": is 1000000001 bytes, over Slack's 1 GB limit"}},
		{"every bad one is named", []string{"C123", "--attach", missing, "--attach", good, "--attach", dir}, []string{missing + ": ", dir + ": is a directory"}},
	}
	if os.Geteuid() != 0 { // root reads a mode-0 file
		cases = append(cases, struct {
			name  string
			args  []string
			wants []string
		}{"unreadable", []string{"C123", "--attach", unreadable}, []string{unreadable + ": permission denied"}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, stdout, stderr := agentTestEnv(nil, noOpener(t))
			code := runAgent(context.Background(), env, append([]string{"message", "send"}, tc.args...))
			if code != 1 || stdout.Len() != 0 {
				t.Errorf("exit %d stdout %q, want 1 and nothing", code, stdout)
			}
			for _, w := range tc.wants {
				if !strings.Contains(stderr.String(), w) {
					t.Errorf("stderr %q lacks %q", stderr, w)
				}
			}
			if !strings.Contains(stderr.String(), "nothing was uploaded") {
				t.Errorf("stderr %q does not say nothing was uploaded", stderr)
			}
			if strings.Count(stderr.String(), "\n") != 1 {
				t.Errorf("stderr is not one line: %q", stderr)
			}
		})
	}
}

func TestAgentSendAttachUsageErrors(t *testing.T) {
	good := writeTestFile(t, t.TempDir(), "good.txt", "fine")
	for _, args := range [][]string{
		{"message", "send", "C123", "--attach"},                    // no path
		{"message", "send", "--attach", good},                      // no target
		{"message", "send", "C123", "a", "b", "--attach", good},    // one argument too many
		{"message", "send", "C123", "--attach", good, "--attach="}, // empty path
	} {
		env, stdout, stderr := agentTestEnv(nil, noOpener(t))
		code := runAgent(context.Background(), env, args)
		if code != 1 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Errorf("%q: exit %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
}

// Safe mode refuses an upload as it refuses a send: first, before the
// files are looked at or a client is opened.
func TestAgentSafeModeRefusesAttach(t *testing.T) {
	good := writeTestFile(t, t.TempDir(), "good.txt", "fine")
	for _, args := range [][]string{
		{"message", "send", "C123", "--attach", good},
		{"message", "send", "--attach", good, "C123", "with text"},
		{"message", "send", "C123", "--attach", "/no/such/file"},
	} {
		env, stdout, stderr := agentTestEnv(map[string]string{"SLK_SAFE_MODE": "1"}, noOpener(t))
		code := runAgent(context.Background(), env, args)
		if code != 1 || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "safe mode:") {
			t.Errorf("%q: exit %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
}

// A refused upload halfway through a batch fails the command with
// Slack's error code and names the files already posted, so the caller
// does not post them again.
func TestAgentSendAttachFailureNamesWhatWasUploaded(t *testing.T) {
	dir := t.TempDir()
	a := writeTestFile(t, dir, "a.txt", "alpha")
	b := writeTestFile(t, dir, "b.txt", "bravo")
	srv := newUploadFake(t, 2, "")
	code, out, errOut := runUpload(t, srv, "message", "send", "C123", "--attach", a, "--attach", b, "two files")
	wantFail(t, code, out, errOut, 1, "internal_error")
	if !strings.Contains(errOut, b+" (2/2)") || !strings.Contains(errOut, "already uploaded: a.txt (F1)") {
		t.Errorf("stderr %q does not name the failed file and the uploaded one", errOut)
	}
	if n := len(srv.requestsTo("/api/files.completeUploadExternal")); n != 1 {
		t.Errorf("completeUploadExternal %d times, want 1", n)
	}

	srv = newUploadFake(t, 1, "")
	code, out, errOut = runUpload(t, srv, "message", "send", "C123", "--attach", a)
	wantFail(t, code, out, errOut, 1, "nothing was uploaded")
}

// files.completeUploadExternal is the step that posts a file. When it
// fails without Slack refusing it (a 5xx, a dropped connection, an ok
// answer without the file), Slack may have posted the file all the
// same, so the error must not say nothing was uploaded: it says that
// file may already be in the conversation, to be checked before a
// retry posts it twice. Slack's own refusal (ok:false) posted nothing.
func TestAgentSendAttachCompleteFailureMayHavePosted(t *testing.T) {
	dir := t.TempDir()
	a := writeTestFile(t, dir, "a.txt", "alpha")
	b := writeTestFile(t, dir, "b.txt", "bravo")
	cases := []struct {
		name         string
		failAt       int
		status       int
		body         string
		files        []string
		maybe        bool
		wantInStderr []string
	}{
		{"5xx on the only file", 1, http.StatusBadGateway, "bad gateway", []string{a}, true,
			[]string{a + " (1/1)", "file 1 (a.txt) may already have been posted", "check the conversation before retrying"}},
		{"5xx on the second file", 2, http.StatusInternalServerError, "oops", []string{a, b}, true,
			[]string{b + " (2/2)", "file 2 (b.txt) may already have been posted", "already uploaded: a.txt (F1)"}},
		{"ok without the file", 1, http.StatusOK, `{"ok":true,"files":[]}`, []string{a}, true,
			[]string{"file 1 (a.txt) may already have been posted"}},
		{"Slack refuses", 1, http.StatusOK, `{"ok":false,"error":"invalid_channel"}`, []string{a}, false,
			[]string{"invalid_channel", "nothing was uploaded"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newUploadFake(t, 0, "")
			srv.failCompleteAt(tc.failAt, tc.status, tc.body)
			args := []string{"message", "send", "C123"}
			for _, f := range tc.files {
				args = append(args, "--attach", f)
			}
			code, out, errOut := runUpload(t, srv, args...)
			wantFail(t, code, out, errOut, 1, tc.wantInStderr[0])
			for _, w := range tc.wantInStderr[1:] {
				if !strings.Contains(errOut, w) {
					t.Errorf("stderr %q lacks %q", errOut, w)
				}
			}
			if tc.maybe && strings.Contains(errOut, "nothing was uploaded") {
				t.Errorf("stderr %q says nothing was uploaded", errOut)
			}
			if strings.Count(errOut, "\n") != 1 {
				t.Errorf("stderr is not one line: %q", errOut)
			}
		})
	}
}

// files.info failing after the upload loses only the permalink: the
// file is posted, so the command succeeds and says so on stderr.
func TestAgentSendAttachFileInfoFailureKeepsTheUpload(t *testing.T) {
	a := writeTestFile(t, t.TempDir(), "a.txt", "alpha")
	srv := newUploadFake(t, 0, `{"ok":false,"error":"file_not_found"}`)
	code, out, errOut := runUpload(t, srv, "message", "send", "C123", "--attach", a)
	wantOut(t, code, out, errOut, `{"ok":true,"channel_id":"C123","files":[{"id":"F1","name":"a.txt","title":"a.txt"}]}`)
	if !strings.Contains(errOut, "file_not_found") || !strings.Contains(errOut, "F1") {
		t.Errorf("stderr %q does not explain the missing permalink", errOut)
	}
}

// ---------------------------------------------------------------------
// Download: message get / list --download-files
// ---------------------------------------------------------------------

// slackFilesURL is where Slack's file URLs point, and the only place
// --download-files fetches from; newFileServer answers for it.
const slackFilesURL = "https://files.slack.com"

// routeSlackFiles points http.DefaultTransport, which the downloader's
// client uses, at srv for this test, whatever host a request names.
// srv's certificate is checked under a name it holds (httptest's
// certificate covers example.com), not skipped.
func routeSlackFiles(t *testing.T, srv *httptest.Server) {
	t.Helper()
	addr := srv.Listener.Addr().String()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	saved := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "example.com"},
	}
	t.Cleanup(func() { http.DefaultTransport = saved })
}

// fileServer is a TLS server standing in for files.slack.com (every
// download is routed to it, routeSlackFiles): it serves bodies by path
// and counts what was asked.
type fileServer struct {
	*httptest.Server
	mu   sync.Mutex
	hits map[string]int
}

// newFileServer serves files by path at slackFilesURL. A body "404"
// answers 404; a body starting with "<html" is sent as text/html, as
// Slack sends its login page to a request whose auth failed.
func newFileServer(t *testing.T, files map[string]string) *fileServer {
	t.Helper()
	fs := &fileServer{hits: map[string]int{}}
	fs.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.hits[r.URL.Path]++
		fs.mu.Unlock()
		body, ok := files[r.URL.Path]
		if !ok {
			t.Errorf("file server got an unexpected request to %s", r.URL.Path)
		}
		switch {
		case !ok || body == "404":
			http.NotFound(w, r)
		case strings.HasPrefix(body, "<html"):
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(body))
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(fs.Close)
	routeSlackFiles(t, fs.Server)
	return fs
}

func (fs *fileServer) hitCount(path string) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.hits[path]
}

// downloadCache points XDG_CACHE_HOME at a temp dir and returns the
// downloads directory under it.
func downloadCache(t *testing.T) string {
	t.Helper()
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	return filepath.Join(cache, "slk", "downloads")
}

// fileJSON is a Slack file object whose url_private_download is
// base+path; its url_private is another path, which the file server
// does not serve, so a download from it fails the test.
func fileJSON(id, name, base, path string, size int) string {
	return fmt.Sprintf(`{"id":"%s","name":"%s","title":"%s","mimetype":"application/octet-stream","size":%d,"url_private":"%s","url_private_download":"%s"}`,
		id, name, name, size, base+path+"-private", base+path)
}

type downloadedFile struct {
	ID            string `json:"id"`
	Size          *int   `json:"size"`
	Path          string `json:"path"`
	DownloadError string `json:"download_error"`
}

func decodeFiles(t *testing.T, out string) [][]downloadedFile {
	t.Helper()
	var res struct {
		Message  *struct{ Files []downloadedFile }  `json:"message"`
		Messages []struct{ Files []downloadedFile } `json:"messages"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("stdout %q: %v", out, err)
	}
	if res.Message != nil {
		return [][]downloadedFile{res.Message.Files}
	}
	var all [][]downloadedFile
	for _, m := range res.Messages {
		all = append(all, m.Files)
	}
	return all
}

// leftovers lists what is in the downloads directory besides the file
// ID directories wanted, such as a temporary directory a failed
// download left behind.
func leftovers(t *testing.T, root string, want ...string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	keep := map[string]bool{}
	for _, w := range want {
		keep[w] = true
	}
	var extra []string
	for _, e := range entries {
		if !keep[e.Name()] {
			extra = append(extra, e.Name())
		}
	}
	return extra
}

// One good file and two that fail (404, Slack's login page): the good
// one lands at downloads/<id>/<name> with an absolute path and its
// size; the others carry download_error; the command succeeds; a
// failed download leaves nothing behind for a later run to take as
// done. The flag may come before the target.
func TestMessageGetDownloadFiles(t *testing.T) {
	root := downloadCache(t)
	files := newFileServer(t, map[string]string{
		"/files-pri/T1-F1/a.png":      "PNGDATA",
		"/files-pri/T1-F2/gone.txt":   "404",
		"/files-pri/T1-F3/secret.pdf": "<html>sign in</html>",
	})
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.history": readFixed(
		`{"ok":true,"messages":[{"type":"message","user":"U1","text":"files","ts":"1700000001.000100","files":[` +
			fileJSON("F1", "a.png", slackFilesURL, "/files-pri/T1-F1/a.png", 7) + `,` +
			fileJSON("F2", "gone.txt", slackFilesURL, "/files-pri/T1-F2/gone.txt", 3) + `,` +
			fileJSON("F3", "secret.pdf", slackFilesURL, "/files-pri/T1-F3/secret.pdf", 9) + `]}]}`)})

	code, out, errOut := runRead(t, srv, "message", "get", "--download-files", "C123", "--ts", "1700000001.000100")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	got := decodeFiles(t, out)[0]
	if len(got) != 3 {
		t.Fatalf("files %+v", got)
	}
	wantPath := filepath.Join(root, "F1", "a.png")
	if got[0].Path != wantPath || got[0].DownloadError != "" || got[0].Size == nil || *got[0].Size != 7 {
		t.Errorf("F1 = %+v, want path %s, size 7", got[0], wantPath)
	}
	if !filepath.IsAbs(got[0].Path) {
		t.Errorf("path %q is not absolute", got[0].Path)
	}
	if b, err := os.ReadFile(wantPath); err != nil || string(b) != "PNGDATA" {
		t.Errorf("downloaded %q, %v", b, err)
	}
	if got[1].Path != "" || !strings.Contains(got[1].DownloadError, "HTTP 404") || got[1].Size == nil || *got[1].Size != 3 {
		t.Errorf("F2 = %+v, want a 404 download_error and size 3", got[1])
	}
	if got[2].Path != "" || !strings.Contains(got[2].DownloadError, "text/html") {
		t.Errorf("F3 = %+v, want the login page as a download_error", got[2])
	}
	if files.hitCount("/files-pri/T1-F1/a.png") != 1 {
		t.Errorf("url_private_download asked %d times, want 1", files.hitCount("/files-pri/T1-F1/a.png"))
	}
	if extra := leftovers(t, root, "F1"); len(extra) != 0 {
		t.Errorf("downloads directory also holds %v", extra)
	}
}

// Without --download-files the file object keeps its shape: no size,
// path or download_error, and nothing is fetched.
func TestMessageGetWithoutDownloadFilesKeepsShape(t *testing.T) {
	root := downloadCache(t)
	newFileServer(t, map[string]string{}) // any fetch fails the test
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.history": readFixed(
		`{"ok":true,"messages":[{"type":"message","user":"U1","text":"f","ts":"1700000001.000100","files":[` +
			fileJSON("F1", "a.png", slackFilesURL, "/files-pri/T1-F1/a.png", 7) + `]}]}`)})
	code, out, errOut := runRead(t, srv, "message", "get", "C123", "--ts", "1700000001.000100")
	wantOut(t, code, out, errOut, `{"message":{"ts":"1700000001.000100","channel_id":"C123","author":{"user_id":"U1"},"content":"f","files":[{"id":"F1","name":"a.png","title":"a.png","mimetype":"application/octet-stream"}]}}`)
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("downloads directory exists (%v); nothing should be downloaded", err)
	}
}

// A file whose ID directory already holds a file is not fetched again:
// neither on a second run nor when the cache was filled before. The
// same file in two messages is fetched once.
func TestMessageListDownloadFilesUsesTheCache(t *testing.T) {
	root := downloadCache(t)
	seeded := filepath.Join(root, "F9", "earlier.bin")
	if err := os.MkdirAll(filepath.Dir(seeded), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seeded, []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := newFileServer(t, map[string]string{"/files-pri/T1-F1/a.png": "PNGDATA"})
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.history": readFixed(
		`{"ok":true,"messages":[` +
			`{"type":"message","user":"U1","text":"two","ts":"1700000002.000100","files":[` +
			fileJSON("F1", "a.png", slackFilesURL, "/files-pri/T1-F1/a.png", 7) + `,` +
			fileJSON("F9", "b.bin", slackFilesURL, "/files-pri/T1-F9/b.bin", 6) + `]},` +
			`{"type":"message","user":"U1","text":"one","ts":"1700000001.000100","files":[` +
			fileJSON("F1", "a.png", slackFilesURL, "/files-pri/T1-F1/a.png", 7) + `]}]}`)})

	var outs []string
	for range 2 {
		code, out, errOut := runRead(t, srv, "message", "list", "C123", "--download-files")
		if code != 0 {
			t.Fatalf("exit %d, stderr %s", code, errOut)
		}
		outs = append(outs, out)
	}
	if outs[0] != outs[1] {
		t.Errorf("second run differs:\n%s\n%s", outs[0], outs[1])
	}
	msgs := decodeFiles(t, outs[1])
	if len(msgs) != 2 {
		t.Fatalf("messages %+v", msgs)
	}
	f1 := filepath.Join(root, "F1", "a.png")
	if msgs[0][0].Path != f1 || msgs[1][0].Path != f1 || msgs[1][1].Path != seeded {
		t.Errorf("paths %+v, want %s for F1 and %s for F9", msgs, f1, seeded)
	}
	if n := files.hitCount("/files-pri/T1-F1/a.png"); n != 1 {
		t.Errorf("F1 fetched %d times, want 1", n)
	}
	if n := files.hitCount("/files-pri/T1-F9/b.bin"); n != 0 {
		t.Errorf("F9 fetched %d times, want 0 (it was in the cache)", n)
	}
}

// A file name with ../ in it is sanitized into the file's own ID
// directory; an ID that is not a Slack file ID is not downloaded at all
// and creates nothing.
func TestDownloadFilesStaysInTheIDDirectory(t *testing.T) {
	root := downloadCache(t)
	files := newFileServer(t, map[string]string{"/files-pri/T1-F1/evil": "EVIL"})
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.history": readFixed(
		`{"ok":true,"messages":[{"type":"message","user":"U1","text":"x","ts":"1700000001.000100","files":[` +
			fileJSON("F1", "../../evil.txt", slackFilesURL, "/files-pri/T1-F1/evil", 4) + `,` +
			fileJSON("../F2", "x.txt", slackFilesURL, "/files-pri/T1-F2/x", 1) + `]}]}`)})
	code, out, errOut := runRead(t, srv, "message", "get", "C123", "--ts", "1700000001.000100", "--download-files")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	got := decodeFiles(t, out)[0]
	if dir := filepath.Dir(got[0].Path); dir != filepath.Join(root, "F1") {
		t.Errorf("F1 saved at %q, want inside %s", got[0].Path, filepath.Join(root, "F1"))
	}
	if b, err := os.ReadFile(got[0].Path); err != nil || string(b) != "EVIL" {
		t.Errorf("F1 content %q, %v", b, err)
	}
	if got[1].Path != "" || !strings.Contains(got[1].DownloadError, "not a Slack file ID") {
		t.Errorf("../F2 = %+v, want a download_error", got[1])
	}
	if files.hitCount("/files-pri/T1-F2/x") != 0 {
		t.Error("the file with a bad ID was fetched")
	}
	if extra := leftovers(t, root, "F1"); len(extra) != 0 {
		t.Errorf("downloads directory also holds %v", extra)
	}
	for _, p := range []string{filepath.Join(root, "..", "evil.txt"), filepath.Join(root, "evil.txt"), filepath.Join(root, "..", "F2")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s exists", p)
		}
	}
}

// The token and cookie of the workspace openAgentClient picked reach a
// files.slack.com download, as the TUI's downloader sends them.
func TestDownloadFilesAuthenticatesWithTheOpenedWorkspace(t *testing.T) {
	srv := newReadFake(t, map[string]func(url.Values) string{
		"/api/conversations.history": readFixed(`{"ok":true,"messages":[{"type":"message","user":"U1","text":"f","ts":"1700000001.000100","files":[` +
			fileJSON("F1", "a.png", "https://files.slack.com", "/files-pri/T1-F1/a.png", 4) + `]}]}`),
		"/files-pri/T1-F1/a.png": readFixed("DATA"),
	})
	sessionTestSetup(t, srv)
	root := filepath.Join(xdgCache(), "downloads")
	routeSlackFiles(t, srv.Server)

	code, out, errOut := runSessionCommand(t, "message", "get", "C123", "--ts", "1700000001.000100", "--download-files")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	if got := decodeFiles(t, out)[0][0]; got.Path != filepath.Join(root, "F1", "a.png") {
		t.Errorf("file %+v", got)
	}
	h := srv.requestTo(t, "/files-pri/T1-F1/a.png").header
	if h.Get("Authorization") != "Bearer "+sessionTestToken || h.Get("Cookie") != "d="+sessionTestCookie {
		t.Errorf("download sent Authorization %q, Cookie %q; want the opened workspace's", h.Get("Authorization"), h.Get("Cookie"))
	}
}

// Only https URLs on files.slack.com are downloaded. A file object's
// URL comes from the message, and an app can post a file whose URL is
// anywhere; fetching it would send a request wherever the message says
// and store the answer as that Slack file. Any other URL is a
// download_error, and nothing is fetched.
func TestDownloadFilesOnlyFromSlackFiles(t *testing.T) {
	downloadCache(t)
	files := newFileServer(t, map[string]string{"/files-pri/T1-F1/a.png": "PNGDATA"})
	others := map[string]string{
		"F2": "http://files.slack.com",
		"F3": "https://example.com",
		"F4": "https://files.slack.com.example.com",
		"F5": "https://files.slack.com:8443",
		"F6": "https://ann@files.slack.com",
	}
	list := fileJSON("F1", "a.png", slackFilesURL, "/files-pri/T1-F1/a.png", 7)
	for _, id := range []string{"F2", "F3", "F4", "F5", "F6"} {
		list += "," + fileJSON(id, id+".txt", others[id], "/files-pri/T1-"+id+"/x", 1)
	}
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.history": readFixed(
		`{"ok":true,"messages":[{"type":"message","user":"U1","text":"f","ts":"1700000001.000100","files":[` + list + `]}]}`)})
	code, out, errOut := runRead(t, srv, "message", "get", "C123", "--ts", "1700000001.000100", "--download-files")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	got := decodeFiles(t, out)[0]
	if len(got) != 6 || got[0].Path == "" {
		t.Fatalf("files %+v, want F1 downloaded and five more", got)
	}
	for _, f := range got[1:] {
		if f.Path != "" || !strings.Contains(f.DownloadError, "files.slack.com") {
			t.Errorf("%s (%s) = %+v, want a download_error naming files.slack.com", f.ID, others[f.ID], f)
		}
		if n := files.hitCount("/files-pri/T1-" + f.ID + "/x"); n != 0 {
			t.Errorf("%s was fetched %d times", f.ID, n)
		}
	}
}

// A file over 200 MB is not downloaded: the downloader reads a whole
// file into memory. Its download_error says to get it in Slack, and
// nothing is fetched. A file of exactly 200 MB is still downloaded.
func TestDownloadFilesRefusesFilesOver200MB(t *testing.T) {
	downloadCache(t)
	files := newFileServer(t, map[string]string{"/files-pri/T1-F1/a.bin": "A", "/files-pri/T1-F2/b.bin": "B"})
	srv := newReadFake(t, map[string]func(url.Values) string{"/api/conversations.history": readFixed(
		`{"ok":true,"messages":[{"type":"message","user":"U1","text":"f","ts":"1700000001.000100","files":[` +
			fileJSON("F1", "a.bin", slackFilesURL, "/files-pri/T1-F1/a.bin", 200_000_000) + `,` +
			fileJSON("F2", "b.bin", slackFilesURL, "/files-pri/T1-F2/b.bin", 200_000_001) + `]}]}`)})
	code, out, errOut := runRead(t, srv, "message", "get", "C123", "--ts", "1700000001.000100", "--download-files")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	got := decodeFiles(t, out)[0]
	if len(got) != 2 {
		t.Fatalf("files %+v", got)
	}
	if got[0].Path == "" || got[0].DownloadError != "" {
		t.Errorf("F1 (exactly 200 MB) = %+v, want it downloaded", got[0])
	}
	if got[1].Path != "" || !strings.Contains(got[1].DownloadError, "200 MB") || !strings.Contains(got[1].DownloadError, "in Slack") {
		t.Errorf("F2 = %+v, want a download_error about the 200 MB limit", got[1])
	}
	if n := files.hitCount("/files-pri/T1-F2/b.bin"); n != 0 {
		t.Errorf("F2 was fetched %d times", n)
	}
}

// The resolver --download-files builds attaches the workspace's auth to
// files.slack.com only, never to another host.
func TestAgentFileAuthOnlyForSlackFiles(t *testing.T) {
	auth := slackhttp.TeamAuth{TeamID: "T1", Token: "xoxc-dummy", DCookie: "dummy"}
	r := slackhttp.NewAuthResolver([]slackhttp.TeamAuth{auth})
	if got := r.AuthsForURL("https://files.slack.com/files-pri/T1-F1/a.png"); len(got) != 1 || got[0] != auth {
		t.Errorf("files.slack.com: %v", got)
	}
	if got := r.AuthsForURL("https://example.com/files.slack.com/files-pri/T1-F1/a.png"); len(got) != 0 {
		t.Errorf("another host gets auth: %v", got)
	}
}

// The comment is markdown like a send's text: it reaches Slack as mrkdwn,
// so **bold** and [label](url) are formatting, not literal markup.
func TestAgentSendAttachCommentIsMarkdown(t *testing.T) {
	a := writeTestFile(t, t.TempDir(), "a.txt", "alpha")
	srv := newUploadFake(t, 0, "")
	code, _, errOut := runUpload(t, srv, "message", "send", "C123", "**done** see [the log](https://example.com/log)", "--attach", a)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	completes := srv.requestsTo("/api/files.completeUploadExternal")
	if len(completes) != 1 {
		t.Fatalf("completeUploadExternal %d times, want 1", len(completes))
	}
	if got, want := completes[0].form.Get("initial_comment"), "*done* see <https://example.com/log|the log>"; got != want {
		t.Errorf("initial_comment %q, want %q", got, want)
	}
}
