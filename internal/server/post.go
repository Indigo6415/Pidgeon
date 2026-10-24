package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Pigeon Post carries short text between the host and the devices: a link
// pasted on a phone shows up in the dashboard (one key copies it), and
// text typed in the dashboard pops up in a device's browser. Messages live
// only as long as the server runs.

const (
	postMaxBytes  = 16 << 10 // longest message
	postKept      = 200      // messages remembered
	postPerMinute = 30       // messages one device may send per minute
)

// Message is one piece of Pigeon Post.
type Message struct {
	ID    uint64
	Time  time.Time
	From  string // device IP; "" when the host sent it
	Agent string // the sending device's nickname
	To    string // host messages only: the device IP, "" for every device
	Text  string
}

// errPostEmpty and friends describe rejected messages.
var (
	errPostEmpty = errors.New("the message is empty")
	errPostLong  = errors.New("the message is too long")
	errPostBusy  = errors.New("too many messages; wait a moment")
)

// cleanPost trims a message and checks its size and encoding.
func cleanPost(text string) (string, error) {
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		return "", errPostEmpty
	case len(text) > postMaxBytes:
		return "", errPostLong
	case !utf8.ValidString(text):
		return "", errors.New("the message isn't valid text")
	}
	return text, nil
}

// addPost stores a message and returns it with its ID filled in.
func (s *Server) addPost(m Message) Message {
	s.postMu.Lock()
	defer s.postMu.Unlock()
	s.postSeq++
	m.ID, m.Time = s.postSeq, time.Now()
	s.posts = append(s.posts, m)
	// Keep the most recent messages only.
	if len(s.posts) > postKept {
		s.posts = append(s.posts[:0], s.posts[len(s.posts)-postKept:]...)
	}
	return m
}

// Messages returns every message, oldest first.
func (s *Server) Messages() []Message {
	s.postMu.Lock()
	defer s.postMu.Unlock()
	return append([]Message(nil), s.posts...)
}

// Post sends text from the host to every open tab on device ip (every
// device when ip is ""). The message is kept, so a device that opens the
// page later still sees it. It returns how many tabs got it right away.
func (s *Server) Post(ip, text string) (Message, int, error) {
	text, err := cleanPost(text)
	if err != nil {
		return Message{}, 0, err
	}
	m := s.addPost(Message{To: ip, Text: text})
	n := s.emit(func(sub *subscriber) bool { return ip == "" || sub.ip == ip }, "post", postJSON(m, ""))
	return m, n, nil
}

// postJSON is a message's wire form for device ip; "mine" marks the ones
// it sent itself.
func postJSON(m Message, ip string) map[string]any {
	return map[string]any{
		"id": m.ID, "time": m.Time.UnixMilli(), "text": m.Text,
		"mine": m.From != "" && m.From == ip, "host": m.From == "",
	}
}

// postFor reports whether device ip may see message m.
func postFor(m Message, ip string) bool {
	return m.From == ip || (m.From == "" && (m.To == "" || m.To == ip))
}

// handlePostList returns the conversation of the asking device.
func (s *Server) handlePostList(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	out := []map[string]any{}
	// Only its own messages and what the host sent it (or everyone).
	for _, m := range s.Messages() {
		if postFor(m, ip) {
			out = append(out, postJSON(m, ip))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePostSend takes a message for the host: {"text":"..."}. Other tabs
// on the same device see it too.
func (s *Server) handlePostSend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, postMaxBytes+1024)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	text, err := cleanPost(req.Text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ip := clientIP(r)
	// A device can't flood the dashboard.
	if s.recentPosts(ip, time.Minute) >= postPerMinute {
		http.Error(w, errPostBusy.Error(), http.StatusTooManyRequests)
		return
	}
	m := s.addPost(Message{From: ip, Agent: s.agentOf(ip), Text: text})
	s.emit(func(sub *subscriber) bool { return sub.ip == ip }, "post", postJSON(m, ip))
	if s.cfg.OnPost != nil {
		s.cfg.OnPost(m)
	}
	writeJSON(w, http.StatusOK, postJSON(m, ip))
}

// recentPosts counts the messages ip sent within the last d.
func (s *Server) recentPosts(ip string, d time.Duration) int {
	s.postMu.Lock()
	defer s.postMu.Unlock()
	n := 0
	since := time.Now().Add(-d)
	// Newest last: walk back until the window is passed.
	for i := len(s.posts) - 1; i >= 0 && s.posts[i].Time.After(since); i-- {
		if s.posts[i].From == ip {
			n++
		}
	}
	return n
}
