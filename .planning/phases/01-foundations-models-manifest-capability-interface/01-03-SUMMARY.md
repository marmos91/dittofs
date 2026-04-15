---
phase: 01
plan: 03
subsystem: backup/manifest + metadata/capability
tags: [manifest, backup, capability-interface, yaml, SAFETY-03, ENG-04, SAFETY-01]
requires: []
provides:
  - pkg/backup/manifest (Manifest v1 YAML codec)
  - pkg/metadata.Backupable (capability interface)
  - pkg/metadata.PayloadIDSet (GC-hold support type)
  - pkg/metadata.ErrBackupUnsupported (sentinel)
affects:
  - Phase 2 engines (implement Backupable)
  - Phase 3 destinations (write Manifest v1)
  - Phase 5 restore + GC hold (read PayloadIDSet from manifest)
tech-stack:
  added: []
  patterns: [yaml.v3 codec, capability-via-type-assertion, sentinel error]
key-files:
  created:
    - pkg/backup/manifest/manifest.go
    - pkg/backup/manifest/manifest_test.go
    - pkg/metadata/backup.go
    - pkg/metadata/backup_test.go
  modified: []
decisions:
  - Manifest v1 uses YAML via gopkg.in/yaml.v3 (already in go.mod); JSON reserved for API responses
  - Strict version guard rejects ManifestVersion == 0 and != CurrentVersion (SAFETY-03 forward-compat)
  - payload_id_set field is always serialized (NOT omitempty) — Phase 5 GC hold requires the field to exist even when empty (SAFETY-01)
  - PayloadIDSet backed by map[string]struct{}; reads (Contains/Len) are nil-safe, writes (Add) require NewPayloadIDSet
  - Backupable capability check via Go type assertion (compile-time); no runtime registry
metrics:
  duration: ~10m
  completed: 2026-04-15
---

# Phase 1 Plan 03: Manifest v1 and Backupable Capability Interface Summary

Versioned self-describing YAML manifest and stream-based `Backupable` capability interface that every downstream phase compiles against.

## Files Created

- `pkg/backup/manifest/manifest.go` — Manifest v1 struct + codec + validator (CurrentVersion=1)
- `pkg/backup/manifest/manifest_test.go` — 10 tests covering round-trip, version guard, required fields, determinism, PayloadIDSet always present, WriteTo/ReadFrom, broken-YAML rejection
- `pkg/metadata/backup.go` — `Backupable` interface + `PayloadIDSet` + `NewPayloadIDSet` + `ErrBackupUnsupported`
- `pkg/metadata/backup_test.go` — 6 tests including compile-time interface-shape assertion

## Final Manifest v1 Shape

```go
const CurrentVersion = 1

type Encryption struct {
    Enabled   bool   `yaml:"enabled"`
    Algorithm string `yaml:"algorithm,omitempty"`
    KeyRef    string `yaml:"key_ref,omitempty"`
}

type Manifest struct {
    ManifestVersion int               `yaml:"manifest_version"`
    BackupID        string            `yaml:"backup_id"`
    CreatedAt       time.Time         `yaml:"created_at"`
    StoreID         string            `yaml:"store_id"`
    StoreKind       string            `yaml:"store_kind"`
    SHA256          string            `yaml:"sha256"`
    SizeBytes       int64             `yaml:"size_bytes"`
    Encryption      Encryption        `yaml:"encryption"`
    PayloadIDSet    []string          `yaml:"payload_id_set"`    // NOT omitempty (SAFETY-01)
    EngineMetadata  map[string]string `yaml:"engine_metadata,omitempty"`
}

// Entry points
func (m *Manifest) Marshal() ([]byte, error)
func (m *Manifest) WriteTo(w io.Writer) (int64, error)
func Parse(data []byte) (*Manifest, error)
func ReadFrom(r io.Reader) (*Manifest, error)
func (m *Manifest) Validate() error
```

## Backupable Capability Interface

```go
type Backupable interface {
    Backup(ctx context.Context, w io.Writer) (PayloadIDSet, error)
    Restore(ctx context.Context, r io.Reader) error
}

type PayloadIDSet map[string]struct{}

func NewPayloadIDSet() PayloadIDSet
func (s PayloadIDSet) Add(id string)
func (s PayloadIDSet) Contains(id string) bool  // nil-safe
func (s PayloadIDSet) Len() int                  // nil-safe

var ErrBackupUnsupported = errors.New("backup not supported by this metadata store")
```

## Note to Downstream (Phase 5)

`manifest.ReadFrom` uses `io.ReadAll` internally. When reading manifests from untrusted storage (S3, external mounts) the caller MUST wrap the reader with `io.LimitReader` to bound memory usage. Reference: threat T-01-10 in 03-PLAN.md.

## Scope Boundary

No metadata store implementation (`pkg/metadata/store/memory`, `.../badger`, `.../postgres`) was modified in this plan. Phase 2 wires `Backupable` onto each store.

## Deviations from Plan

None — plan executed exactly as written.

## Verification

- `go build ./pkg/backup/manifest/... ./pkg/metadata/...` — clean
- `go vet ./pkg/backup/manifest/... ./pkg/metadata/...` — clean
- `go test ./pkg/backup/manifest/... -count=1` — 10/10 pass
- `go test ./pkg/metadata/ -count=1` — all existing + 6 new pass

## Commits

- `d4b57542` feat(01-03): add backup manifest v1 YAML codec
- `8740f699` feat(01-03): add Backupable capability interface and PayloadIDSet

## Self-Check: PASSED

- pkg/backup/manifest/manifest.go — FOUND
- pkg/backup/manifest/manifest_test.go — FOUND
- pkg/metadata/backup.go — FOUND
- pkg/metadata/backup_test.go — FOUND
- Commit d4b57542 — FOUND
- Commit 8740f699 — FOUND
