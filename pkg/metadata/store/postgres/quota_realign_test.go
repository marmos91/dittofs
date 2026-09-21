//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
)

// TestRecomputeUsageCannotSplitACommitFromItsFold pins the ordering between a
// realign and a committing writer.
//
// A commit and the fold of its delta into the usage cache are two steps. A
// realign that re-derives between them aggregates rows that already include the
// transaction and then has that delta added on top, so the cache over-counts by
// one transaction permanently: nothing re-derives it again. The cache is what
// answers every quota check, so the error surfaces as writes wrongly refused
// for being over quota, and the durable rows stay correct throughout — which is
// what makes it invisible to a test that only reopens the store.
//
// The gap is held open through a test seam rather than raced for. A version of
// this that merely ran writers alongside realigns passed against the unguarded
// code: the window is one goroutine being descheduled across a database round
// trip, which does not happen by chance.
//
// Both SQL backends carry this ordering and both were measured to lose it
// without the guard, over-counting by exactly the raced transaction. The
// sqlite copy of this test runs without a database server; this one pins the
// backend whose connection pool lets many writers reach the gap at once.
func TestRecomputeUsageCannotSplitACommitFromItsFold(t *testing.T) {
	const (
		fileSize = 4096
		owner    = uint32(4243)
		// Long enough that an unguarded realign certainly finishes inside the
		// gap, short enough that the guarded path only pauses once.
		realignWindow = 3 * time.Second
	)

	// The test database outlives a single run, so a fixed share name would
	// accumulate the previous run's rows.
	shareName := fmt.Sprintf("/qr-%x", uint32(time.Now().UnixNano()))

	ctx := context.Background()
	store := newTestStore(t)

	if _, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	}); err != nil {
		t.Fatalf("CreateRootDirectory(%q): %v", shareName, err)
	}

	writeFile := func(path string) error {
		h, err := store.GenerateHandle(ctx, shareName, path)
		if err != nil {
			return err
		}
		_, id, err := metadata.DecodeFileHandle(h)
		if err != nil {
			return err
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
		return store.UpdateAttrs(ctx, f)
	}

	// A settled file, so the realign has something to aggregate besides the
	// transaction it is racing.
	if err := writeFile("/settled"); err != nil {
		t.Fatalf("seeding a settled file: %v", err)
	}

	var (
		armed     atomic.Bool
		committed = make(chan struct{})
		realigned = make(chan struct{})
	)
	armed.Store(true)

	// Fires once, in the gap after the raced commit. Holding the writer there
	// hands the realign the window it would otherwise have to be unlucky to get.
	//
	// The arming is a compare-and-swap rather than a sync.Once because the
	// realign commits its own rebuild through this same seam: Once.Do blocks
	// its second caller until the first returns, so the realign would wait on
	// the very timer it is supposed to be racing, and the test would pass
	// whether or not the ordering it checks exists.
	restore := postgres.SetAfterCommitFold(func() {
		if !armed.CompareAndSwap(true, false) {
			return
		}
		close(committed)
		select {
		case <-realigned:
		case <-time.After(realignWindow):
		}
	})
	defer restore()

	var wg sync.WaitGroup
	wg.Add(1)
	writeErr := make(chan error, 1)
	go func() {
		defer wg.Done()
		writeErr <- writeFile("/raced")
	}()

	<-committed
	if _, err := store.RecomputeUsage(ctx, false); err != nil {
		t.Fatalf("RecomputeUsage: %v", err)
	}
	close(realigned)

	wg.Wait()
	if err := <-writeErr; err != nil {
		t.Fatalf("raced write: %v", err)
	}

	want := int64(2 * fileSize)
	got, err := store.GetUsedBytesForShare(ctx, shareName)
	if err != nil {
		t.Fatalf("GetUsedBytesForShare: %v", err)
	}
	switch {
	case got > want:
		t.Fatalf("cache = %d, want %d: the realign re-derived between the commit and its fold, charging %d bytes twice",
			got, want, got-want)
	case got < want:
		t.Fatalf("cache = %d, want %d: the realign discarded a fold for a row it had not aggregated, losing %d bytes",
			got, want, want-got)
	}

	usage, err := store.GetQuotaUsage(shareName, metadata.QuotaScopeUser, owner)
	if err != nil {
		t.Fatalf("GetQuotaUsage: %v", err)
	}
	if usage.Files != 2 {
		t.Fatalf("cached inode count = %d, want 2", usage.Files)
	}
}
