package storebackups

import (
	"errors"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// Re-exports of Phase-4 sentinels for caller convenience. Callers may
// import either the models or the storebackups package; both identities
// match because these are variable aliases, not new errors.New values.
// This preserves errors.Is matching across the package boundary.
var (
	ErrScheduleInvalid      = models.ErrScheduleInvalid
	ErrRepoNotFound         = models.ErrRepoNotFound
	ErrBackupAlreadyRunning = models.ErrBackupAlreadyRunning
	ErrInvalidTargetKind    = models.ErrInvalidTargetKind
)

// Phase-5 restore sentinels (D-26). Two-layer wrap: runtime layer (here)
// is the canonical definition; pkg/backup/restore/errors.go re-exports
// these as package aliases so callers at either layer match with
// errors.Is. Phase 6 CLI / REST handlers consume these to produce
// 400/409 error responses.
var (
	// ErrRestorePreconditionFailed — one or more shares still enabled
	// for the target store. Restore refuses to run until operator
	// explicitly disables (D-01, D-02). Maps to 409 Conflict.
	ErrRestorePreconditionFailed = errors.New("restore precondition failed: one or more shares still enabled")

	// ErrNoRestoreCandidate — the repo has zero succeeded records to
	// restore from. Caller asked for default-latest (D-15). Maps to 409.
	ErrNoRestoreCandidate = errors.New("no succeeded backup record available to restore")

	// ErrStoreIDMismatch — manifest.store_id != target store's
	// persistent store_id (Pitfall #4 guard, D-06). Hard-reject before
	// any destructive action. Maps to 400.
	ErrStoreIDMismatch = errors.New("manifest store_id does not match target store")

	// ErrStoreKindMismatch — manifest.store_kind (memory|badger|postgres)
	// != target engine kind. Cross-engine restore is deferred (XENG-01).
	// Maps to 400.
	ErrStoreKindMismatch = errors.New("manifest store_kind does not match target engine")

	// ErrRecordNotRestorable — --from <id> resolved a record whose
	// status is not succeeded (pending/running/failed/interrupted).
	// Maps to 409.
	ErrRecordNotRestorable = errors.New("backup record status is not succeeded; not restorable")

	// ErrRecordRepoMismatch — --from <id> resolved a record that
	// belongs to a different repo than the one being restored (D-16).
	// Maps to 400.
	ErrRecordRepoMismatch = errors.New("backup record belongs to a different repo")

	// ErrManifestVersionUnsupported — manifest_version != Phase-1
	// CurrentVersion. Forward-incompatible archive; this binary cannot
	// restore it. Maps to 400.
	ErrManifestVersionUnsupported = errors.New("manifest version not supported by this binary")
)
