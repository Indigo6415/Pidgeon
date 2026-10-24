// Package ssdp announces a UPnP media server (TV mode) on the local network
// with the Simple Service Discovery Protocol: it answers TVs searching for
// media servers (M-SEARCH) and tells the network it is there (NOTIFY
// ssdp:alive) and when it leaves (ssdp:byebye).
package ssdp

import (
	"bufio"
	"bytes"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Indigo6415/pidgeon/internal/mcast"
)

// group is the SSDP multicast address.
var group = netip.MustParseAddrPort("239.255.255.250:1900")

const (
	maxAge        = 1800             // seconds an announcement stays valid
	announceEvery = 10 * time.Minute // well within maxAge
	maxReplyDelay = 2 * time.Second  // cap on the random delay before answering
)

// Device is the media server being announced.
type Device struct {
	UUID   string
	Server string // the SERVER header, "OS/version UPnP/1.0 product/version"
	// Location returns the device description URL as reached through the
	// local address local, or "" when the server isn't reachable there
	// (that listener is switched off).
	Location func(local netip.Addr) string
}

// Advertiser keeps one device announced.
type Advertiser struct {
	d    Device
	g    *mcast.Group
	stop chan struct{}
	wg   sync.WaitGroup
}

// Advertise starts announcing d and answering searches for it.
func Advertise(d Device) (*Advertiser, error) {
	a := &Advertiser{d: d, stop: make(chan struct{})}
	g, err := mcast.Listen(group, a.handle)
	if err != nil {
		return nil, err
	}
	a.g = g
	a.wg.Add(1)
	go a.announce()
	return a, nil
}

// Close says goodbye on the network and stops answering.
func (a *Advertiser) Close() {
	close(a.stop)
	a.wg.Wait()
	a.notify("ssdp:byebye")
	a.g.Close()
}

// targets are the notification types a media server announces.
func (a *Advertiser) targets() []string {
	return []string{
		"upnp:rootdevice",
		"uuid:" + a.d.UUID,
		"urn:schemas-upnp-org:device:MediaServer:1",
		"urn:schemas-upnp-org:service:ContentDirectory:1",
		"urn:schemas-upnp-org:service:ConnectionManager:1",
	}
}

// usn is the unique service name for a notification type.
func (a *Advertiser) usn(nt string) string {
	if nt == "uuid:"+a.d.UUID {
		return nt
	}
	return "uuid:" + a.d.UUID + "::" + nt
}

// announce sends ssdp:alive now, twice more shortly after (UDP gets lost),
// then periodically until Close.
func (a *Advertiser) announce() {
	defer a.wg.Done()
	for _, wait := range []time.Duration{0, time.Second, 3 * time.Second} {
		select {
		case <-a.stop:
			return
		case <-time.After(wait):
			a.notify("ssdp:alive")
		}
	}
	tk := time.NewTicker(announceEvery)
	defer tk.Stop()
	// Re-announce until closed.
	for {
		select {
		case <-a.stop:
			return
		case <-tk.C:
			a.notify("ssdp:alive")
		}
	}
}

// notify multicasts one NOTIFY per target on every interface.
func (a *Advertiser) notify(nts string) {
	for _, nt := range a.targets() {
		a.g.Multicast(func(it mcast.Iface) []byte {
			var b bytes.Buffer
			b.WriteString("NOTIFY * HTTP/1.1\r\nHOST: " + group.String() + "\r\n")
			if nts == "ssdp:alive" {
				loc := a.d.Location(it.Local(netip.Addr{}))
				// Not served on this interface: nothing to announce there.
				if loc == "" {
					return nil
				}
				fmt.Fprintf(&b, "CACHE-CONTROL: max-age=%d\r\nLOCATION: %s\r\nSERVER: %s\r\n", maxAge, loc, a.d.Server)
			}
			fmt.Fprintf(&b, "NT: %s\r\nNTS: %s\r\nUSN: %s\r\n\r\n", nt, nts, a.usn(nt))
			return b.Bytes()
		})
	}
}

// handle answers an M-SEARCH for something this device is.
func (a *Advertiser) handle(p mcast.Packet) {
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(p.Data)))
	if err != nil || req.Method != "M-SEARCH" || strings.Trim(req.Header.Get("MAN"), `"`) != "ssdp:discover" {
		return
	}
	st := req.Header.Get("ST")
	var matches []string
	for _, t := range a.targets() {
		if st == "ssdp:all" || strings.EqualFold(st, t) {
			matches = append(matches, t)
		}
	}
	if len(matches) == 0 {
		return
	}
	loc := a.d.Location(p.Iface.Local(p.Src.Addr()))
	if loc == "" {
		return
	}
	// Answer after a random delay up to MX seconds, as the spec asks, so a
	// search doesn't get every device answering at the same instant.
	mx, _ := strconv.Atoi(req.Header.Get("MX"))
	delay := time.Duration(rand.Int64N(int64(min(maxReplyDelay, time.Duration(max(1, mx))*time.Second))))
	go func() {
		select {
		case <-a.stop:
			return
		case <-time.After(delay):
		}
		// One response per matching target.
		for _, t := range matches {
			resp := fmt.Sprintf("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=%d\r\nDATE: %s\r\nEXT:\r\nLOCATION: %s\r\nSERVER: %s\r\nST: %s\r\nUSN: %s\r\nContent-Length: 0\r\n\r\n",
				maxAge, time.Now().UTC().Format(http.TimeFormat), loc, a.d.Server, t, a.usn(t))
			p.Reply([]byte(resp))
		}
	}()
}
