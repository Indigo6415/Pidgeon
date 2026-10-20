package server

import (
	"archive/zip"
	"compress/flate"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Indigo6415/pidgeon/internal/brand"
	"github.com/Indigo6415/pidgeon/internal/listeners"
	"github.com/Indigo6415/pidgeon/internal/stats"
)

// zipJobTTL is how long a prepared archive link stays valid.
const zipJobTTL = time.Hour

// zipItem is one entry of a planned archive.
type zipItem struct {
	src   string // root-relative source file; "" for generated content
	name  string // name inside the archive; folders end in "/"
	dir   bool
	size  int64
	mode  fs.FileMode
	mtime time.Time
	data  []byte // generated content (the skipped-files note)

	// Stored archives remember each file's CRC-32 once it has been read, so
	// a resumed download can skip the part already received without
	// reading those files again. Guarded by zipJob.mu.
	crc   uint32
	crcOK bool
}

// zipJob is a prepared selection. The browser POSTs the selection (which can
// be long) and then downloads a short GET URL, so the download can be a
// normal browser download that streams to disk.
//
// The selection is walked once, up front, into a plan (items). When every
// entry is stored rather than deflated, the archive's exact size follows
// from the plan, so the download gets a Content-Length (a real progress bar)
// and supports Range requests (resume after a dropped connection).
type zipJob struct {
	base    string   // root-relative directory the selection was made in
	names   []string // root-relative selected paths
	file    string   // archive file name
	created time.Time

	items   []*zipItem
	skipped []string // entries that couldn't be planned, reported in the archive
	store   bool     // every entry is stored: size is exact and ranges work
	size    int64    // exact archive size when store, else -1

	mu sync.Mutex // guards the CRC cache in items
}

// zipRequest is the /api/zip request body.
type zipRequest struct {
	Base  string   `json:"base"`
	Paths []string `json:"paths"`
	Store bool     `json:"store"` // never compress, so the size is known up front
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
	var names []string
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
			names = append(names, name)
		}
	}
	job, err := s.newZipJob(r.Context(), base, names, req.Store)
	if err != nil {
		http.Error(w, "couldn't prepare the archive", http.StatusInternalServerError)
		return
	}
	token := s.addZipJob(job)
	writeJSON(w, http.StatusOK, map[string]any{
		"url":   "/zip/" + token + "/" + escapePath(job.file),
		"name":  job.file,
		"size":  job.size,
		"store": job.store,
	})
}

// newZipJob plans an archive of names (root-relative, all under base).
// The archive is stored (exact size, resumable) when forceStore is set or
// when every file is already compressed and deflate would gain nothing.
func (s *Server) newZipJob(ctx context.Context, base string, names []string, forceStore bool) (*zipJob, error) {
	job := &zipJob{base: base, names: names, created: time.Now(), size: -1}
	job.file = s.zipName(job)
	if err := s.planZip(ctx, job); err != nil {
		return nil, err
	}
	job.store = forceStore
	// Automatic store mode: nothing in the plan would be deflated anyway.
	if !forceStore {
		job.store = true
		for _, it := range job.items {
			if !it.dir && !isCompressed(it.name) {
				job.store = false
				break
			}
		}
	}
	if !job.store {
		return job, nil
	}
	// In a stored archive the skipped-files note is a planned entry too, so
	// it is part of the computed size.
	if len(job.skipped) > 0 {
		note := skippedNote(job.skipped)
		job.items = append(job.items, &zipItem{
			name: brand.Name + "-skipped-files.txt", size: int64(len(note)), mode: 0o644,
			mtime: time.Now(), data: note, crc: crc32.ChecksumIEEE(note), crcOK: true,
		})
	}
	// The size is measured by writing the archive for real into a counter,
	// with zeros standing in for file contents: stored entries are as long
	// as their data whatever it holds, and this keeps the size exactly in
	// step with archive/zip's own layout (ZIP64 records included).
	var cnt countingDiscard
	if err := s.writeStoredZip(ctx, &cnt, nil, job, 0, -1, true); err != nil {
		return nil, err
	}
	job.size = cnt.n
	return job, nil
}

