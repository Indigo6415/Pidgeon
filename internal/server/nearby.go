package server

import (
	"context"
	"net/http"
	"time"
)

// Nearby servers are looked up at most this often; the scan itself takes
// a second or two.
const (
	nearbyFresh = 15 * time.Second
	nearbyScan  = 2 * time.Second
)

// handleNearby lists the other Pidgeon servers on the network, found over
// mDNS, so the web UI can link to them, or with the flock on, open them in
// place.
func (s *Server) handleNearby(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Nearby == nil {
		writeJSON(w, http.StatusOK, []NearbyServer{})
		return
	}
	found := s.scanNearby(r.Context(), nearbyFresh)
	out := make([]NearbyServer, len(found))
	// Copies: the scan is shared, and only this answer gets flock paths.
	for i, n := range found {
		if s.cfg.Flock && n.Proxy != "" && peerIDPattern.MatchString(n.ID) {
			n.Path = "/" + flockDir + "/" + n.ID
		}
		out[i] = n
	}
	writeJSON(w, http.StatusOK, out)
}

// scanNearby returns the last scan of the network, scanning again when it
// is older than maxAge. Concurrent callers share one scan.
func (s *Server) scanNearby(ctx context.Context, maxAge time.Duration) []NearbyServer {
	if s.cfg.Nearby == nil {
		return nil
	}
	s.nearbyMu.Lock()
	defer s.nearbyMu.Unlock()
	// A recent scan is good enough.
	if s.nearbyWhen.IsZero() || time.Since(s.nearbyWhen) > maxAge {
		ctx, cancel := context.WithTimeout(ctx, nearbyScan)
		found := s.cfg.Nearby(ctx)
		cancel()
		if found == nil {
			found = []NearbyServer{}
		}
		s.nearby, s.nearbyWhen = found, time.Now()
		if s.cfg.Flock {
			s.rememberPeers(found)
		}
	}
	return s.nearby
}
