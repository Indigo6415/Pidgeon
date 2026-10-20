// Package listeners discovers the addresses this machine can be reached on
// and manages one live HTTP listener per enabled address.
package listeners

import (
	"net"
	"net/netip"
	"sort"
	"strings"
)

// Kind classifies a bind address so the UI can label it and so sensible
// defaults can be chosen (loopback + LAN on, everything else off).
type Kind int

const (
	KindLoopback Kind = iota
	KindLAN
	KindVPN
	KindVirtual
	KindCustom
	KindWildcard
)

// String returns the short label shown in the TUI.
func (k Kind) String() string {
	switch k {
	case KindLoopback:
		return "loopback"
	case KindLAN:
		return "lan"
	case KindVPN:
		return "vpn"
	case KindVirtual:
		return "virtual"
	case KindCustom:
		return "custom"
	case KindWildcard:
		return "all"
	}
	return "?"
}

// WildcardID is the ID of the listener that binds every interface.
const WildcardID = "*"

// Candidate is one address that a listener could be bound to.
type Candidate struct {
	ID      string     // stable identifier; the IP string, or WildcardID
	Addr    netip.Addr // invalid for the wildcard
	Iface   string     // interface name, e.g. "en0"; "" for wildcard
	Kind    Kind
	Primary bool // the LAN address used for the default route (QR target)
}

// Host returns the host portion suitable for a URL ("[::1]" for IPv6).
func (c Candidate) Host() string {
	// The wildcard has no single address; callers display it specially.
	if c.ID == WildcardID {
		return "0.0.0.0"
	}
	if c.Addr.Is6() {
		return "[" + c.Addr.String() + "]"
	}
	return c.Addr.String()
}

// Tailscale (and other CGNAT-based overlays) hand out 100.64.0.0/10; Tailscale's
// IPv6 ULA range is fd7a:115c:a1e0::/48.
var (
	cgnat        = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleULA = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
	vpnPrefixes  = []string{"utun", "tun", "tap", "wg", "tailscale", "zt", "ppp", "ipsec", "nordlynx", "proton"}
	virtPrefixes = []string{"docker", "br-", "veth", "vmnet", "vboxnet", "virbr", "bridge", "lxc", "lxd", "cni", "flannel", "podman", "vEthernet"}
)

// Discover enumerates every usable address on the machine plus the wildcard.
// IPv6 link-local addresses are skipped because they need a zone to be useful
// in a URL and phones rarely resolve them.
func Discover() []Candidate {
	primary := primaryIP()
	var out []Candidate
	seen := map[netip.Addr]bool{}

	ifaces, _ := net.Interfaces()
	// Walk each interface that is up and collect its unicast addresses.
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		// Each interface can carry several IPv4/IPv6 addresses.
		for _, a := range addrs {
			pfx, err := netip.ParsePrefix(a.String())
			if err != nil {
				continue
			}
			ip := pfx.Addr().Unmap()
			// Skip addresses that cannot sensibly be served on or were seen already.
			if ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() || seen[ip] {
				continue
			}
			// Only the IPv4 loopback is listed; ::1 adds noise without value.
			if ip.IsLoopback() && ip.Is6() {
				continue
			}
			seen[ip] = true
			out = append(out, Candidate{
				ID:      ip.String(),
				Addr:    ip,
				Iface:   ifc.Name,
				Kind:    classify(ip, ifc.Name),
				Primary: ip == primary,
			})
		}
	}

	// If the default-route address wasn't found among LAN addresses (e.g. no
	// network), mark the first LAN IPv4 address as primary instead.
	if !hasPrimary(out) {
		for i := range out {
			if out[i].Kind == KindLAN && out[i].Addr.Is4() {
				out[i].Primary = true
				break
			}
		}
	}

	sortCandidates(out)
	out = append(out, Candidate{ID: WildcardID, Kind: KindWildcard})
	return out
}

// classify decides which Kind an address belongs to from its range and the
// interface name.
func classify(ip netip.Addr, iface string) Kind {
	lower := strings.ToLower(iface)
	switch {
	case ip.IsLoopback():
		return KindLoopback
	case cgnat.Contains(ip) || tailscaleULA.Contains(ip) || hasAnyPrefix(lower, vpnPrefixes):
		return KindVPN
	case hasAnyPrefix(lower, lowerAll(virtPrefixes)):
		return KindVirtual
	}
	return KindLAN
}

// primaryIP finds the local address the OS would use to reach the internet.
// Dialing UDP sends no packets; it only asks the kernel to pick a route.
func primaryIP() netip.Addr {
	conn, err := net.Dial("udp4", "192.0.2.1:9") // TEST-NET-1, never routed
	if err != nil {
		return netip.Addr{}
	}
	defer conn.Close()
	ap, err := netip.ParseAddrPort(conn.LocalAddr().String())
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}

// sortCandidates orders by kind (loopback, LAN, VPN, ...), primary first
// within LAN, IPv4 before IPv6, then by address.
func sortCandidates(c []Candidate) {
	sort.SliceStable(c, func(i, j int) bool {
		a, b := c[i], c[j]
		// Group by kind first so the list reads top-down from most local.
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Primary != b.Primary {
			return a.Primary
		}
		if a.Addr.Is4() != b.Addr.Is4() {
			return a.Addr.Is4()
		}
		return a.Addr.Less(b.Addr)
	})
}

// hasPrimary reports whether any candidate is flagged as the primary address.
func hasPrimary(c []Candidate) bool {
	// Linear scan; the list is a handful of entries.
	for _, x := range c {
		if x.Primary {
			return true
		}
	}
	return false
}

// hasAnyPrefix reports whether s starts with any of the given prefixes.
func hasAnyPrefix(s string, prefixes []string) bool {
	// Check each known interface-name prefix.
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// lowerAll returns a lower-cased copy of ss.
func lowerAll(ss []string) []string {
	out := make([]string, len(ss))
	// Lower-case each entry so matching is case-insensitive.
	for i, s := range ss {
		out[i] = strings.ToLower(s)
	}
	return out
}
