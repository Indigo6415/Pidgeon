package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Indigo6415/pidgeon/internal/listeners"
	"github.com/Indigo6415/pidgeon/internal/stats"
)

// The inbox (--inbox <dir>) is the one place visitors can write to. Each
// upload streams into "<name>.part" inside it and is renamed once complete,
// so a half-sent file never looks finished. Names are reduced to a single
// safe path segment and never overwrite: a clash becomes "name (1).ext".
// All writes go through an os.Root, like reads, so nothing escapes the inbox.
// File-request links upload into a subfolder of it; --inbox-sort date files
// uploads into YYYY/MM folders by when they were taken; exact copies of a
// file the inbox already holds are dropped (see dedupe.go).

// openInbox prepares the upload directory, creating it if needed.
func (s *Server) openInbox(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return fmt.Errorf("cannot create inbox %s: %w", abs, err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return fmt.Errorf("cannot open inbox %s: %w", abs, err)
	}
	s.inbox = root
	s.cfg.Inbox = abs
	// An inbox inside the served folder can also be browsed (and watched
	// live), unless it is hidden from listings.
	if rel, ok := s.root.rel(abs); ok {
		if _, err := s.resolve(rel); err == nil {
			s.inboxRel = rel
		}
	}
	return nil
}

// inboxPath is the inbox as a browsable path ("/", "/Inbox"), or "" when
// it isn't inside the served folder.
func (s *Server) inboxPath() string {
	switch s.inboxRel {
	case "":
		return ""
	case ".":
		return "/"
	}
	return "/" + s.inboxRel
}

