// Package keyprovider defines the master-key custody surface used by the
// encryption decorator. Implementations hold a master symmetric key
// (locally in a passphrase-protected file, or remotely in a KMIP-speaking
// HSM) and provide Wrap / Unwrap operations on per-block data keys.
//
// In NIST SP 800-57 terminology, the master key is a Key-Encryption-Key
// (KEK) and the per-block data key is a Data-Encryption-Key (DEK). The
// plain names "master key" and "block key" are used throughout this
// package's prose to keep the role of each thing obvious.
package keyprovider

import (
	"context"
	"errors"
	"fmt"
)

// KeyProvider holds a master key and wraps / unwraps per-block data keys
// under it. Implementations MUST be safe for concurrent use by multiple
// goroutines — the encryption decorator calls Wrap / Unwrap once per
// block under load.
type KeyProvider interface {
	// Wrap protects a block key (typically 32 bytes) under the provider's
	// master key. Returns the wrapped bytes plus the stable identifier of
	// the master key used; the identifier is recorded in the on-wire
	// frame header so Unwrap can route to the right master key after a
	// future rotation.
	Wrap(ctx context.Context, blockKey []byte) (wrapped []byte, masterKeyID string, err error)

	// Unwrap recovers the original block key. masterKeyID is the value
	// recorded by an earlier Wrap; single-key providers may ignore it.
	// Returns ErrWrongMasterKey if the recorded id does not match any
	// master key this provider knows about.
	Unwrap(ctx context.Context, wrapped []byte, masterKeyID string) ([]byte, error)

	// CurrentMasterKeyID returns the identifier that Wrap will record.
	CurrentMasterKeyID() string

	// Close releases any resources held by the provider (file handles,
	// network connections, in-memory key material).
	Close() error
}

// Kind discriminates between provider implementations.
type Kind string

const (
	// KindLocal selects the passphrase-protected key-file provider.
	KindLocal Kind = "local"

	// KindKMIP selects the KMIP-speaking external HSM provider.
	KindKMIP Kind = "kmip"
)

// Config is the parsed per-remote key-provider configuration. The
// encryption decorator passes one of these to NewProvider when wiring up
// a remote store; the JSON shape lives under "encryption.key" in the
// per-remote BlockStoreConfig.Config blob.
type Config struct {
	Kind Kind `json:"kind"`

	// Local-specific fields (Kind == KindLocal).
	File string `json:"file,omitempty"`

	// KMIP-specific fields (Kind == KindKMIP).
	Endpoint   string `json:"endpoint,omitempty"`
	ServerCA   string `json:"server_ca,omitempty"`
	ClientCert string `json:"client_cert,omitempty"`
	ClientKey  string `json:"client_key,omitempty"`
	KeyUID     string `json:"key_uid,omitempty"`
	TimeoutMS  int    `json:"timeout_ms,omitempty"`
}

// Sentinel errors. All provider implementations wrap these so callers can
// match via errors.Is regardless of the underlying transport.
var (
	// ErrInvalidConfig indicates the Config did not name a recognised
	// provider Kind or omitted a required field.
	ErrInvalidConfig = errors.New("keyprovider: invalid config")

	// ErrWrongMasterKey indicates the masterKeyID recorded in the wrapped
	// payload does not match the master key currently held by the
	// provider.
	ErrWrongMasterKey = errors.New("keyprovider: master key id mismatch")

	// ErrUnwrapFailed indicates the wrapped bytes failed authenticated
	// decryption under the master key (tamper, wrong key, or corruption).
	ErrUnwrapFailed = errors.New("keyprovider: unwrap failed")

	// ErrPassphraseMissing indicates the DITTOFS_ENCRYPTION_PASSPHRASE
	// environment variable is unset or empty when loading a local key
	// file.
	ErrPassphraseMissing = errors.New("keyprovider: DITTOFS_ENCRYPTION_PASSPHRASE is unset")

	// ErrKeyFileCorrupt indicates a local key file failed to parse — bad
	// PEM, bad JSON, or out-of-range KDF parameters.
	ErrKeyFileCorrupt = errors.New("keyprovider: key file corrupt")
)

// NewProvider constructs a KeyProvider from the parsed Config. Dispatch
// is by Kind; unknown kinds return ErrInvalidConfig.
func NewProvider(ctx context.Context, cfg Config) (KeyProvider, error) {
	switch cfg.Kind {
	case KindLocal:
		return newLocalProvider(cfg)
	case KindKMIP:
		return newKMIPProvider(ctx, cfg)
	case "":
		return nil, fmt.Errorf("%w: missing kind", ErrInvalidConfig)
	default:
		return nil, fmt.Errorf("%w: unknown kind %q", ErrInvalidConfig, cfg.Kind)
	}
}
