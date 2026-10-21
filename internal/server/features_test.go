package server

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Indigo6415/pidgeon/internal/stats"
)

// writeZip creates a ZIP at p with the given files; names ending in "*"
// are stored rather than deflated.
func writeZip(t *testing.T, p string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	// Add the members in a stable order.
	for _, n := range names {
		method, name := zip.Deflate, n
		if strings.HasSuffix(n, "*") {
			method, name = zip.Store, strings.TrimSuffix(n, "*")
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: time.Unix(1700000000, 0)})
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, files[n])
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeTar creates a .tar (or .tar.gz when gz) at p with the given files.
func writeTar(t *testing.T, p string, gz bool, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	var w io.Writer = &buf
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(&buf)
		w = zw
	}
	tw := tar.NewWriter(w)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	// Headers and contents, in a stable order.
	for _, n := range names {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), ModTime: time.Unix(1700000000, 0), Typeflag: tar.TypeReg})
		io.WriteString(tw, files[n])
	}
	tw.Close()
	if zw != nil {
		zw.Close()
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// listNames returns the sorted entry names of /api/ls?path=p.
func listNames(t *testing.T, s http.Handler, p string) (listing, []string) {
	t.Helper()
	w := do(s, "GET", "/api/ls?path="+p, nil)
	if w.Code != 200 {
		t.Fatalf("ls %s: status %d %s", p, w.Code, w.Body)
	}
	var l listing
	json.Unmarshal(w.Body.Bytes(), &l)
	var names []string
	for _, e := range l.Entries {
		n := e.Name
		if e.Dir {
			n += "/"
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return l, names
}

func TestArchivesBrowseLikeFolders(t *testing.T) {
	big := strings.Repeat("0123456789", 10_000)
	s, st := newTestServer(t, func(c *Config) {
		writeZip(t, filepath.Join(c.Dir, "pack.zip"), map[string]string{
			"top.txt": "top", "photos/a.jpg*": "stored-bytes", "photos/log.txt": big, ".hidden": "x", "__MACOSX/._junk": "x",
		})
		writeTar(t, filepath.Join(c.Dir, "plain.tar"), false, map[string]string{"dir/one.txt": "first file", "two.txt": "second"})
		writeTar(t, filepath.Join(c.Dir, "packed.tar.gz"), true, map[string]string{"deep/er/three.txt": "third file"})
	})

	// The parent listing marks archives so the UI can open them.
	_, _ = listNames(t, s, "/")
	w := do(s, "GET", "/api/ls?path=/", nil)
	if !strings.Contains(w.Body.String(), `"name":"pack.zip","dir":false,"size"`) || !strings.Contains(w.Body.String(), `"archive":true`) {
		t.Fatalf("archives not flagged in the listing: %s", w.Body)
	}

	// Inside: folders are synthesised, hidden and __MACOSX entries left out.
	l, names := listNames(t, s, "/pack.zip")
	if got := strings.Join(names, ","); got != "photos/,top.txt" || l.Archive != "/pack.zip" {
		t.Fatalf("pack.zip lists %q (archive %q)", got, l.Archive)
	}
	if _, names = listNames(t, s, "/pack.zip/photos"); strings.Join(names, ",") != "a.jpg,log.txt" {
		t.Fatalf("pack.zip/photos lists %v", names)
	}
	if _, names = listNames(t, s, "/plain.tar"); strings.Join(names, ",") != "dir/,two.txt" {
		t.Fatalf("plain.tar lists %v", names)
	}
	if _, names = listNames(t, s, "/packed.tar.gz/deep/er"); strings.Join(names, ",") != "three.txt" {
		t.Fatalf("packed.tar.gz lists %v", names)
	}

	// Entries download like files, with Range on stored, deflated and tar members.
	cases := []struct{ path, rng, want string }{
		{"/f/pack.zip/top.txt", "", "top"},
		{"/f/pack.zip/photos/a.jpg", "bytes=7-", "bytes"},
		{"/f/pack.zip/photos/log.txt", "bytes=99990-99999", "0123456789"},
		{"/f/plain.tar/dir/one.txt", "bytes=6-", "file"},
		{"/f/packed.tar.gz/deep/er/three.txt", "bytes=-4", "file"},
	}
	for _, c := range cases {
		hdr := []string{}
		if c.rng != "" {
			hdr = []string{"Range", c.rng}
		}
		w := do(s, "GET", c.path+"?dl=abcdefgh.1", nil, hdr...)
		if w.Body.String() != c.want || (c.rng != "" && w.Code != 206) {
			t.Errorf("%s %s: %d %q, want %q", c.path, c.rng, w.Code, w.Body.String(), c.want)
		}
	}
	if w := do(s, "GET", "/f/pack.zip/.hidden", nil); w.Code != 404 {
		t.Errorf("hidden archive member served: %d", w.Code)
	}
	if snap := st.Snapshot(); snap.Downloads < 3 {
		t.Errorf("archive downloads not counted: %d", snap.Downloads)
	}

	// Sizes come from the index; ZIPs of archive folders work.
	w = do(s, "GET", "/api/size?base=/pack.zip&p=photos", nil)
	if !strings.Contains(w.Body.String(), `"bytes":100012`) {
		t.Errorf("size inside archive: %s", w.Body)
	}
	w = do(s, "POST", "/api/zip", strings.NewReader(`{"base":"/pack.zip","paths":["photos"],"store":true}`))
	var zr struct {
		URL  string
		Size int64
	}
	json.Unmarshal(w.Body.Bytes(), &zr)
	w = do(s, "GET", zr.URL, nil)
	if int64(w.Body.Len()) != zr.Size {
		t.Fatalf("stored zip of archive folder: %d bytes, planned %d", w.Body.Len(), zr.Size)
	}
	r, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var inZip []string
	for _, f := range r.File {
		inZip = append(inZip, f.Name)
	}
	if got := strings.Join(inZip, ","); got != "photos/,photos/a.jpg,photos/log.txt" {
		t.Errorf("zip of archive folder holds %q", got)
	}
}

func TestSeveralRootsShareAVirtualTop(t *testing.T) {
	base := t.TempDir()
	a, b := filepath.Join(base, "one", "Photos"), filepath.Join(base, "two", "Photos")
	for _, d := range []string{a, b} {
		os.MkdirAll(d, 0o755)
	}
	os.WriteFile(filepath.Join(a, "x.jpg"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(b, ".secret"), []byte("s"), 0o644)
	report := filepath.Join(base, "report.pdf")
	os.WriteFile(report, []byte("%PDF"), 0o644)
	dot := filepath.Join(base, ".config")
	os.MkdirAll(dot, 0o755)

	st := stats.New()
	s, err := New(Config{Paths: []string{a, b, report, dot}, Store: st, NoThumbs: true,
		Assets: fstest.MapFS{"index.html": {Data: []byte("x")}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); s.root.Close() })

	// Clashing names are numbered; a dot-named root is still listed.
	if _, names := listNames(t, s, "/"); strings.Join(names, ",") != ".config/,Photos (2)/,Photos/,report.pdf" {
		t.Fatalf("top level lists %v", names)
	}
	if _, names := listNames(t, s, "/Photos"); strings.Join(names, ",") != "x.jpg" {
		t.Fatalf("/Photos lists %v", names)
	}
	// Dotfiles inside a root stay hidden.
	if _, names := listNames(t, s, "/Photos%20(2)"); len(names) != 0 {
		t.Fatalf("hidden file listed: %v", names)
	}
	if w := do(s, "GET", "/f/report.pdf", nil); w.Body.String() != "%PDF" {
		t.Fatalf("file root: %d %q", w.Code, w.Body)
	}
	for _, p := range []string{"/f/report.pdf/x", "/f/Photos/../../one", "/f/nope/x.jpg", "/f/Photos%20(2)/.secret"} {
		if w := do(s, "GET", p, nil); w.Code == 200 {
			t.Errorf("%s should not be served", p)
		}
	}
	// A ZIP from the top level spans roots; search walks them all.
	w := do(s, "POST", "/api/zip", strings.NewReader(`{"base":"/","paths":["Photos","report.pdf"]}`))
	if w.Code != 200 {
		t.Fatalf("zip across roots: %d %s", w.Code, w.Body)
	}
	if w := do(s, "GET", "/api/search?q=x.jpg", nil); !strings.Contains(w.Body.String(), `"/Photos/x.jpg"`) {
		t.Errorf("search across roots: %s", w.Body)
	}
	if s.name != "Photos + Photos (2) +2" {
		t.Errorf("display name %q", s.name)
	}
}

// jpegWithOrientation encodes img as a JPEG carrying an EXIF orientation tag.
func jpegWithOrientation(t *testing.T, img image.Image, o uint16) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	// TIFF header (little endian), one IFD with one entry: orientation.
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 0x01, 3, 0, 1, 0, 0, 0, byte(o), byte(o >> 8), 0, 0, 0, 0, 0, 0}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	seg := append([]byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}, payload...)
	b := buf.Bytes()
	return append(append(append([]byte{}, b[:2]...), seg...), b[2:]...)
}

func TestThumbnailsFitAndTurnUpright(t *testing.T) {
	wide := image.NewRGBA(image.Rect(0, 0, 800, 400))
	for i := range wide.Pix {
		wide.Pix[i] = 0xff
	}
	var pngBuf bytes.Buffer
	png.Encode(&pngBuf, wide)
	s, _ := newTestServer(t, func(c *Config) {
		c.ThumbDir = t.TempDir()
		os.WriteFile(filepath.Join(c.Dir, "wide.png"), pngBuf.Bytes(), 0o644)
		os.WriteFile(filepath.Join(c.Dir, "turned.jpg"), jpegWithOrientation(t, wide, 6), 0o644)
		os.WriteFile(filepath.Join(c.Dir, "broken.jpg"), []byte("not an image"), 0o644)
		writeZip(t, filepath.Join(c.Dir, "pics.zip"), map[string]string{"in/wide.png": pngBuf.String()})
	})
	size := func(target string) (int, int, *httptest.ResponseRecorder) {
		w := do(s, "GET", target, nil)
		if w.Code != 200 {
			return 0, 0, w
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		return cfg.Width, cfg.Height, w
	}
	if x, y, w := size("/api/thumb?path=/wide.png&w=256"); x != 256 || y != 128 || w.Header().Get("ETag") == "" {
		t.Fatalf("wide.png thumb is %dx%d (status %d)", x, y, w.Code)
	}
	// EXIF orientation 6 turns the 800x400 picture on its side.
	if x, y, _ := size("/api/thumb?path=/turned.jpg&w=200"); x != 128 || y != 256 {
		t.Fatalf("turned.jpg thumb is %dx%d, want 128x256", x, y)
	}
	if x, y, _ := size("/api/thumb?path=/pics.zip/in/wide.png&w=128"); x != 128 || y != 64 {
		t.Fatalf("thumb inside zip is %dx%d", x, y)
	}
	if w := do(s, "GET", "/api/thumb?path=/broken.jpg", nil); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("broken image: %d", w.Code)
	}
	if w := do(s, "GET", "/api/thumb?path=/a.txt", nil); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text file: %d", w.Code)
	}
	// A repeat with the ETag is a 304, and the disk cache has the file.
	_, _, w := size("/api/thumb?path=/wide.png&w=256")
	if w2 := do(s, "GET", "/api/thumb?path=/wide.png&w=256", nil, "If-None-Match", w.Header().Get("ETag")); w2.Code != 304 {
		t.Fatalf("conditional thumb: %d", w2.Code)
	}
	if des, _ := os.ReadDir(s.thumbs.dir); len(des) < 3 {
		t.Fatalf("disk cache has %d files", len(des))
	}
}

