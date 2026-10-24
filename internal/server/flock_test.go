package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// testPeerID is the mDNS ID the test peer is found under.
const testPeerID = "0badf00d"

// newFlock serves a peer over real HTTP (its tree is the usual test tree
// plus peer.txt and page.html) and builds a hub whose flock contains it.
func newFlock(t *testing.T, peerCfg func(*Config)) (hub, peer *Server) {
	t.Helper()
	peer, _ = newTestServer(t, peerCfg)
	os.WriteFile(filepath.Join(peer.Dir(), "peer.txt"), []byte("from the peer"), 0o644)
	os.WriteFile(filepath.Join(peer.Dir(), "page.html"), []byte("<script>alert(1)</script>"), 0o644)
	ts := httptest.NewServer(peer)
	t.Cleanup(ts.Close)
	auth := "open"
	if peer.cfg.Token != "" {
		auth = "token"
	}
	hub, _ = newTestServer(t, func(c *Config) {
		c.Flock = true
		c.Nearby = func(context.Context) []NearbyServer {
			return []NearbyServer{{Name: "Peer on box:8080", URL: ts.URL + "/", Auth: auth, ID: testPeerID, Proxy: ts.URL}}
		}
	})
	return hub, peer
}

// flockPath is a path on the test peer, as the hub names it.
func flockPath(p string) string { return "/.flock/" + testPeerID + p }

func TestFlockBrowsesNearbyServers(t *testing.T) {
	hub, _ := newFlock(t, nil)

	// The Nearby list says where each server opens, but not how it is reached.
	w := do(hub, "GET", "/api/nearby", nil)
	if !strings.Contains(w.Body.String(), `"path":"`+flockPath("")+`"`) || strings.Contains(w.Body.String(), "Proxy") {
		t.Fatalf("nearby: %s", w.Body)
	}
	if w := do(hub, "GET", "/api/info", nil); !strings.Contains(w.Body.String(), `"flock":true`) {
		t.Fatalf("info doesn't mention the flock: %s", w.Body)
	}

	// Remote folders list like local ones, with the peer's own rules (no dotfiles).
	l, names := listNames(t, hub, flockPath(""))
	if l.Path != flockPath("") || strings.Join(names, ",") != "a.txt,docs/,page.html,peer.txt" {
		t.Fatalf("peer root: %s %v", l.Path, names)
	}
	if _, names := listNames(t, hub, flockPath("/docs")); strings.Join(names, ",") != "empty/,readme.md" {
		t.Fatalf("peer docs: %v", names)
	}
	// The local root doesn't grow a .flock folder, and the flock itself isn't a folder.
	if _, names := listNames(t, hub, "/"); strings.Join(names, ",") != "a.txt,docs/" {
		t.Fatalf("local root: %v", names)
	}
	if w := do(hub, "GET", "/api/ls?path=/.flock", nil); w.Code != 404 {
		t.Fatalf("/.flock: %d", w.Code)
	}
	if w := do(hub, "GET", "/api/ls?path=/.flock/feedface", nil); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "isn't on the network") {
		t.Fatalf("unknown peer: %d %s", w.Code, w.Body)
	}

	// Files download whole or in ranges, and the bytes count here too.
	if w := do(hub, "GET", "/f"+flockPath("/peer.txt"), nil); w.Code != 200 || w.Body.String() != "from the peer" {
		t.Fatalf("file: %d %q", w.Code, w.Body)
	}
	w = do(hub, "GET", "/f"+flockPath("/peer.txt"), nil, "Range", "bytes=5-")
	if w.Code != 206 || w.Body.String() != "the peer" || w.Header().Get("Content-Range") != "bytes 5-12/13" {
		t.Fatalf("range: %d %q %q", w.Code, w.Body, w.Header().Get("Content-Range"))
	}
	if w := do(hub, "GET", "/f"+flockPath("/.env"), nil); w.Code != 404 {
		t.Fatalf("peer's dotfile: %d", w.Code)
	}
	// A relayed page can't run as this app.
	if w := do(hub, "GET", "/f"+flockPath("/page.html")+"?inline=1", nil); w.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("relayed html isn't sandboxed: %v", w.Header())
	}

	// Summaries, sizes and hashes come from the peer, mixed with local ones.
	w = do(hub, "GET", "/api/peek?base="+flockPath("")+"&p=docs", nil)
	var pk map[string]peekInfo
	json.Unmarshal(w.Body.Bytes(), &pk)
	if pk["docs"].Items != 2 || len(pk["docs"].Dirs) != 1 {
		t.Fatalf("peek: %s", w.Body)
	}
	w = do(hub, "GET", "/api/size?base=/&p=a.txt&p=.flock/"+testPeerID+"/docs", nil)
	if !strings.Contains(w.Body.String(), `"bytes":15`) || !strings.Contains(w.Body.String(), `"files":2`) {
		t.Fatalf("mixed size: %s", w.Body)
	}
	sum := sha256.Sum256([]byte("from the peer"))
	if w := do(hub, "GET", "/api/hash?path="+flockPath("/peer.txt"), nil); !strings.Contains(w.Body.String(), hex.EncodeToString(sum[:])) {
		t.Fatalf("hash: %s", w.Body)
	}
}

