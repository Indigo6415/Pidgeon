package server

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// file is an open file or folder of the served tree. *os.File satisfies
// it; so does the virtual top-level folder of a multi-root tree.
type file interface {
	io.ReadSeekCloser
	Stat() (fs.FileInfo, error)
	ReadDir(n int) ([]fs.DirEntry, error)
}

// tree is the served file system: one directory (the usual case), or
// several directories and files given on the command line, each shown as a
// top-level entry of a virtual root. Every access still goes through an
// os.Root per directory, so ".." and symlinks never lead outside the roots.
type tree struct {
	single *os.Root          // the one served directory; nil in multi-root mode
	mounts []*mount          // multi-root: the top-level entries, in argument order
	byName map[string]*mount // multi-root: mounts by entry name
	start  time.Time         // modification time reported for the virtual root
}

// mount is one top-level entry of a multi-root tree.
type mount struct {
	name string   // entry name at the top level (unique)
	abs  string   // absolute path, as served
	root *os.Root // the directory, or for a file mount its parent directory
	file string   // a file mount's name inside root; "" for a directory
}

// openTree opens the paths to serve. One directory is served as is; more
// than one path (or a single file) becomes a virtual root listing each of
// them by name, with "name (2)" for clashing names.
func openTree(paths []string) (*tree, error) {
	if len(paths) == 1 {
		if fi, err := os.Stat(paths[0]); err == nil && fi.IsDir() {
			root, err := os.OpenRoot(paths[0])
			if err != nil {
				return nil, fmt.Errorf("cannot serve %s: %w", paths[0], err)
			}
			return &tree{single: root}, nil
		}
	}
	t := &tree{byName: map[string]*mount{}, start: time.Now()}
	// Open each path in turn; a failure closes what was already opened.
	for _, p := range paths {
		m, err := openMount(p)
		if err != nil {
			t.Close()
			return nil, err
		}
		m.name = t.uniqueName(m.name)
		t.mounts = append(t.mounts, m)
		t.byName[m.name] = m
	}
	return t, nil
}

// openMount opens one top-level path of a multi-root tree.
func openMount(p string) (*mount, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("cannot serve %s: %w", p, err)
	}
	m := &mount{name: mountName(p), abs: p}
	if fi.IsDir() {
		if m.root, err = os.OpenRoot(p); err != nil {
			return nil, fmt.Errorf("cannot serve %s: %w", p, err)
		}
		return m, nil
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("cannot serve %s: not a regular file or directory", p)
	}
	// A file is reached through its real parent folder, so a symlink given
	// on the command line works even though os.Root won't follow it out.
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return nil, fmt.Errorf("cannot serve %s: %w", p, err)
	}
	if m.root, err = os.OpenRoot(filepath.Dir(real)); err != nil {
		return nil, fmt.Errorf("cannot serve %s: %w", p, err)
	}
	m.file = filepath.Base(real)
	return m, nil
}

// mountName is the top-level entry name for a served path: its base name,
// or "root" for a filesystem root such as "/" or "C:\".
func mountName(p string) string {
	n := filepath.Base(p)
	if n == "" || n == "." || n == ".." || strings.ContainsAny(n, `/\:`) {
		return "root"
	}
	return n
}

// uniqueName returns n, or "n (2)", "n (3)", ... if n is already taken.
func (t *tree) uniqueName(n string) string {
	if t.byName[n] == nil {
		return n
	}
	stem, ext := splitExt(n)
	// Count up until a free name turns up.
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if t.byName[c] == nil {
			return c
		}
	}
}

// splitExt splits "a.tar.gz" into "a" and ".tar.gz", "photo.jpg" into
// "photo" and ".jpg"; a name without an extension has ext "".
func splitExt(n string) (stem, ext string) {
	lower := strings.ToLower(n)
	// Double extensions first, so "x.tar.gz" becomes "x (2).tar.gz".
	for _, e := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst"} {
		if strings.HasSuffix(lower, e) && len(n) > len(e) {
			return n[:len(n)-len(e)], n[len(n)-len(e):]
		}
	}
	ext = filepath.Ext(n)
	if ext == n {
		ext = ""
	}
	return strings.TrimSuffix(n, ext), ext
}

// multi reports whether the tree has a virtual root.
func (t *tree) multi() bool { return t.single == nil }

// isMount reports whether a top-level name is a mount (multi-root only).
func (t *tree) isMount(name string) bool { return t.multi() && t.byName[name] != nil }

// Paths returns the absolute served paths, in order.
func (t *tree) Paths() []string {
	if !t.multi() {
		return []string{t.single.Name()}
	}
	out := make([]string, len(t.mounts))
	// One entry per mount, as given.
	for i, m := range t.mounts {
		out[i] = m.abs
	}
	return out
}

// locate maps a root-relative name onto a mount and the name inside its
// os.Root. The top level itself (".") has no mount.
func (t *tree) locate(name string) (*mount, string, error) {
	top, rest, _ := strings.Cut(name, "/")
	m := t.byName[top]
	if m == nil {
		return nil, "", fs.ErrNotExist
	}
	// A file mount has nothing below it.
	if m.file != "" {
		if rest != "" {
			return nil, "", fs.ErrNotExist
		}
		return m, m.file, nil
	}
	if rest == "" {
		rest = "."
	}
	return m, rest, nil
}

