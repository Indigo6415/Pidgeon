package mdns

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
)

// Just enough of the DNS wire format (RFC 1035) for service discovery:
// questions and PTR, SRV, TXT, A and AAAA records. Names are written
// without compression (always valid) and read with it (others use it).

// Record types and classes.
const (
	typeA    = 1
	typePTR  = 12
	typeTXT  = 16
	typeAAAA = 28
	typeSRV  = 33
	typeANY  = 255

	classIN = 1
	// classMask strips the mDNS flag bit (cache-flush in records,
	// unicast-response in questions) from a class.
	classMask  = 0x7fff
	cacheFlush = 0x8000
)

// Header flags.
const (
	flagResponse = 0x8000
	flagAA       = 0x0400
)

// name is a domain name as its labels, without the empty root label.
// Labels are kept whole, so an instance name may contain dots and spaces.
type name []string

// equal compares names case-insensitively, as DNS does.
func (n name) equal(o name) bool {
	if len(n) != len(o) {
		return false
	}
	for i := range n {
		if !strings.EqualFold(n[i], o[i]) {
			return false
		}
	}
	return true
}

// String renders the name for display ("a.b.local.").
func (n name) String() string { return strings.Join(n, ".") + "." }

// question asks for records of one type.
type question struct {
	name  name
	typ   uint16
	class uint16
}

// record is one resource record; which data fields matter depends on typ.
type record struct {
	name  name
	typ   uint16
	class uint16
	ttl   uint32

	target name       // PTR, SRV
	port   uint16     // SRV
	txt    []string   // TXT
	addr   netip.Addr // A, AAAA
}

// message is a whole DNS message.
type message struct {
	id         uint16
	flags      uint16
	questions  []question
	answers    []record
	authority  []record
	additional []record
}

// pack encodes the message.
func (m *message) pack() []byte {
	b := make([]byte, 12, 512)
	binary.BigEndian.PutUint16(b[0:], m.id)
	binary.BigEndian.PutUint16(b[2:], m.flags)
	binary.BigEndian.PutUint16(b[4:], uint16(len(m.questions)))
	binary.BigEndian.PutUint16(b[6:], uint16(len(m.answers)))
	binary.BigEndian.PutUint16(b[8:], uint16(len(m.authority)))
	binary.BigEndian.PutUint16(b[10:], uint16(len(m.additional)))
	for _, q := range m.questions {
		b = appendName(b, q.name)
		b = binary.BigEndian.AppendUint16(b, q.typ)
		b = binary.BigEndian.AppendUint16(b, q.class)
	}
	// The three record sections share one encoding.
	for _, sec := range [][]record{m.answers, m.authority, m.additional} {
		for _, r := range sec {
			b = appendRecord(b, r)
		}
	}
	return b
}

