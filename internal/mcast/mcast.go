// Package mcast listens to an IPv4 multicast group on every network
// interface, for the discovery protocols (mDNS and SSDP). It needs no
// dependencies beyond the standard library and golang.org/x/sys.
//
// One socket is joined per interface, so announcements leave through the
// right interface. All of them are bound to the group's port, so the
// kernel may hand the same packet to several of them; packets are therefore
// de-duplicated, and each is attributed to the interface whose subnet
// contains the sender.
package mcast

import (
	"context"
	"hash/fnv"
	"net"
	"net/netip"
	"sync"
	"time"
)

// refreshEvery is how often interfaces are re-scanned (Wi-Fi joined, VPN up).
const refreshEvery = 30 * time.Second

// dedupeWindow is how long a packet is remembered to drop its copies.
const dedupeWindow = 2 * time.Second

// Iface is one interface a packet arrived on or will leave through.
type Iface struct {
	Name  string
	Addrs []netip.Prefix // its IPv4 addresses with their subnets
}

// Local returns the interface address in the same subnet as peer, else its
// first address.
func (i Iface) Local(peer netip.Addr) netip.Addr {
	for _, p := range i.Addrs {
		if p.Contains(peer) {
			return p.Addr()
		}
	}
	if len(i.Addrs) > 0 {
		return i.Addrs[0].Addr()
	}
	return netip.Addr{}
}

// Packet is one datagram received from the group (or sent to us directly).
type Packet struct {
	Data  []byte
	Src   netip.AddrPort
	Iface Iface // the interface facing the sender

	conn *net.UDPConn
}

// Reply sends data straight back to the packet's sender.
func (p Packet) Reply(data []byte) error {
	_, err := p.conn.WriteToUDPAddrPort(data, p.Src)
	return err
}

// Group is a set of sockets joined to one multicast group.
type Group struct {
	group   netip.AddrPort
	handler func(Packet)

	mu     sync.Mutex
	socks  map[string]*sock // by interface name
	recent map[uint64]time.Time
	closed bool
	cancel context.CancelFunc
}

// sock is one interface's socket.
type sock struct {
	conn  *net.UDPConn
	iface Iface
}

// Listen joins group on every suitable interface and calls h for each
// packet received (from any goroutine; h must be safe for that). It keeps
// following interface changes until Close. It fails only if no interface
// could be joined at all.
func Listen(group netip.AddrPort, h func(Packet)) (*Group, error) {
	ctx, cancel := context.WithCancel(context.Background())
	g := &Group{group: group, handler: h, socks: map[string]*sock{}, recent: map[uint64]time.Time{}, cancel: cancel}
	if err := g.refresh(); err != nil {
		cancel()
		return nil, err
	}
	go g.keepFresh(ctx)
	return g, nil
}

// keepFresh re-scans the interfaces until the group is closed.
func (g *Group) keepFresh(ctx context.Context) {
	tk := time.NewTicker(refreshEvery)
	defer tk.Stop()
	// One rescan per tick.
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			g.refresh()
		}
	}
}

// refresh opens sockets on new interfaces and closes those of vanished ones.
func (g *Group) refresh() error {
	ifs := interfaces()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return net.ErrClosed
	}
	seen := map[string]bool{}
	var firstErr error
	// Join the group on each interface not joined yet.
	for _, it := range ifs {
		seen[it.iface.Name] = true
		if s, ok := g.socks[it.iface.Name]; ok {
			s.iface = it.iface // addresses may have changed
			continue
		}
		ifi := it.ifi
		conn, err := net.ListenMulticastUDP("udp4", &ifi, net.UDPAddrFromAddrPort(g.group))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		setLoopback(conn) // so programs on this machine see us too
		s := &sock{conn: conn, iface: it.iface}
		g.socks[it.iface.Name] = s
		go g.read(s)
	}
	// Interfaces that went away take their sockets with them.
	for name, s := range g.socks {
		if !seen[name] {
			s.conn.Close()
			delete(g.socks, name)
		}
	}
	if len(g.socks) == 0 {
		if firstErr == nil {
			firstErr = net.UnknownNetworkError("no multicast-capable network interface")
		}
		return firstErr
	}
	return nil
}

