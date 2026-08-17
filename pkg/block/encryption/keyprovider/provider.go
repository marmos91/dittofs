// Package keyprovider defines the master-key custody surface used by the
// encryption decorator. Implementations hold a master symmetric key
// (locally in a passphrase-protected file, or remotely in a KMIP-speaking
// HSM) and provide Wrap / Unwrap operations on per-block data keys.
//
// In NIST SP 800-57 terminology, the master key is a Key-Encryption-Key
// (KEK) and the per-block data key is a Data-Encryption-Key (DEK). The
// plain names "master key" and "block key" are used throughout this
// package's prose to keep the role of each thing obvious.
//
// # Master-key rotation is NOT supported
//
// Every implementation holds exactly one master key. There is no keyring
// of retired keys: Unwrap returns ErrWrongMasterKey whenever the id
// recorded in a block's frame header differs from the single id the
// provider holds. Pointing a running share at a different master key
// therefore makes every block wrapped under the previous one
// permanently unreadable — there is no recovery path once the old key
// is gone.
//
// Rotating safely would mean re-wrapping every existing block under the
// new master key (read with a provider holding the old key, write with
// one holding the new) and only decommissioning the old key once that
// pass has completed. No such tool ships today, so treat a share's
// master key as fixed for the life of its data.
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
	// frame header so Unwrap can reject a block that was wrapped under a
	// different master key instead of failing with an opaque
	// authentication error.
	Wrap(ctx context.Context, blockKey []byte) (wrapped []byte, masterKeyID string, err error)

	// Unwrap recovers the original block key. masterKeyID is the value
	// recorded by an earlier Wrap. Every implementation holds exactly one
	// master key, so Unwrap returns ErrWrongMasterKey whenever the
	// recorded id is not that one; blocks wrapped under any other key
	// cannot be recovered (see the package doc on rotation).
	Unwrap(ctx context.Context, wrapped []byte, masterKeyID string) ([]byte, error)

	// CurrentMasterKeyID returns the identifier that Wrap will record.
	CurrentMasterKeyID() string

	// Close releases any resources held by the provider (file handles
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
