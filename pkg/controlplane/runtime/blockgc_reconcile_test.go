package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// seedReconcileFB seeds n file_blocks rows for payloadID with the given
// CreatedAt (controls grace gating).
func seedReconcileFB(t *testing.T, ctx context.Context, store metadata.Store, payloadID string, n int, created time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		b := &block.FileBlock{
			ID:         fmt.Sprintf("%s/%d", payloadID, i),
			State:      block.BlockStatePending,
			LocalPath:  fmt.Sprintf("/cache/%s-%d", payloadID, i),
			DataSize:   128,
			RefCount:   1,
			LastAccess: created,
			CreatedAt:  created,
		}
		if err := store.Put(ctx, b); err != nil {
			t.Fatalf("Put(%s): %v", b.ID, err)
		}
	}
}

// TestReapStrandedRows verifies the reconcile reaps file_blocks rows for
// payloads with no live inode, leaves live payloads alone, and respects the
// grace window.
func TestReapStrandedRows(t *testing.T) {
	ctx := context.Background()
	store := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	old := time.Now().Add(-2 * time.Hour)

	// A live inode referencing payload "live".
	const livePID = "live"
	h, err := store.GenerateHandle(ctx, "share", "/live.bin")
	if err != nil {
		t.Fatalf("GenerateHandle: %v", err)
	}
	_, id, err := metadata.DecodeFileHandle(h)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}
	if err := store.PutFile(ctx, &metadata.File{
		ID:        id,
		ShareName: "share",
		Path:      "/live.bin",
		FileAttr: metadata.FileAttr{
			Type:      metadata.FileTypeRegular,
			Mode:      0o644,
			PayloadID: metadata.PayloadID(livePID),
		},
	}); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	seedReconcileFB(t, ctx, store, livePID, 2, old)        // live, must survive
	seedReconcileFB(t, ctx, store, "stranded", 3, old)     // stranded, must reap
	seedReconcileFB(t, ctx, store, "fresh", 1, time.Now()) // stranded but within grace

	rt := &Runtime{}
	graceCutoff := time.Now().Add(-time.Hour)
	reaped, err := rt.reapStrandedRows(ctx, "share", store, graceCutoff, false)
	if err != nil {
		t.Fatalf("reapStrandedRows: %v", err)
	}
	if reaped != 3 {
		t.Errorf("reaped = %d, want 3 (stranded rows only)", reaped)
	}

	// Live payload rows must survive.
	if rows, err := store.ListFileBlocks(ctx, livePID); err != nil || len(rows) != 2 {
		t.Errorf("live rows = %d (err=%v), want 2 (reconcile must not reap live)", len(rows), err)
	}
	// Stranded payload rows must be gone.
	if rows, err := store.ListFileBlocks(ctx, "stranded"); err != nil || len(rows) != 0 {
		t.Errorf("stranded rows = %d (err=%v), want 0", len(rows), err)
	}
	// Within-grace stranded rows must be preserved (TOCTOU guard).
	if rows, err := store.ListFileBlocks(ctx, "fresh"); err != nil || len(rows) != 1 {
		t.Errorf("fresh (in-grace) rows = %d (err=%v), want 1 (grace must protect)", len(rows), err)
	}
}

// TestReapStrandedRows_DryRun verifies dry-run counts but does not reap.
func TestReapStrandedRows_DryRun(t *testing.T) {
	ctx := context.Background()
	store := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	old := time.Now().Add(-2 * time.Hour)
	seedReconcileFB(t, ctx, store, "stranded", 4, old)

	rt := &Runtime{}
	reaped, err := rt.reapStrandedRows(ctx, "share", store, time.Now(), true)
	if err != nil {
		t.Fatalf("reapStrandedRows: %v", err)
	}
	if reaped != 4 {
		t.Errorf("dry-run reaped count = %d, want 4", reaped)
	}
	if rows, err := store.ListFileBlocks(ctx, "stranded"); err != nil || len(rows) != 4 {
		t.Errorf("dry-run deleted rows: got %d, want 4 preserved", len(rows))
	}
}
