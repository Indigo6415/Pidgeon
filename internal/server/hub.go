package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// event is one named Server-Sent Event for a tab.
type event struct {
	name string
	data []byte
}

// subscriber is one open event stream (one browser tab).
type subscriber struct {
	cid   string // the tab's client ID
	ip    string // the device it runs on
	ch    chan event
	watch string // root-relative folder the tab shows, "" until it says; guarded by hubMu
}

// subscribe registers an event stream.
func (s *Server) subscribe(cid, ip string) *subscriber {
	sub := &subscriber{cid: cid, ip: ip, ch: make(chan event, 16)}
	s.hubMu.Lock()
	s.subs[sub] = struct{}{}
	s.hubMu.Unlock()
	return sub
}

// unsubscribe removes an event stream when its request ends.
func (s *Server) unsubscribe(sub *subscriber) {
	s.hubMu.Lock()
	delete(s.subs, sub)
	s.hubMu.Unlock()
}

// emit sends a named event to every stream match accepts and returns how
// many got it. A tab that isn't keeping up loses the event rather than
// stalling everyone else.
func (s *Server) emit(match func(*subscriber) bool, name string, v any) int {
	data, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	ev := event{name: name, data: data}
	n := 0
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	// Non-blocking sends: the buffer absorbs bursts.
	for sub := range s.subs {
		if !match(sub) {
			continue
		}
		select {
		case sub.ch <- ev:
			n++
		default:
		}
	}
	return n
}

// Online returns the number of open tabs per device IP.
func (s *Server) Online() map[string]int {
	out := map[string]int{}
	s.hubMu.Lock()
	// Count streams per address.
	for sub := range s.subs {
		out[sub.ip]++
	}
	s.hubMu.Unlock()
	return out
}

// errSendDir is returned when the operator tries to send a folder.
var errSendDir = errors.New("only files can be sent; use a share link for a folder")

// Send offers a file ("/a/b.pdf") to every open tab on device ip, or on
// every device when ip is "". The browser shows a notification with a
// download button. It returns how many tabs were reached.
func (s *Server) Send(ip, p string) (int, error) {
	name, err := s.resolve(p)
	if err != nil {
		return 0, err
	}
	fi, err := s.stat(name)
	if err != nil {
		return 0, err
	}
	if fi.IsDir() {
		return 0, errSendDir
	}
	if !fi.Mode().IsRegular() {
		return 0, errors.New("not a regular file")
	}
	msg := map[string]any{"path": "/" + name, "name": fi.Name(), "size": fi.Size(), "mtime": fi.ModTime().UnixMilli()}
	n := s.emit(func(sub *subscriber) bool { return ip == "" || sub.ip == ip }, "send", msg)
	if n == 0 {
		return 0, errors.New("that device has no Pidgeon page open")
	}
	return n, nil
}

// handleWatch records which folder a tab is showing (?c=<client>&path=/a),
// so it gets "change" events when that folder changes on disk.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cid := q.Get("c")
	if !clientIDPattern.MatchString(cid) {
		http.Error(w, "bad client id", http.StatusBadRequest)
		return
	}
	name, err := s.resolve(q.Get("path"))
	if err != nil {
		httpError(w, err)
		return
	}
	s.hubMu.Lock()
	// Every stream of this tab (normally one) follows the new folder.
	for sub := range s.subs {
		if sub.cid == cid {
			sub.watch = name
		}
	}
	s.hubMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// Folder watching polls instead of using OS notifications: it is portable,
// needs no dependencies, and only the folders someone is looking at are
// scanned. A folder that is slow to scan is polled proportionally less often.
const (
	watchEvery   = 1500 * time.Millisecond
	watchMaxCost = 20 // a scan may use at most 1/20 of the time between scans
)

// watchState is the last scan of one watched folder.
type watchState struct {
	sig  map[string]entry
	next time.Time
}

// change is the payload of a "change" event.
type change struct {
	Path    string   `json:"path"` // "/" or "/a/b"
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Changed []string `json:"changed"`
	Gone    bool     `json:"gone,omitempty"` // the folder itself can't be read any more
}

// watchLoop scans the folders open in any tab and pushes a "change" event
// to the tabs showing one when its contents change. It runs until Close.
func (s *Server) watchLoop() {
	tk := time.NewTicker(watchEvery)
	defer tk.Stop()
	states := map[string]*watchState{}
	// One pass per tick over the currently watched folders.
	for {
		select {
		case <-s.done:
			return
		case <-tk.C:
		}
		watched := map[string]bool{}
		s.hubMu.Lock()
		for sub := range s.subs {
			if sub.watch != "" {
				watched[sub.watch] = true
			}
		}
		s.hubMu.Unlock()
		// Forget folders nobody is looking at any more.
		for dir := range states {
			if !watched[dir] {
				delete(states, dir)
			}
		}
		now := time.Now()
		for dir := range watched {
			st := states[dir]
			// Slow folders wait their turn.
			if st != nil && now.Before(st.next) {
				continue
			}
			start := time.Now()
			es, err := s.list(dir)
			if err != nil {
				// Deleted or unreadable: tell its viewers once, then stop.
				if st != nil {
					s.emitChange(dir, change{Gone: true})
				}
				states[dir] = &watchState{sig: nil, next: now.Add(10 * watchEvery)}
				continue
			}
			sig := make(map[string]entry, len(es))
			for _, e := range es {
				sig[e.Name] = e
			}
			// The first scan is the baseline; later ones are compared to it.
			if st != nil && st.sig != nil {
				if c, ok := diff(st.sig, sig); ok {
					s.emitChange(dir, c)
				}
			}
			cost := time.Since(start) * watchMaxCost
			states[dir] = &watchState{sig: sig, next: now.Add(max(watchEvery, cost))}
		}
	}
}

// emitChange sends c (with its path filled in) to the tabs showing dir.
func (s *Server) emitChange(dir string, c change) {
	c.Path = "/"
	if dir != "." {
		c.Path = "/" + dir
	}
	s.emit(func(sub *subscriber) bool { return sub.watch == dir }, "change", c)
}

// diff compares two scans of a folder. ok is false when nothing changed.
func diff(old, cur map[string]entry) (c change, ok bool) {
	c = change{Added: []string{}, Removed: []string{}, Changed: []string{}}
	// New or modified entries.
	for name, e := range cur {
		o, had := old[name]
		switch {
		case !had:
			c.Added = append(c.Added, name)
		case o.Dir != e.Dir || o.Size != e.Size || o.MTime != e.MTime:
			c.Changed = append(c.Changed, name)
		}
	}
	// Entries that disappeared.
	for name := range old {
		if _, still := cur[name]; !still {
			c.Removed = append(c.Removed, name)
		}
	}
	return c, len(c.Added)+len(c.Removed)+len(c.Changed) > 0
}
