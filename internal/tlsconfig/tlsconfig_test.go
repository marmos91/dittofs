package tlsconfig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testCert is a generated certificate + key pair (PEM) plus the parsed cert.
type testCert struct {
	certPEM []byte
	keyPEM  []byte
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
}

// generateCert mints a self-signed ECDSA certificate. parent==nil self-signs;
// isCA makes it able to sign others (used as the client-CA in mTLS tests).
func generateCert(t *testing.T, commonName string, isCA bool, parent *testCert) testCert {
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
		DNSNames:              []string{commonName},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	if isCA {
		tmpl.IsCA = true
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	signerCert, signerKey := tmpl, key
	if parent != nil {
		signerCert, signerKey = parent.cert, parent.key
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

func TestConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"unset is valid (plaintext)", Config{}, false},
		{"cert+key valid", Config{CertFile: "c", KeyFile: "k"}, false},
		{"cert without key fails", Config{CertFile: "c"}, true},
		{"key without cert fails", Config{KeyFile: "k"}, true},
		{"client_ca without cert/key fails", Config{ClientCA: "ca"}, true},
		{"client_ca with cert+key valid", Config{CertFile: "c", KeyFile: "k", ClientCA: "ca"}, false},
		{"bad min_version fails", Config{CertFile: "c", KeyFile: "k", MinVersion: "1.1"}, true},
		{"min_version 1.2 valid", Config{CertFile: "c", KeyFile: "k", MinVersion: "1.2"}, false},
		{"min_version 1.3 valid", Config{CertFile: "c", KeyFile: "k", MinVersion: "1.3"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestParseMinVersion(t *testing.T) {
	if v, err := ParseMinVersion("1.2"); err != nil || v != tls.VersionTLS12 {
		t.Fatalf("1.2 → (%d, %v), want (%d, nil)", v, err, tls.VersionTLS12)
	}
	if v, err := ParseMinVersion("1.3"); err != nil || v != tls.VersionTLS13 {
		t.Fatalf("1.3 → (%d, %v), want (%d, nil)", v, err, tls.VersionTLS13)
	}
	if _, err := ParseMinVersion("1.1"); err == nil {
		t.Fatal("expected error for 1.1")
	}
}

func TestServerConfig(t *testing.T) {
	// Disabled → nil, no error.
	cfg, err := ServerConfig(Config{})
	if err != nil || cfg != nil {
		t.Fatalf("disabled ServerConfig = (%v, %v), want (nil, nil)", cfg, err)
	}

	dir := t.TempDir()
	server := generateCert(t, "localhost", false, nil)
	certPath, keyPath := writeCertFiles(t, dir, server)

	// Enabled, no client CA → no client auth.
	cfg, err = ServerConfig(Config{CertFile: certPath, KeyFile: keyPath, MinVersion: "1.3"})
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	if cfg == nil || cfg.GetCertificate == nil {
		t.Fatal("expected non-nil config with GetCertificate set")
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %d, want %d", cfg.MinVersion, tls.VersionTLS13)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Errorf("ClientAuth = %d, want NoClientCert", cfg.ClientAuth)
	}

	// With a client CA → require + verify client cert (mTLS).
	ca := generateCert(t, "test-ca", true, nil)
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, ca.certPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	cfg, err = ServerConfig(Config{CertFile: certPath, KeyFile: keyPath, ClientCA: caPath})
	if err != nil {
		t.Fatalf("ServerConfig mTLS: %v", err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %d, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Error("expected ClientCAs pool to be set")
	}

	// Missing cert files → fail fast.
	if _, err := ServerConfig(Config{CertFile: "/nonexistent/c", KeyFile: "/nonexistent/k"}); err == nil {
		t.Fatal("expected error for missing cert files")
	}
}

func TestCertReloader_HotSwap(t *testing.T) {
	dir := t.TempDir()
	first := generateCert(t, "first.example", false, nil)
	certPath, keyPath := writeCertFiles(t, dir, first)

	reloader, err := NewCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}

	got, err := reloader.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	leaf, _ := x509.ParseCertificate(got.Certificate[0])
	if leaf.Subject.CommonName != "first.example" {
		t.Fatalf("initial cert CN = %q, want first.example", leaf.Subject.CommonName)
	}

	// Rotate the files on disk. Force a later mtime so the change is detected
	// even on coarse-grained filesystem timestamps.
	second := generateCert(t, "second.example", false, nil)
	if err := os.WriteFile(certPath, second.certPEM, 0o600); err != nil {
		t.Fatalf("rewrite cert: %v", err)
	}
	if err := os.WriteFile(keyPath, second.keyPEM, 0o600); err != nil {
		t.Fatalf("rewrite key: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(certPath, future, future)
	_ = os.Chtimes(keyPath, future, future)

	got2, err := reloader.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate after rotation: %v", err)
	}
	leaf2, _ := x509.ParseCertificate(got2.Certificate[0])
	if leaf2.Subject.CommonName != "second.example" {
		t.Fatalf("after rotation cert CN = %q, want second.example", leaf2.Subject.CommonName)
	}
}
