package storebackups

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/backup"
	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/executor"
	"github.com/marmos91/dittofs/pkg/backup/restore"
	"github.com/marmos91/dittofs/pkg/backup/scheduler"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// DefaultShutdownTimeout mirrors the other sub-service conventions
// (adapters.DefaultShutdownTimeout, lifecycle.DefaultShutdownTimeout).
const DefaultShutdownTimeout = 30 * time.Second

// DestinationFactoryFn builds a destination.Destination from a persisted repo.
// Injected into the Service so tests can swap in a fake without wiring the
// full destination registry. In production, Service.New defaults this to
// destination.DestinationFactoryFromRepo.
type DestinationFactoryFn func(ctx context.Context, repo *models.BackupRepo) (destination.Destination, error)

// Service is the runtime sub-service that composes the scheduler, overlap
// guard, executor, retention pass, and target resolver into a single
// lifecycle entity. Structure mirrors runtime/adapters.Service.
type Service struct {
	mu       sync.RWMutex
	store    store.BackupStore
	resolver StoreResolver

	sched   *scheduler.Scheduler
	overlap *scheduler.OverlapGuard
	exec    *executor.Executor
	clock   backup.Clock

	destFactory     DestinationFactoryFn
	shutdownTimeout time.Duration

	// Phase-5 dependencies for RunRestore. Any of these may be nil; if so
	// RunRestore returns a clear "restore not wired" error and the startup
	// orphan sweep (D-14) logs a warning and no-ops. Keeping the fields
	// individually nil-safe preserves backward compatibility for callers
	// (tests, early integrations) that built Service with the Phase-4
	// constructor signature.
	shares           SharesService
	stores           restore.StoresService
	restoreExec      *restore.Executor
	bumpBootVerifier func()
	// metadataConfigs is a narrow typed hook over ListMetadataStores,
	// satisfied DIRECTLY by the composite pkg/controlplane/store.Store.
	// Exists only as a testability seam — production wiring passes the
	// composite Store without any adapter wrapper or noop fallback.
	metadataConfigs MetadataStoreConfigLister

	// metrics + tracer are the Plan 05-09 D-19 observability hooks. Default
	// to NoopMetrics + NoopTracer so callers that never call
	// WithMetricsCollector / WithTracer pay zero overhead. Wire via the
	// WithMetricsCollector / WithTracer options to enable Prometheus +
	// OpenTelemetry.
	metrics MetricsCollector
	tracer  Tracer

	serveOnce sync.Once
	serveErr  error

	// serveCtx derives from the Serve ctx; cancelled by Stop. Shutdown of
	// in-flight executor runs depends on Stop propagating through this ctx.
	serveCtx    context.Context
	serveCancel context.CancelFunc
}

// Option configures a Service at construction.
type Option func(*Service)

// WithMaxJitter overrides the scheduler's default jitter window.
func WithMaxJitter(d time.Duration) Option {
	return func(s *Service) {
		// Rebuild the scheduler with the new jitter and the shared overlap guard.
		s.sched = scheduler.NewScheduler(
			scheduler.WithMaxJitter(d),
			scheduler.WithOverlapGuard(s.overlap),
		)
	}
}

// WithDestinationFactory overrides the default destination factory.
func WithDestinationFactory(fn DestinationFactoryFn) Option {
	return func(s *Service) { s.destFactory = fn }
}

// WithClock injects a test clock (used by the executor + retention paths).
func WithClock(c backup.Clock) Option {
	return func(s *Service) {
		s.clock = c
		if s.exec != nil {
			s.exec.SetClock(c)
		}
		if s.restoreExec != nil {
			s.restoreExec.SetClock(c)
		}
	}
}

// WithShutdownTimeout overrides the default shutdown timeout.
func WithShutdownTimeout(d time.Duration) Option {
	return func(s *Service) {
		if d == 0 {
			d = DefaultShutdownTimeout
		}
		s.shutdownTimeout = d
	}
}

// WithShares wires the shares sub-service for the REST-02 pre-flight gate.
// Without it, RunRestore refuses with a clear "restore not wired" error.
func WithShares(sh SharesService) Option {
	return func(s *Service) { s.shares = sh }
}