// within reports whether target is dir or inside it, and returns its
// slash-separated path relative to dir. Symlinks are resolved first, so
// "/tmp/x" and "/private/tmp/x" compare equal on macOS.
func within(dir, target string) (string, bool) {
	if d, err := filepath.EvalSymlinks(dir); err == nil {
		dir = d
	}
	if t, err := filepath.EvalSymlinks(target); err == nil {
		target = t
	}
	rel, err := filepath.Rel(dir, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// uploadTarget says where an upload goes inside the inbox.
type uploadTarget struct {
	dir   string // inbox-relative folder ("" = the inbox itself)
	share string // the file-request link it came through, if any
}

// uploadResult is what receive reports back to its caller.
type uploadResult struct {
	ok        bool   // the file arrived in full
	duplicate bool   // the inbox already had these exact bytes; nothing was kept
	name      string // inbox-relative path it was saved as (or of the copy)
}

// handleUpload receives one file: PUT /api/upload?name=<file name> with the
// raw file as the body (&mtime=<Unix ms> keeps its modification time). With
// --ask the operator approves it first.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	s.receive(w, r, uploadTarget{})
}

// receive stores one uploaded file under tgt and writes the response. The
// body streams into "<name>.part" and is hashed as it arrives; an exact copy
// of a file already in the target's folder tree is dropped, and otherwise
// the file is renamed into place (into a YYYY/MM folder with
// --inbox-sort date).
func (s *Server) receive(w http.ResponseWriter, r *http.Request, tgt uploadTarget) uploadResult {
	if s.inbox == nil {
		http.Error(w, "uploads are not enabled on this server", http.StatusNotFound)
		return uploadResult{}
	}
	q := r.URL.Query()
	name, err := cleanUploadName(q.Get("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return uploadResult{}
	}
	size := r.ContentLength
	if s.cfg.InboxMax > 0 && size > s.cfg.InboxMax {
		http.Error(w, "this file is larger than the server accepts", http.StatusRequestEntityTooLarge)
		return uploadResult{}
	}
	ip := clientIP(r)
	// --ask: hold the upload until the operator accepts or declines it.
	if reg := s.cfg.Devices; reg != nil && !reg.AskUpload(r.Context(), ip, r.UserAgent(), name, size) {
		http.Error(w, "the host declined this upload", http.StatusForbidden)
		return uploadResult{}
	}

	held, f, err := s.reserveUpload(tgt.dir, name)
	if err != nil {
		http.Error(w, "couldn't create the file", http.StatusInternalServerError)
		return uploadResult{}
	}
	defer s.releaseUpload(held)
	part := held + ".part"

	t := s.cfg.Store.Begin(stats.TransferInfo{
		Listener: listeners.IDFromContext(r.Context()),
		Client:   ip,
		Name:     s.uploadDisplay(held),
		Kind:     stats.KindUpload,
		Total:    size,
		Share:    tgt.share,
	})
	var body io.Reader = r.Body
	if s.cfg.InboxMax > 0 {
		body = http.MaxBytesReader(w, r.Body, s.cfg.InboxMax)
	}
	h := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(f, h), &stats.CountingReader{R: body, T: t, Store: s.cfg.Store}, make([]byte, 256<<10))
	// A short body means the sender went away mid-file.
	if err == nil && size >= 0 && n != size {
		err = io.ErrUnexpectedEOF
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if s.cfg.Devices != nil {
		s.cfg.Devices.AddTraffic(ip, 0, n)
	}
	if err != nil {
		s.inbox.Remove(part)
		s.cfg.Store.End(t, stats.Aborted)
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			http.Error(w, "this file is larger than the server accepts", http.StatusRequestEntityTooLarge)
		case errors.Is(err, stats.ErrCancelled):
			http.Error(w, "the host cancelled this upload", http.StatusGone)
		default:
			http.Error(w, "upload failed", http.StatusBadRequest)
		}
		return uploadResult{}
	}

	// The same bytes are already there: keep the inbox free of copies.
	sum := hex.EncodeToString(h.Sum(nil))
	if dup, ok := s.findCopy(scopeDir(tgt.dir), n, sum); ok {
		s.inbox.Remove(part)
		s.cfg.Store.End(t, stats.Completed)
		writeJSON(w, http.StatusOK, map[string]any{"name": dup, "size": n, "duplicate": true})
		return uploadResult{ok: true, duplicate: true, name: dup}
	}

	mtime := clientTime(q.Get("mtime"))
	dest := tgt.dir
	if s.cfg.InboxSort == "date" {
		dest = path.Join(tgt.dir, s.dateFolder(part, n, mtime))
	}
	final, err := s.finishUpload(part, dest, name)
	if err != nil {
		s.inbox.Remove(part)
		s.cfg.Store.End(t, stats.Aborted)
		http.Error(w, "couldn't save the file", http.StatusInternalServerError)
		return uploadResult{}
	}
	// Keep the sender's modification time, like a copy would.
	if !mtime.IsZero() {
		s.inbox.Chtimes(final, time.Now(), mtime)
	}
	if fi, err := s.inbox.Stat(final); err == nil {
		s.inboxSeen.remember(s.inboxSeen.sums, inboxKey(final, fi), sum)
	}
	s.cfg.Store.End(t, stats.Completed)
	out := map[string]any{"name": final, "size": n, "sha256": sum}
	if p := s.inboxPath(); p != "" {
		out["folder"] = path.Dir(path.Join(p, final))
	}
	writeJSON(w, http.StatusOK, out)
	return uploadResult{ok: true, name: final}
}

// clientTime parses the sender's modification time (Unix ms); the zero
// time when it is missing or implausible.
func clientTime(v string) time.Time {
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}
	}
	t := time.UnixMilli(ms)
	if !plausible(t) {
		return time.Time{}
	}
	return t
}

// dateFolder picks "YYYY/MM" for a received file: when the photo or video
// was taken, else the sender's modification time, else today.
func (s *Server) dateFolder(part string, size int64, mtime time.Time) string {
	when := time.Now()
	if !mtime.IsZero() {
		when = mtime
	}
	if f, err := s.inbox.Open(part); err == nil {
		if t, ok := mediaDate(f, size); ok {
			when = t
		}
		f.Close()
	}
	return when.Format("2006/01")
}

