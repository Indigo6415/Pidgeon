package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/Indigo6415/pidgeon/internal/brand"
	"github.com/Indigo6415/pidgeon/internal/humanize"
)

// Share links hand out one file or folder instead of the whole tree. A link
// is an unguessable ID that works without the access token, can expire and
// can be limited to a number of downloads, and can be revoked from the
// dashboard at any time. Links live only as long as the server runs.
//
// A link can also ask for a PIN first: the visitor types it once and gets a
// cookie for that link; ten wrong guesses lock the link for good.
//
// A file request is a link the other way round: its page is a drop zone,
// and what visitors send lands in its own folder of the inbox. Its limit
// counts files received instead of downloads.

// Bounds for links created over the API.
const (
	shareMaxTTL   = 30 * 24 * time.Hour
	shareMaxUses  = 1000
	sharePINTries = 10 // wrong PINs before a link locks
)

// share is one link. All fields are guarded by Server.shareMu.
type share struct {
	id      string
	path    string // root-relative; "." is the whole served folder
	dir     bool
	created time.Time
	expires time.Time // zero = never
	maxUses int       // 0 = unlimited
	uses    int       // downloads started, or files received for a request
	revoked bool

	pin    string // digits the visitor must enter first; "" = none
	pinKey string // cookie value proving the PIN was entered
	fails  int    // wrong PIN attempts

	request bool   // a file request: visitors upload instead of download
	folder  string // request: inbox-relative folder uploads land in
	pending int    // request: uploads in progress (they hold a use)
}

// Share is a snapshot of a share link.
type Share struct {
	ID      string    `json:"id"`
	Path    string    `json:"path"` // "/a/b"; for a request, where uploads land
	Name    string    `json:"name"`
	Dir     bool      `json:"dir"`
	URL     string    `json:"url"`
	Created time.Time `json:"-"`
	Expires time.Time `json:"-"`
	MaxUses int       `json:"maxUses"`
	Uses    int       `json:"uses"`
	Revoked bool      `json:"revoked"`
	State   string    `json:"state"` // "active", "expired", "used up", "locked" or "revoked"
	Request bool      `json:"request"`
	PIN     string    `json:"-"` // shown in the dashboard only
}

// ShareOptions describe a new share link.
type ShareOptions struct {
	Path    string        // file or folder to share ("/a/b"); unused for a request
	TTL     time.Duration // 0 = until the server stops
	MaxUses int           // downloads allowed (files, for a request); 0 = unlimited
	PIN     string        // optional digits the visitor must type first
	Request bool          // a file request into the inbox instead of a download
	Folder  string        // request: inbox subfolder for the files; "" picks one
}

// stateLocked says whether the link still works, and if not, why.
func (sh *share) stateLocked(now time.Time) string {
	switch {
	case sh.revoked:
		return "revoked"
	case !sh.expires.IsZero() && now.After(sh.expires):
		return "expired"
	case sh.pin != "" && sh.fails >= sharePINTries:
		return "locked"
	case sh.maxUses > 0 && sh.uses >= sh.maxUses:
		return "used up"
	}
	return "active"
}

// snapshotLocked copies a share for callers outside the lock.
func (s *Server) snapshotLocked(sh *share, now time.Time) Share {
	p := "/"
	name := s.name
	switch {
	case sh.request:
		// A served path ("/Inbox/x") when the inbox is browsable, else "inbox/x".
		p, name = s.uploadDisplay(sh.folder), sh.folder
	case sh.path != ".":
		p = "/" + sh.path
		name = path.Base(sh.path)
	}
	return Share{
		ID: sh.id, Path: p, Name: name, Dir: sh.dir, URL: s.ShareURL(sh.id),
		Created: sh.created, Expires: sh.expires, MaxUses: sh.maxUses, Uses: sh.uses,
		Revoked: sh.revoked, State: sh.stateLocked(now), Request: sh.request, PIN: sh.pin,
	}
}

