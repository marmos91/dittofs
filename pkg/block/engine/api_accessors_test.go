package engine

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/health"
)

// TestStore_GetSize_Exists drives the public GetSize/Exists accessors for
// both a present and an absent payload. The local-only engine (no remote)
// resolves entirely from the local store.
func TestStore_GetSize_Exists(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)
	ctx := context.Background()
	const payloadID = "share/sized-file"

	// Absent payload.
	if ok, err := bs.Exists(ctx, payloadID); err != nil || ok {
		t.Fatalf("Exists(absent) = (%v, %v), want (false, nil)", ok, err)
	}

	data := []byte("size-and-exists payload bytes")
	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	sz, err := bs.GetSize(ctx, payloadID)
	if err != nil {
		t.Fatalf("GetSize: %v", err)
	}
	if sz != uint64(len(data)) {
		t.Errorf("GetSize = %d, want %d", sz, len(data))
	}
	if ok, err := bs.Exists(ctx, payloadID); err != nil || !ok {
		t.Fatalf("Exists(present) = (%v, %v), want (true, nil)", ok, err)
	}
}

// TestStore_Accessors covers the cheap public accessor surface that the
// runtime and snapshot layers depend on: HasRemoteStore, RemoteStore,
// RemoteForTesting, LocalForTest, ListFiles, and LocalStats.
func TestStore_Accessors(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)
	ctx := context.Background()

	if bs.HasRemoteStore() {
		t.Error("HasRemoteStore: local-only engine should report false")
	}
	if bs.RemoteStore() != nil {
		t.Error("RemoteStore: want nil for local-only engine")
	}
	if bs.RemoteForTesting() != nil {
		t.Error("RemoteForTesting: want nil for local-only engine")
	}
	if bs.LocalForTest() == nil {
		t.Error("LocalForTest: want non-nil local store")
	}

	const payloadID = "share/listed-file"
	if _, err := bs.WriteAt(ctx, payloadID, nil, []byte("listed"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	files := bs.ListFiles()
	found := false
	for _, f := range files {
		if f == payloadID {
			found = true
		}
	}
	if !found {
		t.Errorf("ListFiles = %v, want to contain %q", files, payloadID)
	}

	// LocalStats must not panic and reflects a non-negative file count.
	_ = bs.LocalStats()
}

// TestStore_RetentionAndEvictionSetters exercises the delegating setters
// (they must not panic and must accept valid policies).
func TestStore_RetentionAndEvictionSetters(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)

	bs.SetRetentionPolicy(block.RetentionLRU, time.Hour)
	bs.SetRetentionPolicy(block.RetentionPin, 0)
	bs.SetEvictionEnabled(true)
	bs.SetEvictionEnabled(false)
}

// TestStore_HealthCheck covers both the legacy error probe and the
// structured Healthcheck for a healthy local-only engine.
func TestStore_HealthCheck(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)
	ctx := context.Background()

	if err := bs.HealthCheck(ctx); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	rep := bs.Healthcheck(ctx)
	if rep.Status != health.StatusHealthy {
		t.Errorf("Healthcheck = %+v, want healthy", rep)
	}
}

// TestStore_EvictLocal drops a file's local state and confirms a
// subsequent Exists reports it gone (no remote to fall back to).
func TestStore_EvictLocal(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)
	ctx := context.Background()
	const payloadID = "share/evict-me"

	if _, err := bs.WriteAt(ctx, payloadID, nil, []byte("evictable bytes"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if ok, _ := bs.Exists(ctx, payloadID); !ok {
		t.Fatal("Exists before evict: want true")
	}

	if err := bs.EvictLocal(ctx, payloadID); err != nil {
		t.Fatalf("EvictLocal: %v", err)
	}
	if ok, _ := bs.Exists(ctx, payloadID); ok {
		t.Error("Exists after evict: want false for local-only engine")
	}
}

// TestStore_ClosedGuards verifies the public accessors that pin against
// Close fail closed with ErrStoreClosed after the store is closed.
func TestStore_ClosedGuards(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)
	ctx := context.Background()

	if err := bs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := bs.GetSize(ctx, "x"); err == nil {
		t.Error("GetSize after Close: want error")
	}
	if _, err := bs.Exists(ctx, "x"); err == nil {
		t.Error("Exists after Close: want error")
	}
	if err := bs.EvictLocal(ctx, "x"); err == nil {
		t.Error("EvictLocal after Close: want error")
	}
	if err := bs.HealthCheck(ctx); err == nil {
		t.Error("HealthCheck after Close: want error")
	}
	if rep := bs.Healthcheck(ctx); rep.Status == health.StatusHealthy {
		t.Error("Healthcheck after Close: want non-healthy")
	}
	// LocalStats returns a zero-value Stats on a closed store rather than
	// erroring — assert it does not panic.
	_ = bs.LocalStats()
}
