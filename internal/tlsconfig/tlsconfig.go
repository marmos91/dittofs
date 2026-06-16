// Package tlsconfig provides shared, file-based TLS plumbing for DittoFS
// servers (the control-plane API and the NFS adapter). It loads certificate
// files from disk and hot-reloads them when a platform rotates the files
// (cert-manager, a mounted Secret, certbot, Vault, etc.) without a restart.
//
// DittoFS only consumes cert files — it does not act as a CA, does not
// generate self-signed certificates, and does not do ACME/issuance/rotation.
// That lifecycle is left to the platform.
package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"time"
)

// minTLSVersions maps the human-readable values accepted in a min_version
// setting to their crypto/tls constants. Only TLS 1.2 and 1.3 are offered;
// older versions are considered insecure and are not exposed.
var minTLSVersions = map[string]uint16{
	"1.2": tls.VersionTLS12,
	"1.3": tls.VersionTLS13,
}

// ParseMinVersion maps a min_version string ("1.2" or "1.3") to its
// crypto/tls constant, returning an error for unsupported values. The error
// message is field-agnostic; callers may prepend their own config-key path.
func ParseMinVersion(s string) (uint16, error) {
	v, ok := minTLSVersions[s]
	if !ok {
		return 0, fmt.Errorf("min_version %q is not supported (use \"1.2\" or \"1.3\")", s)
	}
	return v, nil
}

// Config is the file-based TLS configuration shared by DittoFS servers.
//
// When CertFile and KeyFile are both set, TLS is enabled. When both are unset,
// the caller falls back to plaintext (back-compat). Setting one without the
// other is a configuration error (see Validate).
type Config struct {
	// CertFile is the path to the PEM-encoded server certificate (or chain).
	CertFile string

	// KeyFile is the path to the PEM-encoded private key for CertFile.
	KeyFile string

	// ClientCA is the path to a PEM bundle of CA certificates. When set, the
	// server requires and verifies a client certificate signed by one of these
	// CAs (mutual TLS). When empty, client certificates are not requested.
	ClientCA string

	// MinVersion is the minimum negotiated TLS version: "1.2" or "1.3".
	// Default (empty): "1.2".
	MinVersion string
}

// Enabled reports whether TLS is configured (both cert and key set). A single
// file set without the other is treated as "not enabled" here; Validate
// rejects that partial configuration with a clear error.
func (c Config) Enabled() bool {
	return c.CertFile != "" && c.KeyFile != ""
}

// Validate checks for internally inconsistent TLS settings. It is fail-fast:
// cert without key (or vice versa), a client CA without a server cert, and an
// unsupported min_version are rejected. It does not read the cert files from
// disk — that happens in ServerConfig. Error messages are field-agnostic so a
// caller can wrap them with its own config-key path.
func (c Config) Validate() error {
	certSet := c.CertFile != ""
	keySet := c.KeyFile != ""
	switch {
	case certSet && !keySet:
		return fmt.Errorf("cert_file is set but key_file is missing; both are required to enable TLS")
	case keySet && !certSet:
		return fmt.Errorf("key_file is set but cert_file is missing; both are required to enable TLS")
	}
	if c.ClientCA != "" && !c.Enabled() {
		return fmt.Errorf("client_ca requires cert_file and key_file (mTLS needs a server certificate)")
	}
	if c.MinVersion != "" {
		if _, err := ParseMinVersion(c.MinVersion); err != nil {
			return err
		}
	}
	return nil
}

// CertReloader holds a server certificate and re-reads the cert/key files from
// disk when they change, so a platform rotating the files (cert-manager, a
// mounted Secret, etc.) does not require a server restart.
//
// Reload is lazy and triggered from GetCertificate (the tls.Config callback
// invoked once per handshake): on each call it stats the files and, if either
// mtime advanced since the last successful load, re-parses the pair. There is
// no background goroutine — handshakes are infrequent relative to a stat(2),
// and a failed reload keeps serving the last good certificate rather than
// breaking new connections.
type CertReloader struct {
	certFile string
	keyFile  string

	mu      sync.RWMutex
	cert    *tls.Certificate
	certMod time.Time
	keyMod  time.Time
}

// NewCertReloader loads the initial cert/key pair, failing fast if the files
// are missing or do not parse, and returns a reloader primed with that pair.
func NewCertReloader(certFile, keyFile string) (*CertReloader, error) {
	r := &CertReloader{certFile: certFile, keyFile: keyFile}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// reload re-reads and parses the cert/key pair and atomically swaps it in.
//
// The mtimes are sampled BEFORE the file contents are read. If a rotation
// races this reload, the recorded mtime is the pre-rotation one, so the next
// changed() check still sees the newer mtime and reloads again — the reloader
// can never latch a stale cert against a newer recorded mtime. Sampling the
// mtime after the read would risk recording the new mtime while having read
// the old bytes, permanently masking the rotation.
func (r *CertReloader) reload() error {
	certInfo, err := os.Stat(r.certFile)
	if err != nil {
		return fmt.Errorf("stat TLS cert file %s: %w", r.certFile, err)
	}
	keyInfo, err := os.Stat(r.keyFile)
	if err != nil {
		return fmt.Errorf("stat TLS key file %s: %w", r.keyFile, err)
	}

	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("load TLS key pair (cert=%s key=%s): %w", r.certFile, r.keyFile, err)
	}

	r.mu.Lock()
	r.cert = &cert
	r.certMod = certInfo.ModTime()
	r.keyMod = keyInfo.ModTime()
	r.mu.Unlock()
	return nil
}

// GetCertificate is the tls.Config.GetCertificate callback. It reloads the
// pair on detected file changes and returns the current certificate. A reload
// failure is non-fatal: the last good certificate is served so an
// in-progress, incomplete rotation does not drop the listener.
func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if r.changed() {
		// Best-effort: ignore the error and fall through to the cached cert.
		_ = r.reload()
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cert, nil
}

// changed reports whether either file's mtime advanced past the last load.
func (r *CertReloader) changed() bool {
	certInfo, err := os.Stat(r.certFile)
	if err != nil {
		return false
	}
	keyInfo, err := os.Stat(r.keyFile)
	if err != nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return certInfo.ModTime().After(r.certMod) || keyInfo.ModTime().After(r.keyMod)
}

// ServerConfig assembles a *tls.Config from the TLS configuration. It returns
// nil (and no error) when TLS is not configured, so callers fall back to
// plaintext. The returned config uses GetCertificate for hot-reload and, when
// a client CA is set, requires and verifies client certificates (mTLS).
func ServerConfig(cfg Config) (*tls.Config, error) {
	// Validate first so an inconsistent partial config (e.g. cert_file without
	// key_file, or a client CA without a server cert) is reported loudly rather
	// than silently treated as "TLS disabled" by the Enabled() check below.
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled() {
		return nil, nil
	}

	reloader, err := NewCertReloader(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, err
	}

	minVersion := uint16(tls.VersionTLS12)
	if cfg.MinVersion != "" {
		v, err := ParseMinVersion(cfg.MinVersion)
		if err != nil {
			return nil, err
		}
		minVersion = v
	}

	tlsCfg := &tls.Config{
		GetCertificate: reloader.GetCertificate,
		MinVersion:     minVersion,
	}

	if cfg.ClientCA != "" {
		caPEM, err := os.ReadFile(cfg.ClientCA)
		if err != nil {
			return nil, fmt.Errorf("read client CA file %s: %w", cfg.ClientCA, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("client CA file %s contains no valid PEM certificate", cfg.ClientCA)
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return tlsCfg, nil
}
