package remotetest

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	"github.com/marmos91/dittofs/pkg/health"
)

// Factory creates a new RemoteStore instance for testing.
type Factory func(t *testing.T) remote.RemoteStore

// RunSuite runs the full conformance test suite against a RemoteStore implementation.
func RunSuite(t *testing.T, factory Factory) {
	t.Run("WriteAndRead", func(t *testing.T) { testWriteAndRead(t, factory) })
	t.Run("ReadNotFound", func(t *testing.T) { testReadNotFound(t, factory) })
	t.Run("ReadBlockRange", func(t *testing.T) { testReadBlockRange(t, factory) })
	t.Run("DeleteBlock", func(t *testing.T) { testDeleteBlock(t, factory) })
	t.Run("DeleteByPrefix", func(t *testing.T) { testDeleteByPrefix(t, factory) })
	t.Run("ListByPrefix", func(t *testing.T) { testListByPrefix(t, factory) })
	t.Run("HealthCheck", func(t *testing.T) { testHealthCheck(t, factory) })
	t.Run("HealthcheckReport", func(t *testing.T) { testHealthcheckReport(t, factory) })
	t.Run("ClosedOperations", func(t *testing.T) { testClosedOperations(t, factory) })
	t.Run("DataIsolation", func(t *testing.T) { testDataIsolation(t, factory) })
}

func testWriteAndRead(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := []byte("hello world")
	blockKey := "test/block-0"

	if err := store.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	read, err := store.ReadBlock(ctx, blockKey)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}
	if !bytes.Equal(read, data) {
		t.Fatalf("ReadBlock returned %q, want %q", read, data)
	}
}

func testReadNotFound(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	_, err := store.ReadBlock(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent block")
	}
}

func testReadBlockRange(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := []byte("hello world")
	blockKey := "test/block-0"

	if err := store.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	read, err := store.ReadBlockRange(ctx, blockKey, 0, 5)
	if err != nil {
		t.Fatalf("ReadBlockRange failed: %v", err)
	}
	if string(read) != "hello" {
		t.Fatalf("ReadBlockRange returned %q, want %q", read, "hello")
	}

	read, err = store.ReadBlockRange(ctx, blockKey, 6, 5)
	if err != nil {
		t.Fatalf("ReadBlockRange failed: %v", err)
	}
	if string(read) != "world" {
		t.Fatalf("ReadBlockRange returned %q, want %q", read, "world")
	}
}

func testDeleteBlock(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	blockKey := "test/block-0"
	if err := store.WriteBlock(ctx, blockKey, []byte("data")); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	if err := store.DeleteBlock(ctx, blockKey); err != nil {
		t.Fatalf("DeleteBlock failed: %v", err)
	}

	_, err := store.ReadBlock(ctx, blockKey)
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func testDeleteByPrefix(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		key := "prefix1/block-" + string(rune('0'+i))
		if err := store.WriteBlock(ctx, key, []byte("data")); err != nil {
			t.Fatalf("WriteBlock failed: %v", err)
		}
	}
	if err := store.WriteBlock(ctx, "prefix2/block-0", []byte("data")); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	if err := store.DeleteByPrefix(ctx, "prefix1/"); err != nil {
		t.Fatalf("DeleteByPrefix failed: %v", err)
	}

	keys, err := store.ListByPrefix(ctx, "prefix1/")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected 0 keys after delete, got %d", len(keys))
	}

	keys, err = store.ListByPrefix(ctx, "prefix2/")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key remaining, got %d", len(keys))
	}
}

func testListByPrefix(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	for _, key := range []string{"a/block-0", "a/block-1", "b/block-0"} {
		if err := store.WriteBlock(ctx, key, []byte("data")); err != nil {
			t.Fatalf("WriteBlock failed: %v", err)
		}
	}

	keys, err := store.ListByPrefix(ctx, "a/")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}

	keys, err = store.ListByPrefix(ctx, "")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(keys))
	}
}

func testHealthCheck(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	if err := store.HealthCheck(ctx); err != nil {
		t.Fatalf("HealthCheck failed: %v", err)
	}
}

// testHealthcheckReport is the conformance test for the new
// Healthcheck (lowercase 'c') method that returns a health.Report.
// Implementations must populate Status correctly and stamp CheckedAt
// — without this assertion the conformance suite would silently
// accept a broken Healthcheck that returns a zero-value Report.
func testHealthcheckReport(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	rep := store.Healthcheck(ctx)
	if rep.Status != health.StatusHealthy {
		t.Fatalf("Healthcheck on fresh store: got status %q (%q), want healthy", rep.Status, rep.Message)
	}
	if rep.CheckedAt.IsZero() {
		t.Fatal("Healthcheck must populate CheckedAt")
	}
}

func testClosedOperations(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	if err := store.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if err := store.WriteBlock(ctx, "key", []byte("data")); err == nil {
		t.Error("WriteBlock should fail after Close")
	}

	if _, err := store.ReadBlock(ctx, "key"); err == nil {
		t.Error("ReadBlock should fail after Close")
	}
}

func testDataIsolation(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := []byte("original")
	blockKey := "test/block-0"

	if err := store.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Modify original
	data[0] = 'X'

	read, err := store.ReadBlock(ctx, blockKey)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}
	if read[0] != 'o' {
		t.Fatalf("expected 'o' after mutation, got %c", read[0])
	}
}
