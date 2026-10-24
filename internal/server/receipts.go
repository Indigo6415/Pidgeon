package server

import (
	"context"
	"encoding/hex"
	"io/fs"
	"net/http"
	"time"

	"github.com/Indigo6415/pidgeon/internal/stats"
)

// receiptHashSlots bounds how many files are hashed at once for receipts of
// resumed downloads (whose bytes didn't all pass through one response).
var receiptHashSlots = make(chan struct{}, 1)

// agentOf returns the nickname of the device at ip, if it is known.
func (s *Server) agentOf(ip string) string {
	if s.cfg.Devices == nil {
		return ""
	}
	if d, ok := s.cfg.Devices.Get(ip); ok {
		return d.Agent
	}
	return ""
}

// newReceipt fills in what every receipt shares.
func (s *Server) newReceipt(r *http.Request, t *stats.Transfer, kind, display string, size int64) stats.Receipt {
	ip := clientIP(r)
	return stats.Receipt{
		Time:     time.Now(),
		Client:   ip,
		Agent:    s.agentOf(ip),
		Path:     display,
		Kind:     kind,
		Bytes:    size,
		Share:    t.Share,
		Listener: t.Listener,
		Seconds:  time.Since(t.Started).Seconds(),
	}
}

// recordFile writes the receipt for a file that reached a device in full.
// A download sent in one response was hashed on the way out; a resumed one
// is hashed from disk in the background (the cache makes repeats free).
func (s *Server) recordFile(r *http.Request, t *stats.Transfer, cw *stats.CountingWriter, name string, fi fs.FileInfo) {
	rc := s.newReceipt(r, t, "file", "/"+name, fi.Size())
	rc.MTime = fi.ModTime().UnixMilli()
	if cw.Hash != nil && cw.Status == http.StatusOK {
		rc.SHA256 = hex.EncodeToString(cw.Hash.Sum(nil))
		s.cfg.Store.AddReceipt(rc)
		return
	}
	go func() {
		receiptHashSlots <- struct{}{}
		defer func() { <-receiptHashSlots }()
		// A file that can't be hashed any more still gets its receipt.
		if sum, now, err := s.hashFile(context.Background(), name); err == nil && now.Size() == fi.Size() && now.ModTime().Equal(fi.ModTime()) {
			rc.SHA256 = sum
		}
		s.cfg.Store.AddReceipt(rc)
	}()
}

// recordZip writes the receipt for a ZIP that reached a device in full. Its
// hash is known only when the whole archive went out in one response.
func (s *Server) recordZip(r *http.Request, t *stats.Transfer, cw *stats.CountingWriter, display string) {
	rc := s.newReceipt(r, t, "zip", display, t.Sent())
	if total := t.Total(); total > 0 {
		rc.Bytes = total
	}
	if cw.Hash != nil && cw.Status == http.StatusOK {
		rc.SHA256 = hex.EncodeToString(cw.Hash.Sum(nil))
		rc.Bytes = t.Sent()
	}
	s.cfg.Store.AddReceipt(rc)
}

// markReceived flags the files of a listing that the asking device already
// received in their current version (Unix ms of the delivery).
func (s *Server) markReceived(r *http.Request, dir string, es []entry) {
	ip := clientIP(r)
	// Most visitors have no receipts yet: skip the lookups entirely.
	if !s.cfg.Store.HasReceipts(ip) {
		return
	}
	for i := range es {
		if es[i].Dir {
			continue
		}
		p := "/" + es[i].Name
		if dir != "." {
			p = "/" + dir + "/" + es[i].Name
		}
		if at, ok := s.cfg.Store.Received(ip, p, es[i].Size, es[i].MTime); ok {
			es[i].Got = at.UnixMilli()
		}
	}
}