// ShareURL is the full link for a share ID, on the address other devices
// can reach (falling back to a path when that isn't known).
func (s *Server) ShareURL(id string) string {
	base := ""
	if s.cfg.BaseURL != nil {
		base = strings.TrimSuffix(s.cfg.BaseURL(), "/")
	}
	return base + "/s/" + id
}

// CreateShare makes a link to p ("/a/b"), valid for ttl (0 = until the
// server stops) and for maxUses downloads (0 = unlimited).
func (s *Server) CreateShare(p string, ttl time.Duration, maxUses int) (Share, error) {
	return s.NewShare(ShareOptions{Path: p, TTL: ttl, MaxUses: maxUses})
}

// errNoInbox is returned when a file request is made without --inbox.
var errNoInbox = errors.New("file requests need an inbox (start with --inbox <dir>)")

// NewShare makes a share link or a file request.
func (s *Server) NewShare(o ShareOptions) (Share, error) {
	if o.PIN != "" && !validPIN(o.PIN) {
		return Share{}, errors.New("a PIN is 4 to 12 digits")
	}
	now := time.Now()
	sh := &share{id: randomToken()[:22], created: now, maxUses: max(0, o.MaxUses), pin: o.PIN, dir: true}
	if o.PIN != "" {
		sh.pinKey = randomToken()
	}
	if o.TTL > 0 {
		sh.expires = now.Add(o.TTL)
	}
	if o.Request {
		if s.inbox == nil {
			return Share{}, errNoInbox
		}
		folder := strings.TrimSpace(o.Folder)
		if folder == "" {
			folder = "Request " + now.Format("2006-01-02 1504")
		}
		folder, err := cleanUploadName(folder)
		if err != nil {
			return Share{}, err
		}
		if err := s.inbox.MkdirAll(folder, 0o755); err != nil {
			return Share{}, fmt.Errorf("can't create the folder: %w", err)
		}
		sh.request, sh.folder, sh.path = true, folder, "."
	} else {
		name, err := s.resolve(o.Path)
		if err != nil {
			return Share{}, err
		}
		fi, err := s.stat(name)
		if err != nil {
			return Share{}, err
		}
		if !fi.IsDir() && !fi.Mode().IsRegular() {
			return Share{}, errors.New("only files and folders can be shared")
		}
		sh.path, sh.dir = name, fi.IsDir()
	}
	s.shareMu.Lock()
	defer s.shareMu.Unlock()
	s.shares[sh.id] = sh
	return s.snapshotLocked(sh, now), nil
}

