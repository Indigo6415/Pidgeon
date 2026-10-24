package mdns

import (
	"net/netip"
	"testing"
)

// TestResponseRoundTrip packs the records a responder sends for a browse
// and checks a browser reads the same peer back.
func TestResponseRoundTrip(t *testing.T) {
	r := &Responder{
		svc: Service{
			Instance: "Movies on studio.mac:8080", // dots stay inside the label
			Port:     func() int { return 8080 },
			TXT:      func() map[string]string { return map[string]string{"auth": "token", "tls": "1"} },
		},
		id:   "abcd1234",
		host: name{"pidgeon-abcd1234", "local"},
	}
	r.inst = append(name{r.svc.Instance}, serviceName...)
	addrs := []netip.Addr{netip.MustParseAddr("192.168.1.20"), netip.MustParseAddr("100.64.0.7")}
	m := &message{
		flags:      flagResponse | flagAA,
		answers:    []record{{name: serviceName, typ: typePTR, class: classIN, ttl: ttl, target: r.inst}},
		additional: append([]record{r.srv(ttl), r.txt(ttl)}, r.addrRecords(addrs, ttl)...),
	}
	got, err := unpack(m.pack())
	if err != nil {
		t.Fatal(err)
	}
	peers := peersIn(got, netip.MustParseAddr("192.168.1.20"))
	if len(peers) != 1 {
		t.Fatalf("got %d peers", len(peers))
	}
	p := peers[0]
	if p.Instance != "Movies on studio.mac:8080" || p.Port != 8080 || len(p.Addrs) != 2 {
		t.Fatalf("peer %+v", p)
	}
	if p.TXT["id"] != "abcd1234" || p.TXT["auth"] != "token" {
		t.Fatalf("txt %v", p.TXT)
	}
	if u := p.URL(); u != "https://192.168.1.20:8080" {
		t.Fatalf("url %s", u)
	}
}

// TestCompressedNames reads a name that points back into the message, as
// other mDNS implementations write them.
func TestCompressedNames(t *testing.T) {
	b := []byte{0, 0, 0x84, 0, 0, 0, 0, 1, 0, 0, 0, 0}
	nameAt := len(b)
	b = appendName(b, serviceName)
	b = append(b, 0, typePTR, 0, classIN, 0, 0, 0, 120)
	target := []byte{3, 'f', 'o', 'o', 0xc0, byte(nameAt)} // "foo" + pointer to the service name
	b = append(b, 0, byte(len(target)))
	b = append(b, target...)
	m, err := unpack(b)
	if err != nil {
		t.Fatal(err)
	}
	want := append(name{"foo"}, serviceName...)
	if len(m.answers) != 1 || !m.answers[0].target.equal(want) {
		t.Fatalf("got %+v", m.answers)
	}
	// A pointer loop must fail, not hang.
	loop := []byte{0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xc0, 12, 0, 1, 0, 1}
	if _, err := unpack(loop); err == nil {
		t.Fatal("pointer loop accepted")
	}
}

// TestInstanceNameFitsALabel keeps long folder names within 63 bytes.
func TestInstanceNameFitsALabel(t *testing.T) {
	long := "A very long folder name that goes on and on and on and on and on"
	s := InstanceName(long, "studio.local", 8080)
	if len(s) > 63 || s[len(s)-len(" on studio:8080"):] != " on studio:8080" {
		t.Fatalf("%q (%d bytes)", s, len(s))
	}
}
