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
		now:      time.Now,
	}
}

// Start launches the scheduler goroutine. It ticks at the poll interval until
// ctx is cancelled or Stop is called.
func (s *Service) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stopCh:
				return
			case <-ticker.C:
				s.tick(ctx)
			}
		}
	}()
}

// Stop signals the scheduler goroutine to exit. Idempotent.
func (s *Service) Stop() {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
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
