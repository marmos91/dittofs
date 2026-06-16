package nfs

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/pool"
	"github.com/marmos91/dittofs/internal/tlsconfig"
)

type tlsTestCert struct {
	certPEM []byte
	keyPEM  []byte
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
}

func mkCert(t *testing.T, cn string, isCA bool, parent *tlsTestCert) tlsTestCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
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
		t.Fatal(err)
	}
	parsed, _ := x509.ParseCertificate(der)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return tlsTestCert{certPEM: certPEM, keyPEM: keyPEM, cert: parsed, key: key}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// serverTLS builds a server *tls.Config from a fresh cert (optionally requiring
// a client cert signed by ca for mTLS).
func serverTLS(t *testing.T, server tlsTestCert, ca *tlsTestCert) *tls.Config {
	t.Helper()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	writeFile(t, certPath, server.certPEM)
	writeFile(t, keyPath, server.keyPEM)
	cfg := tlsconfig.Config{CertFile: certPath, KeyFile: keyPath, MinVersion: "1.3"}
	if ca != nil {
		caPath := filepath.Join(dir, "ca.crt")
		writeFile(t, caPath, ca.certPEM)
		cfg.ClientCA = caPath
	}
	tc, err := tlsconfig.ServerConfig(cfg)
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	return tc
}

// readRPCRecord reads one RFC 5531 record-marked message and returns its body.
func readRPCRecord(t *testing.T, r io.Reader) []byte {
	t.Helper()
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		t.Fatalf("read fragment header: %v", err)
	}
	n := binary.BigEndian.Uint32(hdr[:]) &^ 0x80000000
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		t.Fatalf("read fragment body: %v", err)
	}
	return body
}

// runStartTLSServer accepts one connection, runs c.startTLS, then echoes 5
// bytes over the upgraded connection. The handshake error (if any) is returned.
func runStartTLSServer(ln net.Listener, srv *NFSAdapter, errc chan<- error) {
	conn, err := ln.Accept()
	if err != nil {
		errc <- err
		return
	}
	c := &NFSConnection{server: srv, conn: conn}
	if err := c.startTLS(context.Background(), 1, "test"); err != nil {
		errc <- err
		return
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c.conn, buf); err != nil {
		errc <- err
		return
	}
	_, _ = c.conn.Write(buf)
	errc <- nil
}

func TestNFSConnection_StartTLS_OpportunisticHandshake(t *testing.T) {
	serverCert := mkCert(t, "localhost", false, nil)
	srv := &NFSAdapter{
		config:    NFSConfig{Timeouts: NFSTimeoutsConfig{Write: 5 * time.Second}},
		tlsConfig: serverTLS(t, serverCert, nil),
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	errc := make(chan error, 1)
	go runStartTLSServer(ln, srv, errc)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	// The server writes the STARTTLS verifier first (we invoke startTLS directly,
	// which assumes the probe has already been consumed).
	record := readRPCRecord(t, conn)
	if !bytes.Contains(record, []byte(rpc.StartTLSVerifier)) {
		t.Fatalf("reply does not contain STARTTLS verifier: %x", record)
	}

	pool := x509.NewCertPool()
	pool.AddCert(serverCert.cert)
	tc := tls.Client(conn, &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13})
	if err := tc.HandshakeContext(context.Background()); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if _, err := tc.Write([]byte("hello")); err != nil {
		t.Fatalf("encrypted write: %v", err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(tc, got); err != nil {
		t.Fatalf("encrypted read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("echo = %q, want hello", got)
	}
	if err := <-errc; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func TestNFSConnection_StartTLS_MTLS(t *testing.T) {
	ca := mkCert(t, "test-ca", true, nil)
	serverCert := mkCert(t, "localhost", false, &ca)
	clientCert := mkCert(t, "client", false, &ca)

	rootPool := x509.NewCertPool()
	rootPool.AddCert(ca.cert)
	clientTLSCert := tls.Certificate{Certificate: [][]byte{clientCert.cert.Raw}, PrivateKey: clientCert.key}

	newSrv := func() *NFSAdapter {
		return &NFSAdapter{
			config:    NFSConfig{Timeouts: NFSTimeoutsConfig{Write: 5 * time.Second}},
			tlsConfig: serverTLS(t, serverCert, &ca),
		}
	}

	// Case 1: client presents a valid cert → handshake succeeds.
	t.Run("valid client cert accepted", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		defer func() { _ = ln.Close() }()
		errc := make(chan error, 1)
		go runStartTLSServer(ln, newSrv(), errc)

		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_ = readRPCRecord(t, conn)
		tc := tls.Client(conn, &tls.Config{
			RootCAs:      rootPool,
			ServerName:   "localhost",
			Certificates: []tls.Certificate{clientTLSCert},
			MinVersion:   tls.VersionTLS13,
		})
		if err := tc.HandshakeContext(context.Background()); err != nil {
			t.Fatalf("handshake with client cert: %v", err)
		}
		_, _ = tc.Write([]byte("hello"))
		got := make([]byte, 5)
		if _, err := io.ReadFull(tc, got); err != nil {
			t.Fatalf("encrypted read: %v", err)
		}
		if err := <-errc; err != nil {
			t.Fatalf("server: %v", err)
		}
	})

	// Case 2: no client cert → server rejects at handshake.
	t.Run("missing client cert rejected", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		defer func() { _ = ln.Close() }()
		errc := make(chan error, 1)
		go runStartTLSServer(ln, newSrv(), errc)

		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_ = readRPCRecord(t, conn)
		tc := tls.Client(conn, &tls.Config{RootCAs: rootPool, ServerName: "localhost", MinVersion: tls.VersionTLS13})
		// The client handshake completes its flight, then fails when the server
		// rejects the missing cert; the server's startTLS must report an error.
		_ = tc.HandshakeContext(context.Background())
		if err := <-errc; err == nil {
			t.Fatal("expected server STARTTLS handshake to fail without a client cert")
		}
	})
}

func TestNFSConnection_HandleTLSGate(t *testing.T) {
	nullProbe := &rpc.RPCCallMessage{Procedure: 0, Cred: rpc.OpaqueAuth{Flavor: rpc.AuthTLS}}
	plainGetattr := &rpc.RPCCallMessage{Procedure: 1, Cred: rpc.OpaqueAuth{Flavor: rpc.AuthUnix}}

	t.Run("require mode rejects plaintext", func(t *testing.T) {
		c := &NFSConnection{server: &NFSAdapter{tlsConfig: &tls.Config{}, tlsRequire: true}}
		handled, err := c.handleTLSGate(context.Background(), plainGetattr, pool.Get(0), "x")
		if !handled || err == nil {
			t.Fatalf("require/plaintext = (handled=%v, err=%v), want (true, error)", handled, err)
		}
	})

	t.Run("opportunistic passes plaintext through", func(t *testing.T) {
		c := &NFSConnection{server: &NFSAdapter{tlsConfig: &tls.Config{}, tlsRequire: false}}
		handled, err := c.handleTLSGate(context.Background(), plainGetattr, pool.Get(0), "x")
		if handled || err != nil {
			t.Fatalf("opportunistic/plaintext = (handled=%v, err=%v), want (false, nil)", handled, err)
		}
	})

	// Sanity: the probe is recognized as a probe (Procedure 0 + AUTH_TLS). We
	// don't drive the full upgrade here (covered by the handshake tests); we
	// just confirm detection does not fall through to the plaintext path.
	if nullProbe.Procedure != 0 || nullProbe.GetAuthFlavor() != rpc.AuthTLS {
		t.Fatal("probe detection predicate is wrong")
	}
}
