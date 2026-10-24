package server

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Archives (.zip, .tar, .tar.gz / .tgz) are browsed like folders: a path
// such as "backup.zip/photos/a.jpg" names an entry inside an archive, and
// every API that takes a path (listing, download, preview, size, ZIP of a
// selection, share links, hashes, thumbnails) accepts one. Nothing is ever
// extracted to disk. A ZIP's central directory gives random access to each
// entry; a plain .tar is indexed once with the byte offset of every file;
// a .tar.gz has to be decompressed from the start to reach an entry.

// Archive limits: indexing must stay cheap even for a huge or hostile file.
const (
	archiveMaxEntries = 500_000
	archiveScanTime   = 20 * time.Second // a .tar.gz is decompressed to be listed
	archiveCacheSize  = 16               // indexes kept in memory
	archiveIdleClose  = 15 * time.Second // an unused archive's file is closed after this
)

// arcKind is an archive format.
type arcKind int

const (
	arcNone arcKind = iota
	arcZip
	arcTar
	arcTarGz
)

// archiveKind classifies a file name by extension.
func archiveKind(name string) arcKind {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".zip"):
		return arcZip
	case strings.HasSuffix(n, ".tar"):
		return arcTar
	case strings.HasSuffix(n, ".tar.gz"), strings.HasSuffix(n, ".tgz"):
		return arcTarGz
	}
	return arcNone
}

// arcEntry is one file or folder inside an archive.
type arcEntry struct {
	name  string // inner path, "a/b.txt"
	dir   bool
	size  int64
	mtime time.Time
	mode  fs.FileMode
	zf    *zip.File // ZIP entries
	off   int64     // .tar: offset of the data in the archive; -1 when it must be streamed
	seq   int       // tar entries: position in the archive, to find it again by streaming
}

// arcIndex is the parsed table of contents of one archive.
type arcIndex struct {
	name    string // root-relative archive path
	kind    arcKind
	size    int64     // archive size and mtime when indexed: a change
	mtime   time.Time // to either rebuilds the index
	src     *lazyFile
	entries map[string]*arcEntry   // by inner path
	kids    map[string][]*arcEntry // folder inner path ("" for the top) -> children
	partial bool                   // indexing hit a limit; some entries are missing
	used    time.Time              // guarded by Server.arcMu
}

// arcBuild is an index being built; concurrent callers wait for it.
type arcBuild struct {
	done chan struct{}
	ix   *arcIndex
	err  error
}

// errNotArchive is returned for archives that can't be read.
var errNotArchive = errors.New("not a readable archive")

// splitArchive finds the archive in a root-relative name: for
// "a/backup.zip/x/y" it returns "a/backup.zip" and "x/y"; for the archive
// itself, inner is "". Folders that merely look like archives by name are
// skipped, and archives inside archives are treated as plain files.
func (s *Server) splitArchive(name string) (arc, inner string, ok bool) {
	// Archives on other servers are read by those servers.
	if name == "." || (s.cfg.Flock && inFlock(name)) {
		return "", "", false
	}
	segs := strings.Split(name, "/")
	// Only segments with an archive extension need a look on disk.
	for i, seg := range segs {
		if archiveKind(seg) == arcNone {
			continue
		}
		p := strings.Join(segs[:i+1], "/")
		fi, err := s.root.Stat(p)
		if err != nil {
			return "", "", false
		}
		if fi.Mode().IsRegular() {
			return p, strings.Join(segs[i+1:], "/"), true
		}
	}
	return "", "", false
}

// archive returns the (cached) index of the archive at a root-relative name.
func (s *Server) archive(name string) (*arcIndex, error) {
	fi, err := s.root.Stat(name)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errNotArchive
	}
	for {
		s.arcMu.Lock()
		// A cached index is good while the archive is unchanged on disk.
		if ix := s.arcs[name]; ix != nil && ix.size == fi.Size() && ix.mtime.Equal(fi.ModTime()) {
			ix.used = time.Now()
			s.arcMu.Unlock()
			return ix, nil
		}
		// Someone else is indexing it: wait and look again.
		if b := s.arcBuilding[name]; b != nil {
			s.arcMu.Unlock()
			<-b.done
			if b.err != nil {
				return nil, b.err
			}
			continue
		}
		b := &arcBuild{done: make(chan struct{})}
		s.arcBuilding[name] = b
		s.arcMu.Unlock()

		b.ix, b.err = s.buildIndex(name, fi)
		s.arcMu.Lock()
		delete(s.arcBuilding, name)
		if b.err == nil {
			b.ix.used = time.Now()
			s.storeIndexLocked(b.ix)
		}
		s.arcMu.Unlock()
		close(b.done)
		return b.ix, b.err
	}
}

