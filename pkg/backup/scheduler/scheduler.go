package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	cron "github.com/robfig/cron/v3"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// Target is the minimum shape the scheduler needs to fire a backup tick.
// storebackups.Service adapts *models.BackupRepo → Target (Wave 4 Plan 05).
// Keeping this interface store-agnostic preserves D-24's reusability contract
// so a future block-store-backup milestone can register BlockStoreTarget
// without refactoring the scheduler.
type Target interface {
	// ID returns the stable identity used for jitter hashing + overlap keying.
	ID() string
	// Schedule returns a cron expression, optionally "CRON_TZ=..." prefixed.
	Schedule() string
}

// JobFn is invoked for each scheduled tick that passes the overlap guard.
// ctx is derived from the scheduler's lifecycle ctx and cancels on Stop.
// Errors are logged by the scheduler but do NOT remove the entry — next
// tick fires normally.
type JobFn func(ctx context.Context, targetID string) error

// repoEntry tracks an installed cron entry alongside its jitter offset so
// Unregister can locate and remove it.
type repoEntry struct {
	entryID cron.EntryID
	target  Target
	offset  time.Duration
}

// Scheduler wraps robfig/cron/v3 with per-repo FNV-1a jitter (D-03) and
// a per-repo overlap guard (D-07).
//
// Lifecycle:
//
//	s := NewScheduler(opts...)
//	s.SetJobFn(run)
//	s.Register(target)   // may be called before or after Start
//	s.Start()
//	...
//	s.Stop(ctx)          // cancels in-flight runs per D-18
//
// Overlap contract (D-07): a tick that finds the per-repo mutex held by a
// still-running prior run is skipped with a WARN log, NOT enqueued. On-demand
// backup callers (Plan 05 storebackups.Service) inject a shared OverlapGuard
// via WithOverlapGuard so both paths contend the same mutex (D-23).
//
// Missed-run policy (D-01): matches the robfig/cron/v3 default — when the
// server was down across a scheduled tick, that tick is dropped. Next run
// fires on the normal next cron occurrence. No fire-once, no fire-all.
type Scheduler struct {
	mu      sync.RWMutex
	entries map[string]*repoEntry // keyed by target.ID()
	cron    *cron.Cron
	overlap *OverlapGuard

	jobFn  JobFn
	maxJit time.Duration

	// runCtx is the parent context for all in-flight fire() invocations.
	// Created lazily by Start and cancelled by Stop so that fires currently
	// sleeping on their jitter offset abort promptly (D-18).
	runCtx    context.Context
	runCancel context.CancelFunc
}

// Option configures a Scheduler.
type Option func(*Scheduler)

// WithMaxJitter sets the jitter window. Zero disables jitter.
func WithMaxJitter(d time.Duration) Option {
	return func(s *Scheduler) { s.maxJit = d }
}

// WithOverlapGuard injects a pre-constructed OverlapGuard. The storebackups
// Service shares the same guard between the scheduler (cron path) and the
// on-demand RunBackup path (D-23) so both contend the same mutex.
func WithOverlapGuard(g *OverlapGuard) Option {
	return func(s *Scheduler) { s.overlap = g }
}