// addZipJob registers a job and returns its URL token.
func (s *Server) addZipJob(job *zipJob) string {
	token := randomToken()
	s.zipMu.Lock()
	defer s.zipMu.Unlock()
	// Opportunistically expire old jobs; there are never many.
	for k, j := range s.zips {
		if time.Since(j.created) > zipJobTTL {
			delete(s.zips, k)
		}
	}
	s.zips[token] = job
	return token
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

// planZip walks the selection into job.items. Entry names are relative to
// the folder the selection was made in, so the archive extracts with the
// tree intact; empty folders get explicit entries.
func (s *Server) planZip(ctx context.Context, job *zipJob) error {
	// Walk each selected item in turn.
	for _, top := range job.names {
		err := fs.WalkDir(s.fsys, top, func(p string, d fs.DirEntry, err error) error {
			// Unreadable entries are noted and skipped, not fatal.
			if err != nil {
				job.skipped = append(job.skipped, p+": "+err.Error())
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
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
				job.skipped = append(job.skipped, p+": "+err.Error())
				return nil
			}
			switch {
			case d.IsDir():
				job.items = append(job.items, &zipItem{name: rel + "/", dir: true, mode: fs.ModeDir | 0o755, mtime: fi.ModTime()})
			case fi.IsDir():
				// A symlink to a folder: not followed, to avoid cycles.
				job.skipped = append(job.skipped, p+": symlinked folder not followed")
			case fi.Mode().IsRegular():
				job.items = append(job.items, &zipItem{src: p, name: rel, size: fi.Size(), mode: fi.Mode().Perm(), mtime: fi.ModTime()})
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// handleZipStream serves a prepared archive.
func (s *Server) handleZipStream(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	s.zipMu.Lock()
	job, ok := s.zips[token]
	s.zipMu.Unlock()
	// Unknown or expired link.
	if !ok || time.Since(job.created) > zipJobTTL {
		http.Error(w, "this download link has expired; select the files again", http.StatusGone)
		return
	}
	display := "/" + job.file
	if job.base != "." {
		display = "/" + path.Join(job.base, job.file)
	}
	s.serveZip(w, r, job, token, display, "")
}

// serveZip writes an archive directly to the response as it is built.
// Nothing is buffered beyond one copy buffer and the deflate window; the
// central directory (a few dozen bytes per file) is the only thing held
// until the end. Stored archives have an exact Content-Length and honour a
// single Range (with If-Range against the job's ETag), so browsers can
// resume them; deflated ones are streamed chunked, with no known size.
func (s *Server) serveZip(w http.ResponseWriter, r *http.Request, job *zipJob, token, display, share string) {
	if s.overBudget() {
		http.Error(w, "this server has reached its data limit", http.StatusServiceUnavailable)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": job.file}))
	h.Set("Cache-Control", "no-cache")

	status := http.StatusOK
	start, end := int64(0), int64(-1)
	total := int64(-1)
	if job.store {
		etag := `"pz-` + token + `"`
		h.Set("ETag", etag)
		h.Set("Accept-Ranges", "bytes")
		end = job.size - 1
		// A Range is honoured unless If-Range says the client's copy is of
		// a different archive.
		if rh := r.Header.Get("Range"); rh != "" {
			if ir := r.Header.Get("If-Range"); ir == "" || ir == etag {
				switch a, b, k := parseRange(rh, job.size); k {
				case rangePartial:
					start, end, status = a, b, http.StatusPartialContent
					h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, job.size))
				case rangeUnsatisfiable:
					h.Set("Content-Range", fmt.Sprintf("bytes */%d", job.size))
					http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
					return
				}
			}
		}
		total = end - start + 1
		h.Set("Content-Length", strconv.FormatInt(total, 10))
	}
	// HEAD only asks about the archive; don't build it.
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}

	t := s.cfg.Store.Begin(stats.TransferInfo{
		DLID:     validDLID(r.URL.Query().Get("dl")),
		Listener: listeners.IDFromContext(r.Context()),
		Client:   clientIP(r),
		Name:     display,
		Kind:     stats.KindZip,
		Total:    total,
		Share:    share,
	})
	cw := stats.NewCountingWriter(w, t)
	cw.WriteHeader(status)

	var err error
	if job.store {
		err = s.writeStoredZip(r.Context(), &rangeWriter{w: cw, start: start, end: end}, t, job, start, end, false)
		// The requested range is complete; the rest of the archive isn't wanted.
		if errors.Is(err, errRangeDone) {
			err = nil
		}
	} else {
		err = s.writeDeflateZip(r.Context(), cw, t, job)
	}
	if err != nil {
		s.cfg.Store.End(t, stats.Aborted)
		// The 200 header is long gone, so the only honest signal left is to
		// cut the connection: the browser marks the download as failed
		// instead of saving a truncated archive that looks complete.
		panic(http.ErrAbortHandler)
	}
	// A range that reaches the end finishes a (possibly resumed) download.
	outcome := stats.Completed
	if job.store && end != job.size-1 {
		outcome = stats.PartialDone
	}
	s.cfg.Store.End(t, outcome)
}

// writeStoredZip writes a stored archive from the plan. Every file entry is
// written raw with its size known in advance and its CRC-32 in a trailing
// data descriptor, so the layout depends only on the plan.
//
// skipBefore/stopAfter describe the byte range the caller will keep (the
// writer discards the rest); they only let this skip work: files wholly
// before the range whose CRC is already known aren't read again, and the
// walk stops once the range is passed. With dry set, contents are replaced
// by zeros and no file is opened: that measures the archive's size.
func (s *Server) writeStoredZip(ctx context.Context, out io.Writer, t *stats.Transfer, job *zipJob, skipBefore, stopAfter int64, dry bool) error {
	pw := &posWriter{w: out}
	zw := zip.NewWriter(pw)
	buf := make([]byte, 256<<10)
	zeros := make([]byte, 256<<10)
	// One entry at a time, in plan order.
	for _, it := range job.items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if t != nil && t.Cancelled() {
			return stats.ErrCancelled
		}
		fh := it.header()
		if it.dir {
			if _, err := zw.CreateRaw(fh); err != nil {
				return err
			}
			continue
		}
		fh.Flags |= 0x8 // CRC and sizes follow the data in a descriptor
		fh.CompressedSize64 = uint64(it.size)
		fh.UncompressedSize64 = uint64(it.size)
		fw, err := zw.CreateRaw(fh)
		if err != nil {
			return err
		}
		// Flush so pw.n is exactly where this entry's data begins.
		if err := zw.Flush(); err != nil {
			return err
		}
		dataStart := pw.n
		if stopAfter >= 0 && dataStart > stopAfter {
			return errRangeDone
		}
		job.mu.Lock()
		crc, known := it.crc, it.crcOK
		job.mu.Unlock()
		switch {
		case dry:
			err = writeZeros(fw, it.size, zeros)
		case it.data != nil:
			_, err = fw.Write(it.data)
		case known && dataStart+it.size <= skipBefore:
			// Already delivered before a resume: the bytes are discarded
			// anyway, so stand-ins of the right length will do.
			err = writeZeros(fw, it.size, zeros)
		default:
			crc, err = s.copyStored(fw, it, buf)
			if err == nil {
				job.mu.Lock()
				it.crc, it.crcOK = crc, true
				job.mu.Unlock()
			}
		}
		if err != nil {
			return err
		}
		// The descriptor is written on the next Create (or Close) and reads
		// the CRC from the header, so it can be filled in now.
		fh.CRC32 = crc
	}
	return zw.Close()
}

// copyStored copies exactly it.size bytes of a file into w and returns their
// CRC-32. A file that shrank since it was planned can't fill its entry and
// fails the archive; one that grew is cut at the planned size.
func (s *Server) copyStored(w io.Writer, it *zipItem, buf []byte) (uint32, error) {
	f, err := s.fsys.Open(it.src)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	h := crc32.NewIEEE()
	n, err := io.CopyBuffer(io.MultiWriter(w, h), io.LimitReader(onlyReader{f}, it.size), buf)
	if err != nil {
		return 0, err
	}
	if n < it.size {
		return 0, fmt.Errorf("%s changed while it was being archived", it.src)
	}
	return h.Sum32(), nil
}

// writeDeflateZip streams a compressed archive from the plan. Sizes aren't
// known up front, so files are read in full as they are now; a file that
// can't be opened any more is listed in the skipped-files note.
func (s *Server) writeDeflateZip(ctx context.Context, w io.Writer, t *stats.Transfer, job *zipJob) error {
	zw := zip.NewWriter(w)
	// Favour throughput over ratio: on a LAN the network, not the archive
	// size, is what the user waits for.
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestSpeed)
	})
	buf := make([]byte, 256<<10)
	skipped := append([]string(nil), job.skipped...)
	// One entry at a time, in plan order.
	for _, it := range job.items {
		// Stop promptly if the client left or the operator cancelled.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if t.Cancelled() {
			return stats.ErrCancelled
		}
		if it.dir {
			if _, err := zw.CreateHeader(&zip.FileHeader{Name: it.name, Method: zip.Store, Modified: it.mtime}); err != nil {
				return err
			}
			continue
		}
		f, err := s.fsys.Open(it.src)
		// Open before writing the header so an unreadable file is just skipped.
		if err != nil {
			skipped = append(skipped, it.src+": "+err.Error())
			continue
		}
		fh := &zip.FileHeader{Name: it.name, Method: zip.Deflate, Modified: it.mtime}
		fh.SetMode(it.mode)
		// Already-compressed formats gain nothing from deflate; store them.
		if isCompressed(it.name) {
			fh.Method = zip.Store
		}
		fw, err := zw.CreateHeader(fh)
		if err == nil {
			// A read error mid-file would leave a corrupt entry, so it is fatal.
			_, err = io.CopyBuffer(fw, onlyReader{f}, buf)
		}
		f.Close()
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
		fw.Write(skippedNote(skipped))
	}
	return zw.Close()
}

