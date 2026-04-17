package storebackups

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/backup/manifest"
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
func (s *Service) RunRestore(ctx context.Context, repoID string, recordID *string) (job *models.BackupJob, err error) {
	if s.restoreExec == nil {
		return nil, fmt.Errorf("restore path not wired: Service constructed without restore executor")
	}
	if s.shares == nil || s.stores == nil {
		return nil, fmt.Errorf("restore path not wired: Service constructed without shares and/or stores sub-services " +
			"(use WithShares + WithStores)")
	}

	unlock, acquired := s.overlap.TryLock(repoID)
	if !acquired {
		return nil, fmt.Errorf("%w: repo %s", ErrBackupAlreadyRunning, repoID)
	}
	defer unlock()

	// D-19: open the restore.run span + attach terminal-state metrics.
	// s.metrics and s.tracer are set once at construction (via Options) so
	// no mutex is required on the hot path — they always hold valid values.
	// Use the returned span ctx as the parent for the run ctx so downstream
	// storage / destination spans nest under restore.run (Copilot #384).
	spanCtx, finishSpan := s.tracer.Start(ctx, SpanRestoreRun)
	defer func() {
		outcome := classifyOutcome(err)
		s.metrics.RecordOutcome(KindRestore, outcome)
		if outcome == OutcomeSucceeded {
			s.metrics.RecordLastSuccess(repoID, KindRestore, s.now())
		}
		finishSpan(err)
	}()

	// Bind the span ctx to serveCtx so Stop() cancels in-flight restores
	// (D-17 — mirrors the backup path via deriveRunCtx).
	runCtx, cancelRun := s.deriveRunCtx(spanCtx)
	defer cancelRun()

	repo, err := s.store.GetBackupRepoByID(runCtx, repoID)
	if err != nil {
		if errors.Is(err, models.ErrBackupRepoNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrRepoNotFound, repoID)
		}
		return nil, fmt.Errorf("load repo: %w", err)
	}

	// Resolve target + surface cfg + storeName for the REST-02 gate and
	// for restore.Params.TargetStoreCfg.
	resolver, ok := s.resolver.(RestoreResolver)
	if !ok {
		return nil, fmt.Errorf("resolver does not implement RestoreResolver (need ResolveWithName + ResolveCfg)")
	}
	_, storeID, storeKind, storeName, err := resolver.ResolveWithName(runCtx, repo.TargetKind, repo.TargetID)
	if err != nil {
		return nil, err
	}
	targetCfg, err := resolver.ResolveCfg(runCtx, repo.TargetKind, repo.TargetID)
	if err != nil {
		return nil, err
	}

	// REST-02 pre-flight gate — shares must be disabled before restore.
	if enabled := s.shares.ListEnabledSharesForStore(storeName); len(enabled) > 0 {
		return nil, newRestorePreconditionError(storeName, enabled)
	}

	// D-15 / D-16 record selection.
	selectedID, err := s.selectRestoreRecord(runCtx, repoID, recordID)
	if err != nil {
		return nil, err
	}

	// Build the destination driver for this repo.
	dst, err := s.destFactory(runCtx, repo)
	if err != nil {
		return nil, fmt.Errorf("build destination: %w", err)
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
	// Phase 6 D-43: pass the onJobCreated callback so the run-ctx cancel
	// func is registered as soon as the BackupJob row is persisted —
	// BEFORE the payload streaming begins — so CancelBackupJob can abort
	// an in-flight restore. Per-call option, safe for concurrent restores.
	var registeredJobID string
	job, err = s.restoreExec.RunRestore(runCtx, params,
		restore.WithOnJobCreated(func(j *models.BackupJob) {
			if j == nil {
				return
			}
			registeredJobID = j.ID
			s.registerRunCtx(j.ID, cancelRun)
		}),
	)
	if registeredJobID != "" {
		defer s.unregisterRunCtx(registeredJobID)
	}
	return job, err
}

