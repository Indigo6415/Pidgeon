package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

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
	cfg := Config{Dir: dir, Store: st, Assets: fstest.MapFS{"index.html": {Data: []byte("<title>{{NAME}}</title>")}}}
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