// skippedNote is the text of the skipped-files entry.
func skippedNote(skipped []string) []byte {
	return fmt.Appendf(nil, "%d item(s) could not be included:\n\n%s\n", len(skipped), strings.Join(skipped, "\n"))
}

// header builds the raw-mode header for a stored entry, filling in what
// zip.Writer.CreateHeader would: versions, the UTF-8 flag, the MS-DOS time
// and the extended-timestamp extra field (exact modification time).
func (it *zipItem) header() *zip.FileHeader {
	fh := &zip.FileHeader{Name: it.name, Method: zip.Store, Modified: it.mtime}
	fh.SetMode(it.mode)
	fh.CreatorVersion = fh.CreatorVersion&0xff00 | 20
	fh.ReaderVersion = 20
	if it.size >= 0xffffffff {
		fh.ReaderVersion = 45 // ZIP64
	}
	// Raw entries skip CreateHeader's conversion of Modified, so the legacy
	// MS-DOS fields have to be set by hand.
	fh.ModifiedDate, fh.ModifiedTime = msDosTime(it.mtime)
	fh.Extra = extendedTime(it.mtime)
	if !isASCII(it.name) && utf8.ValidString(it.name) {
		fh.Flags |= 0x800
	}
	return fh
}

// msDosTime converts t to the MS-DOS date and time fields (2-second
// precision, years 1980–2107).
func msDosTime(t time.Time) (date, tm uint16) {
	y := min(max(t.Year(), 1980), 2107)
	date = uint16(t.Day() + int(t.Month())<<5 + (y-1980)<<9)
	tm = uint16(t.Second()/2 + t.Minute()<<5 + t.Hour()<<11)
	return date, tm
}

