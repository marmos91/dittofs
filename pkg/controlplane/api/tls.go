package api

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"time"
)

// minTLSVersions maps the human-readable values accepted in
// controlplane.tls.min_version to their crypto/tls constants. Only TLS 1.2 and
// 1.3 are offered; older versions are considered insecure and are not exposed.
var minTLSVersions = map[string]uint16{
	"1.2": tls.VersionTLS12,
	"1.3": tls.VersionTLS13,
}

// certReloader holds the server certificate and re-reads the cert/key files
// from disk when they change, so a platform rotating the files (cert-manager,
// a mounted Secret, etc.) does not require a server restart.
//
// Reload is lazy and triggered from GetCertificate (the tls.Config callback
// invoked once per handshake): on each call it stats the files and, if either
// mtime advanced since the last successful load, re-parses the pair. There is
// no background goroutine — handshakes are infrequent relative to a stat(2),
// and a failed reload keeps serving the last good certificate rather than
// breaking new connections.
type certReloader struct {
	certFile string
	keyFile  string

	mu      sync.RWMutex
	cert    *tls.Certificate
	certMod time.Time
	keyMod  time.Time
}

// newCertReloader loads the initial cert/key pair, failing fast if the files
// are missing or do not parse, and returns a reloader primed with that pair.
func newCertReloader(certFile, keyFile string) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile}
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
func (r *certReloader) reload() error {
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

// getCertificate is the tls.Config.GetCertificate callback. It reloads the
// pair on detected file changes and returns the current certificate. A reload
// failure is non-fatal: the last good certificate is served so an
// in-progress, incomplete rotation does not drop the listener.
func (r *certReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if r.changed() {
		// Best-effort: ignore the error and fall through to the cached cert.
		_ = r.reload()
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cert, nil
}

// changed reports whether either file's mtime advanced past the last load.
func (r *certReloader) changed() bool {
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

// buildTLSConfig assembles a *tls.Config from the API TLS configuration. It
// returns nil (and no error) when TLS is not configured, so callers fall back
// to plain HTTP. The returned config uses GetCertificate for hot-reload and,
// when a client CA is set, requires and verifies client certificates (mTLS).
func buildTLSConfig(cfg TLSConfig) (*tls.Config, error) {
	if !cfg.Enabled() {
		return nil, nil
	}

	reloader, err := newCertReloader(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, err
	}

	var minVersion uint16 = tls.VersionTLS12
	if cfg.MinVersion != "" {
		v, ok := minTLSVersions[cfg.MinVersion]
		if !ok {
			return nil, fmt.Errorf("controlplane.tls.min_version %q is not supported (use \"1.2\" or \"1.3\")", cfg.MinVersion)
		}
		minVersion = v
	}

	tlsCfg := &tls.Config{
		GetCertificate: reloader.getCertificate,
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
