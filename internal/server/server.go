// Package server implements the read-only HTTP file server: directory
// listing API, streamed file downloads with Range support, on-the-fly ZIP
// archives, a live progress feed for the web UI, and the embedded web app.
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
	"github.com/Indigo6415/pidgeon/internal/listeners"
	"github.com/Indigo6415/pidgeon/internal/stats"
)

// Config configures a Server.
type Config struct {
	Dir        string // directory to serve (made absolute by New)
	ShowHidden bool   // serve dotfiles and dot-directories
	Token      string // optional access token (query / cookie / bearer)
	BasicUser  string // optional HTTP basic auth user
	BasicPass  string
	Store      *stats.Store
	Assets     fs.FS // web UI assets (index.html, app.js, app.css, ...)
}

// Server is the http.Handler shared by every listener.
type Server struct {
	cfg     Config
	root    *os.Root // all file access goes through this: no path escapes
	fsys    fs.FS    // root.FS(), used for directory walks
	name    string   // display name of the served directory
	index   []byte   // index.html with the product name filled in
	handler http.Handler

	zipMu sync.Mutex
	zips  map[string]*zipJob

	done      chan struct{} // closed by Close to end long-lived SSE streams
	closeOnce sync.Once
}

// errHidden is returned for dotfile paths when --hidden isn't set. It is
// reported to clients as a plain 404 so hidden names aren't confirmed.
var errHidden = errors.New("hidden path")

// New opens the served directory and builds the handler.
func New(cfg Config) (*Server, error) {
	abs, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, err
	}
	cfg.Dir = abs
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("cannot serve %s: %w", abs, err)
	}
	idx, err := fs.ReadFile(cfg.Assets, "index.html")
	if err != nil {
		return nil, fmt.Errorf("web assets missing index.html: %w", err)
	}
	s := &Server{
		cfg:  cfg,
		root: root,
		fsys: root.FS(),
		name: filepath.Base(abs),
		index: []byte(strings.NewReplacer(
			"{{NAME}}", html.EscapeString(brand.Name),
			"{{TAGLINE}}", html.EscapeString(brand.Tagline),
			"{{PIGEON}}", pigeonHTML(),
		).Replace(string(idx))),
		zips: map[string]*zipJob{},
		done: make(chan struct{}),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", noCache(http.FileServerFS(cfg.Assets))))
	mux.HandleFunc("GET /api/info", s.handleInfo)
	mux.HandleFunc("GET /api/ls", s.handleList)
	mux.HandleFunc("GET /api/peek", s.handlePeek)
	mux.HandleFunc("GET /api/size", s.handleSize)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/zip", s.handleZipCreate)
	mux.HandleFunc("GET /zip/{token}/{file}", s.handleZipStream)
	mux.HandleFunc("GET /f/{path...}", s.handleFile)

	// Order matters: instrument is outermost so rejected (401) requests are
	// still counted and logged.
	s.handler = s.instrument(securityHeaders(s.auth(mux)))
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// Dir returns the absolute directory being served.
func (s *Server) Dir() string { return s.cfg.Dir }

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
	// Refuse dotfile segments unless explicitly allowed.
	if !s.cfg.ShowHidden && hasHiddenSegment(name) {
		return "", errHidden
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
	case errors.Is(err, errHidden), errors.Is(err, fs.ErrNotExist):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, fs.ErrPermission):
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, fs.ErrInvalid):
		http.Error(w, "bad path", http.StatusBadRequest)
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