func TestFlockStaysOneHopAndLocalFeaturesStayLocal(t *testing.T) {
	hub, _ := newFlock(t, nil)
	// A request a flock relayed is never relayed again.
	if w := do(hub, "GET", "/api/ls?path="+flockPath(""), nil, flockHeader, "1"); w.Code != 404 {
		t.Fatalf("second hop: %d", w.Code)
	}
	if w := do(hub, "GET", "/api/info", nil, flockHeader, "1"); strings.Contains(w.Body.String(), "flock") {
		t.Fatalf("a relayed request is offered the flock: %s", w.Body)
	}
	// Share links and playlists only reach this server's own files.
	if w := do(hub, "POST", "/api/shares", strings.NewReader(`{"path":"`+flockPath("/peer.txt")+`"}`)); w.Code < 400 {
		t.Fatalf("share of a remote file: %d %s", w.Code, w.Body)
	}
	if w := do(hub, "GET", "/api/m3u?path="+flockPath(""), nil); w.Code != 404 {
		t.Fatalf("playlist of a remote folder: %d", w.Code)
	}
	// Without the flock, its paths are just missing dotfiles.
	plain, _ := newTestServer(t, nil)
	if w := do(plain, "GET", "/api/ls?path="+flockPath(""), nil); w.Code != 404 {
		t.Fatalf("flock off: %d", w.Code)
	}
}

func TestFlockLogsInPerVisitor(t *testing.T) {
	hub, _ := newFlock(t, func(c *Config) { c.Token = "tok" })
	// The peer wants a token: the hub says so without a 401, which would
	// send the visitor to the hub's own login.
	w := do(hub, "GET", "/api/ls?path="+flockPath(""), nil)
	if w.Code != 403 || w.Header().Get(flockLogin) != "token" {
		t.Fatalf("locked peer: %d %v", w.Code, w.Header())
	}
	if w := do(hub, "POST", "/api/flock/login", strings.NewReader(`{"id":"`+testPeerID+`","token":"nope"}`)); w.Code != 403 || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("wrong token: %d %v", w.Code, w.Header())
	}
	w = do(hub, "POST", "/api/flock/login", strings.NewReader(`{"id":"`+testPeerID+`","token":"tok"}`))
	ck := w.Result().Cookies()
	if w.Code != 200 || len(ck) != 1 || ck[0].Name != "pidgeon_flock_"+testPeerID || !ck[0].HttpOnly {
		t.Fatalf("login: %d %v", w.Code, ck)
	}
	// The visitor who logged in gets in; anyone else still doesn't.
	if w := do(hub, "GET", "/api/ls?path="+flockPath(""), nil, "Cookie", ck[0].String()); w.Code != 200 {
		t.Fatalf("with the cookie: %d %s", w.Code, w.Body)
	}
	if w := do(hub, "GET", "/f"+flockPath("/peer.txt"), nil); w.Code != 403 {
		t.Fatalf("another visitor: %d", w.Code)
	}
}

func TestFlockZipMixesServers(t *testing.T) {
	hub, _ := newFlock(t, nil)
	// Both kinds of archive: stored (exact size, planned up front) and deflated.
	for _, store := range []bool{true, false} {
		body := `{"base":"/","paths":["a.txt",".flock/` + testPeerID + `/docs",".flock/` + testPeerID + `/peer.txt"],"store":` + map[bool]string{true: "true", false: "false"}[store] + `}`
		w := do(hub, "POST", "/api/zip", strings.NewReader(body))
		if w.Code != 200 {
			t.Fatalf("create: %d %s", w.Code, w.Body)
		}
		var res struct {
			URL  string `json:"url"`
			Size int64  `json:"size"`
		}
		json.Unmarshal(w.Body.Bytes(), &res)
		w = do(hub, "GET", res.URL, nil)
		if store && int64(w.Body.Len()) != res.Size {
			t.Fatalf("stored size %d, planned %d", w.Body.Len(), res.Size)
		}
		zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			got[f.Name] = string(b)
		}
		sort.Strings(names)
		// The peer's files sit in a folder named after it.
		want := "Peer on box-8080/docs/,Peer on box-8080/docs/empty/,Peer on box-8080/docs/readme.md,Peer on box-8080/peer.txt,a.txt"
		if strings.Join(names, ",") != want {
			t.Fatalf("store=%v entries:\n%s\nwant\n%s", store, strings.Join(names, ","), want)
		}
		if got["Peer on box-8080/peer.txt"] != "from the peer" || got["a.txt"] != "hello world" {
			t.Fatalf("store=%v contents: %v", store, got)
		}
	}
	// A selection made inside a remote folder keeps paths relative to it.
	w := do(hub, "POST", "/api/zip", strings.NewReader(`{"base":"`+flockPath("")+`","paths":["docs"]}`))
	var res map[string]any
	json.Unmarshal(w.Body.Bytes(), &res)
	if res["name"] != "docs.zip" {
		t.Fatalf("remote folder zip: %d %s", w.Code, w.Body)
	}
}

func TestFlockDistrustsPeers(t *testing.T) {
	// A peer that sends names with slashes and a script dressed as an image.
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ls" {
			w.Write([]byte(`{"path":"/","entries":[{"name":"../../a.txt"},{"name":"ok.txt"},{"name":"a/b"},{"name":".."}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Set-Cookie", "pidgeon_token=stolen")
		w.Write([]byte("<script>alert(1)</script>"))
	}))
	t.Cleanup(evil.Close)
	hub, _ := newTestServer(t, func(c *Config) {
		c.Flock = true
		c.Nearby = func(context.Context) []NearbyServer {
			return []NearbyServer{{Name: "Evil", ID: testPeerID, Proxy: evil.URL}}
		}
	})
	if _, names := listNames(t, hub, flockPath("")); strings.Join(names, ",") != "ok.txt" {
		t.Fatalf("names kept: %v", names)
	}
	w := do(hub, "GET", "/f"+flockPath("/photo.jpg"), nil)
	if w.Header().Get("Content-Security-Policy") != "sandbox" || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("relayed headers: %v", w.Header())
	}
}
