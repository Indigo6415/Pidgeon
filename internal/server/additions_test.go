package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// doFrom performs a request as if it came from device ip.
func doFrom(s http.Handler, ip, method, target string, body string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.RemoteAddr = ip + ":40000"
	// Headers come as name/value pairs.
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestShareLinkPIN(t *testing.T) {
	s, _ := newTestServer(t, nil)
	sh, err := s.NewShare(ShareOptions{Path: "/a.txt", PIN: "2468"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.NewShare(ShareOptions{Path: "/a.txt", PIN: "12a4"}); err == nil {
		t.Fatal("a PIN with a letter was accepted")
	}
	// Without the PIN: the PIN page, not the file.
	if w := do(s, "GET", "/s/"+sh.ID, nil); w.Code != 401 || !strings.Contains(w.Body.String(), "Enter the PIN") {
		t.Fatalf("no PIN: %d", w.Code)
	}
	form := func(pin string) *httptest.ResponseRecorder {
		return do(s, "POST", "/s/"+sh.ID, strings.NewReader("pin="+pin+"&next=/s/"+sh.ID), "Content-Type", "application/x-www-form-urlencoded")
	}
	if w := form("1111"); w.Code != 401 || !strings.Contains(w.Body.String(), "didn't match") {
		t.Fatalf("wrong PIN: %d", w.Code)
	}
	w := form("2468")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/s/"+sh.ID {
		t.Fatalf("right PIN: %d %s", w.Code, w.Header().Get("Location"))
	}
	cookie := w.Result().Cookies()[0]
	if cookie.Path != "/s/"+sh.ID || !cookie.HttpOnly {
		t.Fatalf("cookie %+v", cookie)
	}
	if w := do(s, "GET", "/s/"+sh.ID, nil, "Cookie", cookie.Name+"="+cookie.Value); w.Code != 200 || w.Body.String() != "hello world" {
		t.Fatalf("with cookie: %d", w.Code)
	}
	// Scripts send the PIN as a header.
	if w := do(s, "GET", "/s/"+sh.ID+"?json=1", nil, "X-Pidgeon-PIN", "2468"); w.Code != 200 {
		t.Fatalf("with header: %d", w.Code)
	}
	// Ten wrong guesses lock the link for good, even for the right PIN.
	for range sharePINTries {
		form("0000")
	}
	if w := form("2468"); w.Code != http.StatusGone {
		t.Fatalf("locked link: %d", w.Code)
	}
}

func TestFileRequestsCollectFiles(t *testing.T) {
	inbox := t.TempDir()
	s, _ := newTestServer(t, func(c *Config) { c.Inbox = inbox; c.Token = "tok" })
	// Created over the API (which needs the token), named by the visitor.
	w := do(s, "POST", "/api/shares", strings.NewReader(`{"request":true,"folder":"../Trip/ photos","uses":2}`), "Authorization", "Bearer tok")
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var sh struct{ ID, Path string }
	json.Unmarshal(w.Body.Bytes(), &sh)
	folder := filepath.Join(inbox, "photos")
	if _, err := os.Stat(folder); err != nil {
		t.Fatalf("folder name not reduced to one safe segment: %v (%s)", err, sh.Path)
	}
	// The page is a drop zone; nothing can be downloaded through it.
	if w := do(s, "GET", "/s/"+sh.ID, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Choose files") {
		t.Fatalf("page: %d", w.Code)
	}
	if w := do(s, "GET", "/s/"+sh.ID+"/a.txt", nil); w.Code != 404 {
		t.Fatalf("request link downloaded something: %d", w.Code)
	}
	up := func(name, body string) *httptest.ResponseRecorder {
		return do(s, "PUT", "/s/"+sh.ID+"/upload?name="+url.QueryEscape(name), strings.NewReader(body))
	}
	if w := up("one.txt", "1"); w.Code != 200 {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	// An exact copy is dropped and doesn't use up the request.
	if w := up("again.txt", "1"); w.Code != 200 || !strings.Contains(w.Body.String(), `"duplicate":true`) {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body)
	}
	if w := up("two.txt", "22"); w.Code != 200 {
		t.Fatalf("second file: %d", w.Code)
	}
	if w := up("three.txt", "333"); w.Code != http.StatusGone {
		t.Fatalf("over the limit: %d", w.Code)
	}
	names, _ := filepath.Glob(filepath.Join(folder, "*"))
	if len(names) != 2 {
		t.Fatalf("folder has %v", names)
	}
}

// exifJPEG is the start of a JPEG whose EXIF says it was taken at when.
func exifJPEG(when string) []byte {
	var t bytes.Buffer
	be := binary.BigEndian
	t.WriteString("MM\x00*")
	binary.Write(&t, be, uint32(8))
	// IFD0: one entry pointing at the EXIF IFD (offset 26).
	binary.Write(&t, be, uint16(1))
	binary.Write(&t, be, []uint16{0x8769, 4})
	binary.Write(&t, be, []uint32{1, 26, 0})
	// EXIF IFD: DateTimeOriginal, 20 ASCII bytes at offset 44.
	binary.Write(&t, be, uint16(1))
	binary.Write(&t, be, []uint16{0x9003, 2})
	binary.Write(&t, be, []uint32{20, 44, 0})
	t.WriteString(when + "\x00")
	seg := append([]byte("Exif\x00\x00"), t.Bytes()...)
	out := []byte{0xFF, 0xD8, 0xFF, 0xE1}
	out = binary.BigEndian.AppendUint16(out, uint16(len(seg)+2))
	out = append(out, seg...)
	return append(out, 0xFF, 0xD9)
}

func TestInboxSortsByDateAndSkipsCopies(t *testing.T) {
	inbox := t.TempDir()
	s, _ := newTestServer(t, func(c *Config) { c.Inbox = inbox; c.InboxSort = "date" })
	photo := exifJPEG("2019:07:14 10:20:30")
	if w := do(s, "PUT", "/api/upload?name=IMG_1.jpg", bytes.NewReader(photo)); w.Code != 200 {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(inbox, "2019", "07", "IMG_1.jpg")); err != nil {
		t.Fatalf("photo not filed by its EXIF date: %v", err)
	}
	// No date inside: the sender's modification time decides, and is kept.
	mt := time.Date(2021, 3, 9, 12, 0, 0, 0, time.Local)
	if w := do(s, "PUT", "/api/upload?name=note.txt&mtime="+itoa(mt.UnixMilli()), strings.NewReader("hi")); w.Code != 200 {
		t.Fatalf("upload: %d", w.Code)
	}
	fi, err := os.Stat(filepath.Join(inbox, "2021", "03", "note.txt"))
	if err != nil || !fi.ModTime().Equal(mt) {
		t.Fatalf("note.txt: %v %v", err, fi)
	}
	// The pre-check knows the photo by its fingerprint; a new file isn't known.
	fp, _ := fingerprint(bytes.NewReader(photo), int64(len(photo)))
	body := `{"files":[{"name":"x.jpg","size":` + itoa(int64(len(photo))) + `,"fp":"` + fp + `"},{"name":"y","size":5,"fp":"00000000"}]}`
	if w := do(s, "POST", "/api/upload/check", strings.NewReader(body)); w.Code != 200 || !strings.Contains(w.Body.String(), `"have":[true,false]`) {
		t.Fatalf("check: %d %s", w.Code, w.Body)
	}
	// Sending the same photo again keeps one copy.
	if w := do(s, "PUT", "/api/upload?name=IMG_1%20copy.jpg", bytes.NewReader(photo)); !strings.Contains(w.Body.String(), `"duplicate":true`) {
		t.Fatalf("copy: %s", w.Body)
	}
	if names, _ := filepath.Glob(filepath.Join(inbox, "2019", "07", "*")); len(names) != 1 {
		t.Fatalf("copies: %v", names)
	}
}

// itoa formats an int64.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestMovieDate(t *testing.T) {
	// ftyp, then moov with an mvhd (version 0) created 2020-05-01 UTC.
	var b bytes.Buffer
	box := func(kind string, body []byte) []byte {
		out := binary.BigEndian.AppendUint32(nil, uint32(8+len(body)))
		return append(append(out, kind...), body...)
	}
	b.Write(box("ftyp", []byte("isom\x00\x00\x02\x00")))
	b.Write(box("mdat", make([]byte, 1000)))
	created := time.Date(2020, 5, 1, 8, 0, 0, 0, time.UTC)
	mvhd := make([]byte, 100)
	binary.BigEndian.PutUint32(mvhd[4:], uint32(created.Sub(movieEpoch)/time.Second))
	b.Write(box("moov", box("mvhd", mvhd)))
	got, ok := mediaDate(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if !ok || !got.Equal(created) {
		t.Fatalf("got %v %v", got, ok)
	}
}

func TestPigeonPost(t *testing.T) {
	s, _ := newTestServer(t, nil)
	var heard []Message
	s.cfg.OnPost = func(m Message) { heard = append(heard, m) }
	if w := doFrom(s, "10.0.0.5", "POST", "/api/post", `{"text":"  https://example.com/x  "}`); w.Code != 200 {
		t.Fatalf("send: %d %s", w.Code, w.Body)
	}
	if w := doFrom(s, "10.0.0.5", "POST", "/api/post", `{"text":"   "}`); w.Code != 400 {
		t.Fatalf("empty message: %d", w.Code)
	}
	s.Post("10.0.0.5", "for five")
	s.Post("10.0.0.6", "for six")
	s.Post("", "for everyone")
	if len(heard) != 1 || heard[0].Text != "https://example.com/x" || heard[0].From != "10.0.0.5" {
		t.Fatalf("host heard %+v", heard)
	}
	// Each device sees its own messages, what was sent to it and to everyone.
	var got []struct {
		Text string
		Mine bool
		Host bool
	}
	json.Unmarshal(doFrom(s, "10.0.0.5", "GET", "/api/post", "").Body.Bytes(), &got)
	if len(got) != 3 || !got[0].Mine || got[1].Text != "for five" || !got[2].Host {
		t.Fatalf("device 5 sees %+v", got)
	}
}

func TestReceiptsTickDeliveredFiles(t *testing.T) {
	s, st := newTestServer(t, nil)
	// A preview isn't a delivery; a whole download is.
	doFrom(s, "10.0.0.5", "GET", "/f/a.txt?inline=1", "")
	if n := st.Deliveries(); n != 0 {
		t.Fatalf("preview made %d receipts", n)
	}
	doFrom(s, "10.0.0.5", "GET", "/f/a.txt", "")
	rs := st.Receipts()
	sum := sha256.Sum256([]byte("hello world"))
	if len(rs) != 1 || rs[0].Path != "/a.txt" || rs[0].SHA256 != hex.EncodeToString(sum[:]) || rs[0].Client != "10.0.0.5" {
		t.Fatalf("receipts %+v", rs)
	}
	// The listing ticks it for that device only.
	got := func(ip string) int64 {
		var l listing
		json.Unmarshal(doFrom(s, ip, "GET", "/api/ls?path=/", "").Body.Bytes(), &l)
		for _, e := range l.Entries {
			if e.Name == "a.txt" {
				return e.Got
			}
		}
		return -1
	}
	if got("10.0.0.5") == 0 || got("10.0.0.6") != 0 {
		t.Fatal("tick on the wrong device")
	}
	// A new version on disk is no longer "received".
	os.WriteFile(filepath.Join(s.Dir(), "a.txt"), []byte("hello, world!"), 0o644)
	if got("10.0.0.5") != 0 {
		t.Fatal("tick survived a change")
	}
	// A resumed download that reaches the end also gets a receipt (hashed from disk).
	doFrom(s, "10.0.0.7", "GET", "/f/a.txt", "", "Range", "bytes=5-")
	deadline := time.Now().Add(2 * time.Second)
	for st.Deliveries() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if rs := st.Receipts(); len(rs) != 2 || rs[0].SHA256 == "" {
		t.Fatalf("resumed receipt %+v", rs)
	}
}

func TestPlaylists(t *testing.T) {
	s, _ := newTestServer(t, func(c *Config) { c.Token = "tok" })
	for _, n := range []string{"Ep 10.mp4", "Ep 2.mkv", "cover.jpg"} {
		os.WriteFile(filepath.Join(s.Dir(), "docs", n), []byte("x"), 0o644)
	}
	w := do(s, "GET", "/api/m3u?path=/docs", nil, "Authorization", "Bearer tok")
	body := w.Body.String()
	if w.Code != 200 || !strings.HasPrefix(body, "#EXTM3U") || strings.Contains(body, "cover") {
		t.Fatalf("%d %s", w.Code, body)
	}
	if i, j := strings.Index(body, "Ep%202.mkv"), strings.Index(body, "Ep%2010.mp4"); i < 0 || j < i {
		t.Fatalf("not in natural order:\n%s", body)
	}
	if !strings.Contains(body, "?token=tok") {
		t.Fatal("players can't log in: links need the token")
	}
}

func TestDLNABrowseAndKey(t *testing.T) {
	s, _ := newTestServer(t, func(c *Config) { c.TV = true; c.Token = "tok" })
	os.WriteFile(filepath.Join(s.Dir(), "docs", "film.mp4"), []byte("movie"), 0o644)
	_, desc, ok := s.DLNA()
	if !ok {
		t.Fatal("TV mode off")
	}
	base := strings.TrimSuffix(desc, "/desc.xml")
	// TVs can't log in: the key in the path is the credential, nothing else.
	if w := do(s, "GET", desc, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "MediaServer:1") {
		t.Fatalf("desc: %d", w.Code)
	}
	if w := do(s, "GET", "/dlna/wrongkey/desc.xml", nil); w.Code != 404 {
		t.Fatalf("wrong key: %d", w.Code)
	}
	browse := func(id, flag string) string {
		body := `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1"><ObjectID>` + id +
			`</ObjectID><BrowseFlag>` + flag + `</BrowseFlag><StartingIndex>0</StartingIndex><RequestedCount>0</RequestedCount></u:Browse></s:Body></s:Envelope>`
		w := do(s, "POST", base+"/cds/control", strings.NewReader(body), "SOAPACTION", `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`)
		return w.Body.String()
	}
	root := browse("0", "BrowseDirectChildren")
	// Folders and media only; a.txt isn't playable.
	if !strings.Contains(root, "&lt;dc:title&gt;docs&lt;/dc:title&gt;") || strings.Contains(root, "a.txt") {
		t.Fatalf("root:\n%s", root)
	}
	// TVs treat a folder without childCount as empty.
	if meta := browse("0", "BrowseMetadata"); !strings.Contains(meta, "childCount=&quot;1&quot;") {
		t.Fatalf("root metadata:\n%s", meta)
	}
	// Samsung TVs ask for this before browsing.
	w := do(s, "POST", base+"/cds/control", strings.NewReader(`<s:Envelope/>`), "SOAPACTION", `"urn:schemas-upnp-org:service:ContentDirectory:1#X_GetFeatureList"`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "samsung.com_BASICVIEW") {
		t.Fatalf("X_GetFeatureList: %d %s", w.Code, w.Body.String())
	}
	docs := browse("/docs", "BrowseDirectChildren")
	if !strings.Contains(docs, "film") || !strings.Contains(docs, base+"/f/docs/film.mp4") {
		t.Fatalf("docs:\n%s", docs)
	}
	// Hidden files stay hidden here too.
	if !strings.Contains(browse("/.env", "BrowseMetadata"), "No such object") {
		t.Fatal("dotfile reachable over DLNA")
	}
	if w := do(s, "GET", base+"/f/docs/film.mp4", nil); w.Code != 200 || w.Header().Get("transferMode.dlna.org") != "Streaming" {
		t.Fatalf("stream: %d", w.Code)
	}
	// Only media files stream through TV mode.
	if w := do(s, "GET", base+"/f/a.txt", nil); w.Code != 404 {
		t.Fatalf("non-media over DLNA: %d", w.Code)
	}
}

func TestDLNAKeySurvivesRestart(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "tv.secret")
	s1, _ := newTestServer(t, func(c *Config) { c.TV = true; c.TVSecretFile = secret })
	_, desc1, _ := s1.DLNA()
	s2, _ := newTestServer(t, func(c *Config) { c.TV = true; c.TVSecretFile = secret; c.Dir = s1.Dir() })
	_, desc2, _ := s2.DLNA()
	// Samsung TVs keep the address they first saw; it must not change.
	if desc1 != desc2 {
		t.Fatalf("key changed across runs: %s vs %s", desc1, desc2)
	}
}
