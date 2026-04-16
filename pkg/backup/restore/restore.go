package restore

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/oklog/ulid/v2"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/backup"
	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/manifest"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// JobStore is the narrow persistence interface the restore Executor
// needs. Subset of store.BackupStore — callers pass the full store but
// the Executor consumes only these methods, which keeps test fakes
// trivial. Mirrors pkg/backup/executor.JobStore.
//
// The read-side methods (GetBackupRecord, ListSucceededRecordsByRepo)
// are declared here for Plan 07's storebackups.Service to satisfy with
// the same concrete store type it uses for writes; the restore engine
// itself only invokes the job-write methods during RunRestore (record
// selection — D-15 / D-16 — happens in Plan 07 before RunRestore is
// called).
//
// Method names match pkg/controlplane/store.BackupStore verbatim so
// the real *GORMStore satisfies this interface without adapters.
type JobStore interface {
	CreateBackupJob(ctx context.Context, job *models.BackupJob) (string, error)
	UpdateBackupJob(ctx context.Context, job *models.BackupJob) error
	GetBackupRecord(ctx context.Context, id string) (*models.BackupRecord, error)
	ListSucceededRecordsByRepo(ctx context.Context, repoID string) ([]*models.BackupRecord, error)
}

// Executor holds the injectable dependencies for one restore run.
// Construct with New(); call RunRestore per restore attempt. The
// Executor is stateless across calls — concurrent RunRestore
// invocations are safe (the per-repo overlap guard lives in Plan 07's
// storebackups.Service wrapper).
type Executor struct {
	store JobStore
	clock backup.Clock
}

// New constructs an Executor with the given job store and clock. A nil
// clock falls back to backup.RealClock{} — matches executor.New.
func New(store JobStore, clock backup.Clock) *Executor {
	if clock == nil {
		clock = backup.RealClock{}
	}
	return &Executor{store: store, clock: clock}
}

// SetClock swaps the clock at runtime. Test-only convenience; callers
// generally pass a fake clock to New.
func (e *Executor) SetClock(c backup.Clock) {
	if c == nil {
		c = backup.RealClock{}
	}
	e.clock = c
}

// Params bundles the per-call inputs to RunRestore. Plan 07's
// storebackups.Service builds this from the resolved target + selected
// record (D-15 / D-16).
type Params struct {
	// Repo is the BackupRepo the restore is sourced from. Non-nil.
	Repo *models.BackupRepo

	// Dst is the destination driver for Repo. Non-nil.
	Dst destination.Destination

	// RecordID is the BackupRecord ID the caller has already resolved
	// (D-15 default-latest or D-16 explicit --from <id>). Non-empty.
	RecordID string

	// TargetStoreKind: "memory" | "badger" | "postgres". Must match
	// manifest.store_kind (D-05 step 4).
	TargetStoreKind string

	// TargetStoreID is the target store's persistent engine-level ID
	// (Plan 02). Must match manifest.store_id (D-05 step 4, D-06).
	TargetStoreID string

	// TargetStoreCfg is the metadata-store config row for the target.
	// Non-nil. Drives OpenFreshEngineAtTemp.
	TargetStoreCfg *models.MetadataStoreConfig

	// StoresService is the restore-narrowed stores.Service interface.
	// Non-nil.
	StoresService StoresService

	// BumpBootVerifier is invoked on a successful swap to force NFSv4
	// clients into the reclaim-grace path (D-09). nil is acceptable
	// (tests pass nil; Plan 07 passes
	// writehandlers.BumpBootVerifier).
	BumpBootVerifier func()
}

