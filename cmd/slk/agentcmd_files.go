// This file holds the file side of the agent commands: uploading with
// `slk message send --attach` and downloading with `message get` /
// `message list --download-files`. The contract is the "上传" and
// "下载附件" sections of
// docs/superpowers/specs/2026-11-12-agent-cli-design.md.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/gammons/slk/internal/filedl"
	slackclient "github.com/gammons/slk/internal/slack"
	"github.com/gammons/slk/internal/slack/mrkdwn"
	"github.com/gammons/slk/internal/slackhttp"
	"github.com/slack-go/slack"
)

// ---------------------------------------------------------------------
// Upload
// ---------------------------------------------------------------------

// agentMaxUploadBytes is the largest file --attach takes: Slack's 1 GB,
// read as decimal so a file Slack might refuse is refused before any
// upload starts rather than halfway through a batch.
const agentMaxUploadBytes = 1_000_000_000

// agentPathList is a flag.Value collecting every --attach, in order.
type agentPathList []string

func (l *agentPathList) String() string { return strings.Join(*l, ",") }

func (l *agentPathList) Set(v string) error {
	if v == "" {
		return errors.New("empty path")
	}
	*l = append(*l, v)
	return nil
}

// agentAttachment is one --attach file, open from the check until the
// upload, so the file uploaded is the one that was checked.
type agentAttachment struct {
	path string
	name string // the name it is uploaded under: the path's last element
	file *os.File
	size int64
}

// openAgentAttachments opens every path for upload. Each must be a
// readable regular file of 1 to agentMaxUploadBytes bytes (slack-go
// refuses an empty one). If any is not, all are closed and the error
// names each one that failed, so nothing is uploaded.
func openAgentAttachments(paths []string) ([]agentAttachment, error) {
	var (
		out      []agentAttachment
		problems []string
	)
	for _, p := range paths {
		a, problem := openAgentAttachment(p)
		if problem != "" {
			problems = append(problems, fmt.Sprintf("%s: %s", p, problem))
			continue
		}
		out = append(out, a)
	}
	if len(problems) > 0 {
		closeAgentAttachments(out)
		return nil, fmt.Errorf("--attach: %s; nothing was uploaded", strings.Join(problems, "; "))
	}
	return out, nil
}

// openAgentAttachment opens one --attach path, or says what is wrong
// with it. The type is checked before opening, since opening a FIFO
// blocks; the size is read from the open file.
func openAgentAttachment(p string) (agentAttachment, string) {
	fi, err := os.Stat(p)
	if err != nil {
		return agentAttachment{}, pathErrText(err)
	}
	if problem := agentAttachmentProblem(fi); problem != "" {
		return agentAttachment{}, problem
	}
	f, err := os.Open(p)
	if err != nil {
		return agentAttachment{}, pathErrText(err)
	}
	var problem string
	if fi, err = f.Stat(); err != nil {
		problem = pathErrText(err)
	} else {
		problem = agentAttachmentProblem(fi)
	}
	if problem != "" {
		f.Close()
		return agentAttachment{}, problem
	}
	return agentAttachment{path: p, name: filepath.Base(p), file: f, size: fi.Size()}, ""
}

// agentAttachmentProblem says why a file cannot be uploaded, or "".
func agentAttachmentProblem(fi fs.FileInfo) string {
	switch {
	case fi.IsDir():
		return "is a directory"
	case !fi.Mode().IsRegular():
		return "is not a regular file"
	case fi.Size() == 0:
		return "is empty (Slack takes no empty file)"
	case fi.Size() > agentMaxUploadBytes:
		return fmt.Sprintf("is %d bytes, over Slack's 1 GB limit", fi.Size())
	}
	return ""
}

// pathErrText is an os error without the path it repeats ("no such
// file or directory" rather than "stat x: no such file or directory").
func pathErrText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

func closeAgentAttachments(files []agentAttachment) {
	for _, a := range files {
		a.file.Close()
	}
}

type agentUploadResult struct {
	OK        bool                `json:"ok"`
	ChannelID string              `json:"channel_id"`
	ThreadTS  string              `json:"thread_ts,omitempty"`
	Files     []agentUploadedFile `json:"files"`
}

type agentUploadedFile struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Title string `json:"title"`
	// Permalink is left out only when files.info failed after the
	// upload; stderr then says so.
	Permalink string `json:"permalink,omitempty"`
}

