package certs

import (
	"net/netip"
	"testing"
)

// TestAutoCertIsKeptUntilAddressesChange: the fingerprint survives a
// restart, and a new address gets a new certificate that names it.
func TestAutoCertIsKeptUntilAddressesChange(t *testing.T) {
	dir := t.TempDir()
	lan := []netip.Addr{netip.MustParseAddr("192.168.1.20")}
	_, fp1, err := Load("auto", dir, lan)
	if err != nil {
		t.Fatal(err)
	}
	if _, fp2, _ := Load("auto", dir, lan); fp2 != fp1 {
		t.Fatal("the certificate changed between runs")
	}
	moved := append(lan, netip.MustParseAddr("10.0.0.9"))
	_, fp3, _ := Load("auto", dir, moved)
	if fp3 == fp1 {
		t.Fatal("a new address should get a certificate that names it")
	}
	if _, fp4, _ := Load("auto", dir, moved); fp4 != fp3 {
		t.Fatal("the new certificate wasn't kept")
	}
	if _, _, err := Load("nonsense", dir, lan); err == nil {
		t.Fatal("bad spec accepted")
	}
}

func TestFingerprintFormats(t *testing.T) {
	hex := "90b75c04d9af2359dc786b340d5cf5e420f2439674f23408880e6e2590d12cd8"
	if got := Pretty(hex, 3); got != "90:B7:5C…" {
		t.Fatalf("pretty %q", got)
	}
	// What a browser shows, with a prefix, is read back to plain hex.
	if got := Normalize("SHA256: " + Pretty(hex, 0)); got != hex {
		t.Fatalf("normalize %q", got)
	}
	if Normalize("abcd") != "" {
		t.Fatal("short fingerprint accepted")
	}
}
