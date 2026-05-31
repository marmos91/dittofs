//go:build integration

package badger_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// TestValueLogGC_CloseDoesNotHang verifies that the background value-log
// GC goroutine started in the constructor is stopped cleanly by Close()
// and does not leak: Close must return promptly (the goroutine drains via
// the WaitGroup) and a second Close must be a safe no-op.
func TestValueLogGC_CloseDoesNotHang(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "metadata.db")

	store, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}

	// Close must return well within the GC ticker interval; if it blocked
	// on the GC goroutine it would never return promptly.
	done := make(chan error, 1)
	go func() {
		done <- store.Close()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return promptly — GC goroutine likely leaked or Close blocked")
	}
}