// uploadDisplay names an upload for the dashboard: its served path when the
// inbox is browsable, otherwise "inbox/<name>".
func (s *Server) uploadDisplay(name string) string {
	if p := s.inboxPath(); p != "" {
		return path.Join(p, name)
	}
	return "inbox/" + name
}

// reserveUpload picks a free name based on name in the inbox folder dir and
// creates its ".part" file. The returned inbox-relative name stays reserved
// until releaseUpload, so two uploads of the same file at once get
// different names.
func (s *Server) reserveUpload(dir, name string) (string, *os.File, error) {
	if dir != "" {
		if err := s.inbox.MkdirAll(dir, 0o755); err != nil {
			return "", nil, err
		}
	}
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	// Try "name", "name (1)", "name (2)", ... until one is free.
	for i := range 1000 {
		cand := path.Join(dir, numbered(name, i))
		if s.uploading[cand] || s.inboxHas(cand) || s.inboxHas(cand+".part") {
			continue
		}
		f, err := s.inbox.OpenFile(cand+".part", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		s.uploading[cand] = true
		return cand, f, nil
	}
	return "", nil, errors.New("too many files with this name")
}

// finishUpload renames a completed ".part" file into folder dir as name.
// If that name is taken (or reserved by another upload), the next free
// number is used instead. It returns the inbox-relative path.
func (s *Server) finishUpload(part, dir, name string) (string, error) {
	if dir != "" {
		if err := s.inbox.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	// Never replace a file that is already there.
	for i := range 1001 {
		cand := path.Join(dir, numbered(name, i))
		// This upload's own reservation is free to take; others' aren't.
		own := cand+".part" == part
		if s.inboxHas(cand) || (!own && s.uploading[cand]) {
			continue
		}
		return cand, s.inbox.Rename(part, cand)
	}
	return "", errors.New("too many files with this name")
}

// releaseUpload frees a reserved name.
func (s *Server) releaseUpload(name string) {
	s.uploadMu.Lock()
	delete(s.uploading, name)
	s.uploadMu.Unlock()
}

// inboxHas reports whether a name exists in the inbox.
func (s *Server) inboxHas(name string) bool {
	_, err := s.inbox.Lstat(name)
	return err == nil
}

// numbered returns name for i == 0, else "base (i).ext".
func numbered(name string, i int) string {
	if i == 0 {
		return name
	}
	ext := path.Ext(name)
	// A leading dot isn't an extension ("(1).bashrc" would look odd).
	if ext == name {
		ext = ""
	}
	return fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), i, ext)
}

// maxUploadName bounds a stored name; most filesystems allow 255 bytes.
const maxUploadName = 200

// cleanUploadName reduces a client-supplied file name to one safe path
// segment: no directories, no control or reserved characters, no leading
// dot (so uploads can't create hidden files), and a sane length.
func cleanUploadName(raw string) (string, error) {
	if !utf8.ValidString(raw) {
		return "", errors.New("the file name isn't valid UTF-8")
	}
	n := path.Base(strings.ReplaceAll(raw, "\\", "/"))
	n = strings.Map(func(r rune) rune {
		// Control characters and the characters Windows reserves.
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, n)
	n = strings.TrimLeft(strings.TrimSpace(n), ".")
	n = strings.TrimRight(n, ". ") // Windows drops trailing dots and spaces
	if n == "" || n == "/" {
		return "", errors.New("missing file name")
	}
	// Shorten long names but keep the extension.
	if len(n) > maxUploadName {
		ext := path.Ext(n)
		if len(ext) > 20 {
			ext = ""
		}
		stem := n[:len(n)-len(ext)]
		cut := maxUploadName - len(ext)
		// Back up to a rune boundary.
		for cut > 0 && !utf8.RuneStart(stem[cut]) {
			cut--
		}
		n = stem[:cut] + ext
	}
	return n, nil
}
