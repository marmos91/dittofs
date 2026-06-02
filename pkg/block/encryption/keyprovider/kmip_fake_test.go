package keyprovider

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
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gemalto/kmip-go"
	"github.com/gemalto/kmip-go/kmip14"
	"github.com/gemalto/kmip-go/ttlv"
)

// genSelfSigned writes a self-signed ECDSA cert + key PEM pair to dir and
// returns their paths plus the PEM-encoded certificate (usable as a CA).
// Deterministic shape; the only randomness is the key material, which does
// not affect any assertion.
func genSelfSigned(t *testing.T, dir, name string) (certPath, keyPath string, certPEM []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	certPath = filepath.Join(dir, name+"-cert.pem")
	keyPath = filepath.Join(dir, name+"-key.pem")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath, certPEM
}

// fakeKMIPServer is a single-shot TLS listener that answers exactly one
// KMIP Get request with a server-provided ResponseMessage. It speaks the
// minimal subset of the protocol the provider drives: read one framed TTLV
// request, write one framed TTLV response.
type fakeKMIPServer struct {
	ln   net.Listener
	resp ttlv.TTLV // pre-marshalled response to send back
	wg   sync.WaitGroup
}

// kmipSuccessResponse marshals a Get ResponseMessage carrying a symmetric
// key with the given raw material as a ByteString KeyMaterial.
func kmipSuccessResponse(t *testing.T, keyUID string, material []byte) ttlv.TTLV {
	t.Helper()
	resp := kmip.ResponseMessage{
		ResponseHeader: kmip.ResponseHeader{
			ProtocolVersion: kmip.ProtocolVersion{ProtocolVersionMajor: 1, ProtocolVersionMinor: 4},
			TimeStamp:       time.Now(),
			BatchCount:      1,
		},
		BatchItem: []kmip.ResponseBatchItem{{
			Operation:    kmip14.OperationGet,
			ResultStatus: kmip14.ResultStatusSuccess,
			ResponsePayload: kmip.GetResponsePayload{
				ObjectType:       kmip14.ObjectTypeSymmetricKey,
				UniqueIdentifier: keyUID,
				SymmetricKey: &kmip.SymmetricKey{
					KeyBlock: kmip.KeyBlock{
						KeyFormatType: kmip14.KeyFormatTypeRaw,
						KeyValue: &kmip.KeyValue{
							KeyMaterial: material,
						},
						CryptographicAlgorithm: kmip14.CryptographicAlgorithmAES,
						CryptographicLength:    len(material) * 8,
					},
				},
			},
		}},
	}
	raw, err := ttlv.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal kmip response: %v", err)
	}
	return raw
}

// kmipFailureResponse marshals a Get ResponseMessage carrying a
// non-success result status.
func kmipFailureResponse(t *testing.T) ttlv.TTLV {
	t.Helper()
	resp := kmip.ResponseMessage{
		ResponseHeader: kmip.ResponseHeader{
			ProtocolVersion: kmip.ProtocolVersion{ProtocolVersionMajor: 1, ProtocolVersionMinor: 4},
			TimeStamp:       time.Now(),
			BatchCount:      1,
		},
		BatchItem: []kmip.ResponseBatchItem{{
			Operation:     kmip14.OperationGet,
			ResultStatus:  kmip14.ResultStatusOperationFailed,
			ResultReason:  kmip14.ResultReasonItemNotFound,
			ResultMessage: "no such object",
		}},
	}
	raw, err := ttlv.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal kmip failure response: %v", err)
	}
	return raw
}

// startFakeKMIP runs a TLS listener that answers one connection with resp.
func startFakeKMIP(t *testing.T, tlsCfg *tls.Config, resp ttlv.TTLV) *fakeKMIPServer {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	s := &fakeKMIPServer{ln: ln, resp: resp}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		// Drain the request: read one framed TTLV so we don't reply before
		// the client finishes writing (avoids a RST on some platforms).
		if _, err := readOneFrame(conn); err != nil {
			return
		}
		_, _ = conn.Write(s.resp)
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeKMIPServer) addr() string { return s.ln.Addr().String() }

// readOneFrame mirrors the framing in readKMIPMessage so the fake server
// consumes exactly one request before replying.
func readOneFrame(r io.Reader) ([]byte, error) {
	const headerLen = 8
	header := make([]byte, headerLen)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	bodyLen := binary.BigEndian.Uint32(header[4:8])
	pad := (8 - int(bodyLen%8)) % 8
	body := make([]byte, headerLen+int(bodyLen)+pad)
	copy(body, header)
	if _, err := io.ReadFull(r, body[headerLen:]); err != nil {
		return nil, err
	}
	return body, nil
}

