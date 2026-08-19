package provision

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"ngxsetup/internal/logx"
	"ngxsetup/internal/state"
	"ngxsetup/internal/system"
	"ngxsetup/internal/tmpl"
)

// SelfSignedDir holds locally generated certificates.
const SelfSignedDir = "/etc/ngxsetup/certs"

// issueCertificate obtains TLS material for a new site, if it wants any.
func (c *Ctx) issueCertificate(rec *state.Site, req SiteRequest) error {
	switch {
	case req.TLS:
		return c.issueLetsEncrypt(rec)
	case req.SelfSigned:
		return c.issueSelfSigned(rec)
	default:
		return nil
	}
}

// issueLetsEncrypt requests a certificate over the HTTP-01 challenge.
//
// The webroot plugin is used rather than certbot's nginx plugin: the nginx
// plugin rewrites server blocks in place, which would fight with this tool's
// generated configuration and make both unpredictable. The shared ACME webroot
// is served by the default server, so validation succeeds before the site's own
// vhost exists.
func (c *Ctx) issueLetsEncrypt(rec *state.Site) error {
	if c.Config.ACMEEmail == "" {
		return fmt.Errorf("an email address is required for Let's Encrypt; set it with `ngxsetup config set acme_email you@example.com`")
	}
	if !c.Runner.Look("certbot") {
		return fmt.Errorf("certbot is not installed; run `ngxsetup setup` first")
	}
	if c.Writer.DryRun {
		logx.Change("[dry-run] would request a certificate for %s", rec.ServerNames())
		setLetsEncryptCertPaths(rec)
		return nil
	}

	if err := c.Writer.EnsureDir(ACMERoot, 0o755, "www-data"); err != nil {
		return err
	}

	args := []string{"certonly", "--webroot", "-w", c.Path(ACMERoot),
		"--email", c.Config.ACMEEmail, "--agree-tos", "--non-interactive",
		"--keep-until-expiring", "--cert-name", rec.Domain,
	}
	for _, name := range append([]string{rec.Domain}, rec.Aliases...) {
		args = append(args, "-d", name)
	}

	logx.Step("requesting a certificate for %s", rec.ServerNames())
	if err := c.Runner.Run(c.Context, "certbot", args...); err != nil {
		// A DNS or firewall problem is by far the most common cause, and the
		// error certbot prints does not always make that obvious.
		return fmt.Errorf("certificate issuance failed: %w\n"+
			"    Check that %s resolves to this server and that ports 80 and 443 are reachable.\n"+
			"    To create the site without TLS for now, re-run with --self-signed.", err, rec.Domain)
	}

	setLetsEncryptCertPaths(rec)
	logx.Change("issued a Let's Encrypt certificate for %s", rec.Domain)
	return nil
}

// setLetsEncryptCertPaths records where certbot places a certificate's files.
//
// fullchain.pem (leaf + intermediates) is what nginx presents to clients via
// ssl_certificate. chain.pem (intermediates only) is a separate file certbot
// writes specifically for ssl_trusted_certificate, which OCSP stapling
// verification needs to validate the stapled response's signer. Without it,
// nginx logs "issuer certificate not found" and silently skips stapling on
// every reload — confirmed live, where it fired unconditionally because this
// field was declared but never actually populated.
func setLetsEncryptCertPaths(rec *state.Site) {
	rec.TLS = true
	rec.CertSource = "letsencrypt"
	live := "/etc/letsencrypt/live/" + rec.Domain + "/"
	rec.CertPath = live + "fullchain.pem"
	rec.ChainPath = live + "chain.pem"
}

// issueSelfSigned generates a local certificate.
//
// Used for staging, for a site behind a CDN that terminates TLS itself, and as
// the fallback when a domain's DNS is not yet pointing here. An ECDSA P-256 key
// is used rather than RSA-2048: it is generated instantly, produces a smaller
// handshake, and is supported by every client that supports TLS 1.2.
func (c *Ctx) issueSelfSigned(rec *state.Site) error {
	dir := filepath.Join(SelfSignedDir, rec.Slug)
	certPath := filepath.Join(dir, "fullchain.pem")
	keyPath := filepath.Join(dir, "privkey.pem")

	rec.TLS = true
	rec.CertSource = "self-signed"
	rec.CertPath = certPath

	if c.Writer.DryRun {
		logx.Change("[dry-run] would generate a self-signed certificate for %s", rec.Domain)
		return nil
	}
	if err := c.Writer.EnsureDir(dir, 0o700, ""); err != nil {
		return err
	}

	names := append([]string{rec.Domain}, rec.Aliases...)
	certPEM, keyPEM, err := SelfSignedCert(names, nil)
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.Path(certPath), certPEM, 0o644); err != nil {
		return err
	}
	// The private key must never be group- or world-readable; nginx reads it
	// as root during startup, before dropping to www-data.
	if err := os.WriteFile(c.Path(keyPath), keyPEM, 0o600); err != nil {
		return err
	}
	logx.Change("generated a self-signed certificate for %s (browsers will warn until a real one is issued)", rec.Domain)
	return nil
}

// SelfSignedCert generates a locally-trusted-nowhere certificate for the
// given DNS names and, optionally, IP addresses (needed when the client will
// connect by address rather than name — the web UI on a random port, mainly,
// where there is no DNS name to put in front of it). An ECDSA P-256 key is
// used rather than RSA-2048: it is generated instantly, produces a smaller
// handshake, and is supported by every client that supports TLS 1.2.
func SelfSignedCert(names []string, ips []net.IP) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}

	commonName := "ngxsetup"
	if len(names) > 0 {
		commonName = names[0]
	} else if len(ips) > 0 {
		commonName = ips[0].String()
	}

	now := time.Now()
	tmplCert := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		// Backdated an hour so a small clock skew between this machine and a
		// client does not make a freshly issued certificate invalid.
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              names,
		IPAddresses:           ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmplCert, &tmplCert, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		nil
}

// RenewCertificates runs certbot renewal and reloads nginx on success.
func (c *Ctx) RenewCertificates() error {
	if !c.Runner.Look("certbot") {
		return fmt.Errorf("certbot is not installed")
	}
	logx.Step("renewing certificates")
	if err := c.Runner.Run(c.Context, "certbot", "renew", "--webroot", "-w", c.Path(ACMERoot), "--quiet"); err != nil {
		return err
	}
	return system.Reload(c.Context, c.Runner, "nginx.service")
}

// generateSalts produces the eight WordPress authentication keys.
//
// Generated locally from the system CSPRNG rather than fetched from the
// WordPress salt API. The keys protect every session cookie on the site;
// requesting them over the network means a third party has seen them, and it
// makes provisioning fail when the machine has no outbound access.
func generateSalts() ([]tmpl.Salt, error) {
	names := []string{
		"AUTH_KEY", "SECURE_AUTH_KEY", "LOGGED_IN_KEY", "NONCE_KEY",
		"AUTH_SALT", "SECURE_AUTH_SALT", "LOGGED_IN_SALT", "NONCE_SALT",
	}
	out := make([]tmpl.Salt, 0, len(names))
	for _, n := range names {
		v, err := system.Password(64)
		if err != nil {
			return nil, err
		}
		out = append(out, tmpl.Salt{Name: n, Value: v})
	}
	return out, nil
}
