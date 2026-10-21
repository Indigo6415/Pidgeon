// Package server implements the HTTP file server: directory listing and
// search APIs, streamed file downloads with Range support, on-the-fly ZIP
// archives, share links, an optional upload inbox, a live event feed for the
// web UI (progress, folder changes, files sent from the dashboard), and the
// embedded web app. The served directory itself is never written to.
package server

import (
	"errors"
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Indigo6415/pidgeon/internal/brand"
	"github.com/Indigo6415/pidgeon/internal/devices"
	"github.com/Indigo6415/pidgeon/internal/listeners"
	"github.com/Indigo6415/pidgeon/internal/stats"
)

// Config configures a Server.
type Config struct {
	Dir        string   // directory to serve (made absolute by New)
	Paths      []string // several directories and files to serve side by side; overrides Dir
	ShowHidden bool     // serve dotfiles and dot-directories
	Token      string   // optional access token (query / cookie / bearer)
	BasicUser  string   // optional HTTP basic auth user
	BasicPass  string
	Store      *stats.Store
	Assets     fs.FS // web UI assets (index.html, app.js, app.css, ...)

	Devices  *devices.Registry // device list and --ask gate; nil admits everyone
	Inbox    string            // directory uploads are written to; "" disables uploads
	InboxMax int64             // largest accepted upload in bytes; 0 = no limit

	// Session limits, enforced by the app; the server refuses new downloads
	// once MaxBytes have been served and reports the limits to the web UI.
	MaxBytes int64
	Expires  time.Time // when the server will stop; zero = never
	Once     bool      // the server stops after the first completed download

	// BaseURL returns a URL other devices can reach this server on (no
	// trailing slash), used to build share links. Optional.
	BaseURL func() string

	NoThumbs bool   // don't generate image thumbnails for the grid view
	ThumbDir string // where thumbnails are cached; "" picks the user cache dir
}

// Server is the http.Handler shared by every listener.
type Server struct {
	cfg     Config
	root    *tree  // all file access goes through this: no path escapes
	fsys    fs.FS  // root.FS(), used for directory walks
	name    string // display name of the served directory (or directories)
	index   []byte // index.html with the product name filled in
	handler http.Handler

	zipMu     sync.Mutex
	zips      map[string]*zipJob
	shareZips map[string]string // share ID + path -> zip token, so share ZIPs can resume

	shareMu sync.Mutex
	shares  map[string]*share

	inbox     *os.Root // upload target; nil when uploads are off
	inboxRel  string   // inbox as a root-relative folder ("." or "a/b") when it is browsable, else ""
	uploadMu  sync.Mutex
	uploading map[string]bool // inbox names reserved by uploads in progress

	hubMu sync.Mutex
	subs  map[*subscriber]struct{} // open event streams

	arcMu       sync.Mutex
	arcs        map[string]*arcIndex // archive tables of contents, by root-relative path
	arcBuilding map[string]*arcBuild // archives being indexed right now

	thumbs *thumbCache // nil when thumbnails are off
	hashes *hashCache

	done      chan struct{} // closed by Close to end long-lived SSE streams
	closeOnce sync.Once
}

// errHidden is returned for dotfile paths when --hidden isn't set. It is
// reported to clients as a plain 404 so hidden names aren't confirmed.
var errHidden = errors.New("hidden path")

