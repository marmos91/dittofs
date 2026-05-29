//go:build integration

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func TestRestoreMarker_PutGetDelete_RoundTrip(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	// Get on an absent marker → ErrRestoreMarkerNotFound.
	if _, err := store.GetRestoreMarker(ctx, "alpha"); !errors.Is(err, models.ErrRestoreMarkerNotFound) {
		t.Fatalf("Get absent: err = %v, want ErrRestoreMarkerNotFound", err)
	}

	m := &models.RestoreMarker{
		ShareName:        "alpha",
		TargetSnapshotID: "target-1",
		SafetySnapshotID: "safety-1",
		Step:             models.RestoreStepStarted,
	}
	if err := store.PutRestoreMarker(ctx, m); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := store.GetRestoreMarker(ctx, "alpha")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TargetSnapshotID != "target-1" || got.SafetySnapshotID != "safety-1" || got.Step != models.RestoreStepStarted {
		t.Fatalf("Get returned unexpected row: %+v", got)
	}

	// Delete → absent again.
	if err := store.DeleteRestoreMarker(ctx, "alpha"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.GetRestoreMarker(ctx, "alpha"); !errors.Is(err, models.ErrRestoreMarkerNotFound) {
		t.Fatalf("Get after delete: err = %v, want ErrRestoreMarkerNotFound", err)
	}

	// Delete of an absent marker is idempotent (no error).
	if err := store.DeleteRestoreMarker(ctx, "alpha"); err != nil {
		t.Fatalf("Delete idempotent: %v", err)
	}
}

func TestRestoreMarker_Put_UpsertsStep(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	first := &models.RestoreMarker{
		ShareName:        "beta",
		TargetSnapshotID: "t",
		SafetySnapshotID: "s",
		Step:             models.RestoreStepStarted,
	}
	if err := store.PutRestoreMarker(ctx, first); err != nil {
		t.Fatalf("Put first: %v", err)
	}

	// Re-put for the same share with a later step → overwrites, no error.
	second := &models.RestoreMarker{
		ShareName:        "beta",
		TargetSnapshotID: "t",
		SafetySnapshotID: "s",
		Step:             models.RestoreStepMetaReset,
	}
	if err := store.PutRestoreMarker(ctx, second); err != nil {
		t.Fatalf("Put second (upsert): %v", err)
	}

	got, err := store.GetRestoreMarker(ctx, "beta")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Step != models.RestoreStepMetaReset {
		t.Fatalf("Step = %q, want %q after upsert", got.Step, models.RestoreStepMetaReset)
	}
}

func TestRestoreMarker_List(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	// Empty → empty slice, not nil.
	markers, err := store.ListRestoreMarkers(ctx)
	if err != nil {
		t.Fatalf("List empty: %v", err)
	}
	if markers == nil {
		t.Fatal("List returned nil, want non-nil empty slice")
	}
	if len(markers) != 0 {
		t.Fatalf("List empty: len = %d, want 0", len(markers))
	}

	for _, share := range []string{"one", "two", "three"} {
		if err := store.PutRestoreMarker(ctx, &models.RestoreMarker{
			ShareName:        share,
			TargetSnapshotID: "t-" + share,
			SafetySnapshotID: "s-" + share,
			Step:             models.RestoreStepStarted,
		}); err != nil {
			t.Fatalf("Put %s: %v", share, err)
		}
	}

	markers, err = store.ListRestoreMarkers(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(markers) != 3 {
		t.Fatalf("List: len = %d, want 3", len(markers))
	}
}
