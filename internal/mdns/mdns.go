// Package mdns announces a Pidgeon server on the local network over
// multicast DNS (RFC 6762) as a "_pidgeon._tcp" service (DNS-SD, RFC 6763),
// and finds the other ones: `pidgeon find`, `pidgeon pull` without a URL,
// and the web UI's "Nearby" list.
//
// The responder only answers questions; it doesn't send unsolicited
// announcements. Answers go straight back to whoever asked (unicast), which
// every browser here relies on, and which also keeps the network quiet.
package mdns

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/Indigo6415/pidgeon/internal/mcast"
)

// group is the mDNS multicast address.
var group = netip.MustParseAddrPort("224.0.0.251:5353")

// Names this package answers for.
var (
	serviceName  = name{"_pidgeon", "_tcp", "local"}
	servicesName = name{"_services", "_dns-sd", "_udp", "local"}
)

// ttl is how long answers may be cached (legacy unicast askers get less).
const (
	ttl       = 120
	legacyTTL = 10
)

// Service is the server being announced. The functions are called for
// every question, so the answer follows listeners being switched on or off.
type Service struct {
	Instance string                   // the name people see, e.g. "Movies on studio-mac:8080"
	Port     func() int               // the port it listens on
	Addrs    func() []netip.Addr      // addresses it is reachable on now; none = stay silent
	TXT      func() map[string]string // extra key=value facts (auth, tls, name, ...)
}

// Responder answers questions about one Service.
type Responder struct {
	svc  Service
	id   string // random, sent as TXT "id" so a browser can recognise itself
	host name   // our own host name, so it can't clash with the machine's
	inst name
	g    *mcast.Group
}

// Advertise starts answering for svc. It fails when the mDNS port can't be
// joined on any interface.
func Advertise(svc Service) (*Responder, error) {
	b := make([]byte, 4)
	rand.Read(b)
	id := hex.EncodeToString(b)
	r := &Responder{
		svc:  svc,
		id:   id,
		host: name{"pidgeon-" + id, "local"},
		inst: append(name{svc.Instance}, serviceName...),
	}
	g, err := mcast.Listen(group, r.handle)
	if err != nil {
		return nil, err
	}
	r.g = g
	return r, nil
}

// ID is the value this responder sends as TXT "id".
func (r *Responder) ID() string { return r.id }

// Close stops answering.
func (r *Responder) Close() { r.g.Close() }

// handle answers one incoming query.
func (r *Responder) handle(p mcast.Packet) {
	m, err := unpack(p.Data)
	if err != nil || m.flags&flagResponse != 0 {
		return
	}
	addrs := slices.Clone(r.svc.Addrs())
	if len(addrs) == 0 {
		return
	}
	// Addresses on the asker's own subnet go first: that's the one to use.
	slices.SortStableFunc(addrs, func(a, b netip.Addr) int {
		return boolRank(onIface(p.Iface, b)) - boolRank(onIface(p.Iface, a))
	})
	// Legacy unicast (a plain resolver asking from a random port) wants the
	// question echoed back and short TTLs.
	legacy := p.Src.Port() != group.Port()
	t := uint32(ttl)
	if legacy {
		t = legacyTTL
	}
	var ans, extra []record
	want := map[uint16]bool{}
	for _, q := range m.questions {
		qt := q.typ
		switch {
		case q.name.equal(servicesName) && (qt == typePTR || qt == typeANY):
			ans = append(ans, record{name: servicesName, typ: typePTR, class: classIN, ttl: t, target: serviceName})
		case q.name.equal(serviceName) && (qt == typePTR || qt == typeANY):
			ans = append(ans, record{name: serviceName, typ: typePTR, class: classIN, ttl: t, target: r.inst})
			want[typeSRV], want[typeTXT], want[typeA] = true, true, true
		case q.name.equal(r.inst):
			if qt == typeSRV || qt == typeANY {
				ans = append(ans, r.srv(t))
				want[typeA] = true
			}
			if qt == typeTXT || qt == typeANY {
				ans = append(ans, r.txt(t))
			}
		case q.name.equal(r.host) && (qt == typeA || qt == typeAAAA || qt == typeANY):
			ans = append(ans, r.addrRecords(addrs, t)...)
		}
	}
	if len(ans) == 0 {
		return
	}
	// Everything a browser needs comes in one packet.
	if want[typeSRV] {
		extra = append(extra, r.srv(t))
	}
	if want[typeTXT] {
		extra = append(extra, r.txt(t))
	}
	if want[typeA] {
		extra = append(extra, r.addrRecords(addrs, t)...)
	}
	resp := &message{flags: flagResponse | flagAA, answers: ans, additional: extra}
	if legacy {
		resp.id, resp.questions = m.id, m.questions
	}
	p.Reply(resp.pack())
}

