//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

func TestUserGrace_SetListDelete_RoundTrip(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	t0 := time.Now().UTC().Truncate(time.Second)

	if err := store.SetUserGrace(ctx, "alpha", 1000, t0); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := store.SetUserGrace(ctx, "alpha", 1001, t0.Add(time.Minute)); err != nil {
		t.Fatalf("set: %v", err)
	}
	// A row for a different share must not bleed into alpha's listing.
	if err := store.SetUserGrace(ctx, "beta", 1000, t0); err != nil {
		t.Fatalf("set beta: %v", err)
	}

	rows, err := store.ListUserGrace(ctx, "alpha")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("alpha rows = %d, want 2", len(rows))
	}
	got := map[uint32]time.Time{}
	for _, r := range rows {
		got[r.UID] = r.GraceStartedAt.UTC().Truncate(time.Second)
	}
	if !got[1000].Equal(t0) {
		t.Errorf("uid 1000 grace = %v, want %v", got[1000], t0)
	}
	if !got[1001].Equal(t0.Add(time.Minute)) {
		t.Errorf("uid 1001 grace = %v, want %v", got[1001], t0.Add(time.Minute))
	}

	// Delete one row; the other survives.
	if err := store.DeleteUserGrace(ctx, "alpha", 1000); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows, err = store.ListUserGrace(ctx, "alpha")
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(rows) != 1 || rows[0].UID != 1001 {
		t.Fatalf("after delete, rows = %+v, want only uid 1001", rows)
	}

	// Deleting a missing row is not an error (best-effort reap).
	if err := store.DeleteUserGrace(ctx, "alpha", 999); err != nil {
		t.Errorf("delete missing row: %v", err)
	}
}

func TestUserGrace_SetIsUpsert(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	t0 := time.Now().UTC().Truncate(time.Second)
	if err := store.SetUserGrace(ctx, "alpha", 1000, t0); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Re-set the same (share, uid) with a later time: refreshes, no duplicate.
	t1 := t0.Add(2 * time.Hour)
	if err := store.SetUserGrace(ctx, "alpha", 1000, t1); err != nil {
		t.Fatalf("re-set: %v", err)
	}

	rows, err := store.ListUserGrace(ctx, "alpha")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (upsert, not insert)", len(rows))
	}
	if !rows[0].GraceStartedAt.UTC().Truncate(time.Second).Equal(t1) {
		t.Errorf("grace = %v, want refreshed %v", rows[0].GraceStartedAt, t1)
	}
}

func TestUserGrace_DeleteForShare(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	t0 := time.Now().UTC()
	for _, uid := range []uint32{1000, 1001, 1002} {
		if err := store.SetUserGrace(ctx, "alpha", uid, t0); err != nil {
			t.Fatalf("set %d: %v", uid, err)
		}
	}
	if err := store.SetUserGrace(ctx, "beta", 1000, t0); err != nil {
		t.Fatalf("set beta: %v", err)
	}

	if err := store.DeleteUserGraceForShare(ctx, "alpha"); err != nil {
		t.Fatalf("delete for share: %v", err)
	}

	rows, err := store.ListUserGrace(ctx, "alpha")
	if err != nil {
		t.Fatalf("list alpha: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("alpha rows after purge = %d, want 0", len(rows))
	}
	// beta is untouched.
	rows, err = store.ListUserGrace(ctx, "beta")
	if err != nil {
		t.Fatalf("list beta: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("beta rows = %d, want 1", len(rows))
	}
}