// New opens the served directory (or directories) and builds the handler.
func New(cfg Config) (*Server, error) {
	paths := cfg.Paths
	if len(paths) == 0 {
		paths = []string{cfg.Dir}
	}
	abs := make([]string, len(paths))
	// Every served path is made absolute up front.
	for i, p := range paths {
		a, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		abs[i] = a
	}
	cfg.Paths, cfg.Dir = abs, abs[0]
	root, err := openTree(abs)
	if err != nil {
		return nil, err
	}
	idx, err := fs.ReadFile(cfg.Assets, "index.html")
	if err != nil {
		root.Close()
		return nil, fmt.Errorf("web assets missing index.html: %w", err)
	}
	s := &Server{
		cfg:  cfg,
		root: root,
		fsys: root.FS(),
		name: treeName(root),
		index: []byte(strings.NewReplacer(
			"{{NAME}}", html.EscapeString(brand.Name),
			"{{TAGLINE}}", html.EscapeString(brand.Tagline),
			"{{PIGEON}}", pigeonHTML(),
		).Replace(string(idx))),
		zips:      map[string]*zipJob{},
		shareZips: map[string]string{},
		shares:    map[string]*share{},
		uploading: map[string]bool{},
		subs:      map[*subscriber]struct{}{},
		done:      make(chan struct{}),

		arcs:        map[string]*arcIndex{},
		arcBuilding: map[string]*arcBuild{},
		hashes:      newHashCache(),
	}
	if !cfg.NoThumbs {
		s.thumbs = newThumbCache(cfg.ThumbDir)
	}
	if cfg.Inbox != "" {
		if err := s.openInbox(cfg.Inbox); err != nil {
			return nil, err
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", noCache(http.FileServerFS(cfg.Assets))))
	mux.HandleFunc("GET /api/info", s.handleInfo)
	mux.HandleFunc("GET /api/ls", s.handleList)
	mux.HandleFunc("GET /api/peek", s.handlePeek)
	mux.HandleFunc("GET /api/size", s.handleSize)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/watch", s.handleWatch)
	mux.HandleFunc("POST /api/zip", s.handleZipCreate)
	mux.HandleFunc("GET /zip/{token}/{file}", s.handleZipStream)
	mux.HandleFunc("GET /f/{path...}", s.handleFile)
	mux.HandleFunc("GET /api/shares", s.handleShareList)
	mux.HandleFunc("POST /api/shares", s.handleShareCreate)
	mux.HandleFunc("GET /s/{id}", s.handleShare)
	mux.HandleFunc("GET /s/{id}/{rest...}", s.handleShare)
	mux.HandleFunc("PUT /api/upload", s.handleUpload)
	mux.HandleFunc("GET /api/thumb", s.handleThumb)
	mux.HandleFunc("GET /api/hash", s.handleHash)

	// Order matters: instrument is outermost so rejected (401) requests are
	// still counted and logged; the device gate sits inside auth so only
	// visitors with valid credentials (or a share link) ever reach a prompt.
	s.handler = s.instrument(securityHeaders(s.auth(s.gate(mux))))
	go s.watchLoop()
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// Dir returns the absolute directory being served (the first one, when
// several paths are served).
func (s *Server) Dir() string { return s.cfg.Dir }

// Paths returns every absolute path being served, in order.
func (s *Server) Paths() []string { return s.root.Paths() }

// treeName is the display name of what is served: the folder's name, or
// for several paths the first two names and how many more there are.
func treeName(t *tree) string {
	if !t.multi() {
		return filepath.Base(t.single.Name())
	}
	names := make([]string, 0, 2)
	// Name the first two; count the rest.
	for i, m := range t.mounts {
		if i == 2 {
			return strings.Join(names, " + ") + fmt.Sprintf(" +%d", len(t.mounts)-2)
		}
		names = append(names, m.name)
	}
	return strings.Join(names, " + ")
}

// Inbox returns the absolute upload directory, or "" when uploads are off.
func (s *Server) Inbox() string { return s.cfg.Inbox }

// Close ends long-lived streams (SSE) so a graceful shutdown isn't held up.
// The directory handle stays open: transfers still finishing during the
// shutdown grace period (ZIP walks in particular) need it. It is released
// when the process exits.
func (s *Server) Close() {
	s.closeOnce.Do(func() { close(s.done) })
}

// instrument counts and logs every request. The progress feed is excluded
// from the log because it is one long request per open browser tab.
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := s.cfg.Store
		st.CountRequest()
		rec := stats.NewRecorder(w, st)
		start := time.Now()
		// Deferred so the entry is written even when a handler aborts the
		// connection with panic(http.ErrAbortHandler).
		defer func() {
			// Credit the device; uploads credit their received bytes themselves.
			if s.cfg.Devices != nil {
				s.cfg.Devices.AddTraffic(clientIP(r), rec.Bytes, 0)
			}
			if r.URL.Path == "/api/events" {
				return
			}
			st.Log(stats.LogEntry{
				Time:     start,
				Client:   clientIP(r),
				Method:   r.Method,
				Path:     r.URL.Path,
				Status:   rec.Status,
				Bytes:    rec.Bytes,
				Duration: time.Since(start),
				Listener: listeners.IDFromContext(r.Context()),
			})
		}()
		next.ServeHTTP(rec, r)
	})
}

// admitWait is how long a request from a device awaiting approval is held
// open before the visitor is shown a "waiting" page.
const admitWait = 20 * time.Second

