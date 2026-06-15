package snapshotsched

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// fakeDeps is an in-memory Deps for deterministic scheduler tests.
type fakeDeps struct {
	policies  []*models.SnapshotPolicy
	byShare   map[string]*models.SnapshotPolicy
	snaps     map[string][]*models.Snapshot
	createErr error

	createdNames []string
	createdFor   []string
	touched      []string
	deleted      []string
	nextID       int
}

func (f *fakeDeps) ListPolicies(ctx context.Context) ([]*models.SnapshotPolicy, error) {
	return f.policies, nil
}

func (f *fakeDeps) GetPolicy(ctx context.Context, share string) (*models.SnapshotPolicy, error) {
	p, ok := f.byShare[share]
	if !ok {
		return nil, models.ErrSnapshotPolicyNotFound
	}
	return p, nil
}

func (f *fakeDeps) CreateScheduledSnapshot(ctx context.Context, share, name string) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.nextID++
	f.createdNames = append(f.createdNames, name)
	f.createdFor = append(f.createdFor, share)
	return fmt.Sprintf("snap-%d", f.nextID), nil
}

func (f *fakeDeps) ListSnapshots(ctx context.Context, share string) ([]*models.Snapshot, error) {
	return f.snaps[share], nil
}

func (f *fakeDeps) DeleteSnapshot(ctx context.Context, share, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeDeps) TouchPolicyRun(ctx context.Context, share string, ranAt time.Time) error {
	f.touched = append(f.touched, share)
	return nil
}

func ptr(t time.Time) *time.Time { return &t }

func newSvc(deps Deps, now time.Time) *Service {
	s := New(deps, time.Minute)
	s.now = func() time.Time { return now }
	return s
}

func TestTick_CreatesWhenNeverRun(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	deps := &fakeDeps{
		policies: []*models.SnapshotPolicy{
			{ShareName: "alpha", Enabled: true, Interval: time.Hour, NamePrefix: "scheduled"},
		},
	}
	newSvc(deps, now).tick(context.Background())

	if len(deps.createdFor) != 1 || deps.createdFor[0] != "alpha" {
		t.Fatalf("expected 1 create for alpha, got %v", deps.createdFor)
	}
	if len(deps.touched) != 1 || deps.touched[0] != "alpha" {
		t.Fatalf("expected touch for alpha, got %v", deps.touched)
	}
	if !strings.HasPrefix(deps.createdNames[0], "scheduled-") {
		t.Errorf("snapshot name = %q, want scheduled- prefix", deps.createdNames[0])
	}
}

func TestTick_SkipsWhenNotDue(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	deps := &fakeDeps{
		policies: []*models.SnapshotPolicy{
			{ShareName: "alpha", Enabled: true, Interval: time.Hour, LastRunAt: ptr(now.Add(-30 * time.Minute))},
		},
	}
	newSvc(deps, now).tick(context.Background())

	if len(deps.createdFor) != 0 {
		t.Fatalf("expected no create (not due), got %v", deps.createdFor)
	}
}

func TestTick_CreatesWhenDue(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	deps := &fakeDeps{
		policies: []*models.SnapshotPolicy{
			{ShareName: "alpha", Enabled: true, Interval: time.Hour, LastRunAt: ptr(now.Add(-2 * time.Hour))},
		},
	}
	newSvc(deps, now).tick(context.Background())

	if len(deps.createdFor) != 1 {
		t.Fatalf("expected create (due), got %v", deps.createdFor)
	}
}

func TestTick_SkipsDisabled(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	deps := &fakeDeps{
		policies: []*models.SnapshotPolicy{
			{ShareName: "alpha", Enabled: false, Interval: time.Hour},
		},
	}
	newSvc(deps, now).tick(context.Background())

	if len(deps.createdFor) != 0 {
		t.Fatalf("disabled policy must not run, got %v", deps.createdFor)
	}
}

func TestTick_InFlightConflictSkipsWithoutTouch(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	deps := &fakeDeps{
		createErr: models.ErrSnapshotStateConflict,
		policies: []*models.SnapshotPolicy{
			{ShareName: "alpha", Enabled: true, Interval: time.Hour},
		},
	}
	// Must not panic / propagate; just skip.
	newSvc(deps, now).tick(context.Background())

	if len(deps.touched) != 0 {
		t.Fatalf("conflict must not advance LastRunAt, got touched %v", deps.touched)
	}
}

