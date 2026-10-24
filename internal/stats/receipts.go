package stats

import (
	"time"
)

// Delivery receipts record every file (or ZIP) a device received in full:
// who got what, when, how big it was and its SHA-256. Previews, partial
// range requests (video seeking) and aborted downloads never produce one.
// The dashboard lists them, the web UI marks files a device already has,
// and --receipts appends each one to a JSON Lines file.

const (
	// receiptsKept is how many receipts the dashboard can scroll through.
	receiptsKept = 500
	// receivedMax bounds the per-device "already has it" index.
	receivedMax = 20_000
)

// Receipt is one completed delivery.
type Receipt struct {
	Time     time.Time `json:"time"`
	Client   string    `json:"device"`          // IP address
	Agent    string    `json:"agent,omitempty"` // nickname, e.g. "iPhone · Safari"
	Path     string    `json:"path"`            // "/a/b.pdf", or the ZIP's display name
	Kind     string    `json:"kind"`            // "file" or "zip"
	Bytes    int64     `json:"bytes"`           // size of what was delivered
	SHA256   string    `json:"sha256,omitempty"`
	Share    string    `json:"share,omitempty"`    // share link ID it came through
	Listener string    `json:"listener,omitempty"` // address it was served on
	Seconds  float64   `json:"seconds"`            // how long the transfer took

	// MTime identifies the version of a file that was delivered (Unix ms),
	// so a later change on disk means the device no longer has it.
	MTime int64 `json:"mtime,omitempty"`
}

// receivedKey indexes receipts by device and path.
func receivedKey(ip, path string) string { return ip + "\x00" + path }

// OnReceipt registers a callback invoked (synchronously) for each new
// receipt. It must be set before the server starts handling requests.
func (s *Store) OnReceipt(fn func(Receipt)) { s.onReceipt = fn }

// AddReceipt records a completed delivery.
func (s *Store) AddReceipt(r Receipt) {
	s.mu.Lock()
	s.receipts = append(s.receipts, r)
	// Keep only the most recent receipts in memory.
	if len(s.receipts) > receiptsKept {
		s.receipts = append(s.receipts[:0], s.receipts[len(s.receipts)-receiptsKept:]...)
	}
	// Files (not ZIPs) feed the "already has it" index, up to its cap.
	if k := receivedKey(r.Client, r.Path); r.Kind == "file" && (len(s.received) < receivedMax || s.received[k] != nil) {
		if s.received[k] == nil {
			s.receivedBy[r.Client]++
		}
		rc := r
		s.received[k] = &rc
	}
	s.deliveries++
	s.mu.Unlock()
	if s.onReceipt != nil {
		s.onReceipt(r)
	}
}

// Receipts returns the recent receipts, newest first.
func (s *Store) Receipts() []Receipt {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Receipt, len(s.receipts))
	// Reverse while copying so the newest comes first.
	for i, r := range s.receipts {
		out[len(out)-1-i] = r
	}
	return out
}

// Received reports whether device ip has been delivered path ("/a/b") in
// the version identified by size and mtime (Unix ms), and when.
func (s *Store) Received(ip, path string, size, mtime int64) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.received[receivedKey(ip, path)]
	if r == nil || r.Bytes != size || r.MTime != mtime {
		return time.Time{}, false
	}
	return r.Time, true
}

// HasReceipts reports whether device ip has received anything at all, so
// listings can skip per-entry lookups for new visitors.
func (s *Store) HasReceipts(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.receivedBy[ip] > 0
}

// Deliveries returns how many receipts have been recorded this session.
func (s *Store) Deliveries() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deliveries
}
