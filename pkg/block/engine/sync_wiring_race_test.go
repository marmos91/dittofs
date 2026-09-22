package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	memorylocal "github.com/marmos91/dittofs/pkg/block/local/memory"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// sealingRemote is a block-keyed remote that is also its own ChunkSealer,
// which is the shape SetRemoteBlockStore derives the sealer from: it publishes
// the store and the sealer it type-asserts out of that same object under one
// lock. id makes the pair checkable — a snapshot whose sealer and store carry
// different ids was assembled from two different wirings.
//
// The embedded interface is nil: the test builds flush closures, it never runs
// one, so no method below SealChunk is ever called.
type sealingRemote struct {
	remote.RemoteBlockStore
	id int
}

func (s *sealingRemote) SealChunk(_ context.Context, _ block.ContentHash, plaintext []byte) ([]byte, error) {
	return plaintext, nil
}

// TestFlushFnReadsWiringUnderLock pins two properties of the wiring read that
// builds a flush closure: it is synchronized against the setters that publish
// those fields, and it is one snapshot rather than four reads.
//
// remoteBlockStore, chunkSealer, blockCommitter and syncedHashStore are all
// written by the setters under m.mu and were read with no lock at all. The
// detector stayed quiet because no caller re-wires a share today: both
// production callers finish wiring before Store.Start. The lock is what makes
// the setters' documented "idempotent, safe to call after construction"
// contract true of the code rather than true only of current caller order.
//
// The pairing assertion is the second property, and the one the detector
// cannot name. SetRemoteBlockStore publishes the store and its derived sealer
// together, so a snapshot pairing one wiring's sealer with another wiring's
// store can only come from reading the fields separately — exactly the design
// the single snapshot rejects.
func TestFlushFnReadsWiringUnderLock(t *testing.T) {
	local := memorylocal.New()
	t.Cleanup(func() { _ = local.Close() })
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })

	m := NewRemoteSync(local, rs, newStubFileChunkStore(), RemoteSyncConfig{})

	wiringA := &sealingRemote{id: 1}
	wiringB := &sealingRemote{id: 2}

	const rounds = 2000
	const readsPerRound = 64
	var wg sync.WaitGroup
	wg.Add(2)

	// Re-wire on a serving share, which is what the setters exist to allow.
	go func() {
		defer wg.Done()
		for i := 0; i < rounds*readsPerRound; i++ {
			if i%2 == 0 {
				m.SetRemoteBlockStore(wiringA)
			} else {
				m.SetRemoteBlockStore(wiringB)
			}
			m.SetSyncedHashStore(nil)
		}
	}()

	// Read the wiring against whatever is wired at the time, and build a flush
	// closure from it the way every drain path does.
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			for j := 0; j < readsPerRound; j++ {
				rbs, sealer, _, _ := m.wiring()
				store, _ := rbs.(*sealingRemote)
				seal, _ := sealer.(*sealingRemote)
				if store != nil && seal != nil && store.id != seal.id {
					t.Errorf("torn wiring snapshot: sealer from wiring %d paired with store from wiring %d", seal.id, store.id)
					return
				}
			}
			if flush, after := m.flushFn(); flush == nil || after == nil {
				t.Errorf("flushFn returned a nil closure")
				return
			}
		}
	}()

	wg.Wait()
}

// TestFlushCommitterGateReadsUnderLock covers Flush's own read of
// blockCommitter, which is a second critical section from flushFn's and is
// reached only on the local-only path (nil remote store). Nothing else in the
// package drives that read against a concurrent setter, so without this the
// gate's lock could be removed and every test would stay green.
func TestFlushCommitterGateReadsUnderLock(t *testing.T) {
	local := memorylocal.New()
	t.Cleanup(func() { _ = local.Close() })

	m := NewRemoteSync(local, nil, newStubFileChunkStore(), RemoteSyncConfig{})

	const rounds = 2000
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			m.SetSyncedHashStore(nil)
		}
	}()

	go func() {
		defer wg.Done()
		ctx := context.Background()
		for i := 0; i < rounds; i++ {
			// A bare SyncedHashStore is never a blockCommitter, so the gate
			// must report the soft condition on every pass rather than claim a
			// finalized flush it has no way to write.
			res, err := m.Flush(ctx, "payload-under-rewire")
			if err != nil {
				t.Errorf("Flush: %v", err)
				return
			}
			if res.Finalized {
				t.Errorf("Flush reported Finalized with no committer wired")
				return
			}
		}
	}()

	wg.Wait()
}