// WithStores wires the stores sub-service for the restore fresh-engine
// path + D-14 Postgres orphan sweep. Without it, RunRestore refuses and
// the Postgres branch of SweepRestoreOrphans skips with a log warning.
func WithStores(st restore.StoresService) Option {
	return func(s *Service) { s.stores = st }
}

// WithBumpBootVerifier wires the NFSv4 boot-verifier bump hook (D-09).
// Typically writehandlers.BumpBootVerifier. nil is acceptable — tests
// may pass nil; the restore path treats nil as "no bump".
func WithBumpBootVerifier(fn func()) Option {
	return func(s *Service) { s.bumpBootVerifier = fn }
}

// WithMetadataConfigs wires the narrow MetadataStoreConfigLister hook for
// the D-14 startup orphan sweep. Production callers pass the composite
// pkg/controlplane/store.Store directly (it implements
// ListMetadataStores per pkg/controlplane/store/metadata.go:20). No
// adapter wrapper, no noop fallback.
func WithMetadataConfigs(lister MetadataStoreConfigLister) Option {
	return func(s *Service) { s.metadataConfigs = lister }
}

// WithMetricsCollector wires the Plan 05-09 D-19 terminal-state counter
// and last-success gauge. nil is equivalent to NoopMetrics (zero overhead).
func WithMetricsCollector(m MetricsCollector) Option {
	return func(s *Service) {
		if m == nil {
			m = NoopMetrics{}
		}
		s.metrics = m
	}
}

// WithTracer wires the Plan 05-09 D-19 backup.run / restore.run span.
// nil is equivalent to NoopTracer (zero overhead).
func WithTracer(t Tracer) Option {
	return func(s *Service) {
		if t == nil {
			t = NoopTracer{}
		}
		s.tracer = t
	}
}

// New constructs the Service. shutdownTimeout of 0 applies DefaultShutdownTimeout.
func New(s store.BackupStore, resolver StoreResolver, shutdownTimeout time.Duration, opts ...Option) *Service {
	if shutdownTimeout == 0 {
		shutdownTimeout = DefaultShutdownTimeout
	}

	svc := &Service{
		store:           s,
		resolver:        resolver,
		overlap:         scheduler.NewOverlapGuard(),
		shutdownTimeout: shutdownTimeout,
		destFactory:     destination.DestinationFactoryFromRepo,
		// Default to noop collectors so callers that don't wire
		// observability still get nil-safe RunBackup/RunRestore paths.
		metrics: NoopMetrics{},
		tracer:  NoopTracer{},
	}
	svc.sched = scheduler.NewScheduler(scheduler.WithOverlapGuard(svc.overlap))
	svc.exec = executor.New(s, nil)
	// Phase-5: every Service can handle RunRestore even without the runtime
	// wiring — the executor itself is always constructable from the store
	// + clock. RunRestore returns a clear error when shares/stores are
	// missing; callers that don't want restore just skip WithShares /
	// WithStores / WithBumpBootVerifier.
	svc.restoreExec = restore.New(s, nil)

	for _, opt := range opts {
		opt(svc)
	}
	// Options may have swapped the scheduler; re-bind JobFn to the current instance.
	svc.sched.SetJobFn(svc.runScheduledBackup)
	return svc
}

// SetShutdownTimeout mirrors adapters.Service.SetShutdownTimeout. Safe to call
// before Serve; after Serve the new value applies to subsequent Stop calls.
func (s *Service) SetShutdownTimeout(d time.Duration) {
	if d == 0 {
		d = DefaultShutdownTimeout
	}
	s.mu.Lock()
	s.shutdownTimeout = d
	s.mu.Unlock()
}

// SetBumpBootVerifier wires the NFSv4 boot-verifier bump hook (D-09)
// after construction. Separated from WithBumpBootVerifier so the
// runtime composition site — which cannot import
// internal/adapter/nfs/v4/handlers without creating an import cycle —
// can wire the hook from the adapter layer after both packages are
// initialized. nil clears the hook.
func (s *Service) SetBumpBootVerifier(fn func()) {
	s.mu.Lock()
	s.bumpBootVerifier = fn
	s.mu.Unlock()
}

// BackupStore returns the BackupStore this Service was constructed with.
// Exposed so the runtime's block-GC entrypoint (Runtime.RunBlockGC) can
// construct a storebackups.BackupHold without reaching into private state.
// Returns nil only when the Service itself was constructed with a nil
// store (test scaffolding).
func (s *Service) BackupStore() store.BackupStore { return s.store }