// validPIN reports whether p is 4 to 12 digits.
func validPIN(p string) bool {
	if len(p) < 4 || len(p) > 12 {
		return false
	}
	// Digits only: easy to type on a phone keypad.
	for _, c := range p {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Shares lists every link, newest first, including dead ones (so the
// dashboard can show why a link stopped working).
func (s *Server) Shares() []Share {
	now := time.Now()
	s.shareMu.Lock()
	out := make([]Share, 0, len(s.shares))
	// Snapshot each link under the lock.
	for _, sh := range s.shares {
		out = append(out, s.snapshotLocked(sh, now))
	}
	s.shareMu.Unlock()
	slices.SortFunc(out, func(a, b Share) int { return b.Created.Compare(a.Created) })
	return out
}

// RevokeShare stops a link from working. It reports whether it existed.
func (s *Server) RevokeShare(id string) bool {
	s.shareMu.Lock()
	defer s.shareMu.Unlock()
	sh, ok := s.shares[id]
	if ok {
		sh.revoked = true
	}
	return ok
}

// RemoveShare forgets a link entirely (it stops working too).
func (s *Server) RemoveShare(id string) {
	s.shareMu.Lock()
	delete(s.shares, id)
	s.shareMu.Unlock()
}

// handleShareCreate makes a link from the web UI:
// {"path":"/a/b","ttl":3600,"uses":3,"pin":"1234"} (ttl in seconds; 0 = no
// limit), or a file request: {"request":true,"folder":"Photos","ttl":...}.
func (s *Server) handleShareCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		TTL     int64  `json:"ttl"`
		Uses    int    `json:"uses"`
		PIN     string `json:"pin"`
		Request bool   `json:"request"`
		Folder  string `json:"folder"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ttl := time.Duration(req.TTL) * time.Second
	if ttl < 0 || ttl > shareMaxTTL || req.Uses < 0 || req.Uses > shareMaxUses {
		http.Error(w, "expiry or download limit out of range", http.StatusBadRequest)
		return
	}
	sh, err := s.NewShare(ShareOptions{Path: req.Path, TTL: ttl, MaxUses: req.Uses, PIN: req.PIN, Request: req.Request, Folder: req.Folder})
	if err != nil {
		// Validation problems are worth reading; path problems stay vague.
		if req.Request || req.PIN != "" && !validPIN(req.PIN) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, shareJSON(sh))
}

// handleShareList lists the links for the web UI.
func (s *Server) handleShareList(w http.ResponseWriter, r *http.Request) {
	list := s.Shares()
	out := make([]map[string]any, len(list))
	// Convert each to its wire form.
	for i, sh := range list {
		out[i] = shareJSON(sh)
	}
	writeJSON(w, http.StatusOK, out)
}

// shareJSON is a share's wire form; times are Unix milliseconds (0 = none).
// The PIN itself is never sent, only whether there is one.
func shareJSON(sh Share) map[string]any {
	exp := int64(0)
	if !sh.Expires.IsZero() {
		exp = sh.Expires.UnixMilli()
	}
	return map[string]any{
		"id": sh.ID, "path": sh.Path, "name": sh.Name, "dir": sh.Dir, "url": sh.URL,
		"expires": exp, "maxUses": sh.MaxUses, "uses": sh.Uses, "state": sh.State,
		"request": sh.Request, "pin": sh.PIN != "",
	}
}

// deadMessage explains a link that stopped working.
var deadMessage = map[string]string{
	"expired": "This link has expired.", "revoked": "This link has been revoked.",
	"used up": "This link has been used up.", "locked": "This link is locked after too many wrong PINs.",
}

// lookupShare finds a live link and checks its PIN, writing the error (or
// PIN) page itself when the visitor can't go on. resume lets a used-up
// download link still finish a download that already started.
func (s *Server) lookupShare(w http.ResponseWriter, r *http.Request, resume bool) (*share, Share, bool) {
	id := r.PathValue("id")
	s.shareMu.Lock()
	sh, ok := s.shares[id]
	var snap Share
	if ok {
		snap = s.snapshotLocked(sh, time.Now())
	}
	s.shareMu.Unlock()
	if !ok {
		s.shareError(w, http.StatusNotFound, "This link doesn't exist.", "Ask whoever sent it for a new one.")
		return nil, snap, false
	}
	if snap.State != "active" && !(snap.State == "used up" && resume && !snap.Request) {
		s.shareError(w, http.StatusGone, deadMessage[snap.State], "Ask whoever sent it for a new one.")
		return nil, snap, false
	}
	if snap.PIN != "" && !s.pinOK(r, sh) {
		s.pinPage(w, r, snap, false)
		return nil, snap, false
	}
	return sh, snap, true
}

// pinCookie names the cookie that remembers a link's PIN.
func pinCookie(id string) string { return brand.CookieName + "_pin_" + id }

// pinOK reports whether the request proves it knows the link's PIN: the
// cookie set by the PIN page, or an X-Pidgeon-PIN header (for scripts and
// `pidgeon pull`). A wrong header counts as a wrong guess.
func (s *Server) pinOK(r *http.Request, sh *share) bool {
	s.shareMu.Lock()
	defer s.shareMu.Unlock()
	if c, err := r.Cookie(pinCookie(sh.id)); err == nil && equal(c.Value, sh.pinKey) {
		return true
	}
	if h := r.Header.Get("X-Pidgeon-PIN"); h != "" {
		if equal(h, sh.pin) {
			return true
		}
		sh.fails++
	}
	return false
}

// handleSharePIN checks a PIN typed into the PIN page (POST /s/<id>). The
// right PIN sets a cookie scoped to this link and returns to the page the
// visitor came from; ten wrong ones lock the link.
func (s *Server) handleSharePIN(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	pin, next := r.PostFormValue("pin"), r.PostFormValue("next")
	s.shareMu.Lock()
	sh, ok := s.shares[id]
	var snap Share
	right := false
	if ok && sh.pin != "" && sh.stateLocked(time.Now()) == "active" {
		right = equal(pin, sh.pin)
		if !right {
			sh.fails++
		}
	}
	if ok {
		snap = s.snapshotLocked(sh, time.Now())
	}
	s.shareMu.Unlock()
	switch {
	case !ok:
		s.shareError(w, http.StatusNotFound, "This link doesn't exist.", "Ask whoever sent it for a new one.")
	case snap.State != "active":
		s.shareError(w, http.StatusGone, deadMessage[snap.State], "Ask whoever sent it for a new one.")
	case !right:
		s.pinPage(w, r, snap, true)
	default:
		http.SetCookie(w, &http.Cookie{
			Name: pinCookie(id), Value: sh.pinKey, Path: "/s/" + id,
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil,
		})
		// Only pages of this link are valid places to return to.
		if next != "/s/"+id && !strings.HasPrefix(next, "/s/"+id+"/") {
			next = "/s/" + id
		}
		http.Redirect(w, r, next, http.StatusSeeOther)
	}
}

// handleShare serves /s/<id> and, for folder links, /s/<id>/<sub path>.
// A file link downloads the file. A folder link shows a small page listing
// the folder (and its subfolders), with a ZIP of any folder via ?zip=1. A
// file request shows its upload page. ?json=1 describes a file or lists a
// folder for scripts (`pidgeon pull`) instead. Every download started
// counts as a use; resuming one doesn't.
func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rest := r.PathValue("rest")
	sh, snap, ok := s.lookupShare(w, r, isResume(r))
	if !ok {
		return
	}
	asJSON := r.URL.Query().Get("json") == "1"

	// A file request only has its upload page.
	if snap.Request {
		if rest != "" {
			s.shareError(w, http.StatusNotFound, "Not found.", "")
			return
		}
		s.requestPage(w, snap)
		return
	}

	// A file link: only the bare /s/<id> URL exists.
	if !snap.Dir {
		if rest != "" {
			s.shareError(w, http.StatusNotFound, "Not found.", "")
			return
		}
		if asJSON {
			s.shareFileJSON(w, sh.path)
			return
		}
		if !s.useShare(id, r) {
			s.shareError(w, http.StatusGone, "This link has been used up.", "Ask whoever sent it for a new one.")
			return
		}
		s.serveFile(w, r, sh.path, id)
		return
	}

	// A folder link: pages end in "/" so relative links work.
	if !strings.HasSuffix(r.URL.Path, "/") && rest == "" && !asJSON {
		http.Redirect(w, r, "/s/"+id+"/", http.StatusFound)
		return
	}
	name, err := s.resolve(path.Join("/", sh.path, rest))
	// The path must stay inside the shared folder.
	if err != nil || !(name == sh.path || sh.path == "." || isUnder(name, sh.path)) {
		s.shareError(w, http.StatusNotFound, "Not found.", "")
		return
	}
	fi, err := s.stat(name)
	if err != nil {
		s.shareError(w, http.StatusNotFound, "Not found.", "")
		return
	}
	switch {
	case asJSON && !fi.IsDir():
		s.shareFileJSON(w, name)
	case asJSON:
		es, err := s.list(name)
		if err != nil {
			http.Error(w, "this folder can't be read", http.StatusNotFound)
			return
		}
		// Paths are relative to the shared folder, which is "/".
		rel, title := "", snap.Name
		if name != sh.path {
			rel, title = relTo(sh.path, name), path.Base(name)
		}
		writeJSON(w, http.StatusOK, listing{Name: title, Path: "/" + rel, Entries: es})
	case !fi.IsDir():
		if !s.useShare(id, r) {
			s.shareError(w, http.StatusGone, "This link has been used up.", "Ask whoever sent it for a new one.")
			return
		}
		s.serveFile(w, r, name, id)
	case r.URL.Query().Get("zip") == "1":
		s.serveShareZip(w, r, id, name)
	case !strings.HasSuffix(r.URL.Path, "/"):
		http.Redirect(w, r, r.URL.Path+"/", http.StatusFound)
	default:
		s.shareListing(w, snap, sh.path, name)
	}
}

// shareFileJSON describes one shared file without counting a use.
func (s *Server) shareFileJSON(w http.ResponseWriter, name string) {
	fi, err := s.stat(name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": true, "name": fi.Name(), "size": fi.Size(), "mtime": fi.ModTime().UnixMilli()})
}

// handleShareUpload receives one file for a file request:
// PUT /s/<id>/upload?name=<file name>. Each upload holds one of the link's
// uses while it runs; a duplicate the inbox already has gives it back.
func (s *Server) handleShareUpload(w http.ResponseWriter, r *http.Request) {
	sh, snap, ok := s.lookupShare(w, r, false)
	if !ok {
		return
	}
	if !snap.Request {
		http.Error(w, "this link doesn't accept files", http.StatusMethodNotAllowed)
		return
	}
	s.shareMu.Lock()
	full := sh.maxUses > 0 && sh.uses+sh.pending >= sh.maxUses
	if !full {
		sh.pending++
	}
	s.shareMu.Unlock()
	if full {
		http.Error(w, "this request has all the files it asked for", http.StatusGone)
		return
	}
	res := s.receive(w, r, uploadTarget{dir: sh.folder, share: sh.id})
	s.shareMu.Lock()
	sh.pending--
	if res.ok && !res.duplicate {
		sh.uses++
	}
	s.shareMu.Unlock()
}

// handleShareCheck is the duplicate pre-check for a file request; it looks
// only inside the request's own folder.
func (s *Server) handleShareCheck(w http.ResponseWriter, r *http.Request) {
	sh, snap, ok := s.lookupShare(w, r, false)
	if !ok {
		return
	}
	if !snap.Request {
		http.Error(w, "this link doesn't accept files", http.StatusMethodNotAllowed)
		return
	}
	s.checkUploads(w, r, sh.folder)
}

// isResume reports whether r continues a download rather than starting one.
func isResume(r *http.Request) bool {
	rh := r.Header.Get("Range")
	return rh != "" && !strings.HasPrefix(strings.TrimSpace(rh), "bytes=0-")
}

// useShare counts a download against a link's limit. It returns false if
// the limit is already reached. HEAD requests and resumes are free.
func (s *Server) useShare(id string, r *http.Request) bool {
	if r.Method == http.MethodHead || isResume(r) {
		return true
	}
	s.shareMu.Lock()
	defer s.shareMu.Unlock()
	sh, ok := s.shares[id]
	if !ok || sh.stateLocked(time.Now()) != "active" {
		return false
	}
	sh.uses++
	return true
}

// serveShareZip streams a folder of a share link as a ZIP. The prepared
// archive is reused for later requests, so a dropped download can resume.
func (s *Server) serveShareZip(w http.ResponseWriter, r *http.Request, id, name string) {
	key := id + "\x00" + name
	s.zipMu.Lock()
	token := s.shareZips[key]
	job := s.zips[token]
	s.zipMu.Unlock()
	// No prepared archive yet (or it expired): plan one.
	if job == nil || time.Since(job.created) > zipJobTTL {
		base, names := ".", []string{name}
		if name == "." {
			// The whole served folder: archive its contents.
			es, err := s.list(".")
			if err != nil {
				s.shareError(w, http.StatusNotFound, "Not found.", "")
				return
			}
			names = names[:0]
			for _, e := range es {
				names = append(names, e.Name)
			}
		} else if i := strings.LastIndex(name, "/"); i >= 0 {
			base = name[:i]
		}
		var err error
		job, err = s.newZipJob(r.Context(), base, names, zipOptions{})
		if err != nil {
			http.Error(w, "couldn't prepare the archive", http.StatusInternalServerError)
			return
		}
		token = s.addZipJob(job)
		s.zipMu.Lock()
		s.shareZips[key] = token
		s.zipMu.Unlock()
	}
	if !s.useShare(id, r) {
		s.shareError(w, http.StatusGone, "This link has been used up.", "Ask whoever sent it for a new one.")
		return
	}
	display := "/" + job.file
	if job.base != "." {
		display = "/" + path.Join(job.base, job.file)
	}
	s.serveZip(w, r, job, token, display, id)
}

// shareRow is one entry on a folder link's page.
type shareRow struct {
	Name, Href, Size string
	Dir              bool
}

// shareListing renders the page for a folder of a share link.
func (s *Server) shareListing(w http.ResponseWriter, snap Share, top, name string) {
	es, err := s.list(name)
	if err != nil {
		s.shareError(w, http.StatusNotFound, "This folder can't be read.", "")
		return
	}
	// Folders first, then files, each alphabetically (case-insensitive).
	slices.SortFunc(es, func(a, b entry) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	rows := make([]shareRow, len(es))
	// Links are relative to this page, which always ends in "/".
	for i, e := range es {
		rows[i] = shareRow{Name: e.Name, Dir: e.Dir, Href: url.PathEscape(e.Name)}
		if e.Dir {
			rows[i].Href += "/"
		} else {
			rows[i].Size = humanize.Bytes(e.Size)
		}
	}
	title := snap.Name
	sub := ""
	// Below the shared folder: show where, relative to it.
	if name != top {
		sub = relTo(top, name)
		title = path.Base(name)
	}
	var notes []string
	if !snap.Expires.IsZero() {
		notes = append(notes, "expires in "+humanize.Duration(time.Until(snap.Expires).Round(time.Minute)))
	}
	if snap.MaxUses > 0 {
		left := snap.MaxUses - snap.Uses
		notes = append(notes, fmt.Sprintf("%d download%s left", left, map[bool]string{true: "", false: "s"}[left == 1]))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	sharePage.Execute(w, map[string]any{
		"Brand": brand.Name, "Title": title, "Root": snap.Name, "Sub": sub, "Up": name != top,
		"Rows": rows, "Notes": strings.Join(notes, " · "),
	})
}

// pinPage asks for a link's PIN. It posts back to /s/<id> and then
// returns to the page that was asked for.
func (s *Server) pinPage(w http.ResponseWriter, r *http.Request, snap Share, wrong bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	what := "a file"
	switch {
	case snap.Request:
		what = "a drop box for files"
	case snap.Dir:
		what = "a folder"
	}
	next := r.URL.Path
	if r.URL.RawQuery != "" && r.Method == http.MethodGet {
		next += "?" + r.URL.RawQuery
	}
	pinPageTmpl.Execute(w, map[string]any{"Brand": brand.Name, "ID": snap.ID, "What": what, "Next": next, "Wrong": wrong})
}

// requestPage renders the upload page of a file request.
func (s *Server) requestPage(w http.ResponseWriter, snap Share) {
	var notes []string
	if !snap.Expires.IsZero() {
		notes = append(notes, "open for "+humanize.Duration(time.Until(snap.Expires).Round(time.Minute)))
	}
	left := -1
	if snap.MaxUses > 0 {
		left = max(0, snap.MaxUses-snap.Uses)
		notes = append(notes, fmt.Sprintf("%d file%s left", left, map[bool]string{true: "", false: "s"}[left == 1]))
	}
	if s.cfg.InboxMax > 0 {
		notes = append(notes, "up to "+humanize.Bytes(s.cfg.InboxMax)+" per file")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	requestPageTmpl.Execute(w, map[string]any{
		"Brand": brand.Name, "ID": snap.ID, "Title": snap.Name, "Notes": strings.Join(notes, " · "),
		"Left": left, "Max": s.cfg.InboxMax,
	})
}

// shareError renders a short error page for a share link.
func (s *Server) shareError(w http.ResponseWriter, status int, msg, hint string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	sharePage.Execute(w, map[string]any{"Brand": brand.Name, "Error": msg, "Hint": hint})
}

// sharePage is the folder-link page (and its error page). It is
// self-contained, since share visitors can't load the app's assets.
var sharePage = template.Must(template.New("share").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark light">
<title>{{if .Error}}{{.Brand}}{{else}}{{.Title}} · {{.Brand}}{{end}}</title>
<style>
  :root { --bg:#181818; --card:#1f1f1f; --fg:#cccccc; --strong:#e7e7e7; --muted:#9d9d9d; --line:#2b2b2b; --hover:#2a2d2e; --accent:#0078d4; --link:#4daafc; --folder:#dcb67a; }
  @media (prefers-color-scheme: light) { :root { --bg:#f8f8f8; --card:#fff; --fg:#3b3b3b; --strong:#1f1f1f; --muted:#6f6f6f; --line:#e5e5e5; --hover:#f2f2f2; --accent:#005fb8; --link:#005fb8; --folder:#c49a4a; } }
  * { box-sizing:border-box } body { margin:0; min-height:100dvh; background:var(--bg); color:var(--fg);
    font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",system-ui,sans-serif; padding:24px 16px }
  main { max-width:760px; margin:0 auto; background:var(--card); border:1px solid var(--line); border-radius:12px; overflow:hidden }
  header { display:flex; align-items:flex-end; gap:12px; padding:20px 20px 14px; border-bottom:1px solid var(--line) }
  header div { flex:1; min-width:0 }
  .crumb { color:var(--muted); font-size:12px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap }
  h1 { margin:0; font-size:22px; color:var(--strong); overflow:hidden; text-overflow:ellipsis; white-space:nowrap }
  .notes { color:var(--muted); font-size:12px; margin-top:2px }
  .btn { flex:none; display:inline-flex; align-items:center; height:32px; padding:0 14px; border-radius:7px; background:var(--accent); color:#fff; text-decoration:none; font-weight:600 }
  ul { list-style:none; margin:0; padding:6px }
  li a { display:flex; gap:10px; align-items:center; padding:8px 12px; border-radius:6px; color:var(--fg); text-decoration:none }
  li a:hover { background:var(--hover) }
  .n { flex:1; min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap }
  .d .n { color:var(--strong) } .d .ic { color:var(--folder) }
  .ic { width:18px; text-align:center; color:var(--muted) }
  .s { color:var(--muted); font:12px ui-monospace,Menlo,Consolas,monospace }
  .empty, .err { padding:28px 20px; color:var(--muted); text-align:center }
  .err h1 { white-space:normal; margin-bottom:6px }
  footer { text-align:center; color:var(--muted); font-size:12px; margin-top:14px }
</style></head><body>
{{if .Error}}
<main class="err"><h1>{{.Error}}</h1><div>{{.Hint}}</div></main>
{{else}}
<main>
  <header>
    <div>
      <div class="crumb">{{.Root}}{{if .Sub}} / {{.Sub}}{{end}}</div>
      <h1>{{.Title}}</h1>
      {{if .Notes}}<div class="notes">{{.Notes}}</div>{{end}}
    </div>
    <a class="btn" href="?zip=1">Download ZIP</a>
  </header>
  <ul>
    {{if .Up}}<li class="d"><a href="../"><span class="ic">↰</span><span class="n">..</span></a></li>{{end}}
    {{range .Rows}}<li{{if .Dir}} class="d"{{end}}><a href="{{.Href}}"{{if not .Dir}} download{{end}}><span class="ic">{{if .Dir}}▰{{else}}▤{{end}}</span><span class="n">{{.Name}}</span><span class="s">{{.Size}}</span></a></li>
    {{else}}<li class="empty">This folder is empty.</li>{{end}}
  </ul>
</main>
{{end}}
<footer>Shared with {{.Brand}}</footer>
</body></html>`))
