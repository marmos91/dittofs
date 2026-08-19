package keyprovider

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/gemalto/kmip-go"
	"github.com/gemalto/kmip-go/kmip14"
	"github.com/gemalto/kmip-go/ttlv"

	"github.com/marmos91/dittofs/internal/logger"
)

// kmipDefaultTimeout bounds a single Get round-trip against the KMIP
// server. Surfaced via Config.TimeoutMS for operators that need to
// override per-deployment.
const kmipDefaultTimeout = 5 * time.Second

// kmipProvider fetches a master symmetric key from a KMIP-speaking HSM
// at startup, caches the bytes in memory for the daemon's lifetime, and
// performs per-block wrap / unwrap locally using AES-256-GCM.
//
// Trade-off vs full HSM-resident envelope ops (KMIP Encrypt / Decrypt)
// the master-key bytes live in process memory while the daemon runs, so
// a compromise of the daemon's address space recovers the key. The HSM
// is still the canonical custodian. A follow-up can move Wrap inside the
// HSM via KMIP Encrypt / Decrypt without changing this package's public
// surface (the KeyProvider interface stays the same).
//
// Pointing KeyUID at a different HSM key does NOT rotate the master key:
// only the newly fetched key is held, so every block wrapped under the
// previous key becomes permanently unreadable. See the package doc.
type kmipProvider struct {
	aesGCMKEK
}

func newKMIPProvider(ctx context.Context, cfg Config) (*kmipProvider, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("%w: kmip endpoint required", ErrInvalidConfig)
	}
	if cfg.KeyUID == "" {
		return nil, fmt.Errorf("%w: kmip key_uid required", ErrInvalidConfig)
	}
	if cfg.ClientCert == "" || cfg.ClientKey == "" {
		return nil, fmt.Errorf("%w: kmip client_cert and client_key required", ErrInvalidConfig)
	}
	tlsCfg, err := buildKMIPTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	timeout := kmipDefaultTimeout
	if cfg.TimeoutMS > 0 {
		timeout = time.Duration(cfg.TimeoutMS) * time.Millisecond
	}
	// The state check runs before the Get so that key material the HSM has
	// disowned is never pulled into this process at all.
	state, err := fetchKMIPKeyState(ctx, cfg.Endpoint, tlsCfg, cfg.KeyUID, timeout)
	if err != nil {
		return nil, err
	}
	if state != kmip14.StateActive {
		return nil, fmt.Errorf("%w: kmip key %q is in state %s at the HSM, not Active; "+
			"the encrypted remote will not come up until the configured key_uid names an Active key",
			ErrKeyStateUnusable, cfg.KeyUID, state)
	}
	key, err := fetchKMIPKey(ctx, cfg, tlsCfg, cfg.KeyUID, timeout)
	if err != nil {
		return nil, err
	}
	// A KMIP key is identified by the uid the operator configured, so a
	// retired uid is its own master key id with nothing to look up.
	//
	// A retired key is decrypt-only, so states that would disqualify the
	// current key do not disqualify it: blocks already written under it
	// still have to be readable. Only a key whose material the HSM has
	// destroyed is unusable, and a compromised one is loaded with a loud
	// warning because it is the input to a re-wrap decision.
	retired, err := loadRetiredKeys(cfg.KeyUID, cfg.RetiredKeyUIDs, func(uid string) (string, []byte, error) {
		state, err := fetchKMIPKeyState(ctx, cfg.Endpoint, tlsCfg, uid, timeout)
		if err != nil {
			return uid, nil, err
		}
		switch state {
		case kmip14.StateDestroyed, kmip14.StateDestroyedCompromised:
			return uid, nil, fmt.Errorf("%w: kmip key %q is in state %s at the HSM, so its material is gone",
				ErrKeyStateUnusable, uid, state)
		case kmip14.StateCompromised:
			logger.Warn("retired master key is marked Compromised at the HSM; blocks already written are still being read under it",
				"key_uid", uid)
		}
		k, err := fetchKMIPKey(ctx, cfg, tlsCfg, uid, timeout)
		return uid, k, err
	})
	if err != nil {
		zeroKey(key)
		return nil, err
	}
	return &kmipProvider{aesGCMKEK: aesGCMKEK{
		masterKey:   key,
		masterKeyID: cfg.KeyUID,
		retired:     retired,
	}}, nil
}

