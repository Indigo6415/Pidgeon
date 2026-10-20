package listeners

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// maxPortTries bounds how far the port auto-increment searches.
const maxPortTries = 50

// ConnHooks lets the stats store observe TCP connections opening and closing
// on any listener without this package importing it.
type ConnHooks interface {
	ConnOpened()
	ConnClosed()
}

// Info is a read-only snapshot of one listener for the TUI / startup output.
type Info struct {
	Candidate
	Port    int    // bound (or last bound) port
	Enabled bool   // the user wants this address served
	Bound   bool   // a socket is actually open for this exact address
	Covered bool   // enabled, but served through the wildcard listener
	Err     string // last bind error, if any
	Conns   int    // open TCP connections attributed to this address
}

// Serving reports whether the address is reachable right now.
func (i Info) Serving() bool { return i.Bound || i.Covered }

// URL returns the http URL for this listener ("" if it was never bound).
func (i Info) URL() string {
	if i.Port == 0 {
		return ""
	}
	return "http://" + i.Host() + ":" + strconv.Itoa(i.Port) + "/"
}

// listener is the mutable per-address state. All fields are guarded by
// Manager.mu.
type listener struct {
	cand  Candidate
	want  bool
	port  int
	err   string
	srv   *http.Server
	ln    net.Listener
	drain chan struct{} // closed when this server starts shutting down
}

// Manager owns one *http.Server per bound address. All servers share the same
// handler, so enabling/disabling one never disturbs requests on the others.
//
// The wildcard listener is special: most OSes refuse to bind 0.0.0.0:P while
// 192.168.x.y:P is bound (or vice versa), so enabling the wildcard parks the
// specific listeners ("covered") and disabling it restores them.
type Manager struct {
	mu       sync.Mutex
	handler  http.Handler
	hooks    ConnHooks
	port     int // the port all listeners try first
	items    []*listener
	byID     map[string]*listener
	conns    map[string]int            // open conns keyed by resolved listener ID
	draining map[*http.Server]struct{} // disabled servers finishing in-flight work
	closed   bool

	// baseCtx is the parent of every request context. It is cancelled only on
	// a forced shutdown, which aborts whatever is still running.
	baseCtx   context.Context
	cancelAll context.CancelFunc
}

// NewManager creates a manager for the given candidates. Nothing is bound
// until Start is called.
func NewManager(cands []Candidate, handler http.Handler, port int, hooks ConnHooks) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		handler:   handler,
		hooks:     hooks,
		port:      port,
		byID:      map[string]*listener{},
		conns:     map[string]int{},
		draining:  map[*http.Server]struct{}{},
		baseCtx:   ctx,
		cancelAll: cancel,
	}
	// Register every candidate; none are wanted until Start says so.
	for _, c := range cands {
		l := &listener{cand: c}
		m.items = append(m.items, l)
		m.byID[c.ID] = l
	}
	return m
}

// AddCustom registers an address that discovery didn't find (from --bind).
func (m *Manager) AddCustom(ip netip.Addr) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := ip.String()
	// Already known: nothing to add.
	if _, ok := m.byID[id]; ok {
		return id
	}
	l := &listener{cand: Candidate{ID: id, Addr: ip, Kind: KindCustom}}
	// Insert before the wildcard so it stays last in the list.
	n := len(m.items)
	if n > 0 && m.items[n-1].cand.ID == WildcardID {
		m.items = append(m.items[:n-1], l, m.items[n-1])
	} else {
		m.items = append(m.items, l)
	}
	m.byID[id] = l
	return id
}

// Start binds the wanted listeners. It searches upward from the configured
// port until one is free on every wanted address, so all URLs share a port.
// It returns the chosen port, or an error if nothing could be bound.
func (m *Manager) Start(ids []string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Mark the requested listeners as wanted.
	for _, id := range ids {
		if l, ok := m.byID[id]; ok {
			l.want = true
		}
	}
	targets := m.activeTargetsLocked()
	if len(targets) == 0 {
		return 0, errors.New("no listen addresses selected")
	}

	// Try ports in turn; a port is accepted when no target reports "in use".
	for try := range maxPortTries {
		port := m.port + try
		opened := make(map[*listener]net.Listener)
		inUse := false
		// Attempt every target on this port before deciding.
		for _, l := range targets {
			ln, err := listen(l.cand, port)
			if err != nil {
				// "In use" means try the next port; any other error (e.g. the
				// address isn't on this machine) is reported per listener.
				if isAddrInUse(err) {
					inUse = true
					break
				}
				l.err = friendlyBindError(l.cand, port, err)
				continue
			}
			opened[l] = ln
		}
		// Port clash somewhere: release what we grabbed and move on.
		if inUse {
			for _, ln := range opened {
				ln.Close()
			}
			for _, l := range targets {
				l.err = ""
			}
			continue
		}
		if len(opened) == 0 {
			return 0, fmt.Errorf("could not bind any address on port %d", port)
		}
		m.port = port
		// Hand each opened socket to its own http.Server.
		for l, ln := range opened {
			l.port = port
			l.err = ""
			m.serveLocked(l, ln)
		}
		return port, nil
	}
	return 0, fmt.Errorf("ports %d–%d are all in use", m.port, m.port+maxPortTries-1)
}

