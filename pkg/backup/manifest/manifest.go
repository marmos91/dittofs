// Package manifest implements the v1 backup manifest format (SAFETY-03).
//
// A manifest is the self-describing, versioned descriptor written alongside
// every backup payload. It records enough information for the restore path
// to validate compatibility (store kind, manifest version), verify integrity
// (SHA-256 of the payload archive), and safely interact with block-GC
// (PayloadIDSet — SAFETY-01). The manifest is encoded as YAML for operator
// readability.
//
// The file is intentionally small and dependency-light: Phase 1 ships only
// the codec and version guard; Phase 3 destinations write it, Phase 5 restore
// reads it.
package manifest

import (
	"fmt"
	"io"
	"time"

	"gopkg.in/yaml.v3"
)

// CurrentVersion is the manifest schema version produced by this build.
// Phase 1 accepts only this value; future versions branch in Validate.
const CurrentVersion = 1

// MaxManifestBytes bounds in-memory manifest decode size (T-01-10).
const MaxManifestBytes = 1 << 20 // 1 MiB

// Encryption describes payload-at-rest encryption parameters.
// Never contains the key itself — only a reference resolvable at restore time.
type Encryption struct {
	Enabled   bool   `yaml:"enabled"`
	Algorithm string `yaml:"algorithm,omitempty"` // e.g. "aes-256-gcm"
	KeyRef    string `yaml:"key_ref,omitempty"`   // env var name or file path
}

// Manifest is the self-describing, versioned descriptor written alongside
// every backup payload.
type Manifest struct {
	ManifestVersion int        `yaml:"manifest_version"`
	BackupID        string     `yaml:"backup_id"`  // ULID
	CreatedAt       time.Time  `yaml:"created_at"` // RFC3339 via yaml.v3 default
	StoreID         string     `yaml:"store_id"`   // FK snapshot
	StoreKind       string     `yaml:"store_kind"` // memory|badger|postgres
	SHA256          string     `yaml:"sha256"`
	SizeBytes       int64      `yaml:"size_bytes"`
	Encryption      Encryption `yaml:"encryption"`
	// PayloadIDSet lists all block PayloadIDs referenced by this backup.
	// Required for block-GC hold (SAFETY-01); must not use omitempty —
	// Phase 5 distinguishes "empty list" (valid — zero-block backup)
	// from "missing field" (corrupted manifest).
	PayloadIDSet   []string          `yaml:"payload_id_set"`
	EngineMetadata map[string]string `yaml:"engine_metadata,omitempty"`
}

// Marshal serializes the manifest to YAML.
// The gopkg.in/yaml.v3 encoder is deterministic for struct-tagged types with
// fixed field ordering, producing byte-identical output for identical inputs —
// a prerequisite for reproducible SHA-256 over the manifest.
func (m *Manifest) Marshal() ([]byte, error) {
	return yaml.Marshal(m)
}

// WriteTo streams the YAML encoding to w. Satisfies io.WriterTo semantics.
func (m *Manifest) WriteTo(w io.Writer) (int64, error) {
	data, err := m.Marshal()
	if err != nil {
		return 0, err
	}
	n, err := w.Write(data)
	return int64(n), err
}

// Parse decodes a YAML manifest and validates its version and required fields.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// ErrManifestTooLarge is returned by ReadFrom when the source exceeds
// MaxManifestBytes (T-01-10).
var ErrManifestTooLarge = fmt.Errorf("manifest exceeds %d bytes", MaxManifestBytes)

// ReadFrom decodes a YAML manifest from r, rejecting inputs larger than
// MaxManifestBytes to bound memory for untrusted storage (T-01-10).
func ReadFrom(r io.Reader) (*Manifest, error) {
	// Read one byte past the cap to detect oversize inputs rather than
	// silently truncating to a valid-looking prefix.
	data, err := io.ReadAll(io.LimitReader(r, MaxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if len(data) > MaxManifestBytes {
		return nil, ErrManifestTooLarge
	}
	return Parse(data)
}

// Validate enforces structural invariants and version compatibility.
// Phase 1 accepts only CurrentVersion. Future versions branch here for
// forward-compat (SAFETY-03).
func (m *Manifest) Validate() error {
	if m.ManifestVersion == 0 {
		return fmt.Errorf("manifest_version is required")
	}
	if m.ManifestVersion != CurrentVersion {
		return fmt.Errorf("unsupported manifest_version %d (this build supports %d)", m.ManifestVersion, CurrentVersion)
	}
	if m.BackupID == "" {
		return fmt.Errorf("backup_id is required")
	}
	if m.StoreID == "" {
		return fmt.Errorf("store_id is required")
	}
	if m.StoreKind == "" {
		return fmt.Errorf("store_kind is required")
	}
	if m.SHA256 == "" {
		return fmt.Errorf("sha256 is required")
	}
	if m.CreatedAt.IsZero() {
		return fmt.Errorf("created_at is required")
	}
	// PayloadIDSet must be present (non-nil) but may be empty (zero-block
	// backup). Phase 5 block-GC hold distinguishes "empty list" from
	// "missing field" — a decoded manifest with a missing payload_id_set
	// yields PayloadIDSet == nil and is rejected here (SAFETY-01).
	if m.PayloadIDSet == nil {
		return fmt.Errorf("payload_id_set is required (may be empty)")
	}
	return nil
}