// read delivers one socket's packets until it is closed.
func (g *Group) read(s *sock) {
	buf := make([]byte, 9000)
	// Each datagram is checked for duplicates, then handed on.
	for {
		n, src, err := s.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		src = netip.AddrPortFrom(src.Addr().Unmap(), src.Port())
		data := append([]byte(nil), buf[:n]...)
		if g.duplicate(src, data) {
			continue
		}
		g.handler(Packet{Data: data, Src: src, Iface: g.ifaceFor(src.Addr(), s.iface), conn: s.conn})
	}
}

// duplicate reports whether this packet was already seen moments ago
// (through another interface's socket).
func (g *Group) duplicate(src netip.AddrPort, data []byte) bool {
	h := fnv.New64a()
	b, _ := src.MarshalBinary()
	h.Write(b)
	h.Write(data)
	k := h.Sum64()
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	// Forget old packets as we go.
	for key, at := range g.recent {
		if now.Sub(at) > dedupeWindow {
			delete(g.recent, key)
		}
	}
	if _, ok := g.recent[k]; ok {
		return true
	}
	g.recent[k] = now
	return false
}

// ifaceFor finds the interface whose subnet contains addr, falling back to
// the socket's own.
func (g *Group) ifaceFor(addr netip.Addr, fallback Iface) Iface {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, s := range g.socks {
		for _, p := range s.iface.Addrs {
			if p.Contains(addr) {
				return s.iface
			}
		}
	}
	return fallback
}

// Multicast sends a datagram to the group through every interface. build
// makes the datagram for each interface (so it can name the address the
// interface is reached on); returning nil skips that interface.
func (g *Group) Multicast(build func(Iface) []byte) {
	g.mu.Lock()
	socks := make([]*sock, 0, len(g.socks))
	for _, s := range g.socks {
		socks = append(socks, s)
	}
	g.mu.Unlock()
	dst := net.UDPAddrFromAddrPort(g.group)
	for _, s := range socks {
		if data := build(s.iface); data != nil {
			s.conn.WriteToUDP(data, dst)
		}
	}
}

// Close leaves the group.
func (g *Group) Close() {
	g.cancel()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	for name, s := range g.socks {
		s.conn.Close()
		delete(g.socks, name)
	}
}

// netIface pairs an interface with its IPv4 subnets.
type netIface struct {
	ifi   net.Interface
	iface Iface
}

// interfaces lists the interfaces that are up, can multicast, aren't
// loopback and have an IPv4 address.
func interfaces() []netIface {
	all, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []netIface
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		it := Iface{Name: ifi.Name}
		// Keep the IPv4 subnets; discovery here is IPv4 only.
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok || !ip.Unmap().Is4() || ip.IsLinkLocalUnicast() {
				continue
			}
			bits, _ := ipn.Mask.Size()
			if len(ipn.Mask) == 16 {
				bits -= 96
			}
			it.Addrs = append(it.Addrs, netip.PrefixFrom(ip.Unmap(), bits))
		}
		if len(it.Addrs) > 0 {
			out = append(out, netIface{ifi: ifi, iface: it})
		}
	}
	return out
}

// Query sends one datagram to the group through every interface, from a
// fresh port, and calls onReply for every answer that comes back to that
// port until ctx ends. This is how a client asks "who's there?" without
// claiming the group's port itself.
func Query(ctx context.Context, group netip.AddrPort, data []byte, onReply func(data []byte, src netip.AddrPort)) error {
	var conns []*net.UDPConn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	var mu sync.Mutex
	var wg sync.WaitGroup
	var lastErr error
	dst := net.UDPAddrFromAddrPort(group)
	// One sending socket per interface; replies come back to each.
	for _, it := range interfaces() {
		ifi := it.ifi
		c, err := net.ListenMulticastUDP("udp4", &ifi, &net.UDPAddr{IP: dst.IP, Port: 0})
		if err != nil {
			lastErr = err
			continue
		}
		setLoopback(c)
		conns = append(conns, c)
		if _, err := c.WriteToUDP(data, dst); err != nil {
			lastErr = err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 9000)
			// Read until the socket is closed when ctx ends.
			for {
				n, src, err := c.ReadFromUDPAddrPort(buf)
				if err != nil {
					return
				}
				mu.Lock()
				onReply(append([]byte(nil), buf[:n]...), netip.AddrPortFrom(src.Addr().Unmap(), src.Port()))
				mu.Unlock()
			}
		}()
	}
	if len(conns) == 0 {
		if lastErr == nil {
			lastErr = net.UnknownNetworkError("no multicast-capable network interface")
		}
		return lastErr
	}
	<-ctx.Done()
	for _, c := range conns {
		c.Close()
	}
	wg.Wait()
	return nil
}