// serverTLSConfig builds the server-side TLS config from the given cert.
func serverTLSConfig(t *testing.T, certPath, keyPath string) *tls.Config {
	t.Helper()
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("server LoadX509KeyPair: %v", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
}

// TestKMIP_FetchSymmetricKey_HappyPath drives newKMIPProvider end-to-end
// against a fake KMIP server: dial, marshal Get, read framed response,
// decode the symmetric key, and confirm Wrap/Unwrap round-trips with it.
func TestKMIP_FetchSymmetricKey_HappyPath(t *testing.T) {
	dir := t.TempDir()
	// One self-signed cert used for both server identity and as the CA the
	// client trusts, and (separately) as the client cert.
	srvCert, srvKey, srvPEM := genSelfSigned(t, dir, "server")
	cliCert, cliKey, _ := genSelfSigned(t, dir, "client")
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, srvPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}

	material := bytes.Repeat([]byte{0xA7}, 32)
	resp := kmipSuccessResponse(t, "key-uid-1", material)
	srv := startFakeKMIP(t, serverTLSConfig(t, srvCert, srvKey), resp)

	cfg := Config{
		Kind:       KindKMIP,
		Endpoint:   srv.addr(),
		ClientCert: cliCert,
		ClientKey:  cliKey,
		ServerCA:   caPath,
		KeyUID:     "key-uid-1",
		TimeoutMS:  5000,
	}
	p, err := newKMIPProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newKMIPProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if !bytes.Equal(p.masterKey, material) {
		t.Fatalf("fetched master key mismatch: got %x want %x", p.masterKey, material)
	}

	blockKey := bytes.Repeat([]byte{0x33}, 32)
	wrapped, id, err := p.Wrap(context.Background(), blockKey)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	got, err := p.Unwrap(context.Background(), wrapped, id)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if !bytes.Equal(got, blockKey) {
		t.Fatalf("Unwrap roundtrip mismatch")
	}
}

// TestKMIP_FetchSymmetricKey_WrongLength verifies a non-32-byte key from
// the server is rejected by newKMIPProvider.
func TestKMIP_FetchSymmetricKey_WrongLength(t *testing.T) {
	dir := t.TempDir()
	srvCert, srvKey, srvPEM := genSelfSigned(t, dir, "server")
	cliCert, cliKey, _ := genSelfSigned(t, dir, "client")
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, srvPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}

	resp := kmipSuccessResponse(t, "k", bytes.Repeat([]byte{0x01}, 16)) // AES-128, wrong for us
	srv := startFakeKMIP(t, serverTLSConfig(t, srvCert, srvKey), resp)

	cfg := Config{
		Kind: KindKMIP, Endpoint: srv.addr(),
		ClientCert: cliCert, ClientKey: cliKey, ServerCA: caPath, KeyUID: "k",
	}
	_, err := newKMIPProvider(context.Background(), cfg)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig for wrong-length key, got %v", err)
	}
}

// TestKMIP_FetchSymmetricKey_ServerFailure verifies a non-success KMIP
// result status surfaces as an error.
func TestKMIP_FetchSymmetricKey_ServerFailure(t *testing.T) {
	dir := t.TempDir()
	srvCert, srvKey, srvPEM := genSelfSigned(t, dir, "server")
	cliCert, cliKey, _ := genSelfSigned(t, dir, "client")
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, srvPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}

	srv := startFakeKMIP(t, serverTLSConfig(t, srvCert, srvKey), kmipFailureResponse(t))

	cfg := Config{
		Kind: KindKMIP, Endpoint: srv.addr(),
		ClientCert: cliCert, ClientKey: cliKey, ServerCA: caPath, KeyUID: "k",
	}
	_, err := newKMIPProvider(context.Background(), cfg)
	if err == nil {
		t.Fatal("want error on KMIP failure status, got nil")
	}
	if !strings.Contains(err.Error(), "kmip Get failed") {
		t.Errorf("error should mention Get failure, got %q", err.Error())
	}
}

// TestKMIP_DialError verifies a connection to a dead endpoint surfaces a
// dial error rather than hanging.
func TestKMIP_DialError(t *testing.T) {
	dir := t.TempDir()
	cliCert, cliKey, _ := genSelfSigned(t, dir, "client")

	cfg := Config{
		Kind: KindKMIP,
		// Reserved TEST-NET-1 address; connection will fail fast under the
		// short timeout below.
		Endpoint:   "192.0.2.1:5696",
		ClientCert: cliCert, ClientKey: cliKey, KeyUID: "k",
		TimeoutMS: 200,
	}
	start := time.Now()
	_, err := newKMIPProvider(context.Background(), cfg)
	if err == nil {
		t.Fatal("want dial error, got nil")
	}
	if !strings.Contains(err.Error(), "kmip dial") {
		t.Errorf("error should mention dial, got %q", err.Error())
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("dial took %s; timeout not honored", elapsed)
	}
}