// fetchKMIPKey retrieves one symmetric key from the HSM and enforces the
// AES-256 length the wrap layer requires.
func fetchKMIPKey(ctx context.Context, cfg Config, tlsCfg *tls.Config, uid string, timeout time.Duration) ([]byte, error) {
	key, err := fetchKMIPSymmetricKey(ctx, cfg.Endpoint, tlsCfg, uid, timeout)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%w: kmip key %q has unexpected length %d (want 32 for AES-256)", ErrInvalidConfig, uid, len(key))
	}
	return key, nil
}

func buildKMIPTLSConfig(cfg Config) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.ClientCert, cfg.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("keyprovider: load kmip client cert: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if cfg.ServerCA != "" {
		caPEM, err := os.ReadFile(cfg.ServerCA)
		if err != nil {
			return nil, fmt.Errorf("keyprovider: read kmip server CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("keyprovider: kmip server CA has no PEM certs")
		}
		tlsCfg.RootCAs = pool
	}
	return tlsCfg, nil
}

// fetchKMIPSymmetricKey asks the KMIP server for the object stored under
// the configured unique identifier and returns its raw key material.
func fetchKMIPSymmetricKey(ctx context.Context, endpoint string, tlsCfg *tls.Config, keyUID string, timeout time.Duration) ([]byte, error) {
	raw, err := kmipRoundTrip(ctx, endpoint, tlsCfg, timeout,
		kmip14.OperationGet, kmip.GetRequestPayload{UniqueIdentifier: keyUID})
	if err != nil {
		return nil, err
	}
	var payload kmip.GetResponsePayload
	if err := ttlv.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("keyprovider: kmip decode Get payload: %w", err)
	}
	if payload.ObjectType != kmip14.ObjectTypeSymmetricKey {
		return nil, fmt.Errorf("keyprovider: kmip key %s is not a symmetric key (got object type %d)", keyUID, payload.ObjectType)
	}
	if payload.SymmetricKey == nil {
		return nil, errors.New("keyprovider: kmip Get response missing SymmetricKey")
	}
	return extractSymmetricKeyMaterial(payload.SymmetricKey.KeyBlock.KeyValue)
}

// kmipGetAttributesRequestPayload and kmipGetAttributesResponsePayload
// carry the GetAttributes operation, which the KMIP library models no
// types for. GetAttributes is read-only: it reports on an object and can
// neither create nor modify one, so the client credential needs no write
// capability at the HSM.
type kmipGetAttributesRequestPayload struct {
	UniqueIdentifier string
	AttributeName    []string
}

type kmipGetAttributesResponsePayload struct {
	UniqueIdentifier string
	Attribute        []kmip.Attribute
}

// fetchKMIPKeyState reads the KMIP State attribute of one object. The
// State is how an HSM says "stop using this" — an operator who revokes a
// key expects that to reach every consumer of it — so a state that cannot
// be read is an error rather than an assumed Active.
func fetchKMIPKeyState(ctx context.Context, endpoint string, tlsCfg *tls.Config, keyUID string, timeout time.Duration) (kmip14.State, error) {
	stateName := kmip14.TagState.CanonicalName()
	raw, err := kmipRoundTrip(ctx, endpoint, tlsCfg, timeout,
		kmip14.OperationGetAttributes,
		kmipGetAttributesRequestPayload{UniqueIdentifier: keyUID, AttributeName: []string{stateName}})
	if err != nil {
		return 0, err
	}
	var payload kmipGetAttributesResponsePayload
	if err := ttlv.Unmarshal(raw, &payload); err != nil {
		return 0, fmt.Errorf("keyprovider: kmip decode GetAttributes payload: %w", err)
	}
	for _, attr := range payload.Attribute {
		if attr.AttributeName != stateName {
			continue
		}
		value, ok := attr.AttributeValue.(ttlv.EnumValue)
		if !ok {
			return 0, fmt.Errorf("keyprovider: kmip key %s State attribute has unexpected go type %T", keyUID, attr.AttributeValue)
		}
		return kmip14.State(value), nil
	}
	return 0, fmt.Errorf("keyprovider: kmip key %s returned no State attribute", keyUID)
}