// Port returns the shared port chosen by Start.
func (m *Manager) Port() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.port
}

// Toggle flips a listener between enabled and disabled, binding or draining
// it live. Errors are also recorded on the listener for display.
func (m *Manager) Toggle(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.byID[id]
	// Unknown IDs can only come from a stale UI; ignore them.
	if !ok || m.closed {
		return nil
	}

	wild := m.byID[WildcardID]
	isWild := id == WildcardID

	switch {
	case isWild && !l.want:
		// Enabling the wildcard: park the specific listeners, then bind *.
		parked := []*listener{}
		for _, x := range m.items {
			if x != l && x.srv != nil {
				m.drainLocked(x)
				parked = append(parked, x)
			}
		}
		l.want = true
		if err := m.bindLocked(l); err != nil {
			// Couldn't bind the wildcard: restore what we just parked.
			l.want = false
			for _, x := range parked {
				m.bindLocked(x)
			}
			return err
		}
		return nil

	case isWild && l.want:
		// Disabling the wildcard: drain it and re-bind everything it covered.
		l.want = false
		m.drainLocked(l)
		var firstErr error
		for _, x := range m.items {
			if x != l && x.want {
				if err := m.bindLocked(x); err != nil && firstErr == nil {
					firstErr = err
				}
			}
		}
		return firstErr

	case wild != nil && wild.want:
		// The wildcard already serves this address; just remember the choice
		// so it is restored when the wildcard is switched off.
		l.want = !l.want
		return nil

	case l.want:
		l.want = false
		m.drainLocked(l)
		return nil

	default:
		l.want = true
		if err := m.bindLocked(l); err != nil {
			l.want = false
			return err
		}
		return nil
	}
}

// List returns a snapshot of every listener in display order.
func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	wild := m.byID[WildcardID]
	wildOn := wild != nil && wild.srv != nil
	out := make([]Info, 0, len(m.items))
	// Build an Info for each listener, including covered state.
	for _, l := range m.items {
		port := l.port
		covered := wildOn && l != wild && l.want
		// Covered addresses are reachable on the wildcard's port.
		if covered {
			port = wild.port
		}
		// Never-bound listeners show the port they would try.
		if port == 0 {
			port = m.port
		}
		out = append(out, Info{
			Candidate: l.cand,
			Port:      port,
			Enabled:   l.want,
			Bound:     l.srv != nil,
			Covered:   covered,
			Err:       l.err,
			Conns:     m.conns[l.cand.ID],
		})
	}
	return out
}

// Shutdown stops accepting on every listener and waits for in-flight requests
// until ctx expires, then force-closes whatever remains.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	var servers []*http.Server
	// Collect live servers and move them to draining.
	for _, l := range m.items {
		if l.srv != nil {
			m.drainLocked(l)
		}
	}
	for s := range m.draining {
		servers = append(servers, s)
	}
	m.mu.Unlock()

	// Shut every server down concurrently so one slow client can't delay
	// the others' graceful close.
	var wg sync.WaitGroup
	errs := make(chan error, len(servers))
	for _, s := range servers {
		wg.Add(1)
		go func(s *http.Server) {
			defer wg.Done()
			if err := s.Shutdown(ctx); err != nil {
				errs <- err
			}
		}(s)
	}
	wg.Wait()
	close(errs)

	var err error
	// Any error here means the grace period ran out: abort the rest.
	for e := range errs {
		err = e
	}
	if err != nil {
		m.cancelAll()
		for _, s := range servers {
			s.Close()
		}
	}
	return err
}

// activeTargetsLocked returns the listeners that need their own socket: the
// wildcard alone if wanted, otherwise every wanted specific address.
func (m *Manager) activeTargetsLocked() []*listener {
	if w := m.byID[WildcardID]; w != nil && w.want {
		return []*listener{w}
	}
	var out []*listener
	// Collect every specific listener the user wants enabled.
	for _, l := range m.items {
		if l.want && l.cand.ID != WildcardID {
			out = append(out, l)
		}
	}
	return out
}

// bindLocked opens a socket for l on its previous port (or the shared port),
// stepping upward a few ports if that one has been taken in the meantime.
func (m *Manager) bindLocked(l *listener) error {
	// Already serving: nothing to do.
	if l.srv != nil {
		return nil
	}
	base := l.port
	if base == 0 {
		base = m.port
	}
	var lastErr error
	// Search a small window of ports; non-"in use" errors stop immediately.
	for p := base; p < base+20; p++ {
		ln, err := listen(l.cand, p)
		if err == nil {
			l.port = p
			l.err = ""
			m.serveLocked(l, ln)
			return nil
		}
		lastErr = err
		if !isAddrInUse(err) {
			break
		}
	}
	l.err = friendlyBindError(l.cand, base, lastErr)
	return errors.New(l.err)
}

