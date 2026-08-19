package journal

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// openDirtyExpireStore opens a store whose dirty-age loop fires quickly and
// returns it with a per-shard fsync spy already installed on the shard owning
// id. The spy is installed before the first write, so the loop — which reads
// segSync only once a shard is dirty, behind that shard's mutex — can never
// observe it unset.
func openDirtyExpireStore(t *testing.T, id FileID, expiry time.Duration) (*Store, *shard, *atomic.Int32) {
	t.Helper()
	s, err := Open(t.TempDir(), Config{DirtyExpiry: expiry}, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sh := s.shardFor(id)
	var syncs atomic.Int32
	sh.segSync = func(seg *segmentMeta) error {
		syncs.Add(1)
		return seg.fd.Sync()
	}
	return s, sh, &syncs
}

// TestDirtyExpiry_CommitsWithoutExplicitCommit is the regression test for the
// unbounded non-durable window: a client that writes and never asks for
// durability must still have its bytes fsynced within the dirty-age interval.
func TestDirtyExpiry_CommitsWithoutExplicitCommit(t *testing.T) {
	const id FileID = "unfsynced"
	s, sh, syncs := openDirtyExpireStore(t, id, 20*time.Millisecond)

	if err := s.WriteAt(context.Background(), id, 0, []byte("payload")); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	sh.mu.Lock()
	want := sh.lastVersion
	sh.mu.Unlock()
	if want == 0 {
		t.Fatal("write did not stamp a version")
	}
	if got := sh.syncedVersion.Load(); got >= want {
		t.Fatalf("write must not be durable before any commit: synced=%d last=%d", got, want)
	}

	deadline := time.Now().Add(5 * time.Second)
	for sh.syncedVersion.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("shard never became durable without an explicit Commit: synced=%d last=%d syncs=%d",
				sh.syncedVersion.Load(), want, syncs.Load())
		}
		time.Sleep(time.Millisecond)
	}
	if syncs.Load() == 0 {
		t.Fatal("watermark advanced without an fsync — the guarantee is hollow")
	}
}

// TestDirtyExpiry_IdleStoreDoesNotSync proves the loop is dirty-driven: with
// nothing written it must never fsync, so an idle store pays nothing.
func TestDirtyExpiry_IdleStoreDoesNotSync(t *testing.T) {
	_, _, syncs := openDirtyExpireStore(t, "idle", 5*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	if n := syncs.Load(); n != 0 {
		t.Fatalf("idle store fsynced %d times; the loop must only touch dirty shards", n)
	}
}

// TestDirtyExpiry_Disabled proves a negative interval turns the loop off, so
// the historical "no promise without fsync" posture stays available.
func TestDirtyExpiry_Disabled(t *testing.T) {
	const id FileID = "disabled"
	s, sh, syncs := openDirtyExpireStore(t, id, -1)

	if err := s.WriteAt(context.Background(), id, 0, []byte("payload")); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := syncs.Load(); n != 0 {
		t.Fatalf("disabled dirty-age loop still fsynced %d times", n)
	}
	sh.mu.Lock()
	last := sh.lastVersion
	sh.mu.Unlock()
	if sh.syncedVersion.Load() >= last {
		t.Fatal("disabled loop still advanced the durable watermark")
	}
}
