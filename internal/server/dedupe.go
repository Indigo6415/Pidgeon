package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

// The inbox never keeps two copies of the same file. Before uploading, the
// browser sends each file's size and a quick fingerprint (CRC-32s of three
// 64 KB samples: start, middle, end), and the server says which ones it
// already has, so a phone backing up its camera roll twice only sends what
// is new. After every upload the server also compares the received file's
// SHA-256 with same-sized files already there and drops an exact copy, so
// clients that skip the pre-check (curl) can't create duplicates either.
//
// Only files of the same size are ever read, so the cost stays small even
// for a large inbox.

const (
	fpChunk          = 64 << 10 // bytes per fingerprint sample
	dedupeMaxEntries = 100_000  // inbox entries looked at per search
	dedupeMaxTime    = 3 * time.Second
	inboxCacheMax    = 10_000 // remembered fingerprints and hashes
	checkMaxFiles    = 2000   // files per pre-check request
)

// inboxCache remembers fingerprints and SHA-256 sums of inbox files by
// path, size and modification time.
type inboxCache struct {
	mu   sync.Mutex
	fps  map[string]string
	sums map[string]string
}

// newInboxCache returns an empty cache.
func newInboxCache() *inboxCache {
	return &inboxCache{fps: map[string]string{}, sums: map[string]string{}}
}

// inboxKey identifies one version of an inbox file.
func inboxKey(rel string, fi fs.FileInfo) string {
	return fmt.Sprintf("%s\x00%d\x00%d", rel, fi.Size(), fi.ModTime().UnixNano())
}

// remember stores v under key in m, starting over when the map is full.
func (c *inboxCache) remember(m map[string]string, key, v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(m) >= inboxCacheMax {
		clear(m)
	}
	m[key] = v
}

// lookup returns a cached value.
func (c *inboxCache) lookup(m map[string]string, key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := m[key]
	return v, ok
}

// fpStarts returns where the three fingerprint samples begin. Small files
// are sampled whole (all three samples are the same bytes).
func fpStarts(size int64) [3]int64 {
	return [3]int64{0, max(0, size/2-fpChunk/2), max(0, size-fpChunk)}
}

// fingerprint computes the quick fingerprint of a file of the given size:
// three CRC-32s as 24 lowercase hex digits. The web UI computes the same.
func fingerprint(r io.ReaderAt, size int64) (string, error) {
	var sb strings.Builder
	buf := make([]byte, fpChunk)
	// One CRC per sample, concatenated.
	for _, start := range fpStarts(size) {
		n := min(int64(fpChunk), size-start)
		if _, err := r.ReadAt(buf[:n], start); err != nil && err != io.EOF {
			return "", err
		}
		fmt.Fprintf(&sb, "%08x", crc32.ChecksumIEEE(buf[:n]))
	}
	return sb.String(), nil
}

// inboxCand is an inbox file that might be a copy of an upload.
type inboxCand struct {
	rel string // inbox-relative path
	fi  fs.FileInfo
}

// inboxBySize indexes the files under scope (an inbox-relative folder) by
// size. Partial uploads and hidden files are left out. With only set, just
// those sizes are kept.
func (s *Server) inboxBySize(scope string, only map[int64]bool) map[int64][]inboxCand {
	out := map[int64][]inboxCand{}
	ctx, cancel := context.WithTimeout(context.Background(), dedupeMaxTime)
	defer cancel()
	seen := 0
	fs.WalkDir(s.inbox.FS(), scope, func(p string, d fs.DirEntry, err error) error {
		// Budget exhausted: whatever was found so far will do.
		if seen++; seen > dedupeMaxEntries || ctx.Err() != nil {
			return fs.SkipAll
		}
		if err != nil || p == scope {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasSuffix(d.Name(), ".part") {
			return nil
		}
		fi, err := d.Info()
		if err != nil || (only != nil && !only[fi.Size()]) {
			return nil
		}
		out[fi.Size()] = append(out[fi.Size()], inboxCand{rel: p, fi: fi})
		return nil
	})
	return out
}

// inboxFP returns (and caches) the quick fingerprint of an inbox file.
func (s *Server) inboxFP(c inboxCand) (string, error) {
	key := inboxKey(c.rel, c.fi)
	if v, ok := s.inboxSeen.lookup(s.inboxSeen.fps, key); ok {
		return v, nil
	}
	f, err := s.inbox.Open(c.rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	v, err := fingerprint(f, c.fi.Size())
	if err == nil {
		s.inboxSeen.remember(s.inboxSeen.fps, key, v)
	}
	return v, err
}

// inboxSum returns (and caches) the SHA-256 of an inbox file.
func (s *Server) inboxSum(c inboxCand) (string, error) {
	key := inboxKey(c.rel, c.fi)
	if v, ok := s.inboxSeen.lookup(s.inboxSeen.sums, key); ok {
		return v, nil
	}
	f, err := s.inbox.Open(c.rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, onlyReader{f}, make([]byte, 1<<20)); err != nil {
		return "", err
	}
	v := hex.EncodeToString(h.Sum(nil))
	s.inboxSeen.remember(s.inboxSeen.sums, key, v)
	return v, nil
}

// findCopy looks under scope for a file with exactly these bytes and
// returns its inbox-relative path.
func (s *Server) findCopy(scope string, size int64, sum string) (string, bool) {
	// Only same-sized files can match; each is hashed (once, then cached).
	for _, c := range s.inboxBySize(scope, map[int64]bool{size: true})[size] {
		if v, err := s.inboxSum(c); err == nil && v == sum {
			return c.rel, true
		}
	}
	return "", false
}

// checkUploads answers the browser's pre-check for an upload target:
// {"files":[{"name":"a.jpg","size":123,"fp":"..."}]} gets back
// {"have":[true,false,...]}, true for files the inbox already holds.
func (s *Server) checkUploads(w http.ResponseWriter, r *http.Request, scope string) {
	if s.inbox == nil {
		http.Error(w, "uploads are not enabled on this server", http.StatusNotFound)
		return
	}
	var req struct {
		Files []struct {
			Size int64  `json:"size"`
			FP   string `json:"fp"`
		} `json:"files"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || len(req.Files) > checkMaxFiles {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	sizes := map[int64]bool{}
	for _, f := range req.Files {
		sizes[f.Size] = true
	}
	// One walk of the inbox serves the whole batch.
	idx := s.inboxBySize(scope, sizes)
	have := make([]bool, len(req.Files))
	for i, f := range req.Files {
		for _, c := range idx[f.Size] {
			if v, err := s.inboxFP(c); err == nil && v == strings.ToLower(f.FP) {
				have[i] = true
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"have": have})
}

// handleUploadCheck is the pre-check for uploads into the inbox itself.
func (s *Server) handleUploadCheck(w http.ResponseWriter, r *http.Request) {
	s.checkUploads(w, r, ".")
}

// scopeDir turns an inbox-relative folder into a walk root ("" -> ".").
func scopeDir(dir string) string {
	if dir == "" {
		return "."
	}
	return path.Clean(dir)
}
