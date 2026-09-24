package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	blockgc "github.com/marmos91/dittofs/pkg/block/gc"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
)

var errInjectedMetadata = errors.New("injected transient metadata failure")

// failDeleteBlockRecord is a metadata store whose DeleteBlockRecord always fails.
type failDeleteBlockRecord struct{ metadata.Store }

func (failDeleteBlockRecord) DeleteBlockRecord(context.Context, string) error {
	return errInjectedMetadata
}

// failDeleteSynced is a metadata store whose DeleteSynced always fails.
type failDeleteSynced struct{ metadata.Store }

func (failDeleteSynced) DeleteSynced(context.Context, block.ContentHash) error {
	return errInjectedMetadata
}

// TestGCIndexSweep_FailedReclaimKeepsDedupHonest pins the invariant the carve
// dedup oracle relies on, across a reclaim that fails part-way: whenever the
// oracle answers "already remote-durable" for a hash, the block object holding
// its bytes must still exist. A true answer makes the carver drop its plaintext
// and write a manifest row alone, so an oracle that trusts a marker whose object
// is gone acknowledges data that exists nowhere.
//
// The reclaim of a block's last live chunk touches the remote object, the block
// record and the synced marker. Each subtest makes one of the two metadata steps
// fail transiently; whatever the reclaim leaves behind, the oracle and the
// remote must agree.
func TestGCIndexSweep_FailedReclaimKeepsDedupHonest(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(metadata.Store) metadata.Store
	}{
		{"DeleteBlockRecord fails", func(s metadata.Store) metadata.Store { return failDeleteBlockRecord{s} }},
		{"DeleteSynced fails", func(s metadata.Store) metadata.Store { return failDeleteSynced{s} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			rs := remotememory.New()
			defer func() { _ = rs.Close() }()

			rec := newGCMSReconciler()
			st := rec.addShare("share-a")

			h := hashFromString("partial-reclaim-" + tc.name)
			seedRemoteChunk(t, st, rs, h) // sole chunk of its block, past grace, no manifest row
			blockID := "blk-" + h.String()[:16]

			idx, ok := st.(blockgc.SyncedHashIndex)
			if !ok {
				t.Fatalf("metadata store %T does not implement blockgc.SyncedHashIndex", st)
			}
			stats := blockgc.CollectGarbage(ctx, rec, &blockgc.Options{
				GCStateRoot:     t.TempDir(),
				GracePeriod:     time.Hour,
				SyncedHashIndex: idx,
				BlockReclaimer:  newBlockGCReclaimer(tc.wrap(st), rs),
			})
			if stats.Errors == 0 {
				t.Fatal("sweep reported no error; the injected metadata failure was never reached")
			}

			durable, err := engineDeduper{synced: st}.IsChunkDurable(ctx, ChunkHash(h))
			if err != nil {
				t.Fatalf("IsChunkDurable: %v", err)
			}
			_, getErr := rs.GetBlock(ctx, blockID)
			objectExists := getErr == nil

			if durable && !objectExists {
				t.Fatalf("dedup oracle reports %s durable after a failed reclaim, but block object %s is gone (GetBlock: %v): a carve deduping onto it acknowledges bytes that exist nowhere",
					h, blockID, getErr)
			}
		})
	}
}
