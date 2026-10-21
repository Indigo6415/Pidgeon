package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sync"
)

// hashCacheSize is how many file hashes are remembered.
const hashCacheSize = 4096

// hashCache remembers SHA-256 sums by path, size and modification time,
// so asking again for an unchanged file is instant. Concurrent requests
// for the same file share one read of it.
type hashCache struct {
	mu      sync.Mutex
	sums    map[string]string // key -> hex digest
	order   []string          // keys, oldest first, for eviction
	running map[string]*hashCall
}

// hashCall is a hash being computed; waiters block on done.
type hashCall struct {
	done chan struct{}
	sum  string
	err  error
}

// newHashCache returns an empty cache.
func newHashCache() *hashCache {
	return &hashCache{sums: map[string]string{}, running: map[string]*hashCall{}}
}

// hashKey identifies one version of a file.
func hashKey(name string, fi fs.FileInfo) string {
	return fmt.Sprintf("%s\x00%d\x00%d", name, fi.Size(), fi.ModTime().UnixNano())
}

// handleHash returns the SHA-256 of one file (?path=/a/b.iso), also for
// files inside archives: {"sha256": "...", "size": n, "mtime": ms}.
func (s *Server) handleHash(w http.ResponseWriter, r *http.Request) {
	name, err := s.resolve(r.URL.Query().Get("path"))
	if err != nil {
		httpError(w, err)
		return
	}
	sum, fi, err := s.hashFile(r.Context(), name)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sha256": sum, "size": fi.Size(), "mtime": fi.ModTime().UnixMilli()})
}

// hashFile returns the hex SHA-256 of a root-relative file.
func (s *Server) hashFile(ctx context.Context, name string) (string, fs.FileInfo, error) {
	fi, err := s.stat(name)
	if err != nil {
		return "", nil, err
	}
	if fi.IsDir() {
		return "", nil, errNotFile
	}
	c := s.hashes
	key := hashKey(name, fi)
	c.mu.Lock()
	if sum, ok := c.sums[key]; ok {
		c.mu.Unlock()
		return sum, fi, nil
	}
	call := c.running[key]
	// Someone is already reading this file: wait for their answer.
	if call != nil {
		c.mu.Unlock()
		select {
		case <-call.done:
			return call.sum, fi, call.err
		case <-ctx.Done():
			return "", nil, ctx.Err()
		}
	}
	call = &hashCall{done: make(chan struct{})}
	c.running[key] = call
	c.mu.Unlock()

	// The hash is computed to the end even if this caller leaves, so a
	// second request (or a retry) finds it ready.
	call.sum, call.err = s.computeHash(name)
	c.mu.Lock()
	delete(c.running, key)
	if call.err == nil {
		c.storeLocked(key, call.sum)
	}
	c.mu.Unlock()
	close(call.done)
	return call.sum, fi, call.err
}

// errNotFile is returned when a folder is hashed.
var errNotFile = errors.New("not a file")

// computeHash reads a file and returns its hex SHA-256.
func (s *Server) computeHash(name string) (string, error) {
	f, _, err := s.openFile(name)
	if err != nil {
		return "", err
	}
	if f == nil {
		return "", errNotFile
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, onlyReader{f}, make([]byte, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// storeLocked caches a sum, evicting the oldest when full.
func (c *hashCache) storeLocked(key, sum string) {
	if _, ok := c.sums[key]; !ok {
		c.order = append(c.order, key)
	}
	c.sums[key] = sum
	// Over capacity: forget the oldest entries.
	for len(c.order) > hashCacheSize {
		delete(c.sums, c.order[0])
		c.order = c.order[1:]
	}
}
