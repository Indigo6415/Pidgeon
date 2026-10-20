package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"path"
	"regexp"
	"time"

	"github.com/Indigo6415/pidgeon/internal/brand"
	"github.com/Indigo6415/pidgeon/internal/devices"
	"github.com/Indigo6415/pidgeon/internal/listeners"
)

// entry is one row in a directory listing.
type entry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`  // bytes; 0 for directories
	MTime int64  `json:"mtime"` // Unix milliseconds
	Link  bool   `json:"link,omitempty"`
}

// listing is the /api/ls response.
type listing struct {
	Path    string  `json:"path"` // "/" or "/a/b"
	Entries []entry `json:"entries"`
}

// writeJSON writes v as a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// handleInfo describes the server to the web UI.
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	info := map[string]any{
		"name":    brand.Name,
		"version": brand.ResolvedVersion(),
		"root":    s.name,
		"auth":    "open",
	}
	// The caller is already authenticated (auth middleware runs first), so
	// handing back the token lets the UI build "Copy as cURL" commands; the
	// cookie itself is HttpOnly and invisible to scripts.
	switch {
	case s.cfg.Token != "":
		info["auth"] = "token"
		info["token"] = s.cfg.Token
	case s.cfg.BasicUser != "":
		info["auth"] = "basic"
	}
	// Optional features the UI shows only when they are on.
	if s.inbox != nil {
		info["inbox"] = true
		info["inboxMax"] = s.cfg.InboxMax
		if p := s.inboxPath(); p != "" {
			info["inboxPath"] = p
		}
	}
	if s.cfg.Devices != nil && s.cfg.Devices.Ask() {
		info["ask"] = true
	}
	if !s.cfg.Expires.IsZero() {
		info["expires"] = s.cfg.Expires.UnixMilli()
	}
	if s.cfg.Once {
		info["once"] = true
	}
	if s.cfg.MaxBytes > 0 {
		info["maxBytes"] = s.cfg.MaxBytes
		info["served"] = s.cfg.Store.BytesServed()
	}
	writeJSON(w, http.StatusOK, info)
}

// handleList returns the entries of one directory.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	name, err := s.resolve(r.URL.Query().Get("path"))
	if err != nil {
		httpError(w, err)
		return
	}
	entries, err := s.list(name)
	if err != nil {
		httpError(w, err)
		return
	}
	out := listing{Path: "/", Entries: entries}
	if name != "." {
		out.Path = "/" + name
	}
	writeJSON(w, http.StatusOK, out)
}

// list reads one root-relative directory, applying the visibility rules:
// dotfiles are hidden unless --hidden, symlinks are followed only while they
// stay inside the served root, and devices, sockets and pipes are left out.
func (s *Server) list(name string) ([]entry, error) {
	f, err := s.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	des, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]entry, 0, len(des))
	// Build a row per entry, following symlinks only while they stay inside
	// the served root (os.Root.Stat refuses ones that escape).
	for _, de := range des {
		n := de.Name()
		if !s.cfg.ShowHidden && isHiddenName(n) {
			continue
		}
		e := entry{Name: n}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		// Resolve symlinks to their target's type and size.
		if fi.Mode()&fs.ModeSymlink != 0 {
			e.Link = true
			fi, err = s.root.Stat(path.Join(name, n))
			if err != nil {
				continue // dangling or escaping link: hide it
			}
		}
		// Skip devices, sockets, pipes: they can't be downloaded.
		if !fi.IsDir() && !fi.Mode().IsRegular() {
			continue
		}
		e.Dir = fi.IsDir()
		if !e.Dir {
			e.Size = fi.Size()
		}
		e.MTime = fi.ModTime().UnixMilli()
		out = append(out, e)
	}
	return out, nil
}

// Entry is one item of a folder, for the dashboard's file picker.
type Entry struct {
	Name string
	Dir  bool
	Size int64
}

// Browse lists a folder of the served directory ("/" or "/a/b") with the
// same visibility rules as the web UI.
func (s *Server) Browse(p string) ([]Entry, error) {
	name, err := s.resolve(p)
	if err != nil {
		return nil, err
	}
	es, err := s.list(name)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, len(es))
	// Copy into the exported shape.
	for i, e := range es {
		out[i] = Entry{Name: e.Name, Dir: e.Dir, Size: e.Size}
	}
	return out, nil
}

// peekInfo summarises one folder for the web UI without a full listing.
type peekInfo struct {
	Items int      `json:"items"` // visible entries (files + folders)
	Dirs  []string `json:"dirs"`  // subfolder names; null when there are too many to send
}

// Peek limits keep one request cheap even when the client asks about many
// large folders at once.
const (
	peekMaxFolders = 100
	peekMaxDirs    = 1000
	peekMaxTime    = 3 * time.Second
)

// handlePeek summarises several folders (?p=a&p=b, relative to ?base=) so
// the web UI can lazily fill in item counts and tree chevrons for the
// subfolders that are on screen. Unlike /api/ls it never stats regular
// entries (only symlinks), so it stays fast on huge folders. Folders that
// can't be read are left out of the response.
func (s *Server) handlePeek(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ctx, cancel := context.WithTimeout(r.Context(), peekMaxTime)
	defer cancel()

	out := map[string]peekInfo{}
	// Summarise each requested folder until the count or time budget runs out.
	for i, p := range q["p"] {
		if i >= peekMaxFolders || ctx.Err() != nil {
			break
		}
		name, err := s.resolve(path.Join("/", q.Get("base"), p))
		if err != nil {
			continue
		}
		if info, ok := s.peek(name); ok {
			out[p] = info
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// peek counts the visible entries of one folder and names its subfolders,
// applying the same filtering rules as handleList.
func (s *Server) peek(name string) (peekInfo, bool) {
	f, err := s.root.Open(name)
	if err != nil {
		return peekInfo{}, false
	}
	defer f.Close()
	des, err := f.ReadDir(-1)
	if err != nil {
		return peekInfo{}, false
	}

	info := peekInfo{Dirs: []string{}}
	// Classify each entry from its directory record alone; only symlinks
	// need a Stat, to learn the target's type (and to drop escaping links).
	for _, de := range des {
		n := de.Name()
		if !s.cfg.ShowHidden && isHiddenName(n) {
			continue
		}
		t := de.Type()
		if t&fs.ModeSymlink != 0 {
			fi, err := s.root.Stat(path.Join(name, n))
			if err != nil {
				continue // dangling or escaping link: hidden in listings too
			}
			t = fi.Mode().Type()
		}
		// Folders are named; devices, sockets and pipes aren't listed at all.
		if t.IsDir() {
			info.Dirs = append(info.Dirs, n)
		} else if !t.IsRegular() {
			continue
		}
		info.Items++
	}
	// Too many to be worth sending: the tree fetches them when expanded.
	if len(info.Dirs) > peekMaxDirs {
		info.Dirs = nil
	}
	return info, true
}

// sizeLimit bounds /api/size so a huge tree can't pin a CPU.
const (
	sizeMaxEntries = 200_000
	sizeMaxTime    = 3 * time.Second
)

// handleSize totals the bytes under the given paths (?p=a&p=b, relative to
// ?base=), so the web UI can show the size of a selection containing folders.
func (s *Server) handleSize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ctx, cancel := context.WithTimeout(r.Context(), sizeMaxTime)
	defer cancel()

	var total, files int64
	partial := false
	seen := 0
	// Walk each requested path; stop early on the time / entry budget.
	for _, p := range q["p"] {
		name, err := s.resolve(path.Join("/", q.Get("base"), p))
		if err != nil {
			continue
		}
		fs.WalkDir(s.fsys, name, func(p string, d fs.DirEntry, err error) error {
			// Budget exhausted: report a partial total.
			if seen++; seen > sizeMaxEntries || ctx.Err() != nil {
				partial = true
				return fs.SkipAll
			}
			if err != nil {
				return nil
			}
			if !s.cfg.ShowHidden && p != name && isHiddenName(d.Name()) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			// Only regular files contribute bytes.
			if d.Type().IsRegular() {
				if fi, err := d.Info(); err == nil {
					total += fi.Size()
					files++
				}
			}
			return nil
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"bytes": total, "files": files, "partial": partial})
}

// clientIDPattern bounds the web client ID used to scope the progress feed.
var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// progressItem is one download in the progress feed.
type progressItem struct {
	ID    string  `json:"id"`
	Sent  int64   `json:"sent"`
	Total int64   `json:"total"`
	Rate  float64 `json:"rate"`
	State string  `json:"state"`
}

// handleEvents is a Server-Sent Events stream, one per open tab. It reports
// server-side progress of this browser's downloads as unnamed messages:
// browsers save downloads straight to disk and expose no progress to the
// page, but the server knows exactly how many bytes it has sent, so it
// reports them here keyed by the download ID. Named events carry everything
// else: "change" when the folder the tab shows changes on disk, and "send"
// when the operator sends this device a file from the dashboard.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	cid := r.URL.Query().Get("c")
	if !clientIDPattern.MatchString(cid) {
		http.Error(w, "bad client id", http.StatusBadRequest)
		return
	}
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("retry: 2000\n\n"))
	rc.Flush()

	sub := s.subscribe(cid, clientIP(r))
	defer s.unsubscribe(sub)

	drain := listeners.DrainFromContext(r.Context())
	tick := time.NewTicker(400 * time.Millisecond)
	defer tick.Stop()
	var last []byte
	idle := 0
	prefix := cid + "."

	// Push an update whenever this client's downloads change; send a
	// keep-alive comment every ~15s so proxies and phones don't drop it.
	for {
		select {
		case <-r.Context().Done():
			return
		case <-drain:
			return
		case <-s.done:
			return
		case ev := <-sub.ch:
			// Named events go out as soon as they happen.
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data); err != nil {
				return
			}
			rc.Flush()
			continue
		case <-tick.C:
		}
		// A device blocked from the dashboard loses its live feed too.
		if s.cfg.Devices != nil && s.cfg.Devices.Status(sub.ip) == devices.Blocked {
			return
		}
		views := s.cfg.Store.TransfersByDLPrefix(prefix)
		items := make([]progressItem, len(views))
		// Translate stats views into the compact wire format.
		for i, v := range views {
			items[i] = progressItem{ID: v.DLID, Sent: v.Sent, Total: v.Total, Rate: math.Round(v.Rate), State: v.State.String()}
		}
		buf, _ := json.Marshal(items)
		// Unchanged since the last push: maybe send a heartbeat instead.
		if bytes.Equal(buf, last) {
			if idle++; idle%38 == 0 {
				if _, err := w.Write([]byte(": ping\n\n")); err != nil {
					return
				}
				rc.Flush()
			}
			continue
		}
		idle = 0
		last = buf
		if _, err := w.Write(append(append([]byte("data: "), buf...), '\n', '\n')); err != nil {
			return
		}
		rc.Flush()
	}
}
