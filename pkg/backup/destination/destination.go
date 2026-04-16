package destination

import (
	"context"
	"io"
	"time"

	"github.com/marmos91/dittofs/pkg/backup/manifest"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// Destination publishes backup archives to a backing store and retrieves
// them for restore. Drivers own atomic publish ordering (manifest-last),
// optional AES-256-GCM encryption, and SHA-256 integrity.
//
// PutBackup and GetBackup are the single enforcement points for the
// manifest-last invariant, SHA-256 tee, and encryption envelope (see
// Phase 3 CONTEXT.md D-04, D-11).
type Destination interface {
	// PutBackup publishes a new backup. payload yields cleartext; the
	// driver handles SHA-256 tee, optional AES-256-GCM encryption (per
	// m.Encryption.Enabled), and atomic publish. Driver populates
	// m.SHA256 and m.SizeBytes before writing manifest.yaml. Returns
	// after the manifest-last upload completes.
	//
	// Errors: ErrDestinationUnavailable, ErrPermissionDenied,
	// ErrDuplicateBackupID, ErrIncompatibleConfig, ErrEncryptionKeyMissing,
	// ErrInvalidKeyMaterial.
	PutBackup(ctx context.Context, m *manifest.Manifest, payload io.Reader) error

	// GetManifestOnly returns the manifest for a backup id without
	// fetching the payload. Callers (Phase 5 restore pre-flight, block-GC
	// hold provider) use this to validate or enumerate manifests cheaply —
	// S3 drivers spend no payload bandwidth; FS drivers skip the payload
	// open/close cycle entirely. See Phase 5 CONTEXT.md D-12.
	//
	// Errors:
	//   - ErrManifestMissing: no manifest.yaml for id (orphan, never
	//     published, or deleted).
	//   - ErrDestinationUnavailable: transient I/O or parse failure.
	GetManifestOnly(ctx context.Context, id string) (*manifest.Manifest, error)

	// GetBackup returns the manifest and a payload reader. When
	// m.Encryption.Enabled is true, the reader yields plaintext
	// (post-decrypt). The reader verifies SHA-256 as it streams and
	// returns ErrSHA256Mismatch on Read/Close if the digest differs.
	//
	// Errors: ErrManifestMissing, ErrIncompleteBackup, ErrDecryptFailed,
	// ErrSHA256Mismatch, ErrDestinationUnavailable.
	GetBackup(ctx context.Context, id string) (*manifest.Manifest, io.ReadCloser, error)

	// List returns chronologically-ordered descriptors of PUBLISHED
	// backups (those with a manifest.yaml present). Entries with
	// HasManifest=false are never returned — they are orphans. This is
	// the source of truth when the control-plane DB is inconsistent.
	List(ctx context.Context) ([]BackupDescriptor, error)

	// Stat returns metadata for one backup without fetching the payload.
	Stat(ctx context.Context, id string) (*BackupDescriptor, error)

	// Delete removes a published backup atomically by inverting publish
	// order (manifest.yaml first, then payload.bin) so that a crash
	// mid-delete leaves the backup discoverable-but-orphaned rather than
	// half-gone-and-lost. Called only by Phase 4 retention.
	Delete(ctx context.Context, id string) error

	// ValidateConfig probes the destination at repo-create time (called
	// from Phase 6). Pings connectivity, checks permissions, rejects
	// bucket/prefix collisions (S3, D-13), warns on NFS/SMB/FUSE parent
	// (local, D-14). Returns ErrIncompatibleConfig on rejectable
	// misconfiguration.
	ValidateConfig(ctx context.Context) error

	// Close releases resources held by the destination. Idempotent.
	Close() error
}

// BackupDescriptor summarizes one backup at the destination for List/Stat.
type BackupDescriptor struct {
	ID          string    // ULID
	CreatedAt   time.Time // from manifest if readable, else object LastModified
	SizeBytes   int64     // payload.bin size in storage
	HasManifest bool      // false = orphan, excluded from restore selection
	SHA256      string    // from manifest.yaml (empty if manifest unreadable)
}

// Factory constructs a Destination for a given BackupRepo row. Implementations
// call repo.GetConfig() to parse the driver-specific config map. The context
// is used for any construction-time probes (e.g. S3 bucket existence check).
type Factory func(ctx context.Context, repo *models.BackupRepo) (Destination, error)

// registry is the internal factory map, keyed by models.BackupRepoKind.
// Keying on the typed enum (not a bare string) lets callers pass repo.Kind
// directly without conversion. Protected by no mutex because registration
// occurs once at process startup (see cmd/dfs/main.go). Tests that need
// isolation use ResetRegistryForTest.
var registry = map[models.BackupRepoKind]Factory{}

// Register adds a Factory for the given repo kind. Called from driver
// init() or from cmd/dfs/main.go at process startup. Panics on duplicate
// registration — programmer error, not operator error.
func Register(kind models.BackupRepoKind, f Factory) {
	if kind == "" {
		panic("destination: Register called with empty kind")
	}
	if f == nil {
		panic("destination: Register called with nil factory for kind " + string(kind))
	}
	if _, dup := registry[kind]; dup {
		panic("destination: duplicate factory for kind " + string(kind))
	}
	registry[kind] = f
}

// Lookup returns the Factory registered for kind, or (nil, false) if none.
// Callers that need a strongly typed error (for API responses) should wrap:
//
//	f, ok := destination.Lookup(repo.Kind)
//	if !ok {
//	    return fmt.Errorf("%w: unknown destination kind %q",
//	        destination.ErrIncompatibleConfig, repo.Kind)
//	}
func Lookup(kind models.BackupRepoKind) (Factory, bool) {
	f, ok := registry[kind]
	return f, ok
}

// ResetRegistryForTest clears the registry. Tests only.
func ResetRegistryForTest() { registry = map[models.BackupRepoKind]Factory{} }