// uploadAgentFiles uploads files one at a time with slk's UploadFile
// (files.getUploadURLExternal, the upload, files.completeUploadExternal),
// into threadTS when it is set, with comment as the last file's initial
// comment. Then it asks files.info for each file's permalink. A failed
// upload stops the batch; its error names the files already uploaded,
// which stay posted, and, when the failed file itself may have been
// posted (agentUploadMayHavePosted), says so. A failed files.info only
// loses that permalink: the files are posted, and failing the command
// would invite a retry that posts them twice.
func uploadAgentFiles(ctx context.Context, env *agentEnv, client *slackclient.Client, channelID, threadTS, comment string, files []agentAttachment) error {
	// The comment is markdown, like a send's text. initial_comment takes
	// Slack mrkdwn, so it gets mrkdwn.Convert's mrkdwn half: **bold** and
	// [label](url) then show as formatting instead of literal markup.
	comment, _ = mrkdwn.Convert(strings.TrimSpace(comment))
	out := agentUploadResult{OK: true, ChannelID: channelID, ThreadTS: threadTS, Files: make([]agentUploadedFile, 0, len(files))}
	for i, a := range files {
		caption := ""
		if i == len(files)-1 {
			caption = comment
		}
		sum, err := client.UploadFile(ctx, channelID, threadTS, a.name, io.NewSectionReader(a.file, 0, a.size), a.size, caption)
		if err != nil {
			tail := agentUploadedSoFar(out.Files)
			if agentUploadMayHavePosted(err) {
				tail = fmt.Sprintf("; file %d (%s) may already have been posted: check the conversation before retrying%s", i+1, a.name, agentAlreadyUploaded(out.Files))
			}
			return fmt.Errorf("uploading %s (%d/%d): %w%s", a.path, i+1, len(files), err, tail)
		}
		title := sum.Title
		if title == "" {
			title = a.name
		}
		out.Files = append(out.Files, agentUploadedFile{ID: sum.ID, Name: a.name, Title: title})
	}
	for i := range out.Files {
		up := &out.Files[i]
		info, err := agentRetryRateLimit(ctx, func() (*slack.File, error) { return client.FileInfo(ctx, up.ID) })
		if err != nil {
			fmt.Fprintf(env.stderr, "slk: %s was uploaded as %s, but its permalink could not be read: %v\n", up.Name, up.ID, err)
			continue
		}
		if info.Name != "" {
			up.Name = info.Name
		}
		if info.Title != "" {
			up.Title = info.Title
		}
		up.Permalink = info.Permalink
	}
	return env.writeJSON(out)
}

// agentUploadedSoFar is the tail of a failed batch's error when the
// failed file was not posted: which files were, so the caller does not
// post them again.
func agentUploadedSoFar(done []agentUploadedFile) string {
	if len(done) == 0 {
		return "; nothing was uploaded"
	}
	return agentAlreadyUploaded(done)
}

// agentAlreadyUploaded names the files a batch posted before it failed,
// "" when there are none.
func agentAlreadyUploaded(done []agentUploadedFile) string {
	if len(done) == 0 {
		return ""
	}
	parts := make([]string, len(done))
	for i, f := range done {
		parts[i] = fmt.Sprintf("%s (%s)", f.Name, f.ID)
	}
	return "; already uploaded: " + strings.Join(parts, ", ")
}

// agentUploadMayHavePosted reports whether a failed UploadFile may have
// posted the file anyway. files.completeUploadExternal is the step that
// shares an uploaded file into the conversation; when it fails without
// Slack refusing it (a slack.SlackErrorResponse, which posts nothing),
// say a 5xx or a dropped connection, or answers ok without the file,
// Slack may well have acted on it. The steps before it post nothing.
// UploadFile reports the step only in slack-go's error text, by these
// two prefixes; TestAgentSendAttachCompleteFailureMayHavePosted fails
// if a slack-go upgrade changes them.
func agentUploadMayHavePosted(err error) bool {
	var refused slack.SlackErrorResponse
	if errors.As(err, &refused) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "CompleteUploadExternal: ") ||
		strings.Contains(msg, "file.upload.v2: something went wrong")
}

// ---------------------------------------------------------------------
// Download
// ---------------------------------------------------------------------

// downloadFiles registers --download-files (message get, message list),
// for agentMessageOptions.Downloads.
func (f *agentFlags) downloadFiles() *bool {
	return f.Bool("download-files", false, "download every file to $XDG_CACHE_HOME/slk/downloads/<file id>/ and add its size and path (or download_error)")
}

// agentDownloadWorkers bounds the concurrent downloads of
// --download-files.
const agentDownloadWorkers = 4

// agentFileIDRe is what a file ID must look like before it names a
// directory: Slack's IDs are capital letters and digits (F0123ABCD), so
// an ID cannot climb out of the downloads directory.
var agentFileIDRe = regexp.MustCompile(`^[A-Z0-9]{1,64}$`)

// agentMaxDownloadBytes is the largest file --download-files fetches,
// by the size Slack gives: filedl reads a whole file into memory before
// writing it. Decimal, like agentMaxUploadBytes.
const agentMaxDownloadBytes = 200_000_000

// agentSlackFilesHost is the one host --download-files fetches from, over
// https only: where Slack serves its files (url_private,
// url_private_download), and the only host slackhttp.AuthResolver sends
// the workspace's auth to. A file object's URL comes from the message,
// and an app can post a file whose URL is anywhere.
const agentSlackFilesHost = "files.slack.com"

