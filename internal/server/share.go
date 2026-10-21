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

// Bounds for links created over the API.
const (
	shareMaxTTL  = 30 * 24 * time.Hour
	shareMaxUses = 1000
)

// share is one link. All fields are guarded by Server.shareMu.
type share struct {
	id      string
	path    string // root-relative; "." is the whole served folder
	dir     bool
	created time.Time
	expires time.Time // zero = never
	maxUses int       // 0 = unlimited
	uses    int
	revoked bool
}

// Share is a snapshot of a share link.
type Share struct {
	ID      string    `json:"id"`
	Path    string    `json:"path"` // "/a/b"
	Name    string    `json:"name"`
	Dir     bool      `json:"dir"`
	URL     string    `json:"url"`
	Created time.Time `json:"-"`
	Expires time.Time `json:"-"`
	MaxUses int       `json:"maxUses"`
	Uses    int       `json:"uses"`
	Revoked bool      `json:"revoked"`
	State   string    `json:"state"` // "active", "expired", "used up" or "revoked"
}

// stateLocked says whether the link still works, and if not, why.
func (sh *share) stateLocked(now time.Time) string {
	switch {
	case sh.revoked:
		return "revoked"
	case !sh.expires.IsZero() && now.After(sh.expires):
		return "expired"
	case sh.maxUses > 0 && sh.uses >= sh.maxUses:
		return "used up"
	}
	return "active"
}

// snapshotLocked copies a share for callers outside the lock.
func (s *Server) snapshotLocked(sh *share, now time.Time) Share {
	p := "/"
	name := s.name
	if sh.path != "." {
		p = "/" + sh.path
		name = path.Base(sh.path)
	}
	return Share{
		ID: sh.id, Path: p, Name: name, Dir: sh.dir, URL: s.ShareURL(sh.id),
		Created: sh.created, Expires: sh.expires, MaxUses: sh.maxUses, Uses: sh.uses,
		Revoked: sh.revoked, State: sh.stateLocked(now),
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
	name, err := s.resolve(p)
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
	now := time.Now()
	sh := &share{id: randomToken()[:22], path: name, dir: fi.IsDir(), created: now, maxUses: max(0, maxUses)}
	if ttl > 0 {
		sh.expires = now.Add(ttl)
	}
	s.shareMu.Lock()
	defer s.shareMu.Unlock()
	s.shares[sh.id] = sh
	return s.snapshotLocked(sh, now), nil
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
// {"path":"/a/b","ttl":3600,"uses":3} (ttl in seconds; 0 = no limit).
func (s *Server) handleShareCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		TTL  int64  `json:"ttl"`
		Uses int    `json:"uses"`
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
	sh, err := s.CreateShare(req.Path, ttl, req.Uses)
	if err != nil {
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
func shareJSON(sh Share) map[string]any {
	exp := int64(0)
	if !sh.Expires.IsZero() {
		exp = sh.Expires.UnixMilli()
	}
	return map[string]any{
		"id": sh.ID, "path": sh.Path, "name": sh.Name, "dir": sh.Dir, "url": sh.URL,
		"expires": exp, "maxUses": sh.MaxUses, "uses": sh.Uses, "state": sh.State,
	}
}

// handleShare serves /s/<id> and, for folder links, /s/<id>/<sub path>.
// A file link downloads the file. A folder link shows a small page listing
// the folder (and its subfolders), with a ZIP of any folder via ?zip=1.
// Every download started counts as a use; resuming one doesn't.
func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rest := r.PathValue("rest")
	s.shareMu.Lock()
	sh, ok := s.shares[id]
	var snap Share
	if ok {
		snap = s.snapshotLocked(sh, time.Now())
	}
	s.shareMu.Unlock()
	if !ok {
		s.shareError(w, http.StatusNotFound, "This link doesn't exist.", "Ask whoever sent it for a new one.")
		return
	}
	resume := isResume(r)
	// A used-up link may still finish a download that already started.
	if snap.State == "expired" || snap.State == "revoked" || (snap.State == "used up" && !resume) {
		s.shareError(w, http.StatusGone, "This link has "+map[string]string{"expired": "expired", "revoked": "been revoked", "used up": "been used up"}[snap.State]+".", "Ask whoever sent it for a new one.")
		return
	}

	// A file link: only the bare /s/<id> URL exists.
	if !snap.Dir {
		if rest != "" {
			s.shareError(w, http.StatusNotFound, "Not found.", "")
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
	if !strings.HasSuffix(r.URL.Path, "/") && rest == "" {
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
