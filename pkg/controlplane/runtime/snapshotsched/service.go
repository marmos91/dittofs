// Package snapshotsched implements the background snapshot scheduler: a
// process-local ticker that creates per-share snapshots on the cadence defined
// by each share's SnapshotPolicy and prunes scheduler-created snapshots beyond
// the policy's retention bounds (keep_last and TTL).
//
// It mirrors the recycle-bin reaper (pkg/controlplane/runtime/trash): a single
// goroutine ticking at a poll interval, a narrow Deps interface for
// testability, and an idempotent Stop. Single-node only — there is no
// cross-process coordination; the scheduler runs in whichever process owns the
// control plane.
package snapshotsched

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// defaultPollInterval is the scheduler cadence used when New is given a zero
// interval. One minute is fine-grained enough for hour/day cadences while a
// pass over a policy-free fleet is a cheap no-op (one ListPolicies query).
const defaultPollInterval = time.Minute

// defaultNamePrefix labels scheduler-created snapshots when a policy leaves
// NamePrefix empty.
const defaultNamePrefix = "scheduled"

// Deps is the narrow runtime surface the scheduler needs, kept as an interface
// so the service is testable without a full Runtime. The create signature uses
// primitives (not runtime.CreateSnapshotOpts) to avoid an import cycle — the
// runtime adapter sets Scheduled=true.
type Deps interface {
	// ListPolicies returns every snapshot policy across all shares.
	ListPolicies(ctx context.Context) ([]*models.SnapshotPolicy, error)
	// GetPolicy returns the policy for a single share, or
	// models.ErrSnapshotPolicyNotFound when absent.
	GetPolicy(ctx context.Context, share string) (*models.SnapshotPolicy, error)
	// CreateScheduledSnapshot creates a snapshot marked Scheduled=true and
	// returns its ID. Returns models.ErrSnapshotStateConflict when one is
	// already in flight for the share.
	CreateScheduledSnapshot(ctx context.Context, share, name string) (string, error)
	// ListSnapshots returns all snapshots for a share, newest-first.
	ListSnapshots(ctx context.Context, share string) ([]*models.Snapshot, error)
	// DeleteSnapshot removes a snapshot (row + on-disk dir).
	DeleteSnapshot(ctx context.Context, share, id string) error
	// TouchPolicyRun records that the policy ran at ranAt.
	TouchPolicyRun(ctx context.Context, share string, ranAt time.Time) error
}

// Service is the background snapshot scheduler.
type Service struct {
	deps     Deps
	interval time.Duration
	stopCh   chan struct{}
	// stopOnce closes stopCh exactly once. Stop is reachable from the lifecycle
	// drain and from a test tearing a runtime down, and a check-then-close lets
	// two callers both find the channel open and both close it, which panics.
	stopOnce sync.Once
	// started guards the goroutine against a second Start and tells Stop
	// whether there is anything to wait for.
	started atomic.Bool
	// stopped is closed by the scheduler goroutine as it returns, so Stop can
	// wait for a tick that is already inside the store rather than only
	// signalling it.
	stopped chan struct{}
	// cancelTick aborts a tick already inside the store. stopCh only tells the
	// loop not to start another one, and on the API-error path the context the
	// ticks run under is still live — so without this, Stop's wait is something
	// the caller has to abandon rather than a join that completes.
	// cancelMu guards cancelTick. started is atomic, but the cancel it implies
	// is an ordinary pointer write — a Stop racing a Start would read it with no
	// synchronization at all, which is a data race and can see nil.
	cancelMu   sync.Mutex
	cancelTick context.CancelFunc
	// now is the clock, overridable in tests for deterministic due/prune.
	now func() time.Time
}

// New constructs the scheduler. A zero pollInterval defaults to one minute.
// Call Start to launch the background goroutine.
func New(deps Deps, pollInterval time.Duration) *Service {
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	return &Service{
		deps:     deps,
		interval: pollInterval,
		stopCh:   make(chan struct{}),
		stopped:  make(chan struct{}),
		now:      time.Now,
	}
}

