package storebackups

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/backup/restore"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// SharesService is the narrow contract storebackups needs from the
// runtime/shares sub-service. Plan 07 uses only the REST-02 pre-flight
// gate: enumerate every runtime share that is (a) enabled AND (b)
// referencing the target metadata store by name.
//
// Satisfied by *shares.Service from
// pkg/controlplane/runtime/shares/service.go.
type SharesService interface {
	ListEnabledSharesForStore(metadataStoreName string) []string
}

// MetadataStoreConfigLister is the narrow typed hook for the startup
// orphan sweep (D-14). Satisfied DIRECTLY by the composite control-plane
// store (pkg/controlplane/store.Store — ListMetadataStores verified at
// pkg/controlplane/store/metadata.go:20). Production wiring passes the
// composite Store; tests pass a stub. There is no adapter wrapper and no
// noop fallback — if Service is constructed without this dependency,
// SweepRestoreOrphans no-ops with a visible log line.
type MetadataStoreConfigLister interface {
	ListMetadataStores(ctx context.Context) ([]*models.MetadataStoreConfig, error)
}

// RunRestore executes one restore attempt for the given repo. The single
// callable entrypoint for Phase-6 CLI/REST integration; sibling to
// RunBackup.
//
// Record selection:
//
//   - recordID == nil → most recent succeeded BackupRecord by
//     created_at (D-15). Error ErrNoRestoreCandidate if none.
//
//   - recordID != nil → validate repo match (D-16: ErrRecordRepoMismatch)
//     and Status=succeeded (ErrRecordNotRestorable).
//
// Pre-flight gates:
//
//   - REST-02: if any share referencing the target metadata store has
//     Enabled=true → return ErrRestorePreconditionFailed (409). Operator
//     must explicitly DisableShare each affected share before retry.
//
//   - D-07 overlap guard: same per-repo mutex as RunBackup — concurrent
//     backup+restore on the same repo is rejected with
//     ErrBackupAlreadyRunning.
//
// Delegation:
//
//   - After pre-flight, delegates to restore.Executor.RunRestore which
//     owns steps 3-13 of D-05 (manifest fetch, validation, side-engine
//     open, Backupable.Restore, SHA-256 verify, atomic swap,
//     post-swap cleanup, boot-verifier bump).
//
// Post-conditions:
//
//   - On success: registry points at the restored engine; shares remain
//     disabled (D-04 — operator re-enables explicitly).
//
//   - On failure: registry untouched; fresh engine + temp path reclaimed
//     by the restore Executor's defer; BackupJob row records terminal
//     state (failed / interrupted) for SAFETY-02 visibility.
func (s *Service) RunRestore(ctx context.Context, repoID string, recordID *string) (err error) {
	if s.restoreExec == nil {
		return fmt.Errorf("restore path not wired: Service constructed without restore executor")
	}
	if s.shares == nil || s.stores == nil {
		return fmt.Errorf("restore path not wired: Service constructed without shares and/or stores sub-services " +
			"(use WithShares + WithStores)")
	}

	unlock, acquired := s.overlap.TryLock(repoID)
	if !acquired {
		return fmt.Errorf("%w: repo %s", ErrBackupAlreadyRunning, repoID)
	}
	defer unlock()

	// D-19: open the restore.run span + attach terminal-state metrics.
	// s.metrics and s.tracer are set once at construction (via Options) so
	// no mutex is required on the hot path — they always hold valid values.
	_, finishSpan := s.tracer.Start(ctx, SpanRestoreRun)
	defer func() {
		outcome := classifyOutcome(err)
		s.metrics.RecordOutcome(KindRestore, outcome)
		if outcome == OutcomeSucceeded {
			s.metrics.RecordLastSuccess(repoID, KindRestore, s.now())
		}
		finishSpan(err)
	}()

	// Bind the caller ctx to serveCtx so Stop() cancels in-flight restores
	// (D-17 — mirrors the backup path via deriveRunCtx).
	runCtx, cancelRun := s.deriveRunCtx(ctx)
	defer cancelRun()

	repo, err := s.store.GetBackupRepoByID(runCtx, repoID)
	if err != nil {
		if errors.Is(err, models.ErrBackupRepoNotFound) {
			return fmt.Errorf("%w: %s", ErrRepoNotFound, repoID)
		}
		return fmt.Errorf("load repo: %w", err)
	}

	// Resolve target + surface cfg + storeName for the REST-02 gate and
	// for restore.Params.TargetStoreCfg.
	resolver, ok := s.resolver.(RestoreResolver)
	if !ok {
		return fmt.Errorf("resolver does not implement RestoreResolver (need ResolveWithName + ResolveCfg)")
	}
	_, storeID, storeKind, storeName, err := resolver.ResolveWithName(runCtx, repo.TargetKind, repo.TargetID)
	if err != nil {
		return err
	}
	targetCfg, err := resolver.ResolveCfg(runCtx, repo.TargetKind, repo.TargetID)
	if err != nil {
		return err
	}

	// REST-02 pre-flight gate — shares must be disabled before restore.
	if enabled := s.shares.ListEnabledSharesForStore(storeName); len(enabled) > 0 {
		return fmt.Errorf("%w: store %q has %d enabled share(s): %v",
			ErrRestorePreconditionFailed, storeName, len(enabled), enabled)
	}

	// D-15 / D-16 record selection.
	selectedID, err := s.selectRestoreRecord(runCtx, repoID, recordID)
	if err != nil {
		return err
	}

	// Build the destination driver for this repo.
	dst, err := s.destFactory(runCtx, repo)
	if err != nil {
		return fmt.Errorf("build destination: %w", err)
	}
	defer func() {
		if cerr := dst.Close(); cerr != nil {
			logger.Warn("Destination close error", "repo_id", repoID, "error", cerr)
		}
	}()

	// Read bumpBootVerifier under the lock — SetBumpBootVerifier may be
	// called from another goroutine after construction.
	s.mu.RLock()
	bump := s.bumpBootVerifier
	s.mu.RUnlock()

	params := restore.Params{
		Repo:             repo,
		Dst:              dst,
		RecordID:         selectedID,
		TargetStoreKind:  storeKind,
		TargetStoreID:    storeID,
		TargetStoreCfg:   targetCfg,
		StoresService:    s.stores,
		BumpBootVerifier: bump,
	}
	return s.restoreExec.RunRestore(runCtx, params)
}

