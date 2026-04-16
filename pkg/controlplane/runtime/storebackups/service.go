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
	}
	svc.sched = scheduler.NewScheduler(scheduler.WithOverlapGuard(svc.overlap))
	svc.exec = executor.New(s, nil)

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
func (s *Service) RunBackup(ctx context.Context, repoID string) (*models.BackupRecord, error) {
	unlock, acquired := s.overlap.TryLock(repoID)
	if !acquired {
		return nil, fmt.Errorf("%w: repo %s", ErrBackupAlreadyRunning, repoID)
	}
	defer unlock()

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

	rec, err := s.exec.RunBackup(runCtx, source, dst, repo, storeID, storeKind)
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