// Start launches the scheduler goroutine. It ticks at the poll interval until
// ctx is cancelled or Stop is called. A second Start is a no-op.
func (s *Service) Start(ctx context.Context) {
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	// Derived, so Stop can cancel the ticks without disturbing the caller's
	// context — which on a startup or API error is still very much alive.
	tickCtx, cancelTick := context.WithCancel(ctx)
	s.cancelMu.Lock()
	s.cancelTick = cancelTick
	s.cancelMu.Unlock()

	go func() {
		defer close(s.stopped)
		defer cancelTick()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-tickCtx.Done():
				return
			case <-s.stopCh:
				return
			case <-ticker.C:
				// Re-checked, because a select with two ready cases picks
				// uniformly: a tick due in the same instant Stop is requested
				// has an even chance of winning, and then creates or prunes a
				// snapshot during a shutdown that is already closing the
				// stores under it.
				select {
				case <-tickCtx.Done():
					return
				case <-s.stopCh:
					return
				default:
				}
				s.tick(tickCtx)
			}
		}
	}()
}

// Stop signals the scheduler goroutine to exit and waits for it, without a
// deadline. Kept as the source-compatible entry point: this is an exported
// method on a pkg/ type, so changing its signature breaks every downstream
// caller that compiled against it, and only the callers in this repository
// could have been updated.
//
// It waits unbounded, so it is the wrong call for a caller that is about to
// close the store the ticks read — use StopContext there, which bounds the join
// and says whether it completed.
func (s *Service) Stop() {
	s.StopContext(context.Background())
}

// StopContext signals the scheduler goroutine to exit and waits for it, bounded
// by ctx. It reports whether the wait actually completed.
//
// Waiting is the point: a tick reads and writes snapshot policies through the
// control-plane store, so a caller that is about to close that store needs
// "stopped" to mean the tick has finished, not merely that it was asked to.
// When ctx expires first this returns false, and the caller has been told
// plainly that the guarantee does not hold on that path rather than being left
// to infer it from a comment.
//
// A scheduler that was never started returns immediately. Idempotent, and safe
// for concurrent callers.
func (s *Service) StopContext(ctx context.Context) bool {
	s.stopOnce.Do(func() { close(s.stopCh) })
	// Cancelled as well as signalled: stopCh stops the loop starting another
	// tick, and this one ends a tick already inside the store, so the wait below
	// is a join that completes rather than one the caller abandons.
	s.cancelMu.Lock()
	cancelTick := s.cancelTick
	s.cancelMu.Unlock()
	if cancelTick != nil {
		cancelTick()
	}
	if !s.started.Load() {
		return true
	}
	select {
	case <-s.stopped:
		return true
	case <-ctx.Done():
		// Both channels can be ready at once — a deadline that expires in the
		// same instant the last tick finishes — and a select with two ready
		// cases picks uniformly, so arriving here does not mean the join was
		// lost. Re-check before saying so: reporting a completed join as
		// incomplete makes the caller warn that a tick is still running against
		// stores it is about to close, which is the one thing this return value
		// exists to tell it.
		select {
		case <-s.stopped:
			return true
		default:
		}
		// Reported by the caller, not here: every caller already logs on a
		// false return, and it knows what the lost join means for what it is
		// about to do — close the stores, or abandon a boot. Logging in both
		// places produced two warnings for one timeout.
		return false
	}
}

// tick runs one scheduling pass: create-then-prune for every enabled policy
// whose interval has elapsed. Per-policy errors are logged, never propagated,
// so one bad share cannot stall the others; the next tick retries.
func (s *Service) tick(ctx context.Context) {
	policies, err := s.deps.ListPolicies(ctx)
	if err != nil {
		logger.Warn("Snapshot scheduler: list policies failed", "error", err)
		return
	}
	now := s.now()
	for _, p := range policies {
		if !p.Enabled {
			continue
		}
		if !due(p, now) {
			continue
		}
		s.runPolicy(ctx, p, now)
	}
}