// storeIndexLocked caches ix, evicting the least recently used index when
// the cache is full. Evicted archives close their file once idle.
func (s *Server) storeIndexLocked(ix *arcIndex) {
	if old := s.arcs[ix.name]; old != nil {
		old.src.Close()
	}
	s.arcs[ix.name] = ix
	// Over capacity: drop the index used longest ago.
	for len(s.arcs) > archiveCacheSize {
		var oldest *arcIndex
		for _, c := range s.arcs {
			if oldest == nil || c.used.Before(oldest.used) {
				oldest = c
			}
		}
		delete(s.arcs, oldest.name)
		oldest.src.Close()
	}
}

// buildIndex reads an archive's table of contents.
func (s *Server) buildIndex(name string, fi fs.FileInfo) (*arcIndex, error) {
	ix := &arcIndex{
		name: name, kind: archiveKind(path.Base(name)), size: fi.Size(), mtime: fi.ModTime(),
		src:     newLazyFile(func() (file, error) { return s.root.Open(name) }),
		entries: map[string]*arcEntry{}, kids: map[string][]*arcEntry{},
	}
	var err error
	switch ix.kind {
	case arcZip:
		err = ix.indexZip()
	case arcTar, arcTarGz:
		err = ix.indexTar()
	default:
		err = errNotArchive
	}
	if err != nil {
		ix.src.Close()
		return nil, errNotArchive
	}
	// Children lists come out sorted, so listings and plans are stable.
	for _, ks := range ix.kids {
		slices.SortFunc(ks, func(a, b *arcEntry) int { return strings.Compare(a.name, b.name) })
	}
	return ix, nil
}

// indexZip reads a ZIP's central directory.
func (ix *arcIndex) indexZip() error {
	zr, err := zip.NewReader(ix.src, ix.size)
	if err != nil {
		return err
	}
	// One entry per member; folders are implied by paths too.
	for _, f := range zr.File {
		if len(ix.entries) >= archiveMaxEntries {
			ix.partial = true
			break
		}
		n, ok := cleanInner(f.Name)
		if !ok {
			continue
		}
		dir := strings.HasSuffix(f.Name, "/") || f.FileInfo().IsDir()
		e := &arcEntry{name: n, dir: dir, mtime: f.Modified, mode: f.Mode(), off: -1}
		if !dir {
			// Symlinks and other special members aren't served.
			if !f.Mode().IsRegular() {
				continue
			}
			e.size, e.zf = int64(f.UncompressedSize64), f
		}
		ix.add(e)
	}
	return nil
}

// indexTar reads every header of a .tar or .tar.gz. In a plain .tar the
// data is skipped by seeking, and each file's offset is recorded so it can
// be served (with Range) straight from the archive.
func (ix *arcIndex) indexTar() error {
	sr := io.NewSectionReader(ix.src, 0, ix.size)
	var r io.Reader = sr
	if ix.kind == arcTarGz {
		zr, err := gzip.NewReader(sr)
		if err != nil {
			return err
		}
		defer zr.Close()
		r = zr
	}
	tr := tar.NewReader(r)
	deadline := time.Now().Add(archiveScanTime)
	// Walk the headers until the end (or a limit).
	for seq := 0; ; seq++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A damaged tail still leaves a useful listing of what came before.
			if len(ix.entries) == 0 {
				return err
			}
			ix.partial = true
			break
		}
		if len(ix.entries) >= archiveMaxEntries || time.Now().After(deadline) {
			ix.partial = true
			break
		}
		n, ok := cleanInner(hdr.Name)
		if !ok {
			continue
		}
		fi := hdr.FileInfo()
		e := &arcEntry{name: n, dir: fi.IsDir(), mtime: hdr.ModTime, mode: fi.Mode(), off: -1, seq: seq}
		switch {
		case e.dir:
		case fi.Mode().IsRegular():
			e.size = hdr.Size
			// Only a plain, non-sparse member is stored contiguously.
			if ix.kind == arcTar && !isSparse(hdr) {
				if pos, err := sr.Seek(0, io.SeekCurrent); err == nil {
					e.off = pos
				}
			}
		default:
			continue // links, devices, ...
		}
		ix.add(e)
	}
	return nil
}

// isSparse reports whether a tar member is stored sparsely.
func isSparse(h *tar.Header) bool {
	if h.Typeflag == tar.TypeGNUSparse {
		return true
	}
	// PAX sparse formats announce themselves in the extended records.
	for k := range h.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return true
		}
	}
	return false
}

