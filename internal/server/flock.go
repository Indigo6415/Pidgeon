package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Indigo6415/pidgeon/internal/brand"
	"github.com/Indigo6415/pidgeon/internal/certs"
	"github.com/Indigo6415/pidgeon/internal/listeners"
	"github.com/Indigo6415/pidgeon/internal/stats"
)

// The flock (--flock): the other Pidgeon servers on the network, browsed
// through this one as if they were folders of it. A path under
// /.flock/<peer>/ names something on that peer, by its mDNS ID, and this
// server fetches it from there for the visitor. So the web UI lists,
// previews and downloads remote files through the same API as local ones,
// and one ZIP (or basket) can mix files from several machines.
//
// What keeps this safe:
//
//   - Only peers that answered the mDNS scan are reached, at the address
//     their own answer came from (NearbyServer.Proxy), so a visitor can't
//     point the server at an address of their choosing.
//   - Credentials for a peer are the visitor's own, kept in a cookie of
//     theirs, never on the server: a token one visitor typed opens nothing
//     for another.
//   - Requests to a peer carry X-Pidgeon-Flock, and a request that does is
//     never relayed again, so the flock is one hop deep and can't loop.
//   - Relayed files are sandboxed whatever type the peer claims (PDFs
//     excepted), so a peer can't run script as this app.
//   - Only the browsing API reaches the flock (resolveIn); share links,
//     playlists, TV mode and the folder watcher stay local (resolve).

const (
	flockDir     = ".flock"                // the virtual top-level folder of the flock
	flockHeader  = "X-Pidgeon-Flock"       // marks a request a flock relayed
	flockLogin   = "X-Pidgeon-Flock-Login" // on a refusal: the kind of credentials the peer wants
	flockMaxJSON = 64 << 20                // largest API answer accepted from a peer
	flockMaxWalk = 200_000                 // most entries a ZIP takes from one remote folder
	flockForget  = 10 * time.Minute        // a peer missing from the scans this long is dropped
	flockRescan  = 5 * time.Second         // an unknown peer triggers a scan at most this often
)

// peerIDPattern matches the mDNS IDs that name peers in flock paths.
var peerIDPattern = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// flockPeer is a peer this server can relay to. It is replaced, never
// changed, when a scan finds the peer again.
type flockPeer struct {
	info   NearbyServer
	client *http.Client
	seen   time.Time
}

// flockCreds are a visitor's credentials for one peer.
type flockCreds struct {
	Token string `json:"t,omitempty"`
	User  string `json:"u,omitempty"`
	Pass  string `json:"p,omitempty"`
}

// flockError is a peer's refusal, or the failure to reach it, with the
// status to report it under.
type flockError struct {
	status int
	msg    string
	login  string // the peer wants credentials: its auth mode ("token", "basic", ...)
}

func (e *flockError) Error() string { return e.msg }

// Is makes a peer's 404 an fs.ErrNotExist, like a missing local file.
func (e *flockError) Is(target error) bool {
	return target == fs.ErrNotExist && e.status == http.StatusNotFound
}

// flockListed is a remembered listing of a folder on a peer.
type flockListed struct {
	l   listing
	err error
}

// inFlock reports whether a root-relative name is the flock or inside it.
func inFlock(name string) bool { return name == flockDir || strings.HasPrefix(name, flockDir+"/") }

// splitFlock splits a root-relative name in the flock, ".flock/<peer>/a/b",
// into the peer's ID and the path on that peer ("/a/b"; "/" for its root).
func splitFlock(name string) (id, inner string, ok bool) {
	rest, ok := strings.CutPrefix(name, flockDir+"/")
	if !ok {
		return "", "", false
	}
	id, inner, _ = strings.Cut(rest, "/")
	if !peerIDPattern.MatchString(id) {
		return "", "", false
	}
	return id, "/" + inner, true
}

// plainName reports whether a peer's entry name is one name: anything else
// would make the paths built from it point somewhere else.
func plainName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsAny(n, "/\\\x00") && utf8.ValidString(n)
}