// NewScheduler constructs a Scheduler with a robfig/cron parser that accepts
// 5-field expressions (and the "CRON_TZ=..." prefix). Call SetJobFn before
// Register or the scheduler will log-and-skip ticks.
func NewScheduler(opts ...Option) *Scheduler {
	// Default parser = 5-field cron with CRON_TZ support (robfig default).
	c := cron.New()

	s := &Scheduler{
		entries: make(map[string]*repoEntry),
		cron:    c,
		maxJit:  DefaultMaxJitter,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.overlap == nil {
		s.overlap = NewOverlapGuard()
	}
	return s
}

// SetJobFn sets the function invoked per scheduled tick. Safe to call after
// Start; subsequent ticks will observe the new function.
func (s *Scheduler) SetJobFn(fn JobFn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobFn = fn
}

// Register schedules target. Returns a wrapped ErrScheduleInvalid if the
// target's Schedule() does not parse. Idempotent on equal (ID, schedule):
// re-registering the same pair is a no-op; re-registering with a DIFFERENT
// schedule removes the old entry first (caller-level UpdateRepo =
// Unregister+Register per D-22).
//
// A nil target returns ErrRepoNotFound.
func (s *Scheduler) Register(target Target) error {
	if target == nil {
		return fmt.Errorf("%w: nil target", models.ErrRepoNotFound)
	}
	id := target.ID()
	schedule := target.Schedule()
	if err := ValidateSchedule(schedule); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.entries[id]; ok {
		// No-op if schedule unchanged.
		if existing.target.Schedule() == schedule {
			return nil
		}
		s.cron.Remove(existing.entryID)
		delete(s.entries, id)
	}

	offset := PhaseOffset(id, s.maxJit)

	entryID, err := s.cron.AddFunc(schedule, func() {
		s.fire(id, offset)
	})
	if err != nil {
		// Should not happen — ValidateSchedule already parsed — but belt-and-suspenders.
		return wrapScheduleError(schedule, err)
	}

	s.entries[id] = &repoEntry{
		entryID: entryID,
		target:  target,
		offset:  offset,
	}
	logger.Info("Scheduled backup registered",
		"repo_id", id,
		"schedule", schedule,
		"offset_seconds", int64(offset.Seconds()))
	return nil
}

// Unregister removes the target's cron entry. No-op if target is not registered.
func (s *Scheduler) Unregister(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[id]
	if !ok {
		return
	}
	s.cron.Remove(entry.entryID)
	delete(s.entries, id)
	logger.Info("Scheduled backup unregistered", "repo_id", id)
}

// IsRegistered reports whether id has a cron entry.
func (s *Scheduler) IsRegistered(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.entries[id]
	return ok
}

// Registered returns a snapshot of currently-registered target IDs.
func (s *Scheduler) Registered() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.entries))
	for id := range s.entries {
		out = append(out, id)
	}
	return out
}

// Start begins firing ticks. Returns immediately. Safe to call before or after
// Register. Idempotent — a second Start before Stop is a no-op.
func (s *Scheduler) Start() {
	s.mu.Lock()
	if s.runCtx == nil {
		s.runCtx, s.runCancel = context.WithCancel(context.Background())
	}
	s.mu.Unlock()
	s.cron.Start()
}

// Stop halts the scheduler. In-flight JobFn invocations observe ctx
// cancellation via their derived context and return early per D-18.
// Stop does NOT wait for JobFns that are already running — the caller's
// ctx expires independently. The returned error is never non-nil in
// v0.13.0; reserved for future wait-for-drain semantics.
//
// Idempotent: safe to call before Start or multiple times after.
func (s *Scheduler) Stop(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.runCancel
	s.runCancel = nil
	s.runCtx = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// cron.Stop returns a ctx that closes when running jobs finish; we
	// intentionally do NOT wait on it (D-18). Consume it to avoid leak.
	_ = s.cron.Stop()
	return nil
}

// fire is the cron callback bound at Register time. It sleeps the per-repo
// offset, then attempts to acquire the overlap guard and dispatch JobFn.
//
// Unexported but invoked directly from tests in the same package to bypass
// wall-clock cron firing (see scheduler_test.go TestScheduler_OverlapUnderLoad).
// There is no production path that reaches fire() outside the cron callback.
func (s *Scheduler) fire(targetID string, offset time.Duration) {
	s.mu.RLock()
	runCtx := s.runCtx
	fn := s.jobFn
	s.mu.RUnlock()

	if runCtx == nil {
		// Stop was called between cron firing and this goroutine running,
		// or Start was never called — nothing to do.
		return
	}
	if fn == nil {
		logger.Warn("Scheduler fired but no JobFn set", "repo_id", targetID)
		return
	}

	if offset > 0 {
		select {
		case <-time.After(offset):
		case <-runCtx.Done():
			return
		}
	}

	unlock, ok := s.overlap.TryLock(targetID)
	if !ok {
		logger.Warn("Scheduled backup skipped — prior run still in flight", "repo_id", targetID)
		return
	}
	defer unlock()

	if err := fn(runCtx, targetID); err != nil {
		logger.Warn("Scheduled backup returned error",
			"repo_id", targetID,
			"error", err)
		// D-01 + policy: do NOT remove the entry. Next tick fires normally.
	}
}
