package webui

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"ngxsetup/internal/provision"
)

// certDir holds the certificate the web UI's own listener presents. Separate
// from provision.SelfSignedDir: this one has no domain name, no site, and no
// business being touched by site lifecycle code.
const certDir = "/etc/ngxsetup/webui-cert"

// loadOrCreateCert returns a TLS certificate for the web UI's listener,
// generating and persisting one on first run. Reused across restarts so the
// browser's "I trust this exception" click from last time still holds —
// regenerating it on every `ngxsetup web` invocation would make that
// permanent-until-you-say-otherwise trust decision worthless.
func loadOrCreateCert(bindHost string) (tls.Certificate, error) {
	certPath := filepath.Join(certDir, "cert.pem")
	keyPath := filepath.Join(certDir, "key.pem")

	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return cert, nil
	}

	names := []string{"localhost"}
	if h, err := os.Hostname(); err == nil && h != "" {
		names = append(names, h)
	}
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	if ip := net.ParseIP(bindHost); ip != nil {
		ips = append(ips, ip)
	}
	// Every address this machine actually has, so connecting by whichever
	// one reaches it does not trip a hostname mismatch.
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() {
				ips = append(ips, ipn.IP)
			}
		}
	}

	certPEM, keyPEM, err := provision.SelfSignedCert(names, ips)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generating certificate: %w", err)
	}
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}