// extendedTime is the "UT" extra field holding the exact Unix mtime.
func extendedTime(t time.Time) []byte {
	b := make([]byte, 9)
	binary.LittleEndian.PutUint16(b[0:], 0x5455) // extended timestamp ID
	binary.LittleEndian.PutUint16(b[2:], 5)      // size: flags + mtime
	b[4] = 1                                     // flags: mtime present
	binary.LittleEndian.PutUint32(b[5:], uint32(t.Unix()))
	return b
}

// isASCII reports whether s is plain ASCII.
func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// writeZeros writes n zero bytes to w in chunks of zeros.
func writeZeros(w io.Writer, n int64, zeros []byte) error {
	// Whole chunks, then the remainder.
	for n > 0 {
		k := min(n, int64(len(zeros)))
		if _, err := w.Write(zeros[:k]); err != nil {
			return err
		}
		n -= k
	}
	return nil
}

// posWriter forwards writes and counts their position in the stream.
type posWriter struct {
	w io.Writer
	n int64
}

// Write forwards p and advances the position by what was accepted.
func (p *posWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.n += int64(n)
	return n, err
}

// countingDiscard counts bytes and keeps none of them.
type countingDiscard struct{ n int64 }

// Write counts p.
func (c *countingDiscard) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// errRangeDone ends archive generation once the requested range is sent.
var errRangeDone = errors.New("range complete")

// rangeWriter passes through only stream bytes start..end (inclusive) and
// reports errRangeDone once everything up to end has gone by.
type rangeWriter struct {
	w          io.Writer
	start, end int64
	pos        int64
}

// Write keeps the part of p that falls inside the range.
func (r *rangeWriter) Write(p []byte) (int, error) {
	if r.pos > r.end {
		return 0, errRangeDone
	}
	n := len(p)
	lo, hi := r.pos, r.pos+int64(n) // this chunk covers stream bytes [lo, hi)
	r.pos = hi
	a, b := max(lo, r.start), min(hi, r.end+1)
	// Some of this chunk is wanted: send just that slice.
	if a < b {
		if _, err := r.w.Write(p[a-lo : b-lo]); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// Range parse results.
const (
	rangeNone = iota // no usable Range header: send everything
	rangePartial
	rangeUnsatisfiable
)

// parseRange interprets a single-range "bytes=a-b", "bytes=a-" or
// "bytes=-n" header against size. Multi-range and malformed headers are
// treated as absent, which RFC 9110 allows.
func parseRange(h string, size int64) (start, end int64, kind int) {
	spec, ok := strings.CutPrefix(strings.TrimSpace(h), "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, rangeNone
	}
	a, b, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return 0, 0, rangeNone
	}
	switch {
	case a == "":
		// Suffix range: the last n bytes.
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, rangeNone
		}
		if size == 0 {
			return 0, 0, rangeUnsatisfiable
		}
		return max(0, size-n), size - 1, rangePartial
	default:
		s, err := strconv.ParseInt(a, 10, 64)
		if err != nil || s < 0 {
			return 0, 0, rangeNone
		}
		if s >= size {
			return 0, 0, rangeUnsatisfiable
		}
		e := size - 1
		if b != "" {
			v, err := strconv.ParseInt(b, 10, 64)
			if err != nil || v < s {
				return 0, 0, rangeNone
			}
			e = min(v, size-1)
		}
		return s, e, rangePartial
	}
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