/* ------------------------------------------------------------------ */
/* peers                                                              */
/* ------------------------------------------------------------------ */

// rememberPeers records the peers of a scan that can be relayed to. One
// that drops out of a scan is kept for a while: mDNS answers get lost, and
// a ZIP may still be reading from it.
func (s *Server) rememberPeers(found []NearbyServer) {
	now := time.Now()
	s.flockMu.Lock()
	defer s.flockMu.Unlock()
	for _, n := range found {
		if n.Proxy == "" || !peerIDPattern.MatchString(n.ID) {
			continue
		}
		// A peer at the same address with the same certificate keeps its
		// client, and with it its open connections.
		old := s.flockPeers[n.ID]
		client := peerClient(n)
		if old != nil && old.info.Proxy == n.Proxy && old.info.Fingerprint == n.Fingerprint {
			client = old.client
		}
		s.flockPeers[n.ID] = &flockPeer{info: n, client: client, seen: now}
	}
	// Forget peers that haven't been seen for a long time.
	for id, p := range s.flockPeers {
		if now.Sub(p.seen) > flockForget {
			delete(s.flockPeers, id)
		}
	}
}

// peerClient builds the HTTP client for one peer.
func peerClient(n NearbyServer) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // peers are on the LAN; an HTTP proxy from the environment would only get in the way
	// A self-signed peer is trusted by the fingerprint it announced, as
	// `pidgeon pull` does.
	if fp := certs.Normalize(n.Fingerprint); n.TLS && fp != "" {
		tr.TLSClientConfig = certs.Pin(fp)
	}
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{
		Transport: tr,
		// A redirect from a peer is reported, not followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// peer returns the flock peer with the given ID. One that isn't known yet
// (a bookmarked folder, or no scan since this server started) is looked
// for on the network once.
func (s *Server) peer(ctx context.Context, id string) (*flockPeer, error) {
	known := func() *flockPeer {
		s.flockMu.Lock()
		defer s.flockMu.Unlock()
		return s.flockPeers[id]
	}
	if p := known(); p != nil {
		return p, nil
	}
	s.scanNearby(ctx, flockRescan)
	if p := known(); p != nil {
		return p, nil
	}
	return nil, &flockError{status: http.StatusBadGateway, msg: "that server isn't on the network any more"}
}

// get sends one request for target ("/api/ls?path=%2F") to the peer, with
// the visitor's credentials and the extra headers in hdr.
func (p *flockPeer) get(ctx context.Context, method, target string, c flockCreds, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.info.Proxy+target, nil)
	if err != nil {
		return nil, err
	}
	maps.Copy(req.Header, hdr)
	req.Header.Set("User-Agent", brand.Name+"-flock/"+brand.ResolvedVersion())
	req.Header.Set(flockHeader, "1")
	// One Authorization header: the token wins over basic auth.
	switch {
	case c.Token != "":
		req.Header.Set("Authorization", "Bearer "+c.Token)
	case c.User != "":
		req.SetBasicAuth(c.User, c.Pass)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &flockError{status: http.StatusBadGateway, msg: "couldn't reach " + p.info.Name + ": " + err.Error()}
	}
	return resp, nil
}

// refusal turns a peer's error response into a flockError, closing it.
func (p *flockPeer) refusal(resp *http.Response) error {
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		login, want := p.info.Auth, "an access token"
		switch login {
		case "", "open":
			login = "token"
		case "basic":
			want = "a user name and password"
		}
		// Not a 401: that would send the visitor to this server's own login.
		return &flockError{status: http.StatusForbidden, login: login, msg: p.info.Name + " needs " + want}
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	text := strings.TrimSpace(string(b))
	if text == "" || !utf8.ValidString(text) {
		text = resp.Status
	}
	status := resp.StatusCode
	// A redirect or other surprise is the peer's failure, not the visitor's.
	if status < 400 {
		status = http.StatusBadGateway
	}
	return &flockError{status: status, msg: p.info.Name + ": " + text}
}