func TestOrientMapsEveryExifValue(t *testing.T) {
	// A 2x1 image: red on the left, blue on the right.
	src := image.NewRGBA(image.Rect(0, 0, 2, 1))
	red, blue := color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}
	src.Set(0, 0, red)
	src.Set(1, 0, blue)
	// Where red ends up for each orientation.
	want := map[int]image.Point{1: {0, 0}, 2: {1, 0}, 3: {1, 0}, 4: {0, 0}, 5: {0, 0}, 6: {0, 0}, 7: {0, 1}, 8: {0, 1}}
	for o, p := range want {
		dst := orient(src, o)
		if dst.RGBAAt(p.X, p.Y) != red {
			t.Errorf("orientation %d: red not at %v", o, p)
		}
	}
}

func TestHashesAndSumsInZips(t *testing.T) {
	s, _ := newTestServer(t, func(c *Config) {
		writeZip(t, filepath.Join(c.Dir, "pack.zip"), map[string]string{"inner.txt": "inside"})
	})
	sum := func(b string) string { h := sha256.Sum256([]byte(b)); return hex.EncodeToString(h[:]) }

	w := do(s, "GET", "/api/hash?path=/a.txt", nil)
	if !strings.Contains(w.Body.String(), sum("hello world")) {
		t.Fatalf("hash of a.txt: %s", w.Body)
	}
	if w := do(s, "GET", "/api/hash?path=/pack.zip/inner.txt", nil); !strings.Contains(w.Body.String(), sum("inside")) {
		t.Fatalf("hash inside zip: %s", w.Body)
	}
	if w := do(s, "GET", "/api/hash?path=/docs", nil); w.Code == 200 {
		t.Fatal("folders have no hash")
	}

	// A basket-style ZIP: items from several folders, flat, with sums, in
	// both stored (exact size) and deflated form.
	for _, store := range []bool{true, false} {
		body := `{"base":"/","paths":["docs/readme.md","a.txt","pack.zip/inner.txt"],"flat":true,"sums":true,"name":"my basket","store":` + map[bool]string{true: "true", false: "false"}[store] + `}`
		w := do(s, "POST", "/api/zip", strings.NewReader(body))
		var zr struct {
			URL, Name string
			Size      int64
		}
		json.Unmarshal(w.Body.Bytes(), &zr)
		if zr.Name != "my basket.zip" {
			t.Fatalf("archive name %q", zr.Name)
		}
		w = do(s, "GET", zr.URL, nil)
		if store && int64(w.Body.Len()) != zr.Size {
			t.Fatalf("stored zip with sums: %d bytes, planned %d", w.Body.Len(), zr.Size)
		}
		r, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		var sums string
		for _, f := range r.File {
			names = append(names, f.Name)
			if f.Name == "SHA256SUMS" {
				rc, _ := f.Open()
				b, _ := io.ReadAll(rc)
				sums = string(b)
			}
		}
		if got := strings.Join(names, ","); got != "readme.md,a.txt,inner.txt,SHA256SUMS" {
			t.Fatalf("store=%v: flat zip holds %q", store, got)
		}
		want := sum("# hi") + "  readme.md\n" + sum("hello world") + "  a.txt\n" + sum("inside") + "  inner.txt\n"
		if sums != want {
			t.Fatalf("store=%v: SHA256SUMS =\n%s\nwant\n%s", store, sums, want)
		}
	}

	// A path that disappeared is named in the error.
	w = do(s, "POST", "/api/zip", strings.NewReader(`{"base":"/","paths":["gone.txt"]}`))
	if w.Code != 404 || !strings.Contains(w.Body.String(), "not found: /gone.txt") {
		t.Fatalf("missing basket item: %d %s", w.Code, w.Body)
	}
	// A folder and something inside it are archived once.
	if got := dropNested([]string{"docs", "docs/readme.md", "a.txt"}); strings.Join(got, ",") != "docs,a.txt" {
		t.Fatalf("dropNested: %v", got)
	}
}