// TestBuildKMIPTLSConfig covers the cert-load and server-CA branches.
func TestBuildKMIPTLSConfig(t *testing.T) {
	dir := t.TempDir()
	cliCert, cliKey, cliPEM := genSelfSigned(t, dir, "client")

	t.Run("valid_no_ca", func(t *testing.T) {
		cfg, err := buildKMIPTLSConfig(Config{ClientCert: cliCert, ClientKey: cliKey})
		if err != nil {
			t.Fatalf("buildKMIPTLSConfig: %v", err)
		}
		if len(cfg.Certificates) != 1 {
			t.Errorf("want 1 client cert, got %d", len(cfg.Certificates))
		}
		if cfg.RootCAs != nil {
			t.Error("RootCAs should be nil when no ServerCA configured")
		}
	})

	t.Run("valid_with_ca", func(t *testing.T) {
		caPath := filepath.Join(dir, "ca.pem")
		if err := os.WriteFile(caPath, cliPEM, 0o600); err != nil {
			t.Fatalf("write ca: %v", err)
		}
		cfg, err := buildKMIPTLSConfig(Config{ClientCert: cliCert, ClientKey: cliKey, ServerCA: caPath})
		if err != nil {
			t.Fatalf("buildKMIPTLSConfig with CA: %v", err)
		}
		if cfg.RootCAs == nil {
			t.Error("RootCAs should be set when ServerCA configured")
		}
	})

	t.Run("bad_client_cert", func(t *testing.T) {
		if _, err := buildKMIPTLSConfig(Config{ClientCert: "/nonexistent", ClientKey: "/nonexistent"}); err == nil {
			t.Error("want error for missing client cert, got nil")
		}
	})

	t.Run("missing_server_ca_file", func(t *testing.T) {
		if _, err := buildKMIPTLSConfig(Config{ClientCert: cliCert, ClientKey: cliKey, ServerCA: "/nonexistent-ca"}); err == nil {
			t.Error("want error for missing CA file, got nil")
		}
	})

	t.Run("server_ca_not_pem", func(t *testing.T) {
		badCA := filepath.Join(dir, "bad-ca.pem")
		if err := os.WriteFile(badCA, []byte("not a pem certificate"), 0o600); err != nil {
			t.Fatalf("write bad ca: %v", err)
		}
		_, err := buildKMIPTLSConfig(Config{ClientCert: cliCert, ClientKey: cliKey, ServerCA: badCA})
		if err == nil {
			t.Error("want error for non-PEM CA, got nil")
		}
	})
}

// TestExtractSymmetricKeyMaterial covers every branch of the KeyValue
// type-switch.
func TestExtractSymmetricKeyMaterial(t *testing.T) {
	t.Run("nil_keyvalue", func(t *testing.T) {
		if _, err := extractSymmetricKeyMaterial(nil); err == nil {
			t.Error("want error for nil KeyValue")
		}
	})

	t.Run("byte_slice", func(t *testing.T) {
		material := []byte{1, 2, 3, 4}
		out, err := extractSymmetricKeyMaterial(&kmip.KeyValue{KeyMaterial: material})
		if err != nil {
			t.Fatalf("extract []byte: %v", err)
		}
		if !bytes.Equal(out, material) {
			t.Errorf("got %x want %x", out, material)
		}
		// Defensive-copy: mutating the returned slice must not affect input.
		out[0] = 0xFF
		if material[0] == 0xFF {
			t.Error("extractSymmetricKeyMaterial aliased the input slice")
		}
	})

	t.Run("unexpected_go_type", func(t *testing.T) {
		if _, err := extractSymmetricKeyMaterial(&kmip.KeyValue{KeyMaterial: 12345}); err == nil {
			t.Error("want error for unexpected go type")
		}
	})
}

// TestReadKMIPMessage covers the framing, padding, oversize-cap, and
// short-read paths.
func TestReadKMIPMessage(t *testing.T) {
	t.Run("oversize_cap", func(t *testing.T) {
		// Header with a body length far above the cap.
		hdr := make([]byte, 8)
		binary.BigEndian.PutUint32(hdr[4:8], maxKMIPMessageSize+1)
		_, err := readKMIPMessage(bytes.NewReader(hdr))
		if err == nil || !strings.Contains(err.Error(), "exceeds cap") {
			t.Fatalf("want oversize-cap error, got %v", err)
		}
	})

	t.Run("short_header", func(t *testing.T) {
		if _, err := readKMIPMessage(bytes.NewReader([]byte{0x01, 0x02})); err == nil {
			t.Error("want error on short header")
		}
	})

	t.Run("short_body", func(t *testing.T) {
		hdr := make([]byte, 8)
		binary.BigEndian.PutUint32(hdr[4:8], 16) // promise 16 body bytes
		// Provide only 4 body bytes -> io.ErrUnexpectedEOF.
		buf := append(append([]byte{}, hdr...), 1, 2, 3, 4)
		if _, err := readKMIPMessage(bytes.NewReader(buf)); err == nil {
			t.Error("want error on truncated body")
		}
	})

	t.Run("exact_frame_with_padding", func(t *testing.T) {
		// bodyLen=3 -> pad to 8-byte boundary = 5 padding bytes.
		hdr := make([]byte, 8)
		binary.BigEndian.PutUint32(hdr[4:8], 3)
		body := []byte{0xAA, 0xBB, 0xCC, 0, 0, 0, 0, 0} // 3 + 5 pad
		full := append(append([]byte{}, hdr...), body...)
		out, err := readKMIPMessage(bytes.NewReader(full))
		if err != nil {
			t.Fatalf("readKMIPMessage: %v", err)
		}
		if len(out) != len(full) {
			t.Errorf("frame length = %d, want %d", len(out), len(full))
		}
	})
}
