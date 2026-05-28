//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// mkSnap returns a minimal-fields snapshot fixture. The ID is left empty so
// CreateSnapshot generates a UUID.
func mkSnap(shareName, state string) *models.Snapshot {
	return &models.Snapshot{
		ShareName:      shareName,
		State:          state,
		MetadataEngine: "sqlite",
	}
}

func TestSnapshot_CreateGetList_RoundTrip(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	// Create 1 creating + 2 ready for share "alpha".
	creating := mkSnap("alpha", models.StateCreating)
	_, err := store.CreateSnapshot(ctx, creating)
	if err != nil {
		t.Fatalf("create creating: %v", err)
	}
	// Sleep 1ms between inserts to keep created_at ordering deterministic.
	time.Sleep(time.Millisecond)

	ready1 := mkSnap("alpha", models.StateReady)
	_, err = store.CreateSnapshot(ctx, ready1)
	if err != nil {
		t.Fatalf("create ready1: %v", err)
	}
	time.Sleep(time.Millisecond)

	ready2 := mkSnap("alpha", models.StateReady)
	_, err = store.CreateSnapshot(ctx, ready2)
	if err != nil {
		t.Fatalf("create ready2: %v", err)
	}

	// GetSnapshot round-trips ready1.
	got, err := store.GetSnapshot(ctx, "alpha", ready1.ID)
	if err != nil {
		t.Fatalf("get ready1: %v", err)
	}
	if got.ID != ready1.ID || got.ShareName != "alpha" || got.State != models.StateReady {
		t.Fatalf("get returned unexpected row: %+v", got)
	}

	// ListSnapshots returns all 3 ordered by created_at DESC.
	snaps, err := store.ListSnapshots(ctx, "alpha")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(snaps) != 3 {
		t.Fatalf("expected 3 snapshots, got %d", len(snaps))
	}
	// Most recent first: ready2, ready1, creating.
	if snaps[0].ID != ready2.ID || snaps[1].ID != ready1.ID || snaps[2].ID != creating.ID {
		t.Fatalf("DESC ordering wrong: got [%s, %s, %s]",
			snaps[0].ID, snaps[1].ID, snaps[2].ID)
	}
}

func TestSnapshot_CreateConcurrent_RejectsSecondCreating(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	first := mkSnap("beta", models.StateCreating)
	if _, err := store.CreateSnapshot(ctx, first); err != nil {
		t.Fatalf("first create: %v", err)
	}

	second := mkSnap("beta", models.StateCreating)
	_, err := store.CreateSnapshot(ctx, second)
	if !errors.Is(err, models.ErrSnapshotStateConflict) {
		t.Fatalf("expected ErrSnapshotStateConflict, got %v", err)
	}
}

func TestSnapshot_CreateMultiple_ReadyAllowed(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	// D-08: multiple ready snapshots per share are explicitly allowed.
	for i := 0; i < 5; i++ {
		snap := mkSnap("gamma", models.StateReady)
		if _, err := store.CreateSnapshot(ctx, snap); err != nil {
			t.Fatalf("ready create #%d failed: %v", i, err)
		}
	}

	snaps, err := store.ListSnapshots(ctx, "gamma")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(snaps) != 5 {
		t.Fatalf("expected 5 ready snapshots, got %d", len(snaps))
	}
}

func TestSnapshot_Get_NotFound_ReturnsSentinel(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	_, err := store.GetSnapshot(ctx, "noshare", "noid")
	if !errors.Is(err, models.ErrSnapshotNotFound) {
		t.Fatalf("expected ErrSnapshotNotFound, got %v", err)
	}
}

