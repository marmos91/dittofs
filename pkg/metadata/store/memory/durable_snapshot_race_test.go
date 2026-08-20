package memory_test

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestWriteSnapshot_ConcurrentDurableHandles checks that the store-wide read
// lock WriteSnapshot holds also excludes durable-handle mutators, so encoding
// the handle map never observes a concurrent write.
func TestWriteSnapshot_ConcurrentDurableHandles(t *testing.T) {
	store := memory.NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	for i := range 200 {
		h := &lock.PersistedDurableHandle{ID: fmt.Sprintf("seed-%d", i), DisconnectedAt: time.Now(), TimeoutMs: 3600000}
		if err := store.PutDurableHandle(ctx, h); err != nil {
			t.Fatalf("seed put: %v", err)
		}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h := &lock.PersistedDurableHandle{ID: fmt.Sprintf("live-%d", i), DisconnectedAt: time.Now().Add(-time.Hour), TimeoutMs: 1}
			if err := store.PutDurableHandle(ctx, h); err != nil {
				t.Errorf("put: %v", err)
				return
			}
			if _, err := store.DeleteExpiredDurableHandles(ctx, time.Now()); err != nil {
				t.Errorf("expire: %v", err)
				return
			}
		}
	}()

	for range 200 {
		if _, err := store.WriteSnapshot(ctx, io.Discard); err != nil {
			t.Fatalf("snapshot: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}