// add records an entry and every folder above it. A later entry with the
// same name replaces an earlier one, as extraction would.
func (ix *arcIndex) add(e *arcEntry) {
	if old := ix.entries[e.name]; old != nil {
		// A folder implied by a path is replaced by its real record.
		if old.dir && e.dir {
			old.mtime, old.mode = e.mtime, e.mode
			return
		}
		ix.removeKid(old)
	}
	ix.entries[e.name] = e
	parent := path.Dir(e.name)
	if parent == "." {
		parent = ""
	}
	ix.kids[parent] = append(ix.kids[parent], e)
	// Make sure the parent folder exists, all the way up.
	if parent != "" && ix.entries[parent] == nil {
		ix.add(&arcEntry{name: parent, dir: true, mtime: e.mtime, mode: fs.ModeDir | 0o755, off: -1})
	}
}

// removeKid drops a replaced entry from its parent's children.
func (ix *arcIndex) removeKid(e *arcEntry) {
	parent := path.Dir(e.name)
	if parent == "." {
		parent = ""
	}
	ix.kids[parent] = slices.DeleteFunc(ix.kids[parent], func(x *arcEntry) bool { return x == e })
}

// cleanInner normalises a member name to a clean relative path. Names that
// are empty, invalid or macOS resource-fork clutter are dropped.
func cleanInner(n string) (string, bool) {
	n = strings.ToValidUTF8(strings.ReplaceAll(n, "\\", "/"), "_")
	n = strings.TrimPrefix(path.Clean("/"+n), "/")
	if n == "" || !fs.ValidPath(n) || n == "__MACOSX" || strings.HasPrefix(n, "__MACOSX/") {
		return "", false
	}
	return n, true
}

// lookup finds an inner path; "" is the archive's top folder.
func (ix *arcIndex) lookup(inner string) (*arcEntry, bool) {
	if inner == "" {
		return &arcEntry{dir: true, mtime: ix.mtime, mode: fs.ModeDir | 0o555}, true
	}
	e, ok := ix.entries[inner]
	return e, ok
}

// info describes an archive entry as a file.
func (ix *arcIndex) info(e *arcEntry) fs.FileInfo {
	n := path.Base(e.name)
	if e.name == "" {
		n = path.Base(ix.name)
	}
	return arcInfo{e: e, name: n}
}

// arcInfo is an archive entry as an fs.FileInfo.
type arcInfo struct {
	e    *arcEntry
	name string
}

// Name, Size, ModTime, IsDir and Sys implement fs.FileInfo.
func (i arcInfo) Name() string       { return i.name }
func (i arcInfo) Size() int64        { return i.e.size }
func (i arcInfo) ModTime() time.Time { return i.e.mtime }
func (i arcInfo) IsDir() bool        { return i.e.dir }
func (i arcInfo) Sys() any           { return nil }

// Mode reports a folder or a plain file, whatever the archive recorded.
func (i arcInfo) Mode() fs.FileMode {
	if i.e.dir {
		return fs.ModeDir | 0o555
	}
	return i.e.mode.Perm() | 0o444
}

// listArchive lists one folder inside an archive with the usual rules.
func (s *Server) listArchive(arc, inner string) ([]entry, error) {
	ix, err := s.archive(arc)
	if err != nil {
		return nil, err
	}
	e, ok := ix.lookup(inner)
	if !ok {
		return nil, fs.ErrNotExist
	}
	if !e.dir {
		return nil, errNotDir
	}
	kids := ix.kids[inner]
	out := make([]entry, 0, len(kids))
	// One row per child; hidden names follow the --hidden rule.
	for _, k := range kids {
		n := path.Base(k.name)
		if !s.cfg.ShowHidden && isHiddenName(n) {
			continue
		}
		out = append(out, entry{Name: n, Dir: k.dir, Size: k.size, MTime: k.mtime.UnixMilli()})
	}
	return out, nil
}

// errNotDir is returned when a file is listed as a folder.
var errNotDir = errors.New("not a folder")