// writeFlockError reports a failed relay to the visitor. A peer that wants
// credentials is named in a header, so the web UI can ask for them.
func writeFlockError(w http.ResponseWriter, err error) {
	var fe *flockError
	if !errors.As(err, &fe) {
		fe = &flockError{status: http.StatusBadGateway, msg: err.Error()}
	}
	if fe.login != "" {
		w.Header().Set(flockLogin, fe.login)
	}
	http.Error(w, fe.msg, fe.status)
}

/* ------------------------------------------------------------------ */
/* credentials                                                        */
/* ------------------------------------------------------------------ */

// flockCookie is the cookie that keeps a visitor's credentials for a peer.
func flockCookie(id string) string { return brand.Command + "_flock_" + id }

// credsFor reads the visitor's credentials for peer id from their cookie.
func credsFor(r *http.Request, id string) flockCreds {
	var c flockCreds
	if ck, err := r.Cookie(flockCookie(id)); err == nil {
		if b, err := base64.RawURLEncoding.DecodeString(ck.Value); err == nil {
			json.Unmarshal(b, &c)
		}
	}
	return c
}

// handleFlockLogin checks a visitor's credentials for a peer
// ({"id", "token"} or {"id", "user", "pass"}) and keeps them in a cookie
// of theirs.
func (s *Server) handleFlockLogin(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Flock || r.Header.Get(flockHeader) != "" {
		http.NotFound(w, r)
		return
	}
	var req struct {
		ID    string `json:"id"`
		Token string `json:"token"`
		User  string `json:"user"`
		Pass  string `json:"pass"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil || !peerIDPattern.MatchString(req.ID) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	c := flockCreds{Token: strings.TrimSpace(req.Token), User: req.User, Pass: req.Pass}
	// The peer's info answers only to valid credentials.
	var info map[string]any
	if err := s.flockJSON(r.Context(), req.ID, c, "/api/info", &info); err != nil {
		var fe *flockError
		if errors.As(err, &fe) && fe.login != "" {
			http.Error(w, "that didn't work: check the token shown on the other computer", http.StatusForbidden)
			return
		}
		writeFlockError(w, err)
		return
	}
	b, _ := json.Marshal(c)
	http.SetCookie(w, &http.Cookie{
		Name:     flockCookie(req.ID),
		Value:    base64.RawURLEncoding.EncodeToString(b),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   30 * 24 * 3600,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

/* ------------------------------------------------------------------ */
/* reading from peers                                                 */
/* ------------------------------------------------------------------ */

// flockJSON fetches target from peer id and decodes its JSON answer into v.
func (s *Server) flockJSON(ctx context.Context, id string, c flockCreds, target string, v any) error {
	p, err := s.peer(ctx, id)
	if err != nil {
		return err
	}
	resp, err := p.get(ctx, http.MethodGet, target, c, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return p.refusal(resp)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, flockMaxJSON)).Decode(v); err != nil {
		return &flockError{status: http.StatusBadGateway, msg: p.info.Name + " sent an answer this server can't read"}
	}
	return nil
}

// flockList reads a folder on a peer, keeping only entries with plain
// names. Delivery ticks are the peer's view of this server, not of the
// visitor, so they are dropped.
func (s *Server) flockList(ctx context.Context, id, inner string, c flockCreds) (listing, error) {
	var l listing
	if err := s.flockJSON(ctx, id, c, "/api/ls?path="+url.QueryEscape(inner), &l); err != nil {
		return l, err
	}
	es := make([]entry, 0, len(l.Entries))
	for _, e := range l.Entries {
		if plainName(e.Name) {
			e.Got = 0
			es = append(es, e)
		}
	}
	l.Entries = es
	return l, nil
}

// flockOpen streams a whole file from a peer.
func (s *Server) flockOpen(ctx context.Context, id, inner string, c flockCreds) (io.ReadCloser, error) {
	p, err := s.peer(ctx, id)
	if err != nil {
		return nil, err
	}
	resp, err := p.get(ctx, http.MethodGet, "/f/"+escapePath(strings.TrimPrefix(inner, "/")), c, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, p.refusal(resp)
	}
	return resp.Body, nil
}

/* ------------------------------------------------------------------ */
/* the browsing API                                                   */
/* ------------------------------------------------------------------ */

// flockListing answers /api/ls for a folder on a peer.
func (s *Server) flockListing(w http.ResponseWriter, r *http.Request, name, id, inner string) {
	l, err := s.flockList(r.Context(), id, inner, credsFor(r, id))
	if err != nil {
		writeFlockError(w, err)
		return
	}
	out := listing{Path: "/" + name, Entries: l.Entries, Partial: l.Partial}
	// The peer names the archive by its own path.
	if strings.HasPrefix(l.Archive, "/") {
		out.Archive = "/" + path.Join(flockDir, id, l.Archive)
	}
	writeJSON(w, http.StatusOK, out)
}

// flockPeek summarises folders on one peer in a single request. want maps
// each folder's path on the peer to the key it was asked under in out.
func (s *Server) flockPeek(ctx context.Context, r *http.Request, id string, want map[string]string, out map[string]peekInfo) {
	q := url.Values{"base": {"/"}}
	for inner := range want {
		q.Add("p", inner)
	}
	var got map[string]peekInfo
	// A peer that can't answer just leaves its folders unsummarised.
	if s.flockJSON(ctx, id, credsFor(r, id), "/api/peek?"+q.Encode(), &got) != nil {
		return
	}
	for inner, info := range got {
		key, ok := want[inner]
		if !ok {
			continue
		}
		// Subfolder names become tree paths: keep only plain ones.
		if info.Dirs != nil {
			dirs := make([]string, 0, len(info.Dirs))
			for _, d := range info.Dirs {
				if plainName(d) {
					dirs = append(dirs, d)
				}
			}
			info.Dirs = dirs
		}
		out[key] = info
	}
}

// flockSize totals the bytes under paths on one peer. ok is false when the
// peer couldn't say, so the caller reports a partial total.
func (s *Server) flockSize(ctx context.Context, r *http.Request, id string, inners []string) (bytes, files int64, partial, ok bool) {
	var got struct {
		Bytes   int64 `json:"bytes"`
		Files   int64 `json:"files"`
		Partial bool  `json:"partial"`
	}
	q := url.Values{"base": {"/"}, "p": inners}
	if s.flockJSON(ctx, id, credsFor(r, id), "/api/size?"+q.Encode(), &got) != nil {
		return 0, 0, true, false
	}
	return got.Bytes, got.Files, got.Partial, true
}

// flockFile relays a file from a peer (ranges, conditional requests and
// all), crediting the bytes to a transfer here, so the dashboard and the
// visitor's downloads panel follow it like a local download.
func (s *Server) flockFile(w http.ResponseWriter, r *http.Request, name, id, inner string) {
	// A peer's root is a folder: it is browsed, not downloaded.
	if inner == "/" {
		http.Redirect(w, r, "/#/"+escapePath(name), http.StatusFound)
		return
	}
	p, err := s.peer(r.Context(), id)
	if err != nil {
		writeFlockError(w, err)
		return
	}
	// --max-bytes counts relayed bytes like any others.
	if s.overBudget() {
		http.Error(w, "this server has reached its data limit", http.StatusServiceUnavailable)
		return
	}
	hdr := http.Header{}
	// Pass on what makes resuming, seeking and revalidating work.
	for _, k := range []string{"Range", "If-Range", "If-Modified-Since", "If-None-Match"} {
		if v := r.Header.Get(k); v != "" {
			hdr.Set(k, v)
		}
	}
	inline := r.URL.Query().Get("inline") == "1"
	target := "/f/" + escapePath(strings.TrimPrefix(inner, "/"))
	if inline {
		target += "?inline=1"
	}
	resp, err := p.get(r.Context(), r.Method, target, credsFor(r, id), hdr)
	if err != nil {
		writeFlockError(w, err)
		return
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusNotModified, http.StatusRequestedRangeNotSatisfiable:
	default:
		writeFlockError(w, p.refusal(resp))
		return
	}
	defer resp.Body.Close()
	h := w.Header()
	// Only headers that describe the content are passed on: a peer sets no
	// cookies or policies here.
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag", "Content-Disposition"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	sandboxRelayed(h)
	// Nothing to stream: just the answer.
	if r.Method == http.MethodHead || resp.StatusCode == http.StatusNotModified || resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		h.Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		return
	}

	size := relayedSize(resp)
	t := s.cfg.Store.Begin(stats.TransferInfo{
		DLID:     validDLID(r.URL.Query().Get("dl")),
		Listener: listeners.IDFromContext(r.Context()),
		Client:   clientIP(r),
		Name:     "/" + p.info.Name + inner,
		Kind:     stats.KindFile,
		Total:    size,
		Inline:   inline,
	})
	cw := stats.NewCountingWriter(w, t)
	cw.Ctx = r.Context()
	cw.WriteHeader(resp.StatusCode)
	if _, err := io.CopyBuffer(cw, onlyReader{resp.Body}, make([]byte, 256<<10)); err != nil {
		s.cfg.Store.End(t, stats.Aborted)
		// The header is long gone: cut the connection, so the browser marks
		// the download as failed instead of keeping a truncated file.
		panic(http.ErrAbortHandler)
	}
	s.cfg.Store.End(t, fileOutcome(cw, t, size))
}

// flockRelay passes a small answer from a peer (a thumbnail, a hash)
// through unchanged.
func (s *Server) flockRelay(w http.ResponseWriter, r *http.Request, id, target string) {
	p, err := s.peer(r.Context(), id)
	if err != nil {
		writeFlockError(w, err)
		return
	}
	hdr := http.Header{}
	if v := r.Header.Get("If-None-Match"); v != "" {
		hdr.Set("If-None-Match", v)
	}
	resp, err := p.get(r.Context(), http.MethodGet, target, credsFor(r, id), hdr)
	if err != nil {
		writeFlockError(w, err)
		return
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotModified {
		writeFlockError(w, p.refusal(resp))
		return
	}
	defer resp.Body.Close()
	h := w.Header()
	for _, k := range []string{"Content-Type", "ETag", "Cache-Control", "Last-Modified"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	sandboxRelayed(h)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, flockMaxJSON))
}

// sandboxRelayed confines a relayed response: whatever type the peer
// claims, it can't run script as this app. PDFs are left alone, because
// browsers won't show a sandboxed one and their viewers don't run the
// page's script anyway.
func sandboxRelayed(h http.Header) {
	ct, _, _ := mime.ParseMediaType(h.Get("Content-Type"))
	if ct == "" {
		h.Set("Content-Type", "application/octet-stream")
	}
	if ct != "application/pdf" {
		h.Set("Content-Security-Policy", "sandbox")
	}
}

// relayedSize is the full size of the file a relayed response is part of:
// from Content-Range for a range, else Content-Length; -1 if unknown.
func relayedSize(resp *http.Response) int64 {
	if _, total, ok := strings.Cut(resp.Header.Get("Content-Range"), "/"); ok {
		if n, err := strconv.ParseInt(total, 10, 64); err == nil {
			return n
		}
	}
	return resp.ContentLength
}

/* ------------------------------------------------------------------ */
/* ZIPs                                                               */
/* ------------------------------------------------------------------ */

// zipCreds collects the visitor's credentials for every peer a selection
// reaches into, so the archive can still read from them when it is
// downloaded.
func zipCreds(r *http.Request, names []string) map[string]flockCreds {
	out := map[string]flockCreds{}
	for _, n := range names {
		if id, _, ok := splitFlock(n); ok {
			out[id] = credsFor(r, id)
		}
	}
	return out
}

// planFlock adds an item on a peer (a file, or a folder and everything in
// it) to a ZIP plan, from the peer's listings.
func (s *Server) planFlock(ctx context.Context, job *zipJob, top, id, inner string, rename func(string) string) error {
	e, err := s.planStat(ctx, job, id, inner)
	if err != nil {
		return err
	}
	if !e.Dir {
		job.items = append(job.items, flockItem(top, rename(top), e))
		return nil
	}
	count := 0
	var walk func(dir, name string, mtime int64) error
	// walk adds one remote folder and, depth first, everything under it.
	walk = func(dir, name string, mtime int64) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		job.items = append(job.items, &zipItem{name: rename(name) + "/", dir: true, mode: fs.ModeDir | 0o755, mtime: msTime(mtime)})
		l, err := s.flockList(ctx, id, dir, job.creds[id])
		if err != nil {
			// A folder that vanished is noted and skipped, as on disk; a
			// peer that is gone or wants a token fails the archive.
			if errors.Is(err, fs.ErrNotExist) {
				job.skipped = append(job.skipped, name+": "+err.Error())
				return nil
			}
			return err
		}
		for _, c := range l.Entries {
			if count++; count > flockMaxWalk {
				return &flockError{status: http.StatusRequestEntityTooLarge, msg: fmt.Sprintf("%s has more than %d items: too many for one archive", path.Base(top), flockMaxWalk)}
			}
			child, cname := path.Join(dir, c.Name), name+"/"+c.Name
			switch {
			case c.Dir && c.Link:
				// Not followed, as on disk, to avoid cycles.
				job.skipped = append(job.skipped, cname+": symlinked folder not followed")
			case c.Dir:
				if err := walk(child, cname, c.MTime); err != nil {
					return err
				}
			default:
				job.items = append(job.items, flockItem(cname, rename(cname), c))
			}
		}
		return nil
	}
	return walk(inner, top, e.MTime)
}

// planStat describes a selected path on a peer, found in its folder's
// listing. Each folder is listed once per archive however many of its
// items are selected.
func (s *Server) planStat(ctx context.Context, job *zipJob, id, inner string) (entry, error) {
	if inner == "/" {
		return entry{Dir: true}, nil
	}
	dir := path.Dir(inner)
	got, ok := job.lists[id+dir]
	if !ok {
		got.l, got.err = s.flockList(ctx, id, dir, job.creds[id])
		job.lists[id+dir] = got
	}
	if got.err != nil {
		return entry{}, got.err
	}
	for _, e := range got.l.Entries {
		if e.Name == path.Base(inner) {
			return e, nil
		}
	}
	return entry{}, fs.ErrNotExist
}

// zipRel names p inside an archive made in folder base: relative to it,
// with items from the flock in a folder named after their server rather
// than under ".flock/<id>".
func (s *Server) zipRel(base, p string) string {
	rel := relTo(base, p)
	if id, inner, ok := splitFlock(rel); ok {
		return path.Join(s.peerFolder(id), inner)
	}
	return rel
}

// peerFolder is the folder name a peer's files get in an archive: its
// name, with the characters file systems refuse replaced.
func (s *Server) peerFolder(id string) string {
	s.flockMu.Lock()
	p := s.flockPeers[id]
	s.flockMu.Unlock()
	if p == nil {
		return id
	}
	n := strings.Map(func(r rune) rune {
		// Windows refuses these (and control characters) in file names.
		if r < ' ' || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '-'
		}
		return r
	}, p.info.Name)
	n = strings.Trim(n, ". ")
	if n == "" {
		return id
	}
	return n
}

// flockItem is the ZIP entry for a file on a peer.
func flockItem(src, name string, e entry) *zipItem {
	return &zipItem{src: src, name: name, size: e.Size, mode: 0o644, mtime: msTime(e.MTime)}
}

// msTime converts a listing's Unix milliseconds to a time; an unknown (0)
// time is now.
func msTime(ms int64) time.Time {
	if ms == 0 {
		return time.Now()
	}
	return time.UnixMilli(ms)
}

// openItem opens a planned ZIP entry's file: on disk (or in an archive),
// or on the peer it lives on.
func (s *Server) openItem(ctx context.Context, job *zipJob, src string) (io.ReadCloser, error) {
	if id, inner, ok := splitFlock(src); ok {
		return s.flockOpen(ctx, id, inner, job.creds[id])
	}
	f, _, err := s.openFile(src)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, errNotDir
	}
	return f, nil
}
