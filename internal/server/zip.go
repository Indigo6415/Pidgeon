package server

import (
	"archive/zip"
	"compress/flate"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/Indigo6415/pidgeon/internal/brand"
	"github.com/Indigo6415/pidgeon/internal/listeners"
	"github.com/Indigo6415/pidgeon/internal/stats"
)

// zipJobTTL is how long a prepared archive link stays valid.
const zipJobTTL = time.Hour

// zipJob is a prepared selection. The browser POSTs the selection (which can
// be long) and then downloads a short GET URL, so the download can be a
// normal browser download that streams to disk.
type zipJob struct {
	base    string   // root-relative directory the selection was made in
	names   []string // root-relative selected paths
	file    string   // archive file name
	created time.Time
}

// zipRequest is the /api/zip request body.
type zipRequest struct {
	Base  string   `json:"base"`
	Paths []string `json:"paths"`
}

// handleZipCreate validates a selection and returns a download URL for it.
func (s *Server) handleZipCreate(w http.ResponseWriter, r *http.Request) {
	var req zipRequest
	// Cap the body: even thousands of names fit comfortably in 4 MiB.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil || len(req.Paths) == 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	base, err := s.resolve(req.Base)
	if err != nil {
		httpError(w, err)
		return
	}
	job := &zipJob{base: base, created: time.Now()}
	seen := map[string]bool{}
	// Validate every selected path: it must exist and live under base.
	for _, p := range req.Paths {
		name, err := s.resolve(path.Join("/", base, p))
		if err != nil {
			httpError(w, err)
			return
		}
		if name == base || !isUnder(name, base) {
			http.Error(w, "selection outside folder", http.StatusBadRequest)
			return
		}
		if _, err := s.root.Stat(name); err != nil {
			httpError(w, err)
			return
		}
		// De-duplicate so an item isn't archived twice.
		if !seen[name] {
			seen[name] = true
			job.names = append(job.names, name)
		}
	}
	job.file = s.zipName(job)

	token := randomToken()
	s.zipMu.Lock()
	// Opportunistically expire old jobs; there are never many.
	for k, j := range s.zips {
		if time.Since(j.created) > zipJobTTL {
			delete(s.zips, k)
		}
	}
	s.zips[token] = job
	s.zipMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]string{
		"url":  "/zip/" + token + "/" + escapePath(job.file),
		"name": job.file,
	})
}

// zipName picks a sensible archive name: the item's own name for a single
// selection, otherwise the containing folder's name.
func (s *Server) zipName(j *zipJob) string {
	if len(j.names) == 1 {
		return path.Base(j.names[0]) + ".zip"
	}
	if j.base == "." {
		return s.name + ".zip"
	}
	return path.Base(j.base) + ".zip"
}