// boolRank turns a bool into a sort key.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// onIface reports whether addr is one of the interface's subnets.
func onIface(it mcast.Iface, addr netip.Addr) bool {
	for _, p := range it.Addrs {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// srv is the service's SRV record.
func (r *Responder) srv(t uint32) record {
	return record{name: r.inst, typ: typeSRV, class: classIN | cacheFlush, ttl: t, port: uint16(r.svc.Port()), target: r.host}
}

// txt is the service's TXT record, keys sorted.
func (r *Responder) txt(t uint32) record {
	kv := map[string]string{}
	if r.svc.TXT != nil {
		kv = r.svc.TXT()
	}
	kv["id"] = r.id
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	strs := []string{"txtvers=1"}
	for _, k := range keys {
		strs = append(strs, k+"="+kv[k])
	}
	return record{name: r.inst, typ: typeTXT, class: classIN | cacheFlush, ttl: t, txt: strs}
}

// addrRecords are the A / AAAA records of our host name.
func (r *Responder) addrRecords(addrs []netip.Addr, t uint32) []record {
	out := make([]record, 0, len(addrs))
	for _, a := range addrs {
		typ := uint16(typeA)
		if !a.Is4() {
			typ = typeAAAA
		}
		out = append(out, record{name: r.host, typ: typ, class: classIN | cacheFlush, ttl: t, addr: a})
	}
	return out
}

// Peer is a Pidgeon server found on the network.
type Peer struct {
	Instance string
	Port     int
	Addrs    []netip.Addr      // what it says it is reachable on
	Src      netip.Addr        // the address its answer came from
	TXT      map[string]string // "id", "auth", "tls", "fp", "name", "version", ...
}

// Addr picks the address to connect to: the one the answer came from if
// it is among the advertised ones (it is on our network), else the first.
func (p Peer) Addr() netip.Addr {
	if slices.Contains(p.Addrs, p.Src) || len(p.Addrs) == 0 {
		return p.Src
	}
	return p.Addrs[0]
}

// URL is the server's address, "http(s)://host:port".
func (p Peer) URL() string {
	scheme := "http"
	if p.TXT["tls"] == "1" {
		scheme = "https"
	}
	return scheme + "://" + netip.AddrPortFrom(p.Addr(), uint16(p.Port)).String()
}

// Browse asks the network for Pidgeon servers and collects the answers
// until ctx ends (a second or two is plenty on a LAN).
func Browse(ctx context.Context) ([]Peer, error) {
	q := &message{questions: []question{{name: serviceName, typ: typePTR, class: classIN}}}
	peers := map[string]*Peer{}
	var order []string
	err := mcast.Query(ctx, group, q.pack(), func(data []byte, src netip.AddrPort) {
		m, err := unpack(data)
		if err != nil || m.flags&flagResponse == 0 {
			return
		}
		for _, p := range peersIn(m, src.Addr()) {
			key := strings.ToLower(p.Instance)
			if _, seen := peers[key]; !seen {
				order = append(order, key)
				peers[key] = &p
			}
		}
	})
	out := make([]Peer, 0, len(order))
	for _, k := range order {
		out = append(out, *peers[k])
	}
	return out, err
}

// peersIn reads the services described in one response.
func peersIn(m *message, src netip.Addr) []Peer {
	all := append(append(append([]record{}, m.answers...), m.authority...), m.additional...)
	var out []Peer
	// Every PTR to an instance of our service starts a peer.
	for _, r := range all {
		if r.typ != typePTR || !r.name.equal(serviceName) || len(r.target) == 0 || r.ttl == 0 {
			continue
		}
		p := Peer{Instance: r.target[0], Src: src, TXT: map[string]string{}}
		var host name
		for _, x := range all {
			switch {
			case x.typ == typeSRV && x.name.equal(r.target):
				p.Port, host = int(x.port), x.target
			case x.typ == typeTXT && x.name.equal(r.target):
				for _, s := range x.txt {
					k, v, _ := strings.Cut(s, "=")
					p.TXT[strings.ToLower(k)] = v
				}
			}
		}
		if p.Port == 0 {
			continue
		}
		for _, x := range all {
			if (x.typ == typeA || x.typ == typeAAAA) && x.name.equal(host) && x.addr.IsValid() {
				p.Addrs = append(p.Addrs, x.addr.Unmap())
			}
		}
		out = append(out, p)
	}
	return out
}

// InstanceName builds a readable, unique-enough instance name:
// "<what> on <host>:<port>".
func InstanceName(what, host string, port int) string {
	host, _, _ = strings.Cut(host, ".")
	s := what + " on " + host + ":" + strconv.Itoa(port)
	// A label holds at most 63 bytes; keep the host and port, shorten what.
	if len(s) > 63 {
		tail := " on " + host + ":" + strconv.Itoa(port)
		keep := max(1, 63-len(tail))
		if keep < len(what) {
			what = strings.ToValidUTF8(what[:keep], "")
		}
		s = what + tail
		if len(s) > 63 {
			s = s[:63]
		}
	}
	return s
}