// appendName writes a name as length-prefixed labels.
func appendName(b []byte, n name) []byte {
	for _, l := range n {
		if len(l) > 63 {
			l = l[:63]
		}
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	return append(b, 0)
}

// appendRecord writes one resource record.
func appendRecord(b []byte, r record) []byte {
	b = appendName(b, r.name)
	b = binary.BigEndian.AppendUint16(b, r.typ)
	b = binary.BigEndian.AppendUint16(b, r.class)
	b = binary.BigEndian.AppendUint32(b, r.ttl)
	lenAt := len(b)
	b = append(b, 0, 0) // rdata length, filled in below
	switch r.typ {
	case typePTR:
		b = appendName(b, r.target)
	case typeSRV:
		b = append(b, 0, 0, 0, 0) // priority, weight
		b = binary.BigEndian.AppendUint16(b, r.port)
		b = appendName(b, r.target)
	case typeTXT:
		// An empty TXT record still holds one empty string.
		if len(r.txt) == 0 {
			b = append(b, 0)
		}
		for _, s := range r.txt {
			if len(s) > 255 {
				s = s[:255]
			}
			b = append(b, byte(len(s)))
			b = append(b, s...)
		}
	case typeA, typeAAAA:
		b = append(b, r.addr.AsSlice()...)
	}
	binary.BigEndian.PutUint16(b[lenAt:], uint16(len(b)-lenAt-2))
	return b
}

// errShort is returned for truncated or malformed messages.
var errShort = errors.New("mdns: malformed message")

// unpack decodes a message.
func unpack(b []byte) (*message, error) {
	if len(b) < 12 {
		return nil, errShort
	}
	m := &message{id: binary.BigEndian.Uint16(b[0:]), flags: binary.BigEndian.Uint16(b[2:])}
	counts := [4]int{}
	for i := range counts {
		counts[i] = int(binary.BigEndian.Uint16(b[4+2*i:]))
	}
	off := 12
	for range counts[0] {
		n, next, err := readName(b, off)
		if err != nil || next+4 > len(b) {
			return nil, errShort
		}
		m.questions = append(m.questions, question{name: n, typ: binary.BigEndian.Uint16(b[next:]), class: binary.BigEndian.Uint16(b[next+2:])})
		off = next + 4
	}
	// Answers, authority and additional records, in that order.
	secs := []*[]record{&m.answers, &m.authority, &m.additional}
	for i, sec := range secs {
		for range counts[i+1] {
			r, next, err := readRecord(b, off)
			if err != nil {
				return nil, err
			}
			*sec = append(*sec, r)
			off = next
		}
	}
	return m, nil
}

// readRecord decodes the record at off and returns the offset after it.
func readRecord(b []byte, off int) (record, int, error) {
	n, off, err := readName(b, off)
	if err != nil || off+10 > len(b) {
		return record{}, 0, errShort
	}
	r := record{
		name: n, typ: binary.BigEndian.Uint16(b[off:]), class: binary.BigEndian.Uint16(b[off+2:]),
		ttl: binary.BigEndian.Uint32(b[off+4:]),
	}
	rdlen := int(binary.BigEndian.Uint16(b[off+8:]))
	start := off + 10
	end := start + rdlen
	if end > len(b) {
		return record{}, 0, errShort
	}
	rd := b[start:end]
	switch r.typ {
	case typePTR:
		r.target, _, err = readName(b, start)
	case typeSRV:
		if len(rd) < 7 {
			return record{}, 0, errShort
		}
		r.port = binary.BigEndian.Uint16(rd[4:])
		r.target, _, err = readName(b, start+6)
	case typeTXT:
		// A sequence of length-prefixed strings.
		for i := 0; i < len(rd); {
			l := int(rd[i])
			if i+1+l > len(rd) {
				return record{}, 0, errShort
			}
			if l > 0 {
				r.txt = append(r.txt, string(rd[i+1:i+1+l]))
			}
			i += 1 + l
		}
	case typeA, typeAAAA:
		r.addr, _ = netip.AddrFromSlice(rd)
	}
	if err != nil {
		return record{}, 0, err
	}
	return r, end, nil
}

// readName decodes a possibly compressed name at off and returns the
// offset just after it (after the first pointer, if any).
func readName(b []byte, off int) (name, int, error) {
	var n name
	next := -1
	// Follow labels and compression pointers, with a cap against loops.
	for hops := 0; hops < 64; hops++ {
		if off >= len(b) {
			return nil, 0, errShort
		}
		l := int(b[off])
		switch {
		case l == 0:
			if next < 0 {
				next = off + 1
			}
			return n, next, nil
		case l&0xc0 == 0xc0:
			if off+1 >= len(b) {
				return nil, 0, errShort
			}
			if next < 0 {
				next = off + 2
			}
			off = int(binary.BigEndian.Uint16(b[off:]) & 0x3fff)
		case l&0xc0 != 0:
			return nil, 0, errShort
		default:
			if off+1+l > len(b) {
				return nil, 0, errShort
			}
			n = append(n, string(b[off+1:off+1+l]))
			off += 1 + l
		}
	}
	return nil, 0, errShort
}
