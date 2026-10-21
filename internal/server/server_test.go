package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Indigo6415/pidgeon/internal/devices"
	"github.com/Indigo6415/pidgeon/internal/stats"
)

// newTestServer builds a server over a small tree:
//
//	a.txt, .env, docs/readme.md, docs/empty/, outside link -> parent dir
func newTestServer(t *testing.T, mut func(*Config)) (*Server, *stats.Store) {
	t.Helper()
	dir := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world"), 0o644))
	must(os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1"), 0o644))
	must(os.MkdirAll(filepath.Join(dir, "docs", "empty"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "docs", "readme.md"), []byte("# hi"), 0o644))
	// A symlink that escapes the root must never be served.
	_ = os.Symlink(filepath.Dir(dir), filepath.Join(dir, "outside"))

	st := stats.New()
	cfg := Config{Dir: dir, Store: st, ThumbDir: t.TempDir(), Assets: fstest.MapFS{"index.html": {Data: []byte("<title>{{NAME}}</title>")}}}
	if mut != nil {
		mut(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); s.root.Close() })
	return s, st
}

// do performs a request against the handler and returns the recorder.
func do(s http.Handler, method, target string, body io.Reader, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, body)
	// Headers come as name/value pairs.
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestListHidesDotfilesAndEscapingLinks(t *testing.T) {
	s, _ := newTestServer(t, nil)
	w := do(s, "GET", "/api/ls?path=/", nil)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var l listing
	json.Unmarshal(w.Body.Bytes(), &l)
	var names []string
	for _, e := range l.Entries {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "a.txt,docs" {
		t.Fatalf("entries = %s, want a.txt,docs", got)
	}
}

func TestPeekSummarisesFolders(t *testing.T) {
	s, _ := newTestServer(t, nil)
	w := do(s, "GET", "/api/peek?base=/&p=docs&p=.env&p=outside&p=a.txt&p=missing", nil)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var got map[string]peekInfo
	json.Unmarshal(w.Body.Bytes(), &got)
	docs, ok := got["docs"]
	if !ok || docs.Items != 2 || strings.Join(docs.Dirs, ",") != "empty" {
		t.Fatalf("docs = %+v, want 2 items with subfolder empty", docs)
	}
	// Hidden, escaping, non-folder and missing paths are all left out.
	if len(got) != 1 {
		t.Fatalf("peek returned %v, want only docs", got)
	}
	// An empty folder reports zero items and an empty (not null) subfolder list.
	w = do(s, "GET", "/api/peek?base=/docs&p=empty", nil)
	if body := strings.TrimSpace(w.Body.String()); body != `{"empty":{"items":0,"dirs":[]}}` {
		t.Fatalf("empty folder: %s", body)
	}
}

func TestFileRangeAndOutcome(t *testing.T) {
	s, st := newTestServer(t, nil)
	w := do(s, "GET", "/f/a.txt", nil, "Range", "bytes=6-")
	if w.Code != http.StatusPartialContent || w.Body.String() != "world" {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	// A range that reaches the end of the file counts as a download.
	if n := st.Snapshot().Downloads; n != 1 {
		t.Fatalf("downloads = %d, want 1", n)
	}
	// A mid-file range (video seeking) does not.
	do(s, "GET", "/f/a.txt", nil, "Range", "bytes=0-3")
	if n := st.Snapshot().Downloads; n != 1 {
		t.Fatalf("downloads after partial = %d, want 1", n)
	}
}

func TestPathSafety(t *testing.T) {
	s, _ := newTestServer(t, nil)
	// Each of these must be refused (never 200).
	for _, p := range []string{"/f/.env", "/f/outside/", "/f/docs/../.env", "/f/%2e%2e/etc/passwd"} {
		if w := do(s, "GET", p, nil); w.Code == 200 {
			t.Errorf("%s served with 200", p)
		}
	}
	s2, _ := newTestServer(t, func(c *Config) { c.ShowHidden = true })
	if w := do(s2, "GET", "/f/.env", nil); w.Code != 200 {
		t.Errorf("--hidden: .env status %d", w.Code)
	}
}

func TestZipKeepsTree(t *testing.T) {
	s, st := newTestServer(t, nil)
	w := do(s, "POST", "/api/zip", strings.NewReader(`{"base":"/","paths":["docs","a.txt"]}`))
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var res map[string]string
	json.Unmarshal(w.Body.Bytes(), &res)
	w = do(s, "GET", res["url"], nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("stream: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	want := "a.txt,docs/,docs/empty/,docs/readme.md"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("zip entries = %s, want %s", got, want)
	}
	if st.Snapshot().Downloads != 1 {
		t.Fatal("zip not counted as a completed download")
	}
	// Selections may not reach outside the folder they were made in.
	if w := do(s, "POST", "/api/zip", strings.NewReader(`{"base":"/docs","paths":["../a.txt"]}`)); w.Code != 400 {
		t.Fatalf("escape selection: status %d, want 400", w.Code)
	}
}

func TestTokenAuth(t *testing.T) {
	s, _ := newTestServer(t, func(c *Config) { c.Token = "s3cret" })
	if w := do(s, "GET", "/api/ls", nil); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
	w := do(s, "GET", "/?token=s3cret", nil)
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Set-Cookie"), "s3cret") {
		t.Fatalf("token login: %d %q", w.Code, w.Header().Get("Set-Cookie"))
	}
	if w := do(s, "GET", "/api/ls", nil, "Cookie", "pidgeon_token=s3cret"); w.Code != 200 {
		t.Fatalf("cookie: %d", w.Code)
	}
	if w := do(s, "GET", "/api/ls", nil, "Authorization", "Bearer wrong"); w.Code != 401 {
		t.Fatalf("bad bearer: %d", w.Code)
	}
}

func TestRangeReachesEnd(t *testing.T) {
	cases := map[string]bool{"bytes 0-9/10": true, "bytes 5-9/10": true, "bytes 0-4/10": false, "": false}
	// Each Content-Range header against a 10-byte file.
	for cr, want := range cases {
		if got := rangeReachesEnd(cr, 10); got != want {
			t.Errorf("%q: got %v want %v", cr, got, want)
		}
	}
}

func TestSearchStreamsMatchesFromEveryFolder(t *testing.T) {
	s, _ := newTestServer(t, nil)
	w := do(s, "GET", "/api/search?q=read", nil)
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"path":"/docs/readme.md"`) || !strings.Contains(lines[1], `"done":true`) {
		t.Fatalf("search = %q", w.Body.String())
	}
	// Hidden files never match, and a subsequence match is marked fuzzy.
	if w := do(s, "GET", "/api/search?q=env", nil); strings.Contains(w.Body.String(), ".env") {
		t.Fatalf("hidden file found: %s", w.Body)
	}
	if w := do(s, "GET", "/api/search?q=rdm", nil); !strings.Contains(w.Body.String(), `"fuzzy":true`) {
		t.Fatalf("fuzzy match missing: %s", w.Body)
	}
}

func TestStoredZipHasExactSizeAndResumes(t *testing.T) {
	s, st := newTestServer(t, nil)
	w := do(s, "POST", "/api/zip", strings.NewReader(`{"base":"/","paths":["docs","a.txt"],"store":true}`))
	var res struct {
		URL  string `json:"url"`
		Size int64  `json:"size"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	full := do(s, "GET", res.URL, nil)
	if int64(full.Body.Len()) != res.Size || full.Header().Get("Content-Length") != fmt.Sprint(res.Size) {
		t.Fatalf("size: announced %d, header %s, sent %d", res.Size, full.Header().Get("Content-Length"), full.Body.Len())
	}
	if _, err := zip.NewReader(bytes.NewReader(full.Body.Bytes()), int64(full.Body.Len())); err != nil {
		t.Fatalf("stored archive unreadable: %v", err)
	}
	// Two halves fetched separately must add up to the same bytes.
	cut := res.Size / 2
	a := do(s, "GET", res.URL, nil, "Range", fmt.Sprintf("bytes=0-%d", cut-1))
	b := do(s, "GET", res.URL, nil, "Range", fmt.Sprintf("bytes=%d-", cut), "If-Range", full.Header().Get("ETag"))
	if a.Code != 206 || b.Code != 206 || !bytes.Equal(append(a.Body.Bytes(), b.Body.Bytes()...), full.Body.Bytes()) {
		t.Fatalf("ranges %d/%d don't reassemble the archive", a.Code, b.Code)
	}
	// A stale If-Range gets the whole archive instead.
	if c := do(s, "GET", res.URL, nil, "Range", "bytes=10-", "If-Range", `"other"`); c.Code != 200 {
		t.Fatalf("stale If-Range: %d", c.Code)
	}
	// The full download and the resumed tail each count as a download.
	if n := st.Snapshot().Downloads; n != 3 {
		t.Fatalf("downloads = %d, want 3", n)
	}
}

func TestUploadsLandSafelyInTheInbox(t *testing.T) {
	inbox := t.TempDir()
	s, st := newTestServer(t, func(c *Config) { c.Inbox = inbox })
	put := func(name, body string) *httptest.ResponseRecorder {
		return do(s, "PUT", "/api/upload?name="+url.QueryEscape(name), strings.NewReader(body))
	}
	if w := put("../../.evil/x:y.txt", "one"); w.Code != 200 {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	put("x_y.txt", "two")
	names, _ := filepath.Glob(filepath.Join(inbox, "*"))
	for i := range names {
		names[i] = filepath.Base(names[i])
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "x_y (1).txt,x_y.txt" {
		t.Fatalf("inbox = %s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(inbox, "x_y (1).txt")); string(b) != "two" {
		t.Fatalf("second upload overwrote or lost data: %q", b)
	}
	if snap := st.Snapshot(); snap.Uploads != 2 || snap.Downloads != 0 || snap.BytesRecv != 6 {
		t.Fatalf("stats: uploads %d downloads %d received %d", snap.Uploads, snap.Downloads, snap.BytesRecv)
	}
	// Without an inbox, uploads don't exist.
	s2, _ := newTestServer(t, nil)
	if w := do(s2, "PUT", "/api/upload?name=a", strings.NewReader("x")); w.Code != 404 {
		t.Fatalf("upload without inbox: %d", w.Code)
	}
}

func TestShareLinksAreScopedAndLimited(t *testing.T) {
	s, _ := newTestServer(t, func(c *Config) { c.Token = "s3cret" })
	file, err := s.CreateShare("/a.txt", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	// The link works without the token, exactly once (resumes are free).
	if w := do(s, "GET", "/s/"+file.ID, nil); w.Code != 200 || w.Body.String() != "hello world" {
		t.Fatalf("first use: %d %q", w.Code, w.Body)
	}
	if w := do(s, "GET", "/s/"+file.ID, nil); w.Code != http.StatusGone {
		t.Fatalf("second use: %d", w.Code)
	}
	if w := do(s, "GET", "/s/"+file.ID, nil, "Range", "bytes=6-"); w.Code != 206 {
		t.Fatalf("resume after limit: %d", w.Code)
	}
	// A folder link reaches inside the folder and nowhere else.
	dir, _ := s.CreateShare("/docs", 0, 0)
	if w := do(s, "GET", "/s/"+dir.ID+"/readme.md", nil); w.Code != 200 {
		t.Fatalf("file in shared folder: %d", w.Code)
	}
	for _, p := range []string{"/s/" + dir.ID + "/%2e%2e/a.txt", "/s/" + dir.ID + "/../.env", "/s/" + dir.ID + "/../a.txt"} {
		if w := do(s, "GET", p, nil); w.Code == 200 {
			t.Errorf("%s escaped the shared folder", p)
		}
	}
	// Revoked links stop at once; the rest of the server still needs the token.
	s.RevokeShare(dir.ID)
	if w := do(s, "GET", "/s/"+dir.ID+"/readme.md", nil); w.Code != http.StatusGone {
		t.Fatalf("revoked link: %d", w.Code)
	}
	if w := do(s, "GET", "/f/a.txt", nil); w.Code != 401 {
		t.Fatalf("share link opened the whole server: %d", w.Code)
	}
}

func TestAskGateHoldsNewDevices(t *testing.T) {
	reg := devices.New(true, "")
	s, _ := newTestServer(t, func(c *Config) { c.Devices = reg })
	// httptest requests come from 192.0.2.1: a stranger, so it waits.
	done := make(chan int)
	go func() { done <- do(s, "GET", "/api/ls", nil).Code }()
	var p []devices.Prompt
	// Wait for the prompt to appear, then allow the device.
	for range 200 {
		if p = reg.Prompts(); len(p) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(p) != 1 || p[0].IP != "192.0.2.1" {
		t.Fatalf("prompts = %+v", p)
	}
	reg.Decide(p[0].ID, true, false)
	if code := <-done; code != 200 {
		t.Fatalf("allowed device got %d", code)
	}
	reg.Block("192.0.2.1")
	if w := do(s, "GET", "/api/ls", nil); w.Code != 403 {
		t.Fatalf("blocked device got %d", w.Code)
	}
}

func TestSendReachesOnlyThatDevice(t *testing.T) {
	s, _ := newTestServer(t, nil)
	if _, err := s.Send("192.0.2.1", "/a.txt"); err == nil {
		t.Fatal("send with no page open should fail")
	}
	mine := s.subscribe("tab-aaaaaaaa", "192.0.2.1")
	other := s.subscribe("tab-bbbbbbbb", "192.0.2.9")
	if n, err := s.Send("192.0.2.1", "/a.txt"); n != 1 || err != nil {
		t.Fatalf("send: %d %v", n, err)
	}
	if ev := <-mine.ch; ev.name != "send" || !strings.Contains(string(ev.data), `"path":"/a.txt"`) {
		t.Fatalf("event = %s %s", ev.name, ev.data)
	}
	if len(other.ch) != 0 {
		t.Fatal("another device got the file")
	}
	if _, err := s.Send("192.0.2.1", "/docs"); err == nil {
		t.Fatal("folders can't be sent")
	}
}

func TestParseRange(t *testing.T) {
	type r struct {
		s, e int64
		k    int
	}
	cases := map[string]r{
		"bytes=0-9": {0, 9, rangePartial}, "bytes=5-": {5, 99, rangePartial}, "bytes=-10": {90, 99, rangePartial},
		"bytes=0-500": {0, 99, rangePartial}, "bytes=100-": {0, 0, rangeUnsatisfiable}, "bytes=1-2,4-5": {0, 0, rangeNone},
		"items=0-1": {0, 0, rangeNone},
	}
	// Each header against a 100-byte body.
	for h, want := range cases {
		if s, e, k := parseRange(h, 100); (r{s, e, k}) != want {
			t.Errorf("%s: got %d-%d kind %d, want %+v", h, s, e, k, want)
		}
	}
}
