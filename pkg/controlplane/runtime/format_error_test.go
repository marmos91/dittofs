package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// futureFormatStore bubbles a caller-supplied sentinel from
// CreateRootDirectory, the metadata call AddShare's prepareShare invokes
// first — the wrapper pattern the snapshot fixtures use (embedded
// MemoryMetadataStore, one method overridden).
type futureFormatStore struct {
	*metadatamemory.MemoryMetadataStore
	sentinel error
}

func (s *futureFormatStore) CreateRootDirectory(ctx context.Context, shareName string, attr *metadata.FileAttr) (*metadata.File, error) {
	return nil, s.sentinel
}

// TestLoadSharesFromStore_FormatErrorStops pins the boot-stop contract: a
// share whose AddShare path bubbles a format sentinel must be surfaced by
// LoadSharesFromStore (return the error), not warn-and-skipped — the OR
// branch in LoadSharesFromStore exists so cmd/dfs/commands/start.go can exit
// 78 with the operator directive. Covers both sentinels separately so a
// regression flipping the OR to block-only cannot silently downgrade a
// future journal directory to warn-and-skip.
func TestLoadSharesFromStore_FormatErrorStops(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sentinel error
	}{
		{"block", block.ErrFutureFormat},
		{"journal", journal.ErrFutureFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, s := setupTestRuntime(t)
			ctx := context.Background()

			// Register the broken store under the name the DB config carries;
			// setupTestRuntime already registered a healthy "test-meta", so the
			// broken store gets its own name and the share row references it.
			metaName := "format-meta-" + tc.name
			if _, err := s.CreateMetadataStore(ctx, &models.MetadataStoreConfig{
				Name: metaName,
				Type: "memory",
			}); err != nil {
				t.Fatalf("CreateMetadataStore: %v", err)
			}
			if err := rt.RegisterMetadataStore(metaName, &futureFormatStore{
				MemoryMetadataStore: metadatamemory.NewMemoryMetadataStoreWithDefaults(),
				sentinel:            tc.sentinel,
			}); err != nil {
				t.Fatalf("RegisterMetadataStore: %v", err)
			}

			// Persist a share row referencing test-meta; LoadSharesFromStore
			// resolves it and AddShare bubbles the sentinel from
			// CreateRootDirectory.
			share := &models.Share{
				Name: "/future-format-" + tc.name,
			}
			share.MetadataStoreID = "format-meta-" + tc.name
			if _, err := s.CreateShare(ctx, share); err != nil {
				t.Fatalf("CreateShare: %v", err)
			}

			err := LoadSharesFromStore(ctx, rt, s)
			if err == nil {
				t.Fatalf("LoadSharesFromStore returned nil on %v; want surfaced error", tc.sentinel)
			}
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("surfaced error = %v; want errors.Is %v", err, tc.sentinel)
			}
			// The share was never added: nothing is serving.
			if rt.ShareExists("/future-format-" + tc.name) {
				t.Fatalf("share %s must not be registered after a surfaced format error", share.Name)
			}
		})
	}
}