// openEntry opens an archive entry for reading, with seeking. Stored ZIP
// members and plain .tar members are read in place (fast Range requests);
// compressed ones are decompressed on the fly and seek by reading forward
// (or by starting again, for a backward seek).
func (ix *arcIndex) openEntry(e *arcEntry) (io.ReadSeekCloser, error) {
	switch {
	case e.dir:
		return nil, errNotDir
	case e.zf != nil && e.zf.Method == zip.Store:
		off, err := e.zf.DataOffset()
		if err != nil {
			return nil, err
		}
		return nopCloser{io.NewSectionReader(ix.src, off, e.size)}, nil
	case e.zf != nil:
		return &streamSeeker{size: e.size, open: e.zf.Open}, nil
	case e.off >= 0:
		return nopCloser{io.NewSectionReader(ix.src, e.off, e.size)}, nil
	}
	return &streamSeeker{size: e.size, open: func() (io.ReadCloser, error) { return ix.streamTar(e) }}, nil
}

// streamTar reads a tar(.gz) from the start up to entry e and returns its
// contents.
func (ix *arcIndex) streamTar(e *arcEntry) (io.ReadCloser, error) {
	var r io.Reader = io.NewSectionReader(ix.src, 0, ix.size)
	var closer io.Closer = io.NopCloser(nil)
	if ix.kind == arcTarGz {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, err
		}
		r, closer = zr, zr
	}
	tr := tar.NewReader(r)
	// Skip headers until the entry's position in the archive.
	for seq := 0; ; seq++ {
		if _, err := tr.Next(); err != nil {
			closer.Close()
			if errors.Is(err, io.EOF) {
				err = fs.ErrNotExist
			}
			return nil, err
		}
		if seq == e.seq {
			return readCloser{io.LimitReader(tr, e.size), closer}, nil
		}
	}
}

// archiveSize totals the files under an inner path, like /api/size.
func (s *Server) archiveSize(arc, inner string) (bytes, files int64, err error) {
	ix, err := s.archive(arc)
	if err != nil {
		return 0, 0, err
	}
	e, ok := ix.lookup(inner)
	if !ok {
		return 0, 0, fs.ErrNotExist
	}
	if !e.dir {
		return e.size, 1, nil
	}
	ix.walk(inner, s.cfg.ShowHidden, func(k *arcEntry) {
		if !k.dir {
			bytes += k.size
			files++
		}
	})
	return bytes, files, nil
}

// walk visits everything below an inner folder, depth first in name
// order, leaving out hidden names unless showHidden.
func (ix *arcIndex) walk(inner string, showHidden bool, fn func(*arcEntry)) {
	// Children are sorted, so the walk order is stable.
	for _, k := range ix.kids[inner] {
		if !showHidden && isHiddenName(path.Base(k.name)) {
			continue
		}
		fn(k)
		if k.dir {
			ix.walk(k.name, showHidden, fn)
		}
	}
}

// lazyFile is an io.ReaderAt over an archive file that is opened on first
// use and closed again after a quiet spell, so cached indexes don't pin
// open files (which on Windows would block deleting them). Reads reopen it
// transparently.
type lazyFile struct {
	open   func() (file, error)
	mu     sync.RWMutex // read-locked while reading, write-locked to open or close
	f      io.ReaderAt
	closer io.Closer
	last   atomic.Int64 // UnixNano of the last read
	closed bool         // evicted from the cache: close as soon as idle
}

// newLazyFile returns a reader that opens the file with open when needed.
func newLazyFile(open func() (file, error)) *lazyFile { return &lazyFile{open: open} }

// ReadAt implements io.ReaderAt.
func (l *lazyFile) ReadAt(p []byte, off int64) (int, error) {
	// Open (or reopen) under the write lock, then read under the read lock
	// so the idle closer can't pull the file away mid-read.
	for {
		l.mu.RLock()
		if f := l.f; f != nil {
			n, err := f.ReadAt(p, off)
			l.last.Store(time.Now().UnixNano())
			l.mu.RUnlock()
			return n, err
		}
		l.mu.RUnlock()
		if err := l.reopen(); err != nil {
			return 0, err
		}
	}
}

// reopen opens the file if nobody else has in the meantime.
func (l *lazyFile) reopen() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		return nil
	}
	f, err := l.open()
	if err != nil {
		return err
	}
	ra, ok := f.(io.ReaderAt)
	if !ok {
		f.Close()
		return errNotArchive
	}
	l.f, l.closer = ra, f
	l.last.Store(time.Now().UnixNano())
	time.AfterFunc(archiveIdleClose, l.idle)
	return nil
}

// idle closes the file if it hasn't been read for a while, else checks again later.
func (l *lazyFile) idle() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	quiet := time.Since(time.Unix(0, l.last.Load()))
	// Still in use, and not evicted: look again once it could be idle.
	if quiet < archiveIdleClose && !l.closed {
		time.AfterFunc(archiveIdleClose-quiet, l.idle)
		return
	}
	l.closer.Close()
	l.f, l.closer = nil, nil
}