// Open opens a root-relative file or folder.
func (t *tree) Open(name string) (file, error) {
	if !t.multi() {
		return t.single.Open(name)
	}
	if name == "." {
		return &topDir{t: t}, nil
	}
	m, rel, err := t.locate(name)
	if err != nil {
		return nil, err
	}
	f, err := m.root.Open(rel)
	if err != nil {
		return nil, err
	}
	// A mount's own folder reports the mount name, not ".".
	if rel == "." || m.file != "" {
		return &renamedFile{File: f, name: m.name}, nil
	}
	return f, nil
}

// Stat describes a root-relative path, following symlinks inside a root.
func (t *tree) Stat(name string) (fs.FileInfo, error) {
	if !t.multi() {
		return t.single.Stat(name)
	}
	if name == "." {
		return topInfo{t: t}, nil
	}
	m, rel, err := t.locate(name)
	if err != nil {
		return nil, err
	}
	fi, err := m.root.Stat(rel)
	if err != nil {
		return nil, err
	}
	if rel == "." || m.file != "" {
		return renamedInfo{FileInfo: fi, name: m.name}, nil
	}
	return fi, nil
}

// FS returns the tree as an fs.FS, for directory walks.
func (t *tree) FS() fs.FS {
	if !t.multi() {
		return t.single.FS()
	}
	return treeFS{t}
}

// Close releases every open root.
func (t *tree) Close() error {
	if !t.multi() {
		return t.single.Close()
	}
	var errs []error
	// Close every mount, keeping the first failures.
	for _, m := range t.mounts {
		errs = append(errs, m.root.Close())
	}
	return errors.Join(errs...)
}

// rel returns abs as a root-relative name ("." or "a/b") when it is one of
// the served paths or inside one.
func (t *tree) rel(abs string) (string, bool) {
	if !t.multi() {
		return within(t.single.Name(), abs)
	}
	// The first directory mount that contains abs wins.
	for _, m := range t.mounts {
		if m.file != "" {
			continue
		}
		if r, ok := within(m.abs, abs); ok {
			if r == "." {
				return m.name, true
			}
			return m.name + "/" + r, true
		}
	}
	return "", false
}

// treeFS adapts a multi-root tree to fs.FS (and fs.StatFS) for walks.
type treeFS struct{ t *tree }

// Open implements fs.FS.
func (f treeFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	return f.t.Open(name)
}

// Stat implements fs.StatFS.
func (f treeFS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	return f.t.Stat(name)
}

// topDir is the virtual root folder of a multi-root tree.
type topDir struct {
	t    *tree
	read bool // ReadDir already returned everything
}

// errIsDir is returned when the virtual root is read as a file.
var errIsDir = errors.New("is a directory")

// Read fails: a folder has no contents to read.
func (d *topDir) Read([]byte) (int, error) { return 0, errIsDir }

// Seek fails like Read.
func (d *topDir) Seek(int64, int) (int64, error) { return 0, errIsDir }

// Close releases nothing; the mounts stay open.
func (d *topDir) Close() error { return nil }

// Stat describes the virtual root.
func (d *topDir) Stat() (fs.FileInfo, error) { return topInfo{t: d.t}, nil }

// ReadDir lists the mounts.
func (d *topDir) ReadDir(n int) ([]fs.DirEntry, error) {
	// Everything is returned in one batch; later calls see the end.
	if d.read {
		if n > 0 {
			return nil, io.EOF
		}
		return nil, nil
	}
	d.read = true
	return d.t.topEntries(), nil
}

// topEntries lists the mounts as directory entries. A mount that can no
// longer be read (deleted while serving) is left out.
func (t *tree) topEntries() []fs.DirEntry {
	out := make([]fs.DirEntry, 0, len(t.mounts))
	// Stat each mount through its own root.
	for _, m := range t.mounts {
		rel := m.file
		if rel == "" {
			rel = "."
		}
		fi, err := m.root.Stat(rel)
		if err != nil {
			continue
		}
		out = append(out, fs.FileInfoToDirEntry(renamedInfo{FileInfo: fi, name: m.name}))
	}
	slices.SortFunc(out, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return out
}

// topInfo describes the virtual root.
type topInfo struct{ t *tree }

func (i topInfo) Name() string       { return "." }
func (i topInfo) Size() int64        { return 0 }
func (i topInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o555 }
func (i topInfo) ModTime() time.Time { return i.t.start }
func (i topInfo) IsDir() bool        { return true }
func (i topInfo) Sys() any           { return nil }

// renamedInfo reports a mount under its top-level name.
type renamedInfo struct {
	fs.FileInfo
	name string
}

// Name returns the mount name.
func (r renamedInfo) Name() string { return r.name }

// renamedFile is a mount's own file or folder, reporting the mount name.
type renamedFile struct {
	*os.File
	name string
}

// Stat reports the mount name.
func (r *renamedFile) Stat() (fs.FileInfo, error) {
	fi, err := r.File.Stat()
	if err != nil {
		return nil, err
	}
	return renamedInfo{FileInfo: fi, name: r.name}, nil
}
