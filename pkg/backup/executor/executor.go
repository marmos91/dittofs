package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/oklog/ulid/v2"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/backup"
	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/manifest"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// JobStore is the narrow persistence interface the Executor needs. A subset
// of store.BackupStore — callers pass the full store but the Executor only
// consumes these four methods, which keeps test fakes trivial.
//
// UpdateBackupJobProgress is the Phase 6 D-50 stage-marker hook. Callers log
// WARN on failure and do NOT fail the parent op (best-effort semantics).
type JobStore interface {
	CreateBackupJob(ctx context.Context, job *models.BackupJob) (string, error)
	UpdateBackupJob(ctx context.Context, job *models.BackupJob) error
	UpdateBackupJobProgress(ctx context.Context, jobID string, pct int) error
	CreateBackupRecord(ctx context.Context, rec *models.BackupRecord) (string, error)
}

// Executor runs one backup attempt per RunBackup call.
type Executor struct {
	store JobStore
	clock backup.Clock
}

// RunBackupOption tunes a single RunBackup invocation. Options are
// per-call so concurrent RunBackup calls (different repos) do not race
// on shared executor state.
type RunBackupOption func(*runBackupConfig)

type runBackupConfig struct {
	onJobCreated func(*models.BackupJob)
}

// WithOnJobCreated installs a callback invoked synchronously inside
// RunBackup immediately after CreateBackupJob succeeds (i.e. the
// BackupJob row is persisted with Status=running) and BEFORE the
// destination PutBackup begins. Phase 6 (D-43) uses this hook to
// register the run-ctx's cancel func against job.ID so CancelBackupJob
// can interrupt the in-flight run.
//
// The callback must not block for long — it executes on the RunBackup
// goroutine and delays the payload stream.
func WithOnJobCreated(fn func(*models.BackupJob)) RunBackupOption {
	return func(c *runBackupConfig) { c.onJobCreated = fn }
}

// New constructs an Executor. clock may be nil; backup.RealClock is used.
func New(store JobStore, clock backup.Clock) *Executor {
	if clock == nil {
		clock = backup.RealClock{}
	}
	return &Executor{store: store, clock: clock}
}

// SetClock swaps the clock at runtime. Safe to call before any RunBackup
// invocation; callers use this to reach into an Executor constructed with
// defaults and inject a test clock after the fact.
func (e *Executor) SetClock(c backup.Clock) {
	if c == nil {
		c = backup.RealClock{}
	}
	e.clock = c
}