// selectRestoreRecord implements D-15 (default latest) + D-16 (validate
// --from <id>). Called by RunRestore; exported as a method so tests can
// exercise the branch logic without a full RunRestore harness.
func (s *Service) selectRestoreRecord(ctx context.Context, repoID string, recordID *string) (string, error) {
	if recordID != nil {
		rec, err := s.store.GetBackupRecord(ctx, *recordID)
		if err != nil {
			return "", fmt.Errorf("get record %q: %w", *recordID, err)
		}
		if rec.RepoID != repoID {
			return "", fmt.Errorf("%w: record=%q actual_repo=%q requested_repo=%q",
				ErrRecordRepoMismatch, *recordID, rec.RepoID, repoID)
		}
		if rec.Status != models.BackupStatusSucceeded {
			return "", fmt.Errorf("%w: record=%q status=%q",
				ErrRecordNotRestorable, *recordID, rec.Status)
		}
		return rec.ID, nil
	}
	// D-15: most-recent-succeeded. ListSucceededRecordsByRepo returns
	// newest-first (see pkg/controlplane/store/interface.go).
	recs, err := s.store.ListSucceededRecordsByRepo(ctx, repoID)
	if err != nil {
		return "", fmt.Errorf("list succeeded records: %w", err)
	}
	if len(recs) == 0 {
		return "", fmt.Errorf("%w: repo %s", ErrNoRestoreCandidate, repoID)
	}
	return recs[0].ID, nil
}
