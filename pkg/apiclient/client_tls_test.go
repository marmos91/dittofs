package apiclient

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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// genKeyPair mints a self-signed ECDSA cert/key pair, writes them to dir, and
// returns their paths plus the leaf cert (for trust-pool assembly).
func genKeyPair(t *testing.T, dir, cn string) (certPath, keyPath string, leaf *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{cn},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	parsed, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	certPath = filepath.Join(dir, cn+".crt")
	keyPath = filepath.Join(dir, cn+".key")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	require.NoError(t, os.WriteFile(certPath, certPEM, 0o600))
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0o600))
	return certPath, keyPath, parsed
}

// writeCAFile writes a leaf cert as a PEM CA bundle and returns the path.
func writeCAFile(t *testing.T, dir string, leaf *x509.Certificate) string {
	t.Helper()
	caPath := filepath.Join(dir, "ca.crt")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})
	require.NoError(t, os.WriteFile(caPath, pemBytes, 0o600))
	return caPath
}

func TestClient_CustomCA_TrustsPrivateCert(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	caPath := writeCAFile(t, t.TempDir(), ts.Certificate())

	// Without the CA the self-signed server cert is untrusted → request fails.
	noCA := New(ts.URL)
	assert.Error(t, noCA.do(http.MethodGet, "/", nil, nil),
		"expected untrusted cert to fail without --cacert")

	// With the CA trusted the request succeeds.
	withCA := New(ts.URL, WithCACert(caPath))
	require.NoError(t, withCA.tlsErr)
	assert.NoError(t, withCA.do(http.MethodGet, "/", nil, nil))
}

func TestClient_InsecureSkipVerify(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := New(ts.URL, WithInsecureSkipVerify(true))
	require.NoError(t, c.tlsErr)
	assert.NoError(t, c.do(http.MethodGet, "/", nil, nil))
}

func TestClient_BadCAFile(t *testing.T) {
	c := New("https://example.invalid", WithCACert("/nonexistent/ca.pem"))
	err := c.do(http.MethodGet, "/", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read CA cert")
}

func TestClient_ClientCertWithoutKey_Errors(t *testing.T) {
	certPath, _, _ := genKeyPair(t, t.TempDir(), "client")
	c := New("https://example.invalid", WithClientCert(certPath, ""))
	err := c.do(http.MethodGet, "/", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be provided together")
}

func TestClient_MTLS_PresentsClientCert(t *testing.T) {
	dir := t.TempDir()
	clientCertPath, clientKeyPath, clientLeaf := genKeyPair(t, dir, "client")

	clientPool := x509.NewCertPool()
	clientPool.AddCert(clientLeaf)

	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ts.TLS = &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  clientPool,
		MinVersion: tls.VersionTLS12,
	}
	ts.StartTLS()
	defer ts.Close()

	caPath := writeCAFile(t, dir, ts.Certificate())

	// CA-only (no client cert) → server rejects at handshake.
	noCert := New(ts.URL, WithCACert(caPath))
	require.NoError(t, noCert.tlsErr)
	assert.Error(t, noCert.do(http.MethodGet, "/", nil, nil),
		"mTLS server should reject a client presenting no certificate")

	// CA + client cert → accepted.
	withCert := New(ts.URL, WithCACert(caPath), WithClientCert(clientCertPath, clientKeyPath))
	require.NoError(t, withCert.tlsErr)
	assert.NoError(t, withCert.do(http.MethodGet, "/", nil, nil))
}

func TestClient_NoTLSOptions_UsesDefaultTransport(t *testing.T) {
	// With no TLS option set, the client must not install a custom transport,
	// preserving the shared default-transport behavior.
	c := New("http://example.invalid")
	assert.Nil(t, c.httpClient.Transport)
	assert.NoError(t, c.tlsErr)
}