// RunBackup executes one backup attempt. Returns (rec, job, nil) on success
// where rec is the persisted BackupRecord and job is the synchronously
// persisted BackupJob row; (nil, job-or-nil, err) on failure — job is non-nil
// after the synchronous CreateBackupJob has succeeded (so Phase 6 callers can
// still surface the job ID to clients for polling). Pre-CreateBackupJob
// failures (nil-arg guards, createJobErr) return (nil, nil, err).
//
// storeID is the source metadata store ID snapshotted into manifest.StoreID
// AND BackupRecord.StoreID (cross-store restore guard per Phase 1).
// storeKind is "memory" | "badger" | "postgres" (written to manifest.StoreKind).
//
// Failure semantics (D-16, D-18):
//   - source or destination returns non-nil error → BackupJob transitions to
//     failed (or interrupted on ctx cancel / backup.ErrBackupAborted);
//     NO BackupRecord row is created.
//   - ctx cancellation → BackupJob ends with Status=interrupted.
//   - CreateBackupRecord fails after PutBackup succeeded → BackupJob marked
//     failed with an explicit "archive published but record persist failed"
//     message; operator can reconcile via orphan sweep.
func (e *Executor) RunBackup(
	ctx context.Context,
	source backup.Backupable,
	dst destination.Destination,
	repo *models.BackupRepo,
	storeID string,
	storeKind string,
	opts ...RunBackupOption,
) (*models.BackupRecord, *models.BackupJob, error) {
	if repo == nil {
		return nil, nil, fmt.Errorf("executor: repo is nil")
	}
	if source == nil {
		return nil, nil, fmt.Errorf("executor: source is nil")
	}
	if dst == nil {
		return nil, nil, fmt.Errorf("executor: destination is nil")
	}

	var cfg runBackupConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	// Step 1 (D-21): allocate the record ULID now so it flows through the
	// manifest, the destination archive key, and the DB row identically.
	recordID := ulid.Make().String()

	// Step 2: create BackupJob row (status=running).
	startedAt := e.clock.Now()
	jobID := ulid.Make().String()
	job := &models.BackupJob{
		ID:        jobID,
		Kind:      models.BackupJobKindBackup,
		RepoID:    repo.ID,
		Status:    models.BackupStatusRunning,
		StartedAt: &startedAt,
	}
	if _, err := e.store.CreateBackupJob(ctx, job); err != nil {
		return nil, nil, fmt.Errorf("create backup job: %w", err)
	}

	// Phase 6 D-43: notify the runtime so it can register the run-ctx
	// cancel func against job.ID before the blocking payload stream begins.
	if cfg.onJobCreated != nil {
		cfg.onJobCreated(job)
	}

	// Phase 6 D-50: best-effort progress markers. Log WARN on failure and
	// continue — the UpdateBackupJobProgress call MUST NOT fail the parent
	// op. Captured closure so every call logs with consistent fields.
	updateProgress := func(pct int) {
		if err := e.store.UpdateBackupJobProgress(ctx, jobID, pct); err != nil {
			logger.Warn("Failed to update backup job progress",
				"job_id", jobID, "pct", pct, "error", err)
		}
	}
	updateProgress(0)

	logger.Info("Backup starting",
		"repo_id", repo.ID,
		"job_id", jobID,
		"record_id", recordID,
		"store_id", storeID,
		"store_kind", storeKind,
	)

	// Step 3: build the manifest skeleton. Destination fills SHA256 and
	// SizeBytes during PutBackup (tee + counter). PayloadIDSet is stamped
	// back onto the manifest AFTER source.Backup returns — before that, the
	// set is not yet known. Destination drivers do not read PayloadIDSet
	// until they serialize the manifest (manifest-last invariant per D-21).
	m := &manifest.Manifest{
		ManifestVersion: manifest.CurrentVersion,
		BackupID:        recordID,
		CreatedAt:       startedAt,
		StoreID:         storeID,
		StoreKind:       storeKind,
		Encryption: manifest.Encryption{
			Enabled: repo.EncryptionEnabled,
		},
	}
	if repo.EncryptionEnabled {
		m.Encryption.Algorithm = "aes-256-gcm"
		m.Encryption.KeyRef = repo.EncryptionKeyRef
	}

	// D-50 marker: destination prep complete; about to wire payload pipe.
	updateProgress(10)

	// Step 4: io.Pipe. Source goroutine writes cleartext; destination reads
	// it. Source closes the write side with any error so PutBackup's reader
	// observes EOF or a read error. We wait on srcDone after PutBackup
	// returns to guarantee the source goroutine has finished (no leak).
	pr, pw := io.Pipe()

	var (
		ids    backup.PayloadIDSet
		srcErr error
	)
	srcDone := make(chan struct{})
	go func() {
		defer close(srcDone)
		ids, srcErr = source.Backup(ctx, pw)
		if srcErr != nil {
			_ = pw.CloseWithError(srcErr)
			return
		}
		_ = pw.Close()
	}()

	// D-50 marker: about to begin payload stream (destination drains pipe).
	updateProgress(50)

	// Destination consumes the reader and writes the manifest last. It
	// populates m.SHA256 + m.SizeBytes through the pointer.
	dstErr := dst.PutBackup(ctx, m, pr)

	// Ensure the source goroutine has finished before we inspect srcErr /
	// build the error aggregation.
	<-srcDone

	// Close reader — idempotent on an already-closed pipe.
	_ = pr.Close()

	// Stamp PayloadIDSet into the manifest from the source's return value.
	// Sorted for deterministic YAML output (destination writes manifest last
	// on the destination side; here we update the in-memory copy so callers
	// with a manifest pointer observe the final set).
	m.PayloadIDSet = payloadIDSetToSlice(ids)

	// Aggregate errors in priority order: source beats destination beats ctx.
	var runErr error
	switch {
	case srcErr != nil:
		runErr = fmt.Errorf("source backup: %w", srcErr)
	case dstErr != nil:
		runErr = fmt.Errorf("destination put: %w", dstErr)
	case ctx.Err() != nil:
		runErr = ctx.Err()
	}

	finishedAt := e.clock.Now()

	if runErr != nil {
		status := models.BackupStatusFailed
		// D-18: ctx cancellation or explicit abort → interrupted.
		if errors.Is(runErr, context.Canceled) ||
			errors.Is(runErr, context.DeadlineExceeded) ||
			errors.Is(runErr, backup.ErrBackupAborted) {
			status = models.BackupStatusInterrupted
		}
		if upErr := e.store.UpdateBackupJob(ctx, &models.BackupJob{
			ID:         jobID,
			Status:     status,
			StartedAt:  &startedAt,
			FinishedAt: &finishedAt,
			Error:      runErr.Error(),
		}); upErr != nil {
			logger.Warn("Failed to mark backup job terminal state",
				"job_id", jobID, "intended_status", status, "update_error", upErr)
		}
		logger.Warn("Backup failed",
			"repo_id", repo.ID,
			"job_id", jobID,
			"status", status,
			"error", runErr,
		)
		// Surface the job so Phase-6 callers can report job.ID even on
		// failure. The job struct has been populated with the terminal-state
		// fields via the UpdateBackupJob above; mirror those onto our
		// return-value copy so the caller sees a consistent view even
		// without a follow-up read.
		job.Status = status
		job.FinishedAt = &finishedAt
		job.Error = runErr.Error()
		return nil, job, runErr
	}

	// Step 5 (happy path): persist the BackupRecord. CreateBackupRecord must
	// preserve our pre-allocated ID (the destination already keyed the
	// archive with it per D-21).
	rec := &models.BackupRecord{
		ID:           recordID,
		RepoID:       repo.ID,
		CreatedAt:    finishedAt,
		SizeBytes:    m.SizeBytes,
		Status:       models.BackupStatusSucceeded,
		ManifestPath: fmt.Sprintf("%s/manifest.yaml", recordID),
		SHA256:       m.SHA256,
		StoreID:      storeID,
	}
	if _, err := e.store.CreateBackupRecord(ctx, rec); err != nil {
		// Destination archive is already published; record creation failed.
		// Mark the job failed with an explicit message so operators see the
		// discrepancy in job logs. Phase 5 orphan sweep will NOT delete the
		// archive because manifest.yaml is present (published invariant).
		errMsg := fmt.Sprintf("archive published but record persist failed: %v", err)
		if upErr := e.store.UpdateBackupJob(ctx, &models.BackupJob{
			ID:         jobID,
			Status:     models.BackupStatusFailed,
			StartedAt:  &startedAt,
			FinishedAt: &finishedAt,
			Error:      errMsg,
		}); upErr != nil {
			logger.Warn("Failed to mark backup job failed after record persist failure",
				"job_id", jobID, "update_error", upErr)
		}
		logger.Error("Backup record persist failed after archive publish",
			"repo_id", repo.ID,
			"job_id", jobID,
			"record_id", recordID,
			"error", err,
		)
		job.Status = models.BackupStatusFailed
		job.FinishedAt = &finishedAt
		job.Error = errMsg
		return nil, job, fmt.Errorf("create backup record: %w", err)
	}

	// D-50 marker: record persisted; about to finalize the job row.
	updateProgress(95)

	// Step 6: finalize the job — succeeded, BackupRecordID populated.
	recIDRef := recordID
	if upErr := e.store.UpdateBackupJob(ctx, &models.BackupJob{
		ID:             jobID,
		Status:         models.BackupStatusSucceeded,
		StartedAt:      &startedAt,
		FinishedAt:     &finishedAt,
		BackupRecordID: &recIDRef,
		Progress:       100,
	}); upErr != nil {
		logger.Warn("Failed to finalize backup job after success",
			"job_id", jobID, "record_id", recordID, "update_error", upErr)
	}

	logger.Info("Backup completed",
		"repo_id", repo.ID,
		"job_id", jobID,
		"record_id", recordID,
		"size_bytes", m.SizeBytes,
		"sha256", m.SHA256,
	)
	// Mirror the terminal-state fields the final UpdateBackupJob just
	// persisted so the returned job pointer carries a consistent snapshot.
	job.Status = models.BackupStatusSucceeded
	job.FinishedAt = &finishedAt
	job.BackupRecordID = &recIDRef
	job.Progress = 100
	return rec, job, nil
}

// payloadIDSetToSlice converts a backup.PayloadIDSet to a deterministically
// sorted []string. Manifest.Validate rejects a nil PayloadIDSet (must be
// non-nil, possibly empty — SAFETY-01), so we always return a non-nil slice.
func payloadIDSetToSlice(ids backup.PayloadIDSet) []string {
	if ids == nil {
		return []string{}
	}
	out := make([]string, 0, ids.Len())
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