// RunRestore executes one restore attempt. Returns nil on successful
// swap; non-nil on any failure before/during swap. Post-swap cleanup
// errors (close old / remove old / rename temp) are logged at WARN
// but do not fail the restore.
//
// D-05 step mapping (13-step sequence):
//
//	step  3 — fetch manifest via GetManifestOnly
//	step  4 — validate manifest_version, store_kind, store_id, sha256
//	step  5 — create BackupJob{Kind: restore, Status: running}
//	step  6 — OpenFreshEngineAtTemp (set cleanupTemp=true)
//	step  7 — dst.GetBackup(ctx, recordID) → ReadCloser
//	step  8 — freshBackupable.Restore(ctx, reader)
//	step  9 — reader.Close() verifies SHA-256 (Phase 3 D-11)
//	step 10 — stores.SwapMetadataStore (COMMIT POINT)
//	step 11 — close old store (inside CommitSwap)
//	step 12 — remove old backing + rename temp (inside CommitSwap)
//	step 13 — BumpBootVerifier (nil-safe)
//
// Plus the terminal-state defer that mirrors Phase-4's executor:
// ctx.Canceled / DeadlineExceeded / ErrBackupAborted / ErrRestoreAborted
// → job status=interrupted (D-17). All other errors → status=failed.
// Successful completion → status=succeeded.
//
// Failure semantics at each step:
//   - Steps 3-4 fail pre-swap: old store untouched; no temp path
//     created (OpenFreshEngineAtTemp hasn't been called).
//   - Step 6 fails: OpenFreshEngineAtTemp already best-effort reclaimed
//     the temp; defer is a no-op on the zero TempIdentity.
//   - Steps 7-9 fail: defer closes freshStore and CleanupTempBacking
//     wipes the temp path/schema.
//   - Step 10 is the commit point. Success flips cleanupTemp=false so
//     the defer does NOT wipe the now-live fresh engine.
//   - Steps 11-12 errors: restore has already succeeded; logged WARN,
//     return nil.
func (e *Executor) RunRestore(ctx context.Context, p Params) (err error) {
	if p.Repo == nil || p.Dst == nil || p.TargetStoreCfg == nil || p.StoresService == nil {
		return fmt.Errorf("invalid restore Params: repo/dst/cfg/stores must be non-nil")
	}
	if p.RecordID == "" {
		return fmt.Errorf("invalid restore Params: RecordID is empty")
	}

	// Step 5 (hoisted before pre-flight so every attempt produces an
	// auditable BackupJob row per SAFETY-02). Matches Phase-4
	// executor.RunBackup ordering.
	startedAt := e.clock.Now()
	jobID := ulid.Make().String()
	job := &models.BackupJob{
		ID:        jobID,
		Kind:      models.BackupJobKindRestore,
		RepoID:    p.Repo.ID,
		Status:    models.BackupStatusRunning,
		StartedAt: &startedAt,
	}
	if _, cerr := e.store.CreateBackupJob(ctx, job); cerr != nil {
		return fmt.Errorf("create restore job: %w", cerr)
	}

	logger.Info("Restore starting",
		"repo_id", p.Repo.ID,
		"job_id", jobID,
		"record_id", p.RecordID,
		"target_store_id", p.TargetStoreID,
		"target_store_kind", p.TargetStoreKind,
	)

	// Terminal-state defer. Named-return `err` drives the status
	// classification. Uses a context.Background-derived context for
	// the UPDATE so a cancelled parent ctx doesn't prevent the
	// terminal-state row from landing (SAFETY-02: every restore
	// attempt MUST produce a visible terminal row).
	defer func() {
		finishedAt := e.clock.Now()
		recIDCopy := p.RecordID

		if err == nil {
			if upErr := e.store.UpdateBackupJob(context.Background(), &models.BackupJob{
				ID:             jobID,
				Status:         models.BackupStatusSucceeded,
				StartedAt:      &startedAt,
				FinishedAt:     &finishedAt,
				BackupRecordID: &recIDCopy,
				Progress:       100,
			}); upErr != nil {
				logger.Warn("Failed to finalize restore job",
					"job_id", jobID, "record_id", p.RecordID, "update_error", upErr)
			}
			logger.Info("Restore completed",
				"repo_id", p.Repo.ID,
				"job_id", jobID,
				"record_id", p.RecordID,
			)
			return
		}

		status := models.BackupStatusFailed
		// D-17: ctx cancellation or explicit abort → interrupted.
		if errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, backup.ErrBackupAborted) ||
			errors.Is(err, ErrRestoreAborted) {
			status = models.BackupStatusInterrupted
		}
		if upErr := e.store.UpdateBackupJob(context.Background(), &models.BackupJob{
			ID:             jobID,
			Status:         status,
			StartedAt:      &startedAt,
			FinishedAt:     &finishedAt,
			BackupRecordID: &recIDCopy,
			Error:          err.Error(),
		}); upErr != nil {
			logger.Warn("Failed to mark restore job terminal state",
				"job_id", jobID, "intended_status", status, "update_error", upErr)
		}
		logger.Warn("Restore failed",
			"repo_id", p.Repo.ID,
			"job_id", jobID,
			"status", status,
			"error", err,
		)
	}()

	// Step 3: fetch manifest only (cheap; no payload bandwidth).
	m, err := p.Dst.GetManifestOnly(ctx, p.RecordID)
	if err != nil {
		return fmt.Errorf("fetch manifest: %w", err)
	}

	// Step 4: validate. Hard-reject on any mismatch per D-05 /
	// Pitfall #4. Ordered from cheapest guard (version) to identity
	// guards (kind, id) — all before any destructive action.
	if m.ManifestVersion != manifest.CurrentVersion {
		return fmt.Errorf("%w: got %d want %d",
			ErrManifestVersionUnsupported, m.ManifestVersion, manifest.CurrentVersion)
	}
	if m.StoreKind != p.TargetStoreKind {
		return fmt.Errorf("%w: manifest=%q target=%q",
			ErrStoreKindMismatch, m.StoreKind, p.TargetStoreKind)
	}
	if m.StoreID != p.TargetStoreID {
		return fmt.Errorf("%w: manifest=%q target=%q",
			ErrStoreIDMismatch, m.StoreID, p.TargetStoreID)
	}
	if m.SHA256 == "" {
		return fmt.Errorf("manifest SHA-256 is empty: record %s", p.RecordID)
	}

	// Step 6: open fresh engine at temp path/schema.
	freshStore, tempIdentity, err := OpenFreshEngineAtTemp(ctx, p.StoresService, p.TargetStoreCfg)
	if err != nil {
		return fmt.Errorf("open fresh engine: %w", err)
	}

	// cleanupTemp is flipped to false immediately after a successful
	// SwapMetadataStore (step 10). Before that flip, any early return
	// routes through this defer to close the fresh engine and reclaim
	// its backing.
	cleanupTemp := true
	defer func() {
		if !cleanupTemp {
			return
		}
		if closer, ok := freshStore.(io.Closer); ok {
			if cerr := closer.Close(); cerr != nil {
				logger.Warn("restore: fresh engine close error",
					"error", cerr, "temp", tempIdentity.TempPath)
			}
		}
		if cerr := CleanupTempBacking(ctx, p.StoresService, tempIdentity); cerr != nil {
			logger.Warn("restore: temp cleanup error",
				"error", cerr, "temp", tempIdentity.TempPath, "kind", tempIdentity.Kind)
		}
	}()

	// Step 7: stream payload.
	_, reader, err := p.Dst.GetBackup(ctx, p.RecordID)
	if err != nil {
		return fmt.Errorf("fetch backup payload: %w", err)
	}

	// Step 8: restore into the fresh engine. Phase 2 D-06 "destination
	// must be empty" invariant holds by construction (fresh engine).
	freshBackupable, ok := freshStore.(backup.Backupable)
	if !ok {
		_ = reader.Close()
		return fmt.Errorf("%w: fresh engine %q does not implement Backupable",
			backup.ErrBackupUnsupported, p.TargetStoreCfg.Type)
	}
	restoreErr := freshBackupable.Restore(ctx, reader)

	// Step 9: close the reader. Phase 3 D-11 streaming-verify returns
	// ErrSHA256Mismatch on Close if the payload diverges from the
	// manifest's declared digest.
	closeErr := reader.Close()
	if restoreErr != nil {
		return fmt.Errorf("restore into fresh engine: %w", restoreErr)
	}
	if closeErr != nil {
		return fmt.Errorf("verify payload: %w", closeErr)
	}

	// Step 10: atomic commit. From this moment on, clients see the
	// restored data via the live registry pointer.
	oldStore, err := p.StoresService.SwapMetadataStore(p.TargetStoreCfg.Name, freshStore)
	if err != nil {
		return fmt.Errorf("swap store: %w", err)
	}
	cleanupTemp = false // fresh engine is live; do NOT wipe on defer.

	// Steps 11-12: close old engine + reclaim its backing + rename
	// temp → canonical. Errors are logged, NOT fatal: the restore has
	// succeeded and clients see the new data. Plan 07's orphan sweep
	// reclaims residual temp paths / displaced schemas after the
	// grace window.
	if cerr := CommitSwap(ctx, p.StoresService, oldStore, tempIdentity); cerr != nil {
		logger.Warn("restore: post-swap cleanup had errors (restore still succeeded)",
			"repo_id", p.Repo.ID,
			"record_id", p.RecordID,
			"kind", tempIdentity.Kind,
			"error", cerr,
		)
	}

	// Step 13: bump NFSv4 boot verifier (D-09 belt-and-suspenders).
	// Plan 07 passes writehandlers.BumpBootVerifier; tests may pass
	// nil.
	if p.BumpBootVerifier != nil {
		p.BumpBootVerifier()
	}

	return nil
}