func TestSnapshot_Delete_RemovesRow(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	snap := mkSnap("delta", models.StateReady)
	if _, err := store.CreateSnapshot(ctx, snap); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := store.DeleteSnapshot(ctx, "delta", snap.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Subsequent Get returns ErrSnapshotNotFound.
	if _, err := store.GetSnapshot(ctx, "delta", snap.ID); !errors.Is(err, models.ErrSnapshotNotFound) {
		t.Fatalf("expected ErrSnapshotNotFound after delete, got %v", err)
	}

	// Second Delete returns ErrSnapshotNotFound.
	if err := store.DeleteSnapshot(ctx, "delta", snap.ID); !errors.Is(err, models.ErrSnapshotNotFound) {
		t.Fatalf("expected ErrSnapshotNotFound on second delete, got %v", err)
	}
}

func TestSnapshot_UpdateState_AllowedTransitions(t *testing.T) {
	cases := []struct {
		name string
		from string
		to   string
	}{
		{"creating_to_ready", models.StateCreating, models.StateReady},
		{"creating_to_failed", models.StateCreating, models.StateFailed},
		{"failed_to_creating", models.StateFailed, models.StateCreating},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := createTestStore(t)
			defer store.Close()
			ctx := context.Background()

			snap := mkSnap("epsilon-"+tc.name, tc.from)
			if _, err := store.CreateSnapshot(ctx, snap); err != nil {
				t.Fatalf("create: %v", err)
			}

			if err := store.UpdateSnapshotState(ctx, snap.ID, tc.to); err != nil {
				t.Fatalf("update %s->%s: %v", tc.from, tc.to, err)
			}

			got, err := store.GetSnapshot(ctx, snap.ShareName, snap.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.State != tc.to {
				t.Fatalf("expected state %q, got %q", tc.to, got.State)
			}
		})
	}
}

func TestSnapshot_UpdateState_RejectsDisallowed(t *testing.T) {
	cases := []struct {
		name string
		from string
		to   string
	}{
		{"ready_to_creating", models.StateReady, models.StateCreating},
		{"ready_to_failed", models.StateReady, models.StateFailed},
		{"failed_to_failed", models.StateFailed, models.StateFailed},
		{"creating_to_creating", models.StateCreating, models.StateCreating},
		{"ready_to_ready", models.StateReady, models.StateReady},
		{"failed_to_ready", models.StateFailed, models.StateReady},
		{"unknown_target", models.StateCreating, "bogus"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := createTestStore(t)
			defer store.Close()
			ctx := context.Background()

			snap := mkSnap("zeta-"+tc.name, tc.from)
			if _, err := store.CreateSnapshot(ctx, snap); err != nil {
				t.Fatalf("create: %v", err)
			}

			err := store.UpdateSnapshotState(ctx, snap.ID, tc.to)
			if !errors.Is(err, models.ErrSnapshotStateConflict) {
				t.Fatalf("expected ErrSnapshotStateConflict, got %v", err)
			}

			// State must not have changed.
			got, gerr := store.GetSnapshot(ctx, snap.ShareName, snap.ID)
			if gerr != nil {
				t.Fatalf("get: %v", gerr)
			}
			if got.State != tc.from {
				t.Fatalf("state changed despite rejection: %q -> %q", tc.from, got.State)
			}
		})
	}
}

func TestSnapshot_UpdateState_NotFound(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	err := store.UpdateSnapshotState(ctx, "nonexistent-id", models.StateReady)
	if !errors.Is(err, models.ErrSnapshotNotFound) {
		t.Fatalf("expected ErrSnapshotNotFound, got %v", err)
	}
}

func TestSnapshot_idxShareCreating_ExistsAfterMigrate(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()

	// Query sqlite_master for the partial unique index. Postgres equivalent
	// would use pg_indexes; the integration suite runs against SQLite by
	// default. Skip on non-SQLite dialects until DITTOFS_TEST_POSTGRES_DSN
	// is wired up.
	var name string
	err := store.DB().Raw(
		"SELECT name FROM sqlite_master WHERE type='index' AND name='idx_share_creating'",
	).Scan(&name).Error
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if name != "idx_share_creating" {
		t.Fatalf("idx_share_creating not installed; got name=%q", name)
	}
}