// DestFactory returns the destination factory this Service was constructed
// with. Exposed so the runtime's block-GC entrypoint can pass the same
// factory to storebackups.NewBackupHold — keeping destination-lifecycle
// semantics identical between backup and GC-hold paths.
func (s *Service) DestFactory() DestinationFactoryFn { return s.destFactory }

// Serve starts the scheduler. Runs interrupted-job recovery (D-19 / SAFETY-02),
// loads all repos from the store, installs schedules for those with a non-empty
// cron expression (D-06 skip-with-WARN on invalid), and starts the cron loop.
//
// Returns nil on success; the returned error is non-nil ONLY if the initial
// repo listing fails (infrastructure-level). Serve is idempotent via sync.Once;
// subsequent calls return the first call's error.
func (s *Service) Serve(ctx context.Context) error {
	s.serveOnce.Do(func() {
		s.serveErr = s.serve(ctx)
	})
	return s.serveErr
}

func (s *Service) serve(ctx context.Context) error {
	s.mu.Lock()
	// Derive serveCtx from the parent so SIGTERM → Runtime.Serve ctx cancel →
	// this ctx cancel → fire() offset sleep aborts + in-flight executor runs
	// see ctx.Err() and transition BackupJob to interrupted (D-18).
	s.serveCtx, s.serveCancel = context.WithCancel(ctx)
	s.mu.Unlock()

	// D-19 / SAFETY-02 boot recovery. A failure here logs but does NOT block
	// boot — operators see the warning and can restart once the DB is reachable.
	if n, err := s.store.RecoverInterruptedJobs(ctx); err != nil {
		logger.Warn("Failed to recover interrupted backup jobs on boot", "error", err)
	} else if n > 0 {
		logger.Info("Recovered interrupted backup jobs", "count", n)
	}

	// Phase-5 D-14: sweep orphaned restore temp paths/schemas.
	//
	// s.metadataConfigs is the composite store.Store (implements
	// ListMetadataStores directly per pkg/controlplane/store/metadata.go:20).
	// s.stores is the live *stores.Service which implements
	// PostgresOrphanLister (ListPostgresRestoreOrphans + DropPostgresSchema
	// from Plan 04). If either dependency is missing (partial/test wiring),
	// log a clear warning and skip — no silent degradation, no noop fallback.
	if s.metadataConfigs == nil {
		logger.Warn("SweepRestoreOrphans: MetadataStoreConfigLister not wired; " +
			"orphan sweep skipped (use WithMetadataConfigs at construction)")
	} else if s.stores == nil {
		logger.Warn("SweepRestoreOrphans: stores sub-service not wired; " +
			"orphan sweep skipped (use WithStores at construction)")
	} else if lister, ok := s.stores.(PostgresOrphanLister); !ok {
		logger.Warn("SweepRestoreOrphans: stores.Service does not implement " +
			"PostgresOrphanLister; orphan sweep skipped (Plan 04 wiring required)")
	} else {
		SweepRestoreOrphans(ctx, s.metadataConfigs, lister, DefaultRestoreOrphanGraceWindow)
	}

	// Load all repos and install schedules for the ones with non-empty crons.
	repos, err := s.store.ListAllBackupRepos(ctx)
	if err != nil {
		return fmt.Errorf("list backup repos: %w", err)
	}

	installed := 0
	for _, repo := range repos {
		if repo.Schedule == nil || *repo.Schedule == "" {
			continue
		}
		target := NewBackupRepoTarget(repo)
		if err := s.sched.Register(target); err != nil {
			// D-06: one bad row does NOT deny-of-service the entire scheduler.
			logger.Warn("Skipping repo with invalid schedule",
				"repo_id", repo.ID, "schedule", *repo.Schedule, "error", err)
			continue
		}
		installed++
	}
	logger.Info("storebackups scheduler started",
		"repos_total", len(repos), "repos_scheduled", installed)

	s.sched.Start()
	return nil
}

// Stop cancels in-flight runs (D-18) and stops the scheduler. Idempotent —
// safe to call before Serve or multiple times after.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.serveCancel
	s.serveCancel = nil
	timeout := s.shutdownTimeout
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), timeout)
	defer cancelStop()
	return s.sched.Stop(stopCtx)
}