// agentDownloadProblem says why the file at rawURL, of size bytes, is
// not to be fetched, or "".
func agentDownloadProblem(rawURL string, size int) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host != agentSlackFilesHost || u.User != nil {
		return fmt.Sprintf("download URL %q is not on https://%s; not downloaded", rawURL, agentSlackFilesHost)
	}
	if size > agentMaxDownloadBytes {
		return fmt.Sprintf("file is %d bytes, over the 200 MB download limit; download it in Slack", size)
	}
	return ""
}

// agentFileDownload is the outcome of downloading one file: its
// absolute path, or why there is none.
type agentFileDownload struct {
	Path string
	Err  string
}

// agentDownloadsDir is where --download-files keeps files, one
// directory per file ID: $XDG_CACHE_HOME/slk/downloads (~/.cache/...).
func agentDownloadsDir() string {
	return filepath.Join(xdgCache(), "downloads")
}

// downloadAgentFiles downloads every file of msgs, each file ID once,
// a few at a time, with the opened workspace's token and cookie (the
// TUI's filedl.Downloader and slackhttp.AuthResolver, which attach them
// only to files.slack.com and treat Slack's login page as a failure).
// A file is saved as downloads/<file ID>/<its name, sanitized by
// filedl>; when that directory already holds a file, that file is used
// and nothing is fetched. A failure is recorded for that file and never
// fails the command.
func downloadAgentFiles(ctx context.Context, env *agentEnv, msgs []slack.Message) map[string]agentFileDownload {
	var files []slack.File
	seen := map[string]bool{}
	for _, m := range msgs {
		for _, f := range m.Files {
			if !seen[f.ID] {
				seen[f.ID] = true
				files = append(files, f)
			}
		}
	}
	out := make(map[string]agentFileDownload, len(files))
	if len(files) == 0 {
		return out
	}
	root, err := filepath.Abs(agentDownloadsDir())
	if err != nil {
		for _, f := range files {
			out[f.ID] = agentFileDownload{Err: fmt.Sprintf("downloads directory: %v", err)}
		}
		return out
	}
	resolver := slackhttp.NewAuthResolver([]slackhttp.TeamAuth{env.fileAuth})
	results := make([]agentFileDownload, len(files))
	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, agentDownloadWorkers)
	)
	for i, f := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = downloadAgentFile(ctx, resolver, root, f)
		}()
	}
	wg.Wait()
	for i, f := range files {
		out[f.ID] = results[i]
	}
	return out
}

// downloadAgentFile downloads one file into root/<file ID>/, or finds
// it there. A URL not on https://files.slack.com or a file over
// agentMaxDownloadBytes is refused before anything is fetched
// (agentDownloadProblem), whether or not a copy is cached. The download
// goes into a hidden temporary directory that is renamed to
// root/<file ID> once the file is whole, so the cache never holds a
// partial file: a failed or interrupted download leaves nothing that a
// later run would take as done.
func downloadAgentFile(ctx context.Context, resolver *slackhttp.AuthResolver, root string, f slack.File) agentFileDownload {
	if !agentFileIDRe.MatchString(f.ID) {
		return agentFileDownload{Err: fmt.Sprintf("file id %q is not a Slack file ID; not downloaded", f.ID)}
	}
	fileURL := f.URLPrivateDownload
	if fileURL == "" {
		fileURL = f.URLPrivate
	}
	if fileURL == "" {
		return agentFileDownload{Err: "Slack gave no download URL for this file"}
	}
	if problem := agentDownloadProblem(fileURL, f.Size); problem != "" {
		return agentFileDownload{Err: problem}
	}
	dir := filepath.Join(root, f.ID)
	if p, ok := cachedAgentDownload(dir); ok {
		return agentFileDownload{Path: p}
	}
	name := f.Name
	if name == "" {
		name = f.Title
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return agentFileDownload{Err: err.Error()}
	}
	tmp, err := os.MkdirTemp(root, "."+f.ID+"-")
	if err != nil {
		return agentFileDownload{Err: err.Error()}
	}
	saved, err := filedl.New(resolver, tmp).Download(ctx, fileURL, name)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return agentFileDownload{Err: err.Error()}
	}
	if err := os.Rename(tmp, dir); err != nil {
		_ = os.RemoveAll(tmp)
		// Another run may have finished the same file first.
		if p, ok := cachedAgentDownload(dir); ok {
			return agentFileDownload{Path: p}
		}
		return agentFileDownload{Err: err.Error()}
	}
	return agentFileDownload{Path: filepath.Join(dir, filepath.Base(saved))}
}

// cachedAgentDownload returns the regular file in dir, if there is one
// (the first by name).
func cachedAgentDownload(dir string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			return filepath.Join(dir, e.Name()), true
		}
	}
	return "", false
}
