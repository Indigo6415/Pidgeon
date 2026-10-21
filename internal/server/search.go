package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Search limits keep one query cheap on huge trees. A search that hits one
// reports itself as partial.
const (
	searchMaxResults = 500
	searchMaxEntries = 300_000
	searchMaxTime    = 4 * time.Second
	searchFlushEvery = 120 * time.Millisecond
)

// searchHit is one matching file or folder, one JSON object per line.
type searchHit struct {
	Path  string `json:"path"` // "/a/b.txt"
	Dir   bool   `json:"dir,omitempty"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
	Fuzzy bool   `json:"fuzzy,omitempty"` // matched as a subsequence, not a substring
}

// handleSearch finds files and folders anywhere in the served tree whose
// name matches ?q= (or whose path does, when the query contains a "/"). The
// answer is streamed as newline-delimited JSON while the tree is walked
// breadth-first, so shallow matches arrive first and the quick-open box can
// show results immediately. Substring matches stream as they are found;
// looser subsequence matches follow at the end. The last line is
// {"done":true,"partial":<bool>}.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if q == "" || len(q) > 200 || !utf8.ValidString(q) {
		http.Error(w, "bad query", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), searchMaxTime)
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "application/x-ndjson")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	lastFlush := time.Now()
	sent := 0
	send := func(hit searchHit) {
		enc.Encode(hit)
		sent++
		// Flush in small batches: often enough to feel live, not per line.
		if time.Since(lastFlush) > searchFlushEvery {
			rc.Flush()
			lastFlush = time.Now()
		}
	}

	byPath := strings.Contains(q, "/")
	var fuzzy []searchHit
	partial := false
	seen := 0
	queue := []string{"."}
	// Breadth-first walk; stop on the result, entry or time budget.
walk:
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		des, err := s.readDirSorted(dir)
		if err != nil {
			continue
		}
		for _, de := range des {
			if seen++; seen > searchMaxEntries || ctx.Err() != nil || sent >= searchMaxResults {
				partial = true
				break walk
			}
			n := de.Name()
			if s.hidden(dir, n) {
				continue
			}
			rel := n
			if dir != "." {
				rel = dir + "/" + n
			}
			typ := de.Type()
			link := typ&fs.ModeSymlink != 0
			// Symlinks count as what they point at, while that stays inside
			// the root; they are never descended into (no cycles).
			if link {
				fi, err := s.root.Stat(rel)
				if err != nil {
					continue
				}
				typ = fi.Mode().Type()
			}
			if !typ.IsDir() && !typ.IsRegular() {
				continue
			}
			if typ.IsDir() && !link {
				queue = append(queue, rel)
			}
			target := strings.ToLower(n)
			if byPath {
				target = strings.ToLower(rel)
			}
			exact := strings.Contains(target, q)
			// Cheap test first; only matches pay for a stat.
			if !exact && (len(fuzzy) >= searchMaxResults || !subsequence(target, q)) {
				continue
			}
			hit := searchHit{Path: "/" + rel, Dir: typ.IsDir(), Fuzzy: !exact}
			if fi, err := s.root.Stat(rel); err == nil {
				hit.MTime = fi.ModTime().UnixMilli()
				if !hit.Dir {
					hit.Size = fi.Size()
				}
			}
			if exact {
				send(hit)
			} else {
				fuzzy = append(fuzzy, hit)
			}
		}
	}
	// Looser matches go last, within whatever room is left.
	for _, hit := range fuzzy {
		if sent >= searchMaxResults || r.Context().Err() != nil {
			partial = true
			break
		}
		send(hit)
	}
	enc.Encode(map[string]any{"done": true, "partial": partial})
	rc.Flush()
}

// readDirSorted reads a root-relative directory, sorted by name so results
// come out in a stable order.
func (s *Server) readDirSorted(dir string) ([]fs.DirEntry, error) {
	f, err := s.root.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	des, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(des, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return des, nil
}

// subsequence reports whether every rune of q appears in s, in order.
func subsequence(s, q string) bool {
	// Advance through s once, consuming q as its runes are found.
	for _, c := range s {
		if q == "" {
			return true
		}
		r, size := utf8.DecodeRuneInString(q)
		if c == r {
			q = q[size:]
		}
	}
	return q == ""
}
