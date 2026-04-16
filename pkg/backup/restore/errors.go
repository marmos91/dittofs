// Package restore implements the Phase 5 restore orchestration: side-
// engine open at a temp path, Backupable.Restore into the fresh engine,
// atomic swap via stores.Service, and post-swap cleanup.
//
// See .planning/phases/05-restore-orchestration-safety-rails/05-CONTEXT.md
// D-05 for the 13-step sequence this package implements. The restore
// executor is unit-testable in isolation; Plan 07 wraps it behind
// storebackups.Service.RunRestore with the share-disabled pre-flight
// (REST-02) and the per-repo overlap guard (D-07).
package restore

import (
	"errors"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime/storebackups"
)

// Re-exports preserve errors.Is matching across package boundaries.
// storebackups defines the canonical sentinels for Phase-5 (D-26) so
// CLI / REST layers (Phase 6) can match with a single import; this
// package aliases them so the restore engine's callers (unit tests,
// Plan 07's orchestrator) don't need a second import.
var (
	ErrRestorePreconditionFailed  = storebackups.ErrRestorePreconditionFailed
	ErrNoRestoreCandidate         = storebackups.ErrNoRestoreCandidate
	ErrStoreIDMismatch            = storebackups.ErrStoreIDMismatch
	ErrStoreKindMismatch          = storebackups.ErrStoreKindMismatch
	ErrRecordNotRestorable        = storebackups.ErrRecordNotRestorable
	ErrRecordRepoMismatch         = storebackups.ErrRecordRepoMismatch
	ErrManifestVersionUnsupported = storebackups.ErrManifestVersionUnsupported
)

// Package-local sentinels. Restore-orchestration-specific failure modes
// with no external consumer yet — kept local to avoid polluting the
// runtime layer's error surface.
var (
	// ErrRestoreAborted — restore interrupted mid-operation by an
	// explicit abort (not ctx cancellation). Reserved for future use
	// by operator-initiated aborts; today ctx.Canceled covers the
	// shutdown path.
	ErrRestoreAborted = errors.New("restore aborted mid-operation")

	// ErrFreshEngineExists — OpenFreshEngineAtTemp found the temp path
	// already populated. Unexpected in normal operation (ULID suffixes
	// make collisions astronomically unlikely); surfaced loudly so the
	// operator can inspect for stale orphans.
	ErrFreshEngineExists = errors.New("temp engine path already exists")
)