// serveLocked starts an http.Server for l on ln in its own goroutine.
func (m *Manager) serveLocked(l *listener, ln net.Listener) {
	drain := make(chan struct{})
	ownID := l.cand.ID
	srv := &http.Server{
		Handler:           m.handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: large downloads legitimately take a long time.

		// net/http logs to stderr by default, which would scribble over the
		// TUI. Errors that matter surface through the stats request log.
		ErrorLog: log.New(io.Discard, "", 0),

		// Every request context carries the drain channel so long-lived
		// handlers (SSE) can exit when their listener is disabled.
		BaseContext: func(net.Listener) context.Context {
			return context.WithValue(m.baseCtx, drainKey{}, (<-chan struct{})(drain))
		},
		// Attribute each connection to the specific address it arrived on,
		// so traffic through the wildcard still lights up the right row.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, idKey{}, m.resolveID(ownID, c))
		},
		ConnState: func(c net.Conn, st http.ConnState) {
			m.trackConn(ownID, c, st)
		},
	}
	l.srv = srv
	l.ln = ln
	l.drain = drain
	go srv.Serve(ln)
}

// drainLocked stops l accepting new connections and lets in-flight requests
// finish in the background. Long-lived handlers watch the drain channel.
func (m *Manager) drainLocked(l *listener) {
	// Nothing bound: nothing to drain.
	if l.srv == nil {
		return
	}
	srv := l.srv
	// Close the socket synchronously so the port is free the moment this
	// returns (the wildcard may want to bind it next). Shutdown below then
	// only has to wait for in-flight connections.
	l.ln.Close()
	close(l.drain)
	l.srv, l.ln, l.drain = nil, nil, nil
	m.draining[srv] = struct{}{}
	go func() {
		// Shutdown closes the listening socket immediately and returns once
		// every active connection has gone idle.
		srv.Shutdown(context.Background())
		m.mu.Lock()
		delete(m.draining, srv)
		m.mu.Unlock()
	}()
}

// resolveID maps a connection to the listener ID matching its local address.
func (m *Manager) resolveID(ownID string, c net.Conn) string {
	// Only the wildcard needs resolving; specific listeners know their address.
	if ownID != WildcardID {
		return ownID
	}
	ap, err := netip.ParseAddrPort(c.LocalAddr().String())
	if err != nil {
		return ownID
	}
	id := ap.Addr().Unmap().WithZone("").String()
	m.mu.Lock()
	_, ok := m.byID[id]
	m.mu.Unlock()
	if ok {
		return id
	}
	return ownID
}

// trackConn keeps per-listener and global connection counts in sync with
// net/http's connection state machine.
func (m *Manager) trackConn(ownID string, c net.Conn, st http.ConnState) {
	switch st {
	case http.StateNew:
		id := m.resolveID(ownID, c)
		m.mu.Lock()
		m.conns[id]++
		m.mu.Unlock()
		if m.hooks != nil {
			m.hooks.ConnOpened()
		}
	case http.StateClosed, http.StateHijacked:
		id := m.resolveID(ownID, c)
		m.mu.Lock()
		// Guard against going negative if a state is reported twice.
		if m.conns[id] > 0 {
			m.conns[id]--
		}
		m.mu.Unlock()
		if m.hooks != nil {
			m.hooks.ConnClosed()
		}
	}
}

// listen opens a TCP socket for cand on port. The wildcard uses ":port",
// which is dual-stack (IPv4 + IPv6) on every major OS.
func listen(cand Candidate, port int) (net.Listener, error) {
	addr := ":" + strconv.Itoa(port)
	if cand.ID != WildcardID {
		addr = net.JoinHostPort(cand.Addr.String(), strconv.Itoa(port))
	}
	return net.Listen("tcp", addr)
}

// isAddrInUse reports whether err means the port is taken. Windows reports
// WSAEADDRINUSE, which doesn't match syscall.EADDRINUSE, hence the string test.
func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "address already in use") || strings.Contains(s, "only one usage of each socket address")
}

// friendlyBindError turns a raw bind error into a short, actionable message.
func friendlyBindError(c Candidate, port int, err error) string {
	host := c.Host()
	if c.ID == WildcardID {
		host = "*"
	}
	msg := err.Error()
	switch {
	case isAddrInUse(err):
		msg = "port in use"
	case errors.Is(err, syscall.EADDRNOTAVAIL) || strings.Contains(strings.ToLower(msg), "assign requested address"):
		msg = "address not available on this machine"
	case errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM):
		msg = "permission denied (ports below 1024 may need root)"
	}
	return fmt.Sprintf("can't bind %s:%d: %s", host, port, msg)
}

type idKey struct{}
type drainKey struct{}

// IDFromContext returns the listener ID a request arrived through.
func IDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(idKey{}).(string)
	return id
}

// DrainFromContext returns a channel that closes when the request's listener
// is disabled or the server is shutting down. Nil if unknown.
func DrainFromContext(ctx context.Context) <-chan struct{} {
	ch, _ := ctx.Value(drainKey{}).(<-chan struct{})
	return ch
}