// kmipRoundTrip opens a TLS connection to the KMIP server, sends a
// single-item batch carrying one operation, and returns that item's
// response payload. The connection is closed before return.
func kmipRoundTrip(ctx context.Context, endpoint string, tlsCfg *tls.Config, timeout time.Duration, op kmip14.Operation, reqPayload any) (ttlv.TTLV, error) {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", endpoint, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("keyprovider: kmip dial %s: %w", endpoint, err)
	}
	defer func() { _ = conn.Close() }()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(timeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("keyprovider: kmip set deadline: %w", err)
	}

	req := kmip.RequestMessage{
		RequestHeader: kmip.RequestHeader{
			ProtocolVersion: kmip.ProtocolVersion{ProtocolVersionMajor: 1, ProtocolVersionMinor: 4},
			BatchCount:      1,
		},
		BatchItem: []kmip.RequestBatchItem{{
			Operation:      op,
			RequestPayload: reqPayload,
		}},
	}
	reqTTLV, err := ttlv.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("keyprovider: kmip marshal request: %w", err)
	}
	if _, err := conn.Write(reqTTLV); err != nil {
		return nil, fmt.Errorf("keyprovider: kmip write: %w", err)
	}

	respTTLV, err := readKMIPMessage(conn)
	if err != nil {
		return nil, fmt.Errorf("keyprovider: kmip read response: %w", err)
	}
	var resp kmip.ResponseMessage
	if err := ttlv.Unmarshal(respTTLV, &resp); err != nil {
		return nil, fmt.Errorf("keyprovider: kmip unmarshal response: %w", err)
	}
	if len(resp.BatchItem) == 0 {
		return nil, errors.New("keyprovider: kmip response has no batch items")
	}
	item := resp.BatchItem[0]
	if item.ResultStatus != kmip14.ResultStatusSuccess {
		return nil, fmt.Errorf("keyprovider: kmip %v failed: status=%d reason=%d message=%q",
			op, item.ResultStatus, item.ResultReason, item.ResultMessage)
	}
	raw, ok := item.ResponsePayload.(ttlv.TTLV)
	if !ok {
		return nil, fmt.Errorf("keyprovider: kmip %v payload is not TTLV (got %T)", op, item.ResponsePayload)
	}
	return raw, nil
}

// extractSymmetricKeyMaterial pulls the raw byte material out of a KMIP
// KeyValue. The library decodes ByteString KeyMaterial as []byte.
func extractSymmetricKeyMaterial(kv *kmip.KeyValue) ([]byte, error) {
	if kv == nil {
		return nil, errors.New("keyprovider: kmip Get response missing KeyValue")
	}
	switch m := kv.KeyMaterial.(type) {
	case []byte:
		out := make([]byte, len(m))
		copy(out, m)
		return out, nil
	case ttlv.TTLV:
		if m.Type() != ttlv.TypeByteString {
			return nil, fmt.Errorf("keyprovider: kmip key material has unexpected type %s", m.Type())
		}
		out := make([]byte, len(m.ValueByteString()))
		copy(out, m.ValueByteString())
		return out, nil
	default:
		return nil, fmt.Errorf("keyprovider: kmip key material has unexpected go type %T", m)
	}
}

// maxKMIPMessageSize bounds a single inbound KMIP response. KMIP servers
// pack one RequestMessage / ResponseMessage per TLS write; production
// responses are kilobytes (Get on a 32-byte symmetric key fits in ~512
// bytes). 16 MiB is far above any legitimate response yet keeps a
// malicious or misbehaving peer from forcing an unbounded allocation.
const maxKMIPMessageSize = 16 * 1024 * 1024

// readKMIPMessage reads exactly one KMIP TTLV-framed message off the
// wire. KMIP frames begin with an 8-byte header (3-byte tag, 1-byte
// type, 4-byte length); per KMIP spec §9.1.1.4 the value field is then
// zero-padded to an 8-byte boundary, so the total message length is
// headerLen + length + padding.
//
// Bounds the inbound length to maxKMIPMessageSize so a malicious peer
// cannot trigger a huge allocation via a forged length field.
func readKMIPMessage(r io.Reader) (ttlv.TTLV, error) {
	const headerLen = 8
	header := make([]byte, headerLen)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	bodyLen := binary.BigEndian.Uint32(header[4:8])
	if int64(bodyLen) > maxKMIPMessageSize {
		return nil, fmt.Errorf("keyprovider: kmip message length %d exceeds cap %d", bodyLen, maxKMIPMessageSize)
	}
	pad := (8 - int(bodyLen%8)) % 8
	body := make([]byte, headerLen+int(bodyLen)+pad)
	copy(body, header)
	if _, err := io.ReadFull(r, body[headerLen:]); err != nil {
		return nil, err
	}
	return ttlv.TTLV(bytes.Clone(body)), nil
}