// RunNow creates a snapshot for the share's policy immediately, ignoring the
// interval, then prunes. It is the manual override behind the policy "run"
// endpoint. Returns models.ErrSnapshotPolicyNotFound when the share has no
// policy, or models.ErrSnapshotStateConflict when one is already in flight.
func (s *Service) RunNow(ctx context.Context, share string) (string, error) {
	p, err := s.deps.GetPolicy(ctx, share)
	if err != nil {
		return "", err
	}
	now := s.now()
	id, err := s.deps.CreateScheduledSnapshot(ctx, share, snapshotName(p, now))
	if err != nil {
		return "", err
	}
	if terr := s.deps.TouchPolicyRun(ctx, share, now); terr != nil {
		logger.Warn("Snapshot scheduler: touch run failed", "share", share, "error", terr)
	}
	s.prune(ctx, p)
	return id, nil
}

// due reports whether the policy's interval has elapsed since its last run.
// A policy that has never run is always due.
func due(p *models.SnapshotPolicy, now time.Time) bool {
	if p.LastRunAt == nil {
		return true
	}
	return now.Sub(*p.LastRunAt) >= p.Interval
}

// runPolicy creates one snapshot for the policy and prunes. An in-flight
// conflict is a benign skip (a snapshot is already being made) and does NOT
// advance LastRunAt, so the next tick retries.
func (s *Service) runPolicy(ctx context.Context, p *models.SnapshotPolicy, now time.Time) {
	_, err := s.deps.CreateScheduledSnapshot(ctx, p.ShareName, snapshotName(p, now))
	if err != nil {
		if errors.Is(err, models.ErrSnapshotStateConflict) {
			logger.Debug("Snapshot scheduler: snapshot already in flight, skipping", "share", p.ShareName)
			return
		}
		logger.Warn("Snapshot scheduler: create failed", "share", p.ShareName, "error", err)
		return
	}
	if terr := s.deps.TouchPolicyRun(ctx, p.ShareName, now); terr != nil {
		logger.Warn("Snapshot scheduler: touch run failed", "share", p.ShareName, "error", terr)
	}
	s.prune(ctx, p)
}

// prune deletes scheduler-created ready snapshots that exceed the policy's
// retention bounds: a snapshot is removed when it falls outside the newest
// KeepLast (when KeepLast>0) OR is older than TTL (when TTL>0). A zero bound
// disables that dimension; with both zero nothing is pruned. Manual snapshots
// (Scheduled=false) and non-ready snapshots are never touched.
func (s *Service) prune(ctx context.Context, p *models.SnapshotPolicy) {
	if p.KeepLast <= 0 && p.TTL <= 0 {
		return
	}
	snaps, err := s.deps.ListSnapshots(ctx, p.ShareName)
	if err != nil {
		logger.Warn("Snapshot scheduler: list snapshots failed", "share", p.ShareName, "error", err)
		return
	}
	now := s.now()
	rank := 0 // position among scheduler-created ready snapshots, newest-first
	for _, snap := range snaps {
		if !snap.Scheduled || snap.State != models.StateReady {
			continue
		}
		overCount := p.KeepLast > 0 && rank >= p.KeepLast
		overAge := p.TTL > 0 && now.Sub(snap.CreatedAt) > p.TTL
		rank++
		if !overCount && !overAge {
			continue
		}
		if derr := s.deps.DeleteSnapshot(ctx, p.ShareName, snap.ID); derr != nil {
			logger.Warn("Snapshot scheduler: prune delete failed", "share", p.ShareName, "id", snap.ID, "error", derr)
		}
	}
}

// snapshotName builds the label for a scheduler-created snapshot:
// "<prefix>-<UTC timestamp>".
func snapshotName(p *models.SnapshotPolicy, now time.Time) string {
	prefix := p.NamePrefix
	if prefix == "" {
		prefix = defaultNamePrefix
	}
	return prefix + "-" + now.UTC().Format("20060102-150405")
}