// Close marks the file as no longer cached: it is closed as soon as it is
// idle. Readers that still hold entries reopen it if they need to.
func (l *lazyFile) Close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	go l.idle()
	return nil
}

// streamSeeker makes a forward-only stream seekable: Seek just moves the
// position, and Read catches up by reading forward, or by starting the
// stream again after a backward seek. It suits sequential readers such as
// http.ServeContent, which seeks once (to a Range start) and reads on.
type streamSeeker struct {
	size int64
	open func() (io.ReadCloser, error)
	rc   io.ReadCloser
	pos  int64 // position of rc
	off  int64 // position the next Read should start at
}

// Read reads from the current position.
func (s *streamSeeker) Read(p []byte) (int, error) {
	if s.off >= s.size {
		return 0, io.EOF
	}
	// Behind the stream: start it again.
	if s.rc == nil || s.pos > s.off {
		if s.rc != nil {
			s.rc.Close()
		}
		rc, err := s.open()
		if err != nil {
			return 0, err
		}
		s.rc, s.pos = rc, 0
	}
	// Ahead of the stream: read forward to the wanted position.
	if s.pos < s.off {
		n, err := io.CopyN(io.Discard, s.rc, s.off-s.pos)
		s.pos += n
		if err != nil {
			return 0, err
		}
	}
	n, err := s.rc.Read(p)
	s.pos += int64(n)
	s.off = s.pos
	return n, err
}

// Seek moves the position; no data is read until the next Read.
func (s *streamSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += s.off
	case io.SeekEnd:
		offset += s.size
	default:
		return 0, errors.New("bad whence")
	}
	if offset < 0 {
		return 0, errors.New("negative position")
	}
	s.off = offset
	return offset, nil
}

// Close ends the stream.
func (s *streamSeeker) Close() error {
	if s.rc == nil {
		return nil
	}
	return s.rc.Close()
}

// nopCloser adds a no-op Close to a ReadSeeker.
type nopCloser struct{ io.ReadSeeker }

// Close does nothing.
func (nopCloser) Close() error { return nil }

// readCloser pairs a reader with what must be closed after it.
type readCloser struct {
	io.Reader
	io.Closer
}

// openFile opens a root-relative file for reading, inside an archive or
// not, and describes it. Folders are returned too (callers check IsDir);
// a folder inside an archive has no reader.
func (s *Server) openFile(name string) (io.ReadSeekCloser, fs.FileInfo, error) {
	if arc, inner, ok := s.splitArchive(name); ok && inner != "" {
		ix, err := s.archive(arc)
		if err != nil {
			return nil, nil, err
		}
		e, ok := ix.lookup(inner)
		if !ok {
			return nil, nil, fs.ErrNotExist
		}
		if e.dir {
			return nil, ix.info(e), nil
		}
		r, err := ix.openEntry(e)
		if err != nil {
			return nil, nil, err
		}
		return r, ix.info(e), nil
	}
	f, err := s.root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, fi, nil
}

// stat describes a root-relative path, inside an archive or not.
func (s *Server) stat(name string) (fs.FileInfo, error) {
	if arc, inner, ok := s.splitArchive(name); ok && inner != "" {
		ix, err := s.archive(arc)
		if err != nil {
			return nil, err
		}
		e, ok := ix.lookup(inner)
		if !ok {
			return nil, fs.ErrNotExist
		}
		return ix.info(e), nil
	}
	return s.root.Stat(name)
}

// planArchive adds the entries under an inner path of an archive to a ZIP
// plan. Entry names are made relative to the job's base, like files.
func (s *Server) planArchive(ctx context.Context, job *zipJob, top, arc, inner string, rename func(string) string) error {
	ix, err := s.archive(arc)
	if err != nil {
		return err
	}
	e, ok := ix.lookup(inner)
	if !ok {
		return fs.ErrNotExist
	}
	addItem := func(k *arcEntry) {
		name := arc + "/" + k.name
		rel := rename(name)
		if k.dir {
			job.items = append(job.items, &zipItem{name: rel + "/", dir: true, mode: fs.ModeDir | 0o755, mtime: k.mtime})
			return
		}
		job.items = append(job.items, &zipItem{src: name, name: rel, size: k.size, mode: k.mode.Perm() | 0o444, mtime: k.mtime})
	}
	if !e.dir {
		addItem(e)
		return nil
	}
	if inner != "" {
		addItem(e)
	}
	ix.walk(inner, s.cfg.ShowHidden, addItem)
	return ctx.Err()
}
