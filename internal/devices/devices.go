// Package devices keeps track of every client that talks to the server and
// decides who may: the Devices panel lists them, and with --ask the operator
// approves each new device (and each upload) from the dashboard before it
// gets anything.
//
// A device is identified by its IP address. That is good enough on a home
// network for the duration of a session, and it is what the request log and
// the transfer list already show.
package devices

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Status is whether a device may use the server.
type Status int

const (
	// Pending: --ask is on and the operator hasn't decided yet.
	Pending Status = iota
	// Allowed: requests are served.
	Allowed
	// Blocked: every request is refused.
	Blocked
)

// String names the status for display.
func (s Status) String() string {
	switch s {
	case Pending:
		return "pending"
	case Allowed:
		return "allowed"
	}
	return "blocked"
}

// Device is a snapshot of one client.
type Device struct {
	IP        string
	Agent     string // short nickname from the User-Agent, e.g. "iPhone · Safari"
	FirstSeen time.Time
	LastSeen  time.Time
	Requests  int64
	Sent      int64 // bytes sent to the device
	Received  int64 // bytes the device uploaded
	Status    Status
	Trusted   bool // remembered across runs ("always allow")
	Local     bool // this machine (loopback): always allowed
}

// Label is the device's display name: nickname and address.
func (d Device) Label() string {
	if d.Agent == "" {
		return d.IP
	}
	return d.Agent + " (" + d.IP + ")"
}

// PromptKind is what a prompt asks the operator to decide.
type PromptKind int

const (
	// PromptDevice: a new device wants in.
	PromptDevice PromptKind = iota
	// PromptUpload: a device wants to put a file in the inbox.
	PromptUpload
)

// Prompt is a decision waiting for the operator.
type Prompt struct {
	ID      uint64
	Kind    PromptKind
	IP      string
	Agent   string
	Name    string // upload file name
	Size    int64  // upload size, -1 if unknown
	Created time.Time
}

// prompt is the live version of a Prompt; done closes once it is decided.
type prompt struct {
	Prompt
	done  chan struct{}
	allow bool
}

// Registry is the concurrency-safe device list.
type Registry struct {
	ask       bool
	trustFile string // "" disables "always allow"

	mu      sync.Mutex
	devices map[string]*Device
	trusted map[string]bool
	prompts []*prompt
	nextID  uint64
}

// New creates a registry. With ask set, unknown devices must be approved.
// trustFile, if not empty, persists "always allow" decisions between runs.
func New(ask bool, trustFile string) *Registry {
	r := &Registry{ask: ask, trustFile: trustFile, devices: map[string]*Device{}, trusted: map[string]bool{}}
	r.loadTrusted()
	return r
}

// Ask reports whether new devices need approval.
func (r *Registry) Ask() bool { return r.ask }

// TrustFile returns the default path for remembered devices:
// <user config dir>/<app>/trusted.json, or "" if there is no config dir.
func TrustFile(app string) string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, app, "trusted.json")
}

// touchLocked records a request from ip and returns its device, creating it
// (and, in ask mode, its approval prompt) on first contact.
func (r *Registry) touchLocked(ip, ua string) *Device {
	now := time.Now()
	d, ok := r.devices[ip]
	if !ok {
		d = &Device{IP: ip, FirstSeen: now, Local: isLocal(ip), Trusted: r.trusted[ip]}
		// Who gets in without asking: everyone when --ask is off, this
		// machine, and devices remembered with "always allow".
		switch {
		case !r.ask || d.Local || d.Trusted:
			d.Status = Allowed
		default:
			d.Status = Pending
		}
		r.devices[ip] = d
	}
	d.LastSeen = now
	d.Requests++
	// Keep the nickname current; browsers on one IP can differ (curl vs Safari).
	if ua != "" {
		d.Agent = Nickname(ua)
	}
	// A pending device always has exactly one open prompt.
	if d.Status == Pending && r.findLocked(PromptDevice, ip) == nil {
		r.addPromptLocked(Prompt{Kind: PromptDevice, IP: ip, Agent: d.Agent})
	}
	return d
}

// Admit records a request and returns the device's status. A pending device
// waits up to wait (or until ctx ends) for the operator to decide.
func (r *Registry) Admit(ctx context.Context, ip, ua string, wait time.Duration) Status {
	r.mu.Lock()
	d := r.touchLocked(ip, ua)
	st := d.Status
	var p *prompt
	if st == Pending {
		p = r.findLocked(PromptDevice, ip)
	}
	r.mu.Unlock()
	// Decided already (or nothing to wait on): answer at once.
	if st != Pending || p == nil {
		return st
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-p.done:
	case <-t.C:
	case <-ctx.Done():
	}
	return r.Status(ip)
}

