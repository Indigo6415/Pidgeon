package app

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/Indigo6415/pidgeon/internal/brand"
	"github.com/Indigo6415/pidgeon/internal/listeners"
	"github.com/Indigo6415/pidgeon/internal/mdns"
	"github.com/Indigo6415/pidgeon/internal/server"
	"github.com/Indigo6415/pidgeon/internal/ssdp"
	"github.com/Indigo6415/pidgeon/internal/stats"
)

// This file connects the server to the network beyond HTTP: the mDNS
// announcement (and the "Nearby" list it powers), the SSDP announcement of
// TV mode, and the delivery-receipts file.

// lanAddrs are the addresses other devices can reach the server on right
// now: every serving non-loopback listener, or with the wildcard bound,
// every LAN and VPN address of the machine.
func lanAddrs(mgr *listeners.Manager) []netip.Addr {
	infos := mgr.List()
	wild := false
	for _, l := range infos {
		if l.ID == listeners.WildcardID && l.Bound {
			wild = true
		}
	}
	var out []netip.Addr
	for _, l := range infos {
		if l.ID == listeners.WildcardID || l.Kind == listeners.KindLoopback || !l.Addr.IsValid() {
			continue
		}
		if l.Serving() || (wild && (l.Kind == listeners.KindLAN || l.Kind == listeners.KindVPN)) {
			out = append(out, l.Addr)
		}
	}
	return out
}

// machineAddrs are all the addresses of this machine, for the TLS
// certificate's names.
func machineAddrs(mgr *listeners.Manager) []netip.Addr {
	var out []netip.Addr
	for _, l := range mgr.List() {
		if l.ID != listeners.WildcardID && l.Addr.IsValid() && l.Kind != listeners.KindVirtual {
			out = append(out, l.Addr)
		}
	}
	return out
}

// announce starts the mDNS responder for srv and returns it, with the
// instance name it uses. Failure is not fatal: discovery is a convenience.
func announce(srv *server.Server, mgr *listeners.Manager, authMode, fingerprint string) (*mdns.Responder, string, error) {
	host, _ := os.Hostname()
	if host == "" {
		host = "computer"
	}
	inst := mdns.InstanceName(srv.Name(), host, mgr.Port())
	r, err := mdns.Advertise(mdns.Service{
		Instance: inst,
		Port:     mgr.Port,
		Addrs:    func() []netip.Addr { return lanAddrs(mgr) },
		TXT: func() map[string]string {
			kv := map[string]string{"name": srv.Name(), "auth": authMode, "version": brand.ResolvedVersion()}
			// HTTPS servers say so, with the certificate's fingerprint, so
			// `pidgeon pull` can connect without a warning.
			if fingerprint != "" {
				kv["tls"], kv["fp"] = "1", fingerprint
			}
			return kv
		},
	})
	return r, inst, err
}

// nearby browses for the other Pidgeon servers, leaving this one out.
func nearby(self func() string) func(ctx context.Context) []server.NearbyServer {
	return func(ctx context.Context) []server.NearbyServer {
		peers, _ := mdns.Browse(ctx)
		out := []server.NearbyServer{}
		for _, p := range peers {
			if p.TXT["id"] == self() {
				continue
			}
			n := server.NearbyServer{
				Name: p.Instance, URL: p.URL() + "/", Auth: p.TXT["auth"], TLS: p.TXT["tls"] == "1", Version: p.TXT["version"],
				ID: p.TXT["id"], Fingerprint: p.TXT["fp"],
			}
			// The flock relays only to the address the answer came from, and
			// only when the server claims it: an announcement can't point
			// this machine at someone else, or at its own loopback services.
			if src := p.Src; src.IsValid() && !src.IsLoopback() && !src.IsUnspecified() && src.Zone() == "" && slices.Contains(p.Addrs, src) {
				n.Proxy = strings.TrimSuffix(p.URL(), "/")
			}
			out = append(out, n)
		}
		return out
	}
}

// advertiseTV announces TV mode's media server over SSDP. Each interface
// gets the description URL on its own address, if a listener serves it.
func advertiseTV(srv *server.Server, mgr *listeners.Manager) (*ssdp.Advertiser, error) {
	uuid, desc, _ := srv.DLNA()
	return ssdp.Advertise(ssdp.Device{
		UUID:   uuid,
		Server: runtime.GOOS + "/1.0 UPnP/1.0 " + brand.Name + "/" + brand.ResolvedVersion(),
		Location: func(local netip.Addr) string {
			for _, l := range mgr.List() {
				// The wildcard serves every address; otherwise it must be this one.
				if (l.ID == listeners.WildcardID && l.Bound) || (l.Addr == local && l.Serving()) {
					return "http://" + netip.AddrPortFrom(local, uint16(l.Port)).String() + desc
				}
			}
			return ""
		},
	})
}

// receiptsWriter appends each delivery receipt to path as one JSON line.
func receiptsWriter(path string) (func(stats.Receipt), func(), error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	var mu sync.Mutex
	enc := json.NewEncoder(f)
	write := func(r stats.Receipt) {
		mu.Lock()
		defer mu.Unlock()
		enc.Encode(r)
	}
	return write, func() { f.Close() }, nil
}