// gate records every device and refuses blocked ones. With --ask, a new
// device's requests wait for the operator to allow it in the dashboard.
func (s *Server) gate(next http.Handler) http.Handler {
	reg := s.cfg.Devices
	// No registry: everyone is admitted (tests, embedding).
	if reg == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch reg.Admit(r.Context(), clientIP(r), r.UserAgent(), admitWait) {
		case devices.Allowed:
			next.ServeHTTP(w, r)
		case devices.Blocked:
			http.Error(w, "the host has blocked this device", http.StatusForbidden)
		default:
			// Still undecided: a browser gets a page that keeps trying, and
			// scripts get a status they can retry on.
			if r.Method == http.MethodGet && (r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/s/")) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusForbidden)
				waitingPage.Execute(w, map[string]any{"Name": brand.Name, "Device": devices.Nickname(r.UserAgent()), "IP": clientIP(r)})
				return
			}
			w.Header().Set("Retry-After", "3")
			http.Error(w, "waiting for the host to approve this device", http.StatusForbidden)
		}
	})
}

// securityHeaders adds conservative defaults to every response. no-referrer
// keeps an access token in the URL from leaking to other sites.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// noCache makes browsers revalidate embedded assets, so an upgraded binary
// never serves a stale app.js.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// handleIndex serves the single-page app.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Write(s.index)
}

// resolve converts a URL-style path ("/a/b", "a/b", "") into a name relative
// to the served root ("a/b" or "."). Traversal is neutralised by path.Clean
// here and enforced again by os.Root, which also rejects symlink escapes.
func (s *Server) resolve(p string) (string, error) {
	clean := path.Clean("/" + strings.ReplaceAll(p, "\\", "/"))
	name := strings.TrimPrefix(clean, "/")
	if name == "" {
		return ".", nil
	}
	// Refuse dotfile segments unless explicitly allowed. The top-level
	// names of a multi-root tree were chosen on the command line, so they
	// are never hidden.
	if !s.cfg.ShowHidden {
		check := name
		if top, rest, _ := strings.Cut(name, "/"); s.root.isMount(top) {
			check = rest
		}
		if hasHiddenSegment(check) {
			return "", errHidden
		}
	}
	if !fs.ValidPath(name) {
		return "", fs.ErrInvalid
	}
	return name, nil
}

// hasHiddenSegment reports whether any path segment starts with a dot.
func hasHiddenSegment(name string) bool {
	// Inspect each segment; "." itself never reaches here.
	for seg := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// isHiddenName reports whether a single entry name is a dotfile.
func isHiddenName(n string) bool { return strings.HasPrefix(n, ".") }

// hidden reports whether entry n of root-relative folder dir is left out
// of listings: dotfiles are, unless --hidden, except for the top-level
// names of a multi-root tree.
func (s *Server) hidden(dir, n string) bool {
	return !s.cfg.ShowHidden && isHiddenName(n) && !(dir == "." && s.root.isMount(n))
}

// clientIP extracts the remote IP without the port (and without the IPv6
// "::ffff:" mapping prefix).
func clientIP(r *http.Request) string {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ap.Addr().Unmap().String()
}

// httpError maps filesystem errors onto status codes without leaking paths.
func httpError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errHidden), errors.Is(err, fs.ErrNotExist), errors.Is(err, errNotDir):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, fs.ErrPermission):
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, fs.ErrInvalid):
		http.Error(w, "bad path", http.StatusBadRequest)
	case errors.Is(err, errNotArchive):
		http.Error(w, "this archive can't be read", http.StatusUnprocessableEntity)
	default:
		// os.Root reports escapes ("path escapes from parent") as a plain
		// error; treat anything unexpected as not found.
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// pigeonHTML renders brand.PigeonArt as escaped text with one <span> per run
// of equally-masked cells, so the web splash colours the bird exactly like
// the terminal splash does (classes pg-e, pg-k, pg-n, pg-w, pg-f).
func pigeonHTML() string {
	var sb strings.Builder
	// Walk the art line by line, grouping cells that share a mask class.
	for y, line := range brand.PigeonArt {
		if y > 0 {
			sb.WriteByte('\n')
		}
		var mask []rune
		if y < len(brand.PigeonMask) {
			mask = []rune(brand.PigeonMask[y])
		}
		cur := rune(-1)
		for x, r := range []rune(line) {
			m := ' '
			if x < len(mask) {
				m = mask[x]
			}
			// Spaces inherit the current run so they don't split spans.
			if r == ' ' {
				m = cur
			}
			if m != cur {
				if cur != -1 && cur != ' ' {
					sb.WriteString("</span>")
				}
				if m != ' ' && m != -1 {
					sb.WriteString(`<span class="pg-` + string(m) + `">`)
				}
				cur = m
			}
			sb.WriteString(html.EscapeString(string(r)))
		}
		// Close any span left open at the end of the line.
		if cur != -1 && cur != ' ' {
			sb.WriteString("</span>")
		}
	}
	return sb.String()
}