// DryRunResult is returned by Service.RunRestoreDryRun — pre-flight only,
// no state mutation, no payload download (Phase 6 D-31).
type DryRunResult struct {
	// Record is the BackupRecord that would be restored (latest-succeeded
	// per D-15 when recordID==nil, or the explicit --from selection).
	Record *models.BackupRecord

	// ManifestValid is true when the manifest envelope passes the
	// non-destructive guards (store_id, store_kind, sha256 non-empty).
	// Forward-incompatible manifest_version is NOT reported via this flag
	// — it surfaces as ErrManifestVersionUnsupported so the CLI refuses to
	// proceed with an unusable candidate.
	ManifestValid bool

	// EnabledShares lists shares currently enabled on the target store.
	// Reported (NOT enforced — D-31) so the CLI can show a hint. The real
	// RunRestore path enforces the REST-02 gate and refuses with 409.
	EnabledShares []string
}

// RunRestoreDryRun performs the D-31 pre-flight: record resolution plus
// Dst.GetManifestOnly plus envelope validation. Does NOT enforce the
// shares-enabled gate (unlike RunRestore). Cheap — manifests are KBs, not
// GBs, so the CLI's --dry-run flow can run this freely.
//
// Errors:
//
//   - ErrRepoNotFound / ErrNoRestoreCandidate / ErrRecordRepoMismatch /
//     ErrRecordNotRestorable — as in RunRestore.
//   - ErrManifestVersionUnsupported — fatal; CLI cannot proceed.
//
// Non-fatal manifest mismatches (store_id, store_kind, empty sha256) are
// surfaced via ManifestValid=false so the CLI can render the selected
// record plus a "would fail validation" hint.
func (s *Service) RunRestoreDryRun(ctx context.Context, repoID string, recordID *string) (*DryRunResult, error) {
	if s.restoreExec == nil {
		return nil, fmt.Errorf("restore path not wired: Service constructed without restore executor")
	}
	if s.shares == nil || s.stores == nil {
		return nil, fmt.Errorf("restore path not wired: Service constructed without shares and/or stores sub-services " +
			"(use WithShares + WithStores)")
	}

	repo, err := s.store.GetBackupRepoByID(ctx, repoID)
	if err != nil {
		if errors.Is(err, models.ErrBackupRepoNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrRepoNotFound, repoID)
		}
		return nil, fmt.Errorf("load repo: %w", err)
	}

	resolver, ok := s.resolver.(RestoreResolver)
	if !ok {
		return nil, fmt.Errorf("resolver does not implement RestoreResolver (need ResolveWithName + ResolveCfg)")
	}
	_, storeID, storeKind, storeName, err := resolver.ResolveWithName(ctx, repo.TargetKind, repo.TargetID)
	if err != nil {
		return nil, err
	}

	// Record selection (same logic as RunRestore).
	selectedID, err := s.selectRestoreRecord(ctx, repoID, recordID)
	if err != nil {
		return nil, err
	}
	rec, err := s.store.GetBackupRecord(ctx, selectedID)
	if err != nil {
		return nil, fmt.Errorf("load record %q: %w", selectedID, err)
	}

	// Build the destination and fetch ONLY the manifest — no payload
	// stream, no store mutation.
	dst, err := s.destFactory(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("build destination: %w", err)
	}
	defer func() {
		if cerr := dst.Close(); cerr != nil {
			logger.Warn("Destination close error", "repo_id", repoID, "error", cerr)
		}
	}()

	// GetManifestOnly takes the backup ID (ULID), not the manifest path —
	// the driver appends /manifest.yaml internally. rec.ManifestPath is the
	// relative on-disk path ("<id>/manifest.yaml") and would double-append.
	m, err := dst.GetManifestOnly(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}

	// Version mismatch is fatal (CLI cannot proceed). All other guards
	// (store_id, store_kind, sha256) are reported via ManifestValid=false
	// without failing the dry-run so the CLI can render context.
	if m.ManifestVersion != manifest.CurrentVersion {
		return nil, fmt.Errorf("%w: got %d want %d",
			ErrManifestVersionUnsupported, m.ManifestVersion, manifest.CurrentVersion)
	}
	manifestValid := m.StoreKind == storeKind && m.StoreID == storeID && m.SHA256 != ""

	// Report (do NOT enforce) enabled shares — D-31.
	enabled := s.shares.ListEnabledSharesForStore(storeName)

	return &DryRunResult{
		Record:        rec,
		ManifestValid: manifestValid,
		EnabledShares: enabled,
	}, nil
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
