package listeners

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"testing"
	"time"
)

// get fetches http://127.0.0.1:port/ and returns the body ("" on error).
func get(port int) string {
	c := http.Client{Timeout: 2 * time.Second}
	r, err := c.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
	if err != nil {
		return ""
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

func TestWildcardCoversAndRestores(t *testing.T) {
	lo := Candidate{ID: "127.0.0.1", Addr: netip.MustParseAddr("127.0.0.1"), Kind: KindLoopback}
	wild := Candidate{ID: WildcardID, Kind: KindWildcard}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, IDFromContext(r.Context()))
	})
	m := NewManager([]Candidate{lo, wild}, h, 18750, nil)
	port, err := m.Start([]string{lo.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown(context.Background())

	if got := get(port); got != lo.ID {
		t.Fatalf("loopback: got %q", got)
	}

	// Wildcard on: loopback is covered, and traffic arriving on 127.0.0.1
	// through the wildcard is still attributed to the loopback row.
	if err := m.Toggle(WildcardID); err != nil {
		t.Fatal(err)
	}
	info := m.List()
	if !info[0].Covered || info[0].Bound || !info[1].Bound {
		t.Fatalf("after wildcard on: %+v", info)
	}
	if got := get(port); got != lo.ID {
		t.Fatalf("via wildcard: got %q, want %q", got, lo.ID)
	}

	// Wildcard off: loopback gets its own socket back.
	if err := m.Toggle(WildcardID); err != nil {
		t.Fatal(err)
	}
	info = m.List()
	if !info[0].Bound || info[1].Bound {
		t.Fatalf("after wildcard off: %+v", info)
	}
	if got := get(info[0].Port); got != lo.ID {
		t.Fatalf("restored loopback: got %q", got)
	}

	// Disabling loopback stops it answering.
	m.Toggle(lo.ID)
	if got := get(info[0].Port); got != "" {
		t.Fatalf("disabled listener still answered %q", got)
	}
}
