package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testCert is a generated certificate + key pair (PEM) plus the parsed
// x509.Certificate, used as both a server cert and a client cert in tests.
type testCert struct {
	certPEM []byte
	keyPEM  []byte
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
}

// generateCert mints a self-signed ECDSA certificate. When isCA is true the
// cert can sign others (used for the client-CA in mTLS tests); commonName/dns
// shape the leaf. parent==nil self-signs.
func generateCert(t *testing.T, commonName string, dnsNames []string, isCA bool, parent *testCert) testCert {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	if isCA {
		tmpl.IsCA = true
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}

	signerCert := tmpl
	signerKey := key
	if parent != nil {
		signerCert = parent.cert
		signerKey = parent.key
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, signerCert, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return testCert{certPEM: certPEM, keyPEM: keyPEM, cert: parsed, key: key}
}

// writeCertFiles writes cert + key to dir and returns their paths.
func writeCertFiles(t *testing.T, dir string, c testCert) (certPath, keyPath string) {
	t.Helper()
	certPath = filepath.Join(dir, "tls.crt")
	keyPath = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certPath, c.certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, c.keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

// --- config validation -----------------------------------------------------

func TestAPIConfig_Validate_TLS(t *testing.T) {
	cases := []struct {
		name    string
		tls     TLSConfig
		wantErr bool
	}{
		{"unset is valid (http)", TLSConfig{}, false},
		{"cert+key valid", TLSConfig{CertFile: "c", KeyFile: "k"}, false},
		{"cert without key fails", TLSConfig{CertFile: "c"}, true},
		{"key without cert fails", TLSConfig{KeyFile: "k"}, true},
		{"client_ca without cert/key fails", TLSConfig{ClientCA: "ca"}, true},
		{"client_ca with cert+key valid", TLSConfig{CertFile: "c", KeyFile: "k", ClientCA: "ca"}, false},
		{"bad min_version fails", TLSConfig{CertFile: "c", KeyFile: "k", MinVersion: "1.1"}, true},
		{"min_version 1.2 valid", TLSConfig{CertFile: "c", KeyFile: "k", MinVersion: "1.2"}, false},
		{"min_version 1.3 valid", TLSConfig{CertFile: "c", KeyFile: "k", MinVersion: "1.3"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &APIConfig{TLS: tc.tls}
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestNewServer_TLSCertFilesMissing_FailsFast(t *testing.T) {
	cpStore, cfg := testSetup(t, 0)
	cfg.TLS = TLSConfig{CertFile: "/nonexistent/tls.crt", KeyFile: "/nonexistent/tls.key"}

	_, err := NewServer(cfg, nil, cpStore, 30*time.Minute)
	if err == nil {
		t.Fatal("expected NewServer to fail when cert files are missing")
	}
}

// --- back-compat: HTTP when TLS unset ---------------------------------------

func TestNewServer_HTTPByDefault(t *testing.T) {
	cpStore, cfg := testSetup(t, 0)
	server, err := NewServer(cfg, nil, cpStore, 30*time.Minute)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if server.TLSEnabled() {
		t.Fatal("expected plain HTTP server when TLS is unset")
	}
}

// startServer boots the server on an ephemeral port and returns its address.
func startServer(t *testing.T, server *Server) (addr string, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errChan := make(chan error, 1)
	go func() { errChan <- server.Start(ctx) }()

	// The server binds host:port; resolve the concrete addr for dialing.
	addr = net.JoinHostPort(server.config.Host, fmt.Sprint(server.config.Port))
	waitForServerReady(t, addr, errChan, 5*time.Second)

	return addr, func() {
		cancel()
		select {
		case <-errChan:
		case <-time.After(5 * time.Second):
			t.Error("server did not shut down in time")
		}
	}
}

// freePort returns an OS-allocated free TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func TestAPIServer_HTTPS_Serves_And_RejectsPlainHTTP(t *testing.T) {
	dir := t.TempDir()
	serverCert := generateCert(t, "localhost", []string{"localhost"}, false, nil)
	certPath, keyPath := writeCertFiles(t, dir, serverCert)

	cpStore, cfg := testSetup(t, freePort(t))
	cfg.Host = "127.0.0.1"
	cfg.TLS = TLSConfig{CertFile: certPath, KeyFile: keyPath}

	server, err := NewServer(cfg, nil, cpStore, 30*time.Minute)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if !server.TLSEnabled() {
		t.Fatal("expected TLS-enabled server")
	}

	addr, stop := startServer(t, server)
	defer stop()

	// HTTPS request with the server cert trusted succeeds.
	pool := x509.NewCertPool()
	pool.AddCert(serverCert.cert)
	httpsClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := httpsClient.Get("https://" + addr + "/health")
	if err != nil {
		t.Fatalf("https GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("https /health status = %d, want 200", resp.StatusCode)
	}

	// Plain HTTP against the TLS listener must NOT yield a healthy 200 — the
	// server speaks TLS, so an http:// request is either an error or Go's
	// stdlib "Client sent an HTTP request to an HTTPS server" (HTTP 400). It
	// must never reach the /health handler and return 200.
	httpClient := &http.Client{Timeout: 2 * time.Second}
	httpResp, err := httpClient.Get("http://" + addr + "/health")
	if err == nil {
		body, _ := io.ReadAll(httpResp.Body)
		_ = httpResp.Body.Close()
		if httpResp.StatusCode == http.StatusOK {
			t.Fatalf("plain HTTP reached the health handler on a TLS listener: status=200 body=%q", string(body))
		}
	}
}

// --- mTLS -------------------------------------------------------------------

func TestAPIServer_MTLS_RejectsNoClientCert_AcceptsValid(t *testing.T) {
	dir := t.TempDir()

	// A CA that signs both the server cert and the client cert.
	ca := generateCert(t, "test-ca", nil, true, nil)
	serverCert := generateCert(t, "localhost", []string{"localhost"}, false, &ca)
	clientCert := generateCert(t, "client", nil, false, &ca)

	certPath, keyPath := writeCertFiles(t, dir, serverCert)
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, ca.certPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}

	cpStore, cfg := testSetup(t, freePort(t))
	cfg.Host = "127.0.0.1"
	cfg.TLS = TLSConfig{CertFile: certPath, KeyFile: keyPath, ClientCA: caPath}

	server, err := NewServer(cfg, nil, cpStore, 30*time.Minute)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if !server.mTLSEnabled() {
		t.Fatal("expected mTLS to be enabled when client_ca is set")
	}

	addr, stop := startServer(t, server)
	defer stop()

	rootPool := x509.NewCertPool()
	rootPool.AddCert(ca.cert)

	// No client cert presented → rejected at handshake.
	noCertClient := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: rootPool}},
	}
	if _, err := noCertClient.Get("https://" + addr + "/health"); err == nil {
		t.Fatal("mTLS server accepted a connection with no client certificate")
	}

	// Valid client cert presented → accepted.
	clientTLSCert := tls.Certificate{
		Certificate: [][]byte{clientCert.cert.Raw},
		PrivateKey:  clientCert.key,
	}
	withCertClient := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:      rootPool,
			Certificates: []tls.Certificate{clientTLSCert},
		}},
	}
	resp, err := withCertClient.Get("https://" + addr + "/health")
	if err != nil {
		t.Fatalf("mTLS GET with valid client cert failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("mTLS /health status = %d, want 200", resp.StatusCode)
	}
}

// Note: cert hot-reload is now covered by internal/tlsconfig
// (TestCertReloader_HotSwap). The reloader was lifted into that shared package
// so the control-plane API and the NFS adapter share one cert-loading path.

// --- bind address -----------------------------------------------------------

func TestAPIServer_BindAddressHonored(t *testing.T) {
	port := freePort(t)
	cpStore, cfg := testSetup(t, port)
	cfg.Host = "127.0.0.1"

	server, err := NewServer(cfg, nil, cpStore, 30*time.Minute)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	wantAddr := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	if server.server.Addr != wantAddr {
		t.Fatalf("server.Addr = %q, want %q", server.server.Addr, wantAddr)
	}

	_, stop := startServer(t, server)
	defer stop()

	// Reachable on 127.0.0.1.
	resp, err := http.Get("http://" + wantAddr + "/health")
	if err != nil {
		t.Fatalf("GET on bound addr: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/health status = %d, want 200", resp.StatusCode)
	}
}

func TestAPIConfig_DefaultHost(t *testing.T) {
	cfg := &APIConfig{}
	cfg.ApplyDefaults()
	if cfg.Host != "127.0.0.1" {
		t.Errorf("default host = %q, want 127.0.0.1", cfg.Host)
	}
}
