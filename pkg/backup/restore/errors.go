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
)

// Canonical Phase-5 sentinels (D-26). Defined here — not in
// pkg/controlplane/runtime/storebackups — to break the import cycle
// between the restore executor and the runtime orchestrator that wraps
// it in Plan 07. pkg/controlplane/runtime/storebackups/errors.go
// aliases these values as package-level vars so CLI / REST layers that
// import either package match with errors.Is.
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