// ValidateSchedule exposes the scheduler's validator to Phase 6 handlers.
// Returns an ErrScheduleInvalid-wrapped error on parse failure (D-06 strict).
func (s *Service) ValidateSchedule(expr string) error {
	return scheduler.ValidateSchedule(expr)
}

// RegisterRepo installs a scheduler entry for repoID. Caller has already
// committed the DB row (Phase 6). Returns ErrRepoNotFound when the ID is
// unknown; ErrScheduleInvalid-wrapped when the repo has a malformed schedule.
//
// No-op (returns nil) for repos with empty Schedule — callers may register
// unscheduled repos to enable on-demand RunBackup without a cron entry.
func (s *Service) RegisterRepo(ctx context.Context, repoID string) error {
	repo, err := s.store.GetBackupRepoByID(ctx, repoID)
	if err != nil {
		if errors.Is(err, models.ErrBackupRepoNotFound) {
			return fmt.Errorf("%w: %s", ErrRepoNotFound, repoID)
		}
		return fmt.Errorf("load repo: %w", err)
	}
	if repo.Schedule == nil || *repo.Schedule == "" {
		logger.Info("Repo has no schedule — skipping scheduler install", "repo_id", repoID)
		return nil
	}
	if err := s.sched.Register(NewBackupRepoTarget(repo)); err != nil {
		return fmt.Errorf("install schedule for repo %s: %w", repoID, err)
	}
	return nil
}

// UnregisterRepo removes the scheduler entry for repoID and drops the per-repo
// mutex from the overlap guard. No-op if not registered. Caller has already
// deleted the DB row.
func (s *Service) UnregisterRepo(ctx context.Context, repoID string) error {
	s.sched.Unregister(repoID)
	s.overlap.Forget(repoID)
	return nil
}

// UpdateRepo = Unregister + Register (D-22: "edit = Unregister + Register").
// Safe to call even if the schedule is unchanged; Register is idempotent.
func (s *Service) UpdateRepo(ctx context.Context, repoID string) error {
	s.sched.Unregister(repoID)
	return s.RegisterRepo(ctx, repoID)
}

// RunBackup runs one backup attempt. Called by BOTH the cron tick (via
// runScheduledBackup) AND Phase 6's on-demand POST /backups handler (D-23).
//
// Mutex behavior (D-07, D-08, SCHED-06):
//  1. Acquire per-repo overlap mutex via TryLock; return ErrBackupAlreadyRunning
//     if held (mapped to HTTP 409 by Phase 6).
//  2. Resolve (target_kind, target_id) → source + storeID + storeKind.
//  3. Build Destination via destFactory(repo).
//  4. executor.RunBackup(ctx, source, dst, repo, storeID, storeKind).
//  5. On success: inline RunRetention(ctx, repo, dst, store, clock) under
//     the same mutex so retention never races with an in-flight upload.
//  6. Release mutex + close Destination.
//
// Returns the new BackupRecord on success. On failure, the record return is
// nil and the BackupJob row records the failure (D-16 — no record on fail).
// Retention failures are logged via RetentionReport and do NOT degrade the
// parent job's success status (D-15).
//
// Named return `err` is observed by the deferred MetricsCollector +
// Tracer finish so every terminal state (success/failed/interrupted) emits
// exactly one counter increment plus a span end. Plan 05-09 D-19 / D-20.
func (s *Service) RunBackup(ctx context.Context, repoID string) (rec *models.BackupRecord, err error) {
	unlock, acquired := s.overlap.TryLock(repoID)
	if !acquired {
		return nil, fmt.Errorf("%w: repo %s", ErrBackupAlreadyRunning, repoID)
	}
	defer unlock()

	// D-19: open the backup.run span + attach terminal-state metrics.
	// s.metrics and s.tracer are set once at construction (via Options) so
	// no mutex is required on the hot path — they always hold valid values
	// (Noop* by default). Tests that swap observability mid-flight should
	// do so only from a single goroutine before calling RunBackup.
	_, finishSpan := s.tracer.Start(ctx, SpanBackupRun)
	defer func() {
		outcome := classifyOutcome(err)
		s.metrics.RecordOutcome(KindBackup, outcome)
		if outcome == OutcomeSucceeded {
			s.metrics.RecordLastSuccess(repoID, KindBackup, s.now())
		}
		finishSpan(err)
	}()

	// Bind the caller ctx to the service's serveCtx so that Stop() cancels
	// in-flight runs regardless of whether they were launched by the scheduler
	// or by an on-demand caller (D-18). If Serve has not been called yet,
	// serveCtx is nil and ctx passes through unchanged.
	runCtx, cancelRun := s.deriveRunCtx(ctx)
	defer cancelRun()

	repo, err := s.store.GetBackupRepoByID(runCtx, repoID)
	if err != nil {
		if errors.Is(err, models.ErrBackupRepoNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrRepoNotFound, repoID)
		}
		return nil, fmt.Errorf("load repo: %w", err)
	}

	source, storeID, storeKind, err := s.resolver.Resolve(runCtx, repo.TargetKind, repo.TargetID)
	if err != nil {
		return nil, err
	}

	dst, err := s.destFactory(runCtx, repo)
	if err != nil {
		return nil, fmt.Errorf("build destination: %w", err)
	}
	defer func() {
		if cerr := dst.Close(); cerr != nil {
			logger.Warn("Destination close error", "repo_id", repoID, "error", cerr)
		}
	}()

	rec, err = s.exec.RunBackup(runCtx, source, dst, repo, storeID, storeKind)
	if err != nil {
		return nil, err
	}

	// Inline retention (D-08, SCHED-06). Retention failures do NOT degrade
	// the parent job (D-15) — we log + drop the report, don't propagate.
	report, rerr := RunRetention(runCtx, repo, dst, s.store, s.clock)
	if rerr != nil {
		logger.Warn("Retention pass encountered errors", "repo_id", repoID, "error", rerr)
	}
	if len(report.FailedDeletes) > 0 {
		logger.Warn("Retention had per-record failures",
			"repo_id", repoID, "count", len(report.FailedDeletes))
	}
	logger.Info("Retention pass summary",
		"repo_id", repoID,
		"considered", report.Considered,
		"deleted", len(report.Deleted),
		"skipped_pinned", report.SkippedPinned,
		"skipped_safety", report.SkippedSafety,
		"jobs_pruned", report.JobsPruned,
	)

	return rec, nil
}

