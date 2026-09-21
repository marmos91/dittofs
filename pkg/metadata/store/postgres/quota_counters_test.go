//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
)

// TestQuotaCountersSurviveConcurrentWriters is the reason PersistQuotaDelta
// increments in SQL instead of reading a bucket into Go and writing it back.
//
// Reading a bucket into Go and writing it back would put the counter on the
// transaction's own retry path: at REPEATABLE READ a concurrent update to that
// row raises 40001 and restarts the whole file transaction. The SQL-side
// increment keeps the arithmetic on the row version the statement locks. The
// in-memory cache would hide a shortfall either way — it is mutex-guarded and
// folds every delta exactly once — so the assertion has to be made against a
// store reopened from the durable rows.
//
// Files in a share commonly share one owner uid, which makes that bucket the
// whole share's write path and this the ordinary case rather than a corner.
func TestQuotaCountersSurviveConcurrentWriters(t *testing.T) {
	const (
		writers   = 8
		perWriter = 12
		fileSize  = 4096
		owner     = uint32(4242)
	)

	// The test database outlives a single run and is shared between packages, so
	// a fixed share name would accumulate the previous run's rows and read as a
	// counter that over-counts.
	shareName := fmt.Sprintf("/qc-%x", uint32(time.Now().UnixNano()))

	store := newTestStore(t)
	ctx := context.Background()

	if _, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	}); err != nil {
		t.Fatalf("CreateRootDirectory(%q): %v", shareName, err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				path := fmt.Sprintf("/w%d-f%d", w, i)
				h, err := store.GenerateHandle(ctx, shareName, path)
				if err != nil {
					errs <- err
					return
				}
				_, id, err := metadata.DecodeFileHandle(h)
				if err != nil {
					errs <- err
					return
				}
				f := &metadata.File{
					ShareName: shareName,
					Path:      path,
					FileAttr: metadata.FileAttr{
						Type:      metadata.FileTypeRegular,
						Mode:      0o600,
						UID:       owner,
						GID:       owner,
						PayloadID: metadata.PayloadID(shareName + path),
						Size:      fileSize,
					},
				}
				f.ID = id
				if err := store.UpdateAttrs(ctx, f); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent write: %v", err)
	}

	want := int64(writers * perWriter * fileSize)
	if got, err := store.GetUsedBytesForShare(ctx, shareName); err != nil || got != want {
		t.Fatalf("in-memory GetUsedBytesForShare = (%d, %v), want (%d, nil)", got, err, want)
	}

	// The durable rows are where a lost update would show. Open a second store
	// against the same database: it seeds purely from the counters.
	cfg, caps := postgresTestConfig()
	reopened, err := postgres.NewPostgresMetadataStore(ctx, cfg, caps)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	got, err := reopened.GetUsedBytesForShare(ctx, shareName)
	if err != nil {
		t.Fatalf("reopened GetUsedBytesForShare: %v", err)
	}
	if got != want {
		t.Fatalf("durable counters = %d, want %d (a short total means concurrent increments were lost)", got, want)
	}

	usage, err := reopened.GetQuotaUsage(shareName, metadata.QuotaScopeUser, owner)
	if err != nil {
		t.Fatalf("reopened GetQuotaUsage: %v", err)
	}
	if usage.Files != int64(writers*perWriter) {
		t.Fatalf("durable inode count = %d, want %d", usage.Files, writers*perWriter)
	}
}
