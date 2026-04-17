package storebackups

import (
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/pkg/backup/restore"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// restorePreconditionError carries the list of enabled shares that blocked
// a restore so the REST handler can surface them in the 409 body. Wraps
// ErrRestorePreconditionFailed so errors.Is continues to work.
type restorePreconditionError struct {
	storeName string
	enabled   []string
}

func newRestorePreconditionError(storeName string, enabled []string) *restorePreconditionError {
	return &restorePreconditionError{storeName: storeName, enabled: append([]string(nil), enabled...)}
}

func (e *restorePreconditionError) Error() string {
	return fmt.Sprintf("%s: store %q has %d enabled share(s): %v",
		ErrRestorePreconditionFailed.Error(), e.storeName, len(e.enabled), e.enabled)
}

func (e *restorePreconditionError) Unwrap() error { return ErrRestorePreconditionFailed }

// EnabledShares implements the enabledSharesCarrier contract used by the
// REST handler (extractEnabledShares) to populate the 409 response body.
func (e *restorePreconditionError) EnabledShares() []string { return e.enabled }

// ErrBackupJobNotFound — canceling / looking up a backup or restore job
// whose run-ctx is not registered (either unknown ID or already terminal).
// The REST handler (Phase 6 Plan 02) maps this to 200 OK + current job per
// D-45 idempotent-on-terminal semantics; runtime itself just reports
// absence. Distinct identity from models.ErrBackupJobNotFound: the model
// sentinel signals "DB row missing", this one signals "no active run-ctx
// to cancel".
var ErrBackupJobNotFound = errors.New("backup job not found or already terminal")

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

// Phase-5 restore sentinels (D-26). Canonical definitions live in
// pkg/backup/restore/errors.go — this file aliases them so callers at
// the runtime layer (Phase-6 CLI / REST handlers) match with errors.Is
// against the same identity the restore executor returns. The
// canonical-in-restore-package direction is chosen to avoid the import
// cycle between storebackups → restore (Plan 07 wiring).
var (
	// ErrRestorePreconditionFailed — one or more shares still enabled
	// for the target store. Restore refuses to run until operator
	// explicitly disables (D-01, D-02). Maps to 409 Conflict.
	ErrRestorePreconditionFailed = restore.ErrRestorePreconditionFailed

	// ErrNoRestoreCandidate — the repo has zero succeeded records to
	// restore from. Caller asked for default-latest (D-15). Maps to 409.
	ErrNoRestoreCandidate = restore.ErrNoRestoreCandidate

	// ErrStoreIDMismatch — manifest.store_id != target store's
	// persistent store_id (Pitfall #4 guard, D-06). Hard-reject before
	// any destructive action. Maps to 400.
	ErrStoreIDMismatch = restore.ErrStoreIDMismatch

	// ErrStoreKindMismatch — manifest.store_kind (memory|badger|postgres)
	// != target engine kind. Cross-engine restore is deferred (XENG-01).
	// Maps to 400.
	ErrStoreKindMismatch = restore.ErrStoreKindMismatch

	// ErrRecordNotRestorable — --from <id> resolved a record whose
	// status is not succeeded (pending/running/failed/interrupted).
	// Maps to 409.
	ErrRecordNotRestorable = restore.ErrRecordNotRestorable

	// ErrRecordRepoMismatch — --from <id> resolved a record that
	// belongs to a different repo than the one being restored (D-16).
	// Maps to 400.
	ErrRecordRepoMismatch = restore.ErrRecordRepoMismatch

	// ErrManifestVersionUnsupported — manifest_version != Phase-1
	// CurrentVersion. Forward-incompatible archive; this binary cannot
	// restore it. Maps to 400.
	ErrManifestVersionUnsupported = restore.ErrManifestVersionUnsupported
)