// Status returns a device's current status (Allowed for unknown devices
// when ask is off, Pending when it is on).
func (r *Registry) Status(ip string) Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.devices[ip]; ok {
		return d.Status
	}
	if r.ask && !isLocal(ip) && !r.trusted[ip] {
		return Pending
	}
	return Allowed
}

// AskUpload asks the operator whether ip may upload name (size bytes). It
// returns true at once when ask mode is off, and false if ctx ends first.
func (r *Registry) AskUpload(ctx context.Context, ip, ua, name string, size int64) bool {
	if !r.ask {
		return true
	}
	r.mu.Lock()
	d := r.touchLocked(ip, ua)
	p := r.addPromptLocked(Prompt{Kind: PromptUpload, IP: ip, Agent: d.Agent, Name: name, Size: size})
	r.mu.Unlock()
	select {
	case <-p.done:
		return p.allow
	case <-ctx.Done():
		// The uploader gave up: withdraw the question.
		r.mu.Lock()
		r.removeLocked(p)
		r.mu.Unlock()
		return false
	}
}

// Decide answers a prompt. For a device prompt, allow admits the device
// (remember also trusts it in future runs) and !allow blocks it.
func (r *Registry) Decide(id uint64, allow, remember bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Find the prompt; an unknown ID was already answered or withdrawn.
	for _, p := range r.prompts {
		if p.ID != id {
			continue
		}
		if p.Kind == PromptDevice {
			r.setLocked(p.IP, allow, remember)
		}
		p.allow = allow
		r.removeLocked(p)
		return
	}
}

// Allow admits a device (and, with remember, trusts it in future runs).
func (r *Registry) Allow(ip string, remember bool) { r.decideDevice(ip, true, remember) }

// Block refuses every further request from a device.
func (r *Registry) Block(ip string) { r.decideDevice(ip, false, false) }

// decideDevice sets a device's status and settles its open prompt, if any.
func (r *Registry) decideDevice(ip string, allow, remember bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setLocked(ip, allow, remember)
	if p := r.findLocked(PromptDevice, ip); p != nil {
		p.allow = allow
		r.removeLocked(p)
	}
}

// setLocked applies an allow/block decision to a device.
func (r *Registry) setLocked(ip string, allow, remember bool) {
	d, ok := r.devices[ip]
	if !ok {
		d = &Device{IP: ip, FirstSeen: time.Now(), LastSeen: time.Now(), Local: isLocal(ip)}
		r.devices[ip] = d
	}
	d.Status = Blocked
	if allow {
		d.Status = Allowed
	}
	// Remembering is only ever for allowing; blocking forgets any trust.
	switch {
	case allow && remember:
		d.Trusted = true
		r.trusted[ip] = true
		r.saveTrusted()
	case !allow && d.Trusted:
		d.Trusted = false
		delete(r.trusted, ip)
		r.saveTrusted()
	}
}

// Forget removes a device, so in ask mode it must be approved again. Its
// trust is kept; use Block to revoke that.
func (r *Registry) Forget(ip string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.devices, ip)
	// Withdraw anything it was waiting for; its requests will be refused.
	for _, p := range append([]*prompt(nil), r.prompts...) {
		if p.IP == ip {
			r.removeLocked(p)
		}
	}
}

// AddTraffic credits bytes sent to and received from a device.
func (r *Registry) AddTraffic(ip string, sent, received int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.devices[ip]; ok {
		d.Sent += sent
		d.Received += received
	}
}

// List returns every device, oldest first.
func (r *Registry) List() []Device {
	r.mu.Lock()
	out := make([]Device, 0, len(r.devices))
	// Copy so callers can't race with updates.
	for _, d := range r.devices {
		out = append(out, *d)
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].FirstSeen.Equal(out[j].FirstSeen) {
			return out[i].FirstSeen.Before(out[j].FirstSeen)
		}
		return out[i].IP < out[j].IP
	})
	return out
}

// Get returns one device.
func (r *Registry) Get(ip string) (Device, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.devices[ip]
	if !ok {
		return Device{}, false
	}
	return *d, true
}

// Prompts returns the open prompts, oldest first.
func (r *Registry) Prompts() []Prompt {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Prompt, len(r.prompts))
	// Copy the public part only.
	for i, p := range r.prompts {
		out[i] = p.Prompt
	}
	return out
}

