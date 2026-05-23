package encryption

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/marmos91/dittofs/pkg/blockstore/encryption/keyprovider"
)

// AEAD identifies an authenticated-encryption algorithm. The wire byte
// values are stable: changing them would invalidate every previously
// stored frame.
type AEAD uint8

const (
	// AEADAES256GCM selects AES-256-GCM via Go's crypto/cipher with a
	// 12-byte nonce. Hardware-accelerated on x86 / ARM with AES-NI.
	AEADAES256GCM AEAD = 1

	// AEADChaCha20Poly1305 selects ChaCha20-Poly1305 via
	// golang.org/x/crypto/chacha20poly1305 with a 12-byte nonce. Useful
	// when AES-NI is unavailable.
	AEADChaCha20Poly1305 AEAD = 2

	// AEADXChaCha20Poly1305 selects XChaCha20-Poly1305 with a 24-byte
	// nonce; the larger nonce is safe to generate at random for very
	// high block counts. Same family as age's stream encryption.
	AEADXChaCha20Poly1305 AEAD = 3
)

// String returns the canonical lowercase config string for an AEAD.
func (a AEAD) String() string {
	switch a {
	case AEADAES256GCM:
		return "aes-256-gcm"
	case AEADChaCha20Poly1305:
		return "chacha20-poly1305"
	case AEADXChaCha20Poly1305:
		return "xchacha20-poly1305"
	default:
		return fmt.Sprintf("AEAD(%d)", uint8(a))
	}
}

func parseAEADString(s string) (AEAD, error) {
	switch s {
	case "aes-256-gcm":
		return AEADAES256GCM, nil
	case "chacha20-poly1305":
		return AEADChaCha20Poly1305, nil
	case "xchacha20-poly1305":
		return AEADXChaCha20Poly1305, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrUnsupportedAEAD, s)
	}
}

// EncryptionPolicy is the per-remote encryption configuration. Captured
// at remote-store construction and immutable thereafter.
type EncryptionPolicy struct {
	AEAD AEAD
	Key  keyprovider.Config
}

// policyJSON is the JSON shape stored in BlockStoreConfig.Config under
// the "encryption" key.
type policyJSON struct {
	AEAD string             `json:"aead"`
	Key  keyprovider.Config `json:"key"`
}

// ParsePolicy decodes the JSON value sitting under the "encryption" key.
// Non-object inputs (null, arrays, scalars) are rejected so
// misconfigurations fail fast instead of silently enabling encryption
// with defaults. An empty object yields the AES-256-GCM default with an
// empty key config — callers MUST pass a real key config or NewProvider
// will reject it.
func ParsePolicy(raw json.RawMessage) (EncryptionPolicy, error) {
	if len(raw) == 0 {
		return EncryptionPolicy{}, fmt.Errorf("encryption: parse policy: empty input")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return EncryptionPolicy{}, fmt.Errorf("encryption: parse policy: expected JSON object, got %s", trimmed)
	}
	var pj policyJSON
	if err := json.Unmarshal(trimmed, &pj); err != nil {
		return EncryptionPolicy{}, fmt.Errorf("encryption: parse policy: %w", err)
	}
	algo := AEADAES256GCM
	if pj.AEAD != "" {
		parsed, err := parseAEADString(pj.AEAD)
		if err != nil {
			return EncryptionPolicy{}, err
		}
		algo = parsed
	}
	return EncryptionPolicy{AEAD: algo, Key: pj.Key}, nil
}