func mkReady(id string, scheduled bool, age time.Duration, now time.Time) *models.Snapshot {
	return &models.Snapshot{
		ID:        id,
		ShareName: "alpha",
		State:     models.StateReady,
		Scheduled: scheduled,
		CreatedAt: now.Add(-age),
	}
}

// pruneCase drives prune() against a fixed snapshot set (newest-first, matching
// ListSnapshots' created_at DESC ordering) and returns the deleted IDs.
func pruneCase(t *testing.T, p *models.SnapshotPolicy, snaps []*models.Snapshot, now time.Time) []string {
	t.Helper()
	deps := &fakeDeps{snaps: map[string][]*models.Snapshot{"alpha": snaps}}
	newSvc(deps, now).prune(context.Background(), p)
	return deps.deleted
}

func TestPrune_KeepLastAndTTL(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	snaps := []*models.Snapshot{
		mkReady("s0", true, 1*time.Minute, now),
		mkReady("s1", true, 1*time.Hour, now),
		mkReady("s2", true, 25*time.Hour, now),
		mkReady("s3", false, 30*time.Hour, now), // manual — never pruned
		{ID: "s4", ShareName: "alpha", State: models.StateFailed, Scheduled: true, CreatedAt: now.Add(-40 * time.Hour)},
	}

	t.Run("both bounds", func(t *testing.T) {
		got := pruneCase(t, &models.SnapshotPolicy{ShareName: "alpha", KeepLast: 2, TTL: 24 * time.Hour}, snaps, now)
		if len(got) != 1 || got[0] != "s2" {
			t.Fatalf("deleted = %v, want [s2]", got)
		}
	})

	t.Run("ttl only", func(t *testing.T) {
		got := pruneCase(t, &models.SnapshotPolicy{ShareName: "alpha", KeepLast: 0, TTL: 24 * time.Hour}, snaps, now)
		if len(got) != 1 || got[0] != "s2" {
			t.Fatalf("deleted = %v, want [s2]", got)
		}
	})

	t.Run("keep only", func(t *testing.T) {
		got := pruneCase(t, &models.SnapshotPolicy{ShareName: "alpha", KeepLast: 2, TTL: 0}, snaps, now)
		if len(got) != 1 || got[0] != "s2" {
			t.Fatalf("deleted = %v, want [s2]", got)
		}
	})

	t.Run("both disabled", func(t *testing.T) {
		got := pruneCase(t, &models.SnapshotPolicy{ShareName: "alpha", KeepLast: 0, TTL: 0}, snaps, now)
		if len(got) != 0 {
			t.Fatalf("deleted = %v, want none", got)
		}
	})

	t.Run("manual never pruned", func(t *testing.T) {
		got := pruneCase(t, &models.SnapshotPolicy{ShareName: "alpha", KeepLast: 1, TTL: time.Hour}, snaps, now)
		for _, id := range got {
			if id == "s3" {
				t.Fatalf("manual snapshot s3 was pruned: %v", got)
			}
		}
	})
}

func TestRunNow(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	deps := &fakeDeps{
		byShare: map[string]*models.SnapshotPolicy{
			"alpha": {ShareName: "alpha", Enabled: true, Interval: time.Hour, NamePrefix: "scheduled", LastRunAt: ptr(now.Add(-1 * time.Minute))},
		},
	}
	id, err := newSvc(deps, now).RunNow(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if id == "" {
		t.Fatal("expected snapshot id")
	}
	if len(deps.touched) != 1 {
		t.Errorf("expected touch, got %v", deps.touched)
	}
}

func TestRunNow_NoPolicy(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	deps := &fakeDeps{byShare: map[string]*models.SnapshotPolicy{}}
	if _, err := newSvc(deps, now).RunNow(context.Background(), "alpha"); !errors.Is(err, models.ErrSnapshotPolicyNotFound) {
		t.Fatalf("RunNow missing = %v, want ErrSnapshotPolicyNotFound", err)
	}
}
