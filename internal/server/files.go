package server

import (
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/Indigo6415/pidgeon/internal/listeners"
	"github.com/Indigo6415/pidgeon/internal/stats"
)

// dlIDPattern bounds the client-supplied download ID used for progress.
var dlIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}\.[0-9]{1,10}$`)

// handleFile streams one file. http.ServeContent provides Content-Type
// (by extension, else sniffing), Content-Length, Last-Modified, conditional
// requests and single/multi Range support, reading through the *os.File so
// the file is never loaded into memory.
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	name, err := s.resolve(r.PathValue("path"))
	if err != nil {
		httpError(w, err)
		return
	}
	s.serveFile(w, r, name, "")
}

// serveFile streams the root-relative file name. share is the share link
// it was reached through, if any.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, name, share string) {
	f, err := s.root.Open(name)
	if err != nil {
		httpError(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		httpError(w, err)
		return
	}
	// Folders are browsed in the app, not downloaded through /f/.
	if fi.IsDir() {
		http.Redirect(w, r, "/#/"+escapePath(name), http.StatusFound)
		return
	}
	// Devices, sockets, FIFOs: never stream these.
	if !fi.Mode().IsRegular() {
		http.Error(w, "not a regular file", http.StatusForbidden)
		return
	}
	// --max-bytes reached: no new downloads, only what's already running.
	if s.overBudget() {
		http.Error(w, "this server has reached its data limit", http.StatusServiceUnavailable)
		return
	}

	inline := r.URL.Query().Get("inline") == "1"
	disp := "attachment"
	if inline {
		disp = "inline"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": fi.Name()}))
	// Anything that could execute script when opened inline is sandboxed
	// into a unique origin, so a served .html file can't act as this app.
	if isActiveContent(fi.Name()) {
		w.Header().Set("Content-Security-Policy", "sandbox")
	}

	// HEAD requests don't move bytes; skip transfer tracking for them.
	if r.Method == http.MethodHead {
		http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
		return
	}

	t := s.cfg.Store.Begin(stats.TransferInfo{
		DLID:     validDLID(r.URL.Query().Get("dl")),
		Listener: listeners.IDFromContext(r.Context()),
		Client:   clientIP(r),
		Name:     "/" + name,
		Kind:     stats.KindFile,
		Total:    fi.Size(),
		Inline:   inline,
		Share:    share,
	})
	cw := stats.NewCountingWriter(w, t)
	http.ServeContent(cw, r, fi.Name(), fi.ModTime(), f)
	s.cfg.Store.End(t, fileOutcome(cw, t, fi.Size()))
}

// overBudget reports whether --max-bytes has been reached.
func (s *Server) overBudget() bool {
	return s.cfg.MaxBytes > 0 && s.cfg.Store.BytesServed() >= s.cfg.MaxBytes
}

// fileOutcome classifies a finished file response for the stats store.
func fileOutcome(cw *stats.CountingWriter, t *stats.Transfer, size int64) stats.Outcome {
	switch {
	case cw.Status != http.StatusOK && cw.Status != http.StatusPartialContent:
		// 304, 416, errors: no body worth showing as a transfer.
		return stats.Discarded
	case t.Cancelled() || t.Sent() < t.Total():
		return stats.Aborted
	case cw.Status == http.StatusOK:
		return stats.Completed
	case rangeReachesEnd(cw.Header().Get("Content-Range"), size):
		// A resumed download that fetched the tail counts as a download.
		return stats.Completed
	}
	return stats.PartialDone
}

// rangeReachesEnd reports whether "bytes a-b/size" ends at the last byte.
func rangeReachesEnd(cr string, size int64) bool {
	rest, ok := strings.CutPrefix(cr, "bytes ")
	if !ok {
		return false
	}
	span, _, ok := strings.Cut(rest, "/")
	if !ok {
		return false
	}
	_, end, ok := strings.Cut(span, "-")
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(end, 10, 64)
	return err == nil && n == size-1
}

// isActiveContent reports whether a file could run script if rendered.
func isActiveContent(name string) bool {
	ct := mime.TypeByExtension(strings.ToLower(path.Ext(name)))
	return strings.Contains(ct, "html") || strings.Contains(ct, "xml") || strings.Contains(ct, "svg") ||
		strings.Contains(ct, "javascript")
}

// validDLID returns id if it is a well-formed client download ID, else "".
func validDLID(id string) string {
	if dlIDPattern.MatchString(id) {
		return id
	}
	return ""
}

// escapePath percent-encodes each segment of a slash-separated path.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	// Encode segments individually so the slashes survive.
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}
