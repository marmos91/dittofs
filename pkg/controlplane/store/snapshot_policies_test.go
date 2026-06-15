//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func mkPolicy(share string) *models.SnapshotPolicy {
	return &models.SnapshotPolicy{
		ShareName:  share,
		Enabled:    true,
		Interval:   24 * time.Hour,
		KeepLast:   7,
		TTL:        0,
		NamePrefix: "scheduled",
	}
}

func TestSnapshotPolicy_UpsertGet_RoundTrip(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	p := mkPolicy("alpha")
	if err := store.UpsertSnapshotPolicy(ctx, p); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if p.ID == "" {
		t.Fatal("expected generated ID on insert")
	}

	got, err := store.GetSnapshotPolicy(ctx, "alpha")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ShareName != "alpha" || got.Interval != 24*time.Hour || got.KeepLast != 7 || !got.Enabled || got.NamePrefix != "scheduled" {
		t.Fatalf("unexpected policy: %+v", got)
	}
}

func TestSnapshotPolicy_Get_NotFound(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	if _, err := store.GetSnapshotPolicy(ctx, "missing"); !errors.Is(err, models.ErrSnapshotPolicyNotFound) {
		t.Fatalf("get missing = %v, want ErrSnapshotPolicyNotFound", err)
	}
}

func TestSnapshotPolicy_Upsert_OverwritesConfigPreservesLastRunAndID(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	p := mkPolicy("alpha")
	if err := store.UpsertSnapshotPolicy(ctx, p); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	firstID := p.ID

	// Record a run, then upsert new config for the same share.
	ranAt := time.Now().Truncate(time.Second)
	if err := store.TouchSnapshotPolicyRun(ctx, "alpha", ranAt); err != nil {
		t.Fatalf("touch: %v", err)
	}

	p2 := mkPolicy("alpha")
	p2.Interval = 6 * time.Hour
	p2.KeepLast = 3
	p2.TTL = 48 * time.Hour
	p2.Enabled = false
	if err := store.UpsertSnapshotPolicy(ctx, p2); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, err := store.GetSnapshotPolicy(ctx, "alpha")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != firstID {
		t.Errorf("ID changed on upsert: %q -> %q", firstID, got.ID)
	}
	if got.Interval != 6*time.Hour || got.KeepLast != 3 || got.TTL != 48*time.Hour || got.Enabled {
		t.Errorf("config not updated: %+v", got)
	}
	if got.LastRunAt == nil || !got.LastRunAt.Equal(ranAt) {
		t.Errorf("LastRunAt not preserved across upsert: got %v want %v", got.LastRunAt, ranAt)
	}
}

func TestSnapshotPolicy_List(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	empty, err := store.ListSnapshotPolicies(ctx)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if empty == nil {
		t.Fatal("list returned nil, want empty slice")
	}
	if len(empty) != 0 {
		t.Fatalf("len = %d, want 0", len(empty))
	}

	if err := store.UpsertSnapshotPolicy(ctx, mkPolicy("alpha")); err != nil {
		t.Fatalf("upsert alpha: %v", err)
	}
	if err := store.UpsertSnapshotPolicy(ctx, mkPolicy("beta")); err != nil {
		t.Fatalf("upsert beta: %v", err)
	}

	got, err := store.ListSnapshotPolicies(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestSnapshotPolicy_Delete(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	if err := store.UpsertSnapshotPolicy(ctx, mkPolicy("alpha")); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.DeleteSnapshotPolicy(ctx, "alpha"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.DeleteSnapshotPolicy(ctx, "alpha"); !errors.Is(err, models.ErrSnapshotPolicyNotFound) {
		t.Fatalf("second delete = %v, want ErrSnapshotPolicyNotFound", err)
	}
}

func TestSnapshotPolicy_TouchRun_NotFound(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	if err := store.TouchSnapshotPolicyRun(ctx, "missing", time.Now()); !errors.Is(err, models.ErrSnapshotPolicyNotFound) {
		t.Fatalf("touch missing = %v, want ErrSnapshotPolicyNotFound", err)
	}
}

func TestSnapshotPolicy_CascadesOnShareDelete(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	if _, err := store.CreateShare(ctx, &models.Share{
		Name:              "alpha",
		MetadataStoreID:   "m",
		LocalBlockStoreID: "b",
	}); err != nil {
		t.Fatalf("create share: %v", err)
	}
	if err := store.UpsertSnapshotPolicy(ctx, mkPolicy("alpha")); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}

	if err := store.DeleteShare(ctx, "alpha"); err != nil {
		t.Fatalf("delete share: %v", err)
	}
	if _, err := store.GetSnapshotPolicy(ctx, "alpha"); !errors.Is(err, models.ErrSnapshotPolicyNotFound) {
		t.Fatalf("policy survived share delete: err = %v", err)
	}
}
