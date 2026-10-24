package pull

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Indigo6415/pidgeon/internal/server"
	"github.com/Indigo6415/pidgeon/internal/stats"
)

// serve starts a real server over a small tree:
//
//	Photos/a.jpg, Photos/trip/b.jpg, notes.txt
func serve(t *testing.T, token string) (string, string, *server.Server) {
	t.Helper()
	dir := t.TempDir()
	write := func(p, s string) {
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Photos/a.jpg", strings.Repeat("a", 300_000))
	write("Photos/trip/b.jpg", "bbbb")
	write("notes.txt", "hello")
	srv, err := server.New(server.Config{Dir: dir, Store: stats.New(), Token: token, NoThumbs: true,
		Assets: fstest.MapFS{"index.html": {Data: []byte("x")}}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { ts.Close(); srv.Close() })
	return ts.URL, dir, srv
}

// pull runs a pull and returns its stdout and error.
func pull(t *testing.T, raw, target string, o options) (string, error) {
	t.Helper()
	if o.jobs == 0 {
		o.jobs = 2
	}
	var out, errOut bytes.Buffer
	err := run(context.Background(), raw, target, o, &out, &errOut)
	return out.String(), err
}

// read returns a file's contents, or "" if it is missing.
func read(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func TestPullKeepsTreeAndSkipsWhatItHas(t *testing.T) {
	url, src, _ := serve(t, "sesame")
	dst := filepath.Join(t.TempDir(), "out")

	// Without the token the server says no.
	if _, err := pull(t, url+"/#/Photos", dst, options{}); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("want a token error, got %v", err)
	}
	// The token can come in the URL, as in the QR code link.
	out, err := pull(t, url+"/?token=sesame#/Photos", dst, options{})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if read(filepath.Join(dst, "a.jpg")) != strings.Repeat("a", 300_000) || read(filepath.Join(dst, "trip", "b.jpg")) != "bbbb" {
		t.Fatal("files missing or wrong")
	}
	// The modification time comes along.
	sfi, _ := os.Stat(filepath.Join(src, "Photos", "trip", "b.jpg"))
	dfi, _ := os.Stat(filepath.Join(dst, "trip", "b.jpg"))
	if !sameTime(dfi.ModTime(), sfi.ModTime().UnixMilli()) {
		t.Fatalf("mtime %v, want %v", dfi.ModTime(), sfi.ModTime())
	}

	// A second run fetches nothing.
	out, err = pull(t, url+"/#/Photos", dst, options{token: "sesame"})
	if err != nil || !strings.Contains(out, "0 downloaded") || !strings.Contains(out, "2 already there") {
		t.Fatalf("rerun: %v %q", err, out)
	}

	// A changed file is kept without --mirror and replaced with it.
	later := time.Now().Add(time.Hour)
	os.WriteFile(filepath.Join(src, "Photos", "trip", "b.jpg"), []byte("BBBBBB"), 0o644)
	os.Chtimes(filepath.Join(src, "Photos", "trip", "b.jpg"), later, later)
	out, _ = pull(t, url+"/#/Photos", dst, options{token: "sesame"})
	if !strings.Contains(out, "1 differ") || read(filepath.Join(dst, "trip", "b.jpg")) != "bbbb" {
		t.Fatalf("no mirror: %q", out)
	}
	out, err = pull(t, url+"/#/Photos", dst, options{token: "sesame", mirror: true, verify: true})
	if err != nil || read(filepath.Join(dst, "trip", "b.jpg")) != "BBBBBB" || !strings.Contains(out, "verified") {
		t.Fatalf("mirror: %v %q", err, out)
	}

	// --verify notices a local file whose bytes changed but whose size and
	// time didn't; with --mirror it is fetched again.
	p := filepath.Join(dst, "a.jpg")
	fi, _ := os.Stat(p)
	os.WriteFile(p, []byte(strings.Repeat("z", 300_000)), 0o644)
	os.Chtimes(p, fi.ModTime(), fi.ModTime())
	if _, err := pull(t, url+"/#/Photos", dst, options{token: "sesame", verify: true, mirror: true}); err != nil {
		t.Fatal(err)
	}
	if read(p) != strings.Repeat("a", 300_000) {
		t.Fatal("corrupt file not replaced")
	}
}

func TestPullResumesAPartFile(t *testing.T) {
	url, _, _ := serve(t, "")
	dst := t.TempDir()
	// Half a file left over from an interrupted run.
	os.WriteFile(filepath.Join(dst, "a.jpg.part"), []byte(strings.Repeat("a", 100_000)), 0o644)
	out, err := pull(t, url+"/f/Photos/a.jpg", dst, options{})
	if err != nil {
		t.Fatalf("%v %q", err, out)
	}
	if read(filepath.Join(dst, "a.jpg")) != strings.Repeat("a", 300_000) {
		t.Fatal("resumed file is wrong")
	}
	if _, err := os.Stat(filepath.Join(dst, "a.jpg.part")); err == nil {
		t.Fatal(".part left behind")
	}
}

func TestPullThroughShareLinks(t *testing.T) {
	url, _, srv := serve(t, "sesame")
	// A folder link needs no token, only its PIN.
	sh, err := srv.NewShare(server.ShareOptions{Path: "/Photos", PIN: "4321"})
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "shared")
	if _, err := pull(t, url+"/s/"+sh.ID, dst, options{}); err == nil || !strings.Contains(err.Error(), "PIN") {
		t.Fatalf("want a PIN error, got %v", err)
	}
	if out, err := pull(t, url+"/s/"+sh.ID, dst, options{pin: "4321"}); err != nil {
		t.Fatalf("%v %q", err, out)
	}
	if read(filepath.Join(dst, "trip", "b.jpg")) != "bbbb" {
		t.Fatal("share folder not pulled")
	}
	// A link to one file, saved into the target directory by its name.
	one, _ := srv.CreateShare("/notes.txt", 0, 0)
	dst2 := t.TempDir()
	if out, err := pull(t, url+"/s/"+one.ID, dst2, options{}); err != nil {
		t.Fatalf("%v %q", err, out)
	}
	if read(filepath.Join(dst2, "notes.txt")) != "hello" {
		t.Fatal("share file not pulled")
	}
}

func TestParseURL(t *testing.T) {
	for raw, want := range map[string]remote{
		"http://h:8080/":              {base: "http://h:8080", path: "/"},
		"http://h:8080/#/Photos/2024": {base: "http://h:8080", path: "/Photos/2024"},
		"https://h/?token=abc#/a":     {base: "https://h", path: "/a", token: "abc"},
		"http://h/f/docs/x.pdf":       {base: "http://h", path: "/docs/x.pdf"},
		"http://h/s/0123abcd/":        {base: "http://h", path: "/", share: "0123abcd"},
		"http://h/s/0123abcd/sub/dir": {base: "http://h", path: "/sub/dir", share: "0123abcd"},
		"http://u:p@h/#/../../etc":    {base: "http://h", path: "/etc", user: "u", pass: "p"},
	} {
		got, err := parseURL(raw)
		if err != nil || got != want {
			t.Errorf("%s: got %+v %v, want %+v", raw, got, err, want)
		}
	}
	if _, err := parseURL("ftp://h/"); err == nil {
		t.Error("ftp accepted")
	}
}