// findLocked returns the open prompt of a kind for ip, if any.
func (r *Registry) findLocked(k PromptKind, ip string) *prompt {
	// Prompts are few; a linear scan is fine.
	for _, p := range r.prompts {
		if p.Kind == k && p.IP == ip {
			return p
		}
	}
	return nil
}

// addPromptLocked queues a new prompt.
func (r *Registry) addPromptLocked(pr Prompt) *prompt {
	r.nextID++
	pr.ID = r.nextID
	pr.Created = time.Now()
	p := &prompt{Prompt: pr, done: make(chan struct{})}
	r.prompts = append(r.prompts, p)
	return p
}

// removeLocked takes a prompt off the queue and wakes whoever waits on it.
func (r *Registry) removeLocked(p *prompt) {
	// Remove by identity; closing done wakes every waiter.
	for i, x := range r.prompts {
		if x == p {
			r.prompts = append(r.prompts[:i], r.prompts[i+1:]...)
			close(p.done)
			return
		}
	}
}

// loadTrusted reads the remembered devices, ignoring a missing file.
func (r *Registry) loadTrusted() {
	if r.trustFile == "" {
		return
	}
	b, err := os.ReadFile(r.trustFile)
	if err != nil {
		return
	}
	var ips []string
	if json.Unmarshal(b, &ips) != nil {
		return
	}
	// Only well-formed addresses are taken back.
	for _, ip := range ips {
		if _, err := netip.ParseAddr(ip); err == nil {
			r.trusted[ip] = true
		}
	}
}

// saveTrusted writes the remembered devices. Failures are ignored: the
// decision still holds for this session.
func (r *Registry) saveTrusted() {
	if r.trustFile == "" {
		return
	}
	ips := make([]string, 0, len(r.trusted))
	for ip := range r.trusted {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	b, _ := json.MarshalIndent(ips, "", "  ")
	if os.MkdirAll(filepath.Dir(r.trustFile), 0o700) == nil {
		_ = os.WriteFile(r.trustFile, append(b, '\n'), 0o600)
	}
}

// isLocal reports whether ip is this machine's loopback address.
func isLocal(ip string) bool {
	a, err := netip.ParseAddr(ip)
	return err == nil && a.IsLoopback()
}

// Nickname turns a User-Agent into a short "platform · client" label.
func Nickname(ua string) string {
	l := strings.ToLower(ua)
	// Command-line clients name themselves in the string.
	for _, cli := range []struct{ prefix, name string }{
		{"curl/", "curl"}, {"wget/", "Wget"}, {"pidgeon", "Pidgeon"}, {"go-http-client", "Go client"},
		{"python-requests", "Python"}, {"python-urllib", "Python"}, {"powershell", "PowerShell"}, {"aria2", "aria2"},
	} {
		if strings.Contains(l, cli.prefix) {
			return cli.name
		}
	}
	platform := ""
	// Order matters: iPads report "Macintosh" and Android reports "Linux".
	switch {
	case strings.Contains(l, "iphone"):
		platform = "iPhone"
	case strings.Contains(l, "ipad"):
		platform = "iPad"
	case strings.Contains(l, "android"):
		platform = "Android"
	case strings.Contains(l, "windows"):
		platform = "Windows"
	case strings.Contains(l, "cros"):
		platform = "ChromeOS"
	case strings.Contains(l, "macintosh"), strings.Contains(l, "mac os"):
		platform = "Mac"
	case strings.Contains(l, "linux"):
		platform = "Linux"
	}
	browser := ""
	// Most specific first: every Chromium browser also says "Chrome" and "Safari".
	switch {
	case strings.Contains(l, "edg/"), strings.Contains(l, "edga/"), strings.Contains(l, "edgios/"):
		browser = "Edge"
	case strings.Contains(l, "opr/"), strings.Contains(l, "opera"):
		browser = "Opera"
	case strings.Contains(l, "samsungbrowser"):
		browser = "Samsung Internet"
	case strings.Contains(l, "firefox/"), strings.Contains(l, "fxios/"):
		browser = "Firefox"
	case strings.Contains(l, "chrome/"), strings.Contains(l, "crios/"):
		browser = "Chrome"
	case strings.Contains(l, "safari/"):
		browser = "Safari"
	}
	switch {
	case platform != "" && browser != "":
		return platform + " · " + browser
	case platform != "":
		return platform
	case browser != "":
		return browser
	}
	// Unknown: the product token is better than nothing.
	if tok, _, _ := strings.Cut(ua, " "); tok != "" {
		if name, _, _ := strings.Cut(tok, "/"); len(name) <= 24 {
			return name
		}
	}
	return ""
}