// handleZipStream writes the archive directly to the response as it is
// built. Nothing is buffered beyond one copy buffer and the deflate window;
// the central directory (a few dozen bytes per file) is the only thing held
// until the end. Sizes aren't known up front, so the response is chunked and
// the browser shows bytes received rather than a percentage.
func (s *Server) handleZipStream(w http.ResponseWriter, r *http.Request) {
	s.zipMu.Lock()
	job, ok := s.zips[r.PathValue("token")]
	s.zipMu.Unlock()
	// Unknown or expired link.
	if !ok || time.Since(job.created) > zipJobTTL {
		http.Error(w, "this download link has expired; select the files again", http.StatusGone)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": job.file}))
	h.Set("Cache-Control", "no-store")

	display := "/" + job.file
	if job.base != "." {
		display = "/" + path.Join(job.base, job.file)
	}
	t := s.cfg.Store.Begin(stats.TransferInfo{
		DLID:     validDLID(r.URL.Query().Get("dl")),
		Listener: listeners.IDFromContext(r.Context()),
		Client:   clientIP(r),
		Name:     display,
		Kind:     stats.KindZip,
		Total:    -1,
	})
	cw := stats.NewCountingWriter(w, t)

	if err := s.writeZip(r, cw, t, job); err != nil {
		s.cfg.Store.End(t, stats.Aborted)
		// The 200 header is long gone, so the only honest signal left is to
		// cut the connection: the browser marks the download as failed
		// instead of saving a truncated archive that looks complete.
		panic(http.ErrAbortHandler)
	}
	s.cfg.Store.End(t, stats.Completed)
}

// writeZip walks the selection and streams every entry into w.
func (s *Server) writeZip(r *http.Request, w io.Writer, t *stats.Transfer, job *zipJob) error {
	zw := zip.NewWriter(w)
	// Favour throughput over ratio: on a LAN the network, not the archive
	// size, is what the user waits for.
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestSpeed)
	})
	ctx := r.Context()
	buf := make([]byte, 256<<10)
	var skipped []string

	// Walk each selected item; entry names are relative to the folder the
	// selection was made in, so the archive extracts with the tree intact.
	for _, top := range job.names {
		err := fs.WalkDir(s.fsys, top, func(p string, d fs.DirEntry, err error) error {
			// Unreadable entries are noted and skipped, not fatal.
			if err != nil {
				skipped = append(skipped, p+": "+err.Error())
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			// Stop promptly if the client left or the operator cancelled.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if t.Cancelled() {
				return stats.ErrCancelled
			}
			if !s.cfg.ShowHidden && p != top && isHiddenName(d.Name()) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			rel := relTo(job.base, p)
			fi, err := fs.Stat(s.fsys, p) // follows in-root symlinks only
			if err != nil {
				skipped = append(skipped, p+": "+err.Error())
				return nil
			}
			switch {
			case d.IsDir():
				// Explicit directory entries keep empty folders.
				_, err := zw.CreateHeader(&zip.FileHeader{Name: rel + "/", Method: zip.Store, Modified: fi.ModTime()})
				return err
			case fi.IsDir():
				// A symlink to a folder: not followed, to avoid cycles.
				skipped = append(skipped, p+": symlinked folder not followed")
				return nil
			case !fi.Mode().IsRegular():
				return nil
			}
			return s.zipFile(zw, p, rel, fi, buf, &skipped)
		})
		if err != nil {
			return err
		}
	}

	// Tell the recipient about anything that couldn't be included rather
	// than silently producing an incomplete tree.
	if len(skipped) > 0 {
		fw, err := zw.Create(brand.Name + "-skipped-files.txt")
		if err != nil {
			return err
		}
		fmt.Fprintf(fw, "%d item(s) could not be included:\n\n%s\n", len(skipped), strings.Join(skipped, "\n"))
	}
	return zw.Close()
}

// zipFile adds one regular file to the archive.
func (s *Server) zipFile(zw *zip.Writer, p, rel string, fi fs.FileInfo, buf []byte, skipped *[]string) error {
	f, err := s.fsys.Open(p)
	// Open before writing the header so an unreadable file is just skipped.
	if err != nil {
		*skipped = append(*skipped, p+": "+err.Error())
		return nil
	}
	defer f.Close()
	hdr, err := zip.FileInfoHeader(fi)
	if err != nil {
		return err
	}
	hdr.Name = rel
	hdr.Method = zip.Deflate
	// Already-compressed formats gain nothing from deflate; store them.
	if isCompressed(rel) {
		hdr.Method = zip.Store
	}
	fw, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	// A read error mid-file would leave a corrupt entry, so it is fatal.
	_, err = io.CopyBuffer(fw, onlyReader{f}, buf)
	return err
}

// onlyReader hides WriterTo on *os.File so io.CopyBuffer uses our buffer.
type onlyReader struct{ io.Reader }

// compressedExt lists formats that are already compressed.
var compressedExt = map[string]bool{}

func init() {
	// Populate once; keeping it as a string is easier to edit.
	for e := range strings.FieldsSeq(`jpg jpeg png gif webp heic heif avif jxl mp4 m4v mov mkv webm avi
		wmv flv mp3 m4a aac ogg oga opus flac wma zip gz tgz bz2 xz zst 7z rar lz4 jar apk ipa dmg iso
		docx xlsx pptx odt ods odp epub woff woff2 pdf`) {
		compressedExt["."+e] = true
	}
}

// isCompressed reports whether a file's extension marks it as compressed.
func isCompressed(name string) bool { return compressedExt[strings.ToLower(path.Ext(name))] }

// relTo returns p relative to base (both root-relative, "." is the root).
func relTo(base, p string) string {
	if base == "." {
		return p
	}
	return strings.TrimPrefix(p, base+"/")
}

// isUnder reports whether name is strictly inside dir (root-relative).
func isUnder(name, dir string) bool {
	if dir == "." {
		return name != "."
	}
	return strings.HasPrefix(name, dir+"/")
}

// randomToken returns a 128-bit unguessable hex token.
func randomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(errors.New("crypto/rand unavailable: " + err.Error()))
	}
	return hex.EncodeToString(b)
}