// runScheduledBackup is the JobFn registered with the scheduler. Delegates to
// RunBackup — one entrypoint, two callers (D-23). Errors are returned to the
// scheduler which logs at WARN and leaves the entry in place (D-01 policy).
//
// When serveCtx is active (Serve has been called) we pass it as the parent
// ctx so a Service.Stop cancels any in-flight executor run promptly.
func (s *Service) runScheduledBackup(ctx context.Context, targetID string) error {
	s.mu.RLock()
	runCtx := s.serveCtx
	s.mu.RUnlock()
	if runCtx == nil {
		runCtx = ctx
	}
	_, err := s.RunBackup(runCtx, targetID)
	return err
}

// deriveRunCtx returns a context that cancels when EITHER the caller ctx or
// the service's serveCtx cancels. If Serve has not been called, serveCtx is
// nil and the returned ctx is the caller ctx with a no-op cancel. The
// returned cancel MUST be called to release the watcher goroutine.
//
// This makes Service.Stop propagate into any in-flight RunBackup regardless
// of whether it was launched by the scheduler (where ctx is already serveCtx)
// or by an on-demand caller (where ctx is the request ctx and serveCtx is
// the shutdown signal we need to observe) — D-18 shutdown-cancels-in-flight.
func (s *Service) deriveRunCtx(caller context.Context) (context.Context, context.CancelFunc) {
	s.mu.RLock()
	serve := s.serveCtx
	s.mu.RUnlock()
	if serve == nil || serve == caller {
		return caller, func() {}
	}
	ctx, cancel := context.WithCancel(caller)
	// Watch serveCtx for cancellation and propagate into the derived ctx.
	stop := context.AfterFunc(serve, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// now returns the current time honoring the injected backup.Clock. Used by
// the D-19 last-success gauge hook so tests can pin a deterministic
// timestamp via WithClock.
func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

// classifyOutcome maps RunBackup / RunRestore's final error into the
// observable {succeeded, failed, interrupted} taxonomy (D-19). Kept here
// rather than in metrics.go because the classification is a property of the
// Service's error contract, not of the collector.
func classifyOutcome(err error) string {
	if err == nil {
		return OutcomeSucceeded
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return OutcomeInterrupted
	}
	return OutcomeFailed
}
