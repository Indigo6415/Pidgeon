// Package certs provides the TLS certificate for --tls. "auto" makes a
// self-signed certificate once and keeps it in the user's config folder,
// so its fingerprint stays the same from run to run (browsers remember the
// exception, and `pidgeon pull` can pin it). It is remade when it expires
// or no longer names this machine's addresses.
//
// A self-signed certificate encrypts traffic, but a browser can't know it
// is really yours: it shows a warning once. Comparing the fingerprint the
// browser shows with the one in the dashboard proves it.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// validFor is how long a generated certificate lasts.
const validFor = 2 * 365 * 24 * time.Hour

// Load returns the TLS config for spec: "auto" (self-signed, kept in dir),
// or "cert.pem,key.pem". addrs are the machine's addresses; an "auto"
// certificate that doesn't cover them all is replaced. It also returns the
// leaf certificate's SHA-256 fingerprint as lowercase hex.
func Load(spec, dir string, addrs []netip.Addr) (*tls.Config, string, error) {
	var cert tls.Certificate
	var err error
	switch {
	case spec == "auto":
		cert, err = loadOrCreate(dir, addrs)
	case strings.Contains(spec, ","):
		certFile, keyFile, _ := strings.Cut(spec, ",")
		cert, err = tls.LoadX509KeyPair(strings.TrimSpace(certFile), strings.TrimSpace(keyFile))
	default:
		return nil, "", errors.New(`--tls must be "auto" or "cert.pem,key.pem"`)
	}
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(cert.Certificate[0])
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	return cfg, hex.EncodeToString(sum[:]), nil
}

// Dir is the default folder for the "auto" certificate:
// <user config dir>/<app>/tls.
func Dir(app string) string {
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, app, "tls")
}

// loadOrCreate reuses the kept certificate if it is still good for these
// addresses, and makes (and keeps) a new one otherwise.
func loadOrCreate(dir string, addrs []netip.Addr) (tls.Certificate, error) {
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if dir != "" {
		if c, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil && covers(c, addrs) {
			return c, nil
		}
	}
	certPEM, keyPEM, err := generate(addrs)
	if err != nil {
		return tls.Certificate{}, err
	}
	// Keeping it is best effort: without a config folder it lasts one run.
	if dir != "" && os.MkdirAll(dir, 0o700) == nil {
		_ = os.WriteFile(keyFile, keyPEM, 0o600)
		_ = os.WriteFile(certFile, certPEM, 0o644)
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// covers reports whether a certificate is valid for another week and
// names every address.
func covers(c tls.Certificate, addrs []netip.Addr) bool {
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil || time.Now().Add(7*24*time.Hour).After(leaf.NotAfter) {
		return false
	}
	// Each address must be among the certificate's IP names.
	for _, a := range addrs {
		if !slices.ContainsFunc(leaf.IPAddresses, func(ip net.IP) bool { return ip.Equal(a.AsSlice()) }) {
			return false
		}
	}
	return true
}

// generate makes a self-signed ECDSA P-256 certificate for localhost, the
// host name and every address.
func generate(addrs []netip.Addr) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, nil, err
	}
	host, _ := os.Hostname()
	names := []string{"localhost"}
	if host != "" {
		names = append(names, host)
		if short, _, ok := strings.Cut(host, "."); ok {
			names = append(names, short)
		} else {
			names = append(names, host+".local")
		}
	}
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	for _, a := range addrs {
		ips = append(ips, a.AsSlice())
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Pidgeon on " + host, Organization: []string{"Pidgeon (self-signed)"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validFor),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              names,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), nil
}

// Pretty formats a hex fingerprint the way browsers show it:
// "AB:CD:EF:...". With n > 0 only the first n bytes are shown, then "…".
func Pretty(fp string, n int) string {
	fp = strings.ToUpper(fp)
	var parts []string
	// Two hex digits per byte.
	for i := 0; i+2 <= len(fp); i += 2 {
		if n > 0 && len(parts) == n {
			return strings.Join(parts, ":") + "…"
		}
		parts = append(parts, fp[i:i+2])
	}
	return strings.Join(parts, ":")
}

// Normalize turns a fingerprint in any common form ("AB:CD...", "sha256:
// abcd...", with spaces) into lowercase hex, or "" if it isn't one.
func Normalize(fp string) string {
	fp = strings.ToLower(strings.TrimSpace(fp))
	fp = strings.TrimPrefix(fp, "sha256:")
	fp = strings.TrimPrefix(fp, "sha-256:")
	fp = strings.NewReplacer(":", "", " ", "", "-", "").Replace(fp)
	if len(fp) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(fp); err != nil {
		return ""
	}
	return fp
}

// Pin returns a TLS config that accepts exactly the certificate with this
// fingerprint (lowercase hex), whatever signed it: how a client trusts a
// self-signed server it has verified out of band.
func Pin(fp string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, // replaced by the fingerprint check below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if got := hex.EncodeToString(sum[:]); got != fp {
				return fmt.Errorf("certificate fingerprint %s doesn't match the expected %s", Pretty(got, 8), Pretty(fp, 8))
			}
			return nil
		},
	}
}