// growingWriter appends to a file the first time it is written to,
// simulating a file that changes while it is being downloaded.
type growingWriter struct {
	http.ResponseWriter
	path string
	done bool
}

// Write grows the file once, then forwards.
func (g *growingWriter) Write(p []byte) (int, error) {
	if !g.done {
		g.done = true
		f, _ := os.OpenFile(g.path, os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString(" and more")
		f.Close()
	}
	return g.ResponseWriter.Write(p)
}

func TestFilesChangingMidDownloadAreFlagged(t *testing.T) {
	s, st := newTestServer(t, nil)
	r := httptest.NewRequest("GET", "/f/a.txt?dl=abcdefgh.1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(&growingWriter{ResponseWriter: rec, path: filepath.Join(s.Dir(), "a.txt")}, r)
	if rec.Body.String() != "hello world" {
		t.Fatalf("body %q", rec.Body)
	}
	views := st.TransfersByDLPrefix("abcdefgh.")
	if len(views) != 1 || !views[0].Changed {
		t.Fatalf("change not flagged: %+v", views)
	}
	// An untouched file isn't flagged.
	do(s, "GET", "/f/docs/readme.md?dl=abcdefgh.2", nil)
	for _, v := range st.TransfersByDLPrefix("abcdefgh.") {
		if v.DLID == "abcdefgh.2" && v.Changed {
			t.Fatal("unchanged file flagged")
		}
	}
}
