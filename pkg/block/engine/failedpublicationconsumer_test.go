package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/chunker"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/stretchr/testify/require"
)

// A failed pass-end reap leaves a later-starting old row in the manifest.
// Exercise the real Badger committer, warmer and final cold reader: the next
// clean flush must finish that publication even when no dirty interval remains.
func TestFailedManifestPublicationRetryPreservesReader(t *testing.T) {
	for _, outcome := range []string{"error", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			f := newWarmScopeFixture(t)
			ctx := context.Background()
			stale := *f.dstRow
			stale.ID = "warm-src/2048"
			stale.DataSize = 2048
			require.NoError(t, f.metadata.Put(ctx, &stale))
			fresh := bytes.Repeat([]byte{0x7f}, len(f.source))
			_, err := f.bs.WriteAt(ctx, "warm-src", nil, fresh, 0)
			require.NoError(t, err)

			failure := errors.New("manifest reap unavailable")
			entered, resume := make(chan struct{}), make(chan struct{})
			var resumeOnce sync.Once
			unpause := func() { resumeOnce.Do(func() { close(resume) }) }
			t.Cleanup(unpause)
			var attempts atomic.Int64
			flushed := make(chan error, 1)
			go func() {
				var result error
				defer func() {
					if p := recover(); p != nil {
						result = fmt.Errorf("recovered reap: %v", p)
					}
					flushed <- result
				}()
				result = f.bs.WithPayloadScope(ctx, []string{"warm-src"}, false, func(ctx context.Context) error {
					fn, reap := f.rs.flushFn()
					return f.bs.local.Flush(ctx, "warm-src", journal.FlushOptions{Force: true, AfterFile: func(ctx context.Context, id journal.FileID) error {
						if attempts.Add(1) == 1 {
							close(entered)
							<-resume
							if outcome == "panic" {
								panic(failure)
							}
							return failure
						}
						return reap(ctx, id)
					}}, fn)
				})
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("flush never reached manifest publication")
			}
			got := make([]byte, len(fresh))
			_, err = f.bs.ReadAt(ctx, "warm-src", got, 0)
			require.NoError(t, err)
			require.Equal(t, fresh, got, "resident bytes must remain the authoritative copy")
			unpause()
			require.ErrorContains(t, waitWarmScope(t, flushed), failure.Error())

			dirty, err := f.bs.HasDirty(ctx, "warm-src")
			require.NoError(t, err)
			if !dirty {
				t.Error("failed manifest publication reported clean before retry")
			}
			_, err = f.bs.DrainLocalSynced(ctx)
			require.NoError(t, err)
			resident, err := f.bs.local.IsRangeResident(ctx, "warm-src", 0, int64(len(fresh)))
			require.NoError(t, err)
			if !resident {
				t.Error("failed publication allowed authoritative bytes to be evicted")
			}
			var retryErr error
			_, err = f.bs.WarmAll(ctx, func(done, total int64) {
				if done != 0 {
					return
				}
				retryErr = f.bs.DrainPayload(ctx, "warm-src")
				if retryErr == nil {
					_, retryErr = f.bs.DrainLocalSynced(ctx)
				}
			})
			require.NoError(t, retryErr)
			require.NoError(t, err)
			_, err = f.bs.ReadAt(ctx, "warm-src", got, 0)
			require.NoError(t, err)
			require.True(t, bytes.Equal(fresh, got), "reader served obsolete interior bytes after failed reap: byte 2048=%#x, want %#x", got[2048], fresh[2048])
			require.EqualValues(t, 1, attempts.Load(), "retry must use a fresh pass instead of retaining its old callback")
			dirty, err = f.bs.HasDirty(ctx, "warm-src")
			require.NoError(t, err)
			require.False(t, dirty, "successful publication retry must release the fence")
		})
	}
}

type failPublicationAfterFirstBlock struct {
	engineBlockSink
	failure error
}

func (s failPublicationAfterFirstBlock) CommitBlock(ctx context.Context, chunks []CarveChunk) error {
	if len(chunks) > 0 && chunks[0].FileOffset != 0 {
		return s.failure
	}
	return s.engineBlockSink.CommitBlock(ctx, chunks)
}

// One fn call can upload a prefix and then fail on its next block. Its reap
// must include that same-call prefix before journal credits it as durable.
func TestFailedUploadPublishesCommittedPrefixBeforeColdRead(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx := context.Background()
	stale := *f.dstRow
	stale.ID = "warm-src/2048"
	stale.DataSize = 2048
	require.NoError(t, f.metadata.Put(ctx, &stale))
	fresh := bytes.Repeat([]byte{0x7f}, 4096)
	for off := uint64(0); off < 4*4096; off += 4096 {
		_, err := f.bs.WriteAt(ctx, "warm-src", nil, fresh, off)
		require.NoError(t, err)
	}
	failure := errors.New("later upload failed")
	rbs, sealer, committer, hashes := f.rs.wiring()
	sink := failPublicationAfterFirstBlock{
		engineBlockSink: engineBlockSink{rbs: rbs, sealer: sealer, committer: committer, commitLocks: &carveCommitLocks{}},
		failure:         failure,
	}
	err := f.bs.WithPayloadScope(ctx, []string{"warm-src"}, false, func(ctx context.Context) error {
		fn, reap := newFlushClosure(f.bs.local, chunker.Params{Min: 4096, Avg: 4096, Max: 4096}, 8192, engineDeduper{synced: hashes}, sink, nil)
		return f.bs.local.Flush(ctx, "warm-src", journal.FlushOptions{Force: true, AfterFile: reap}, fn)
	})
	require.ErrorIs(t, err, failure)
	require.EqualValues(t, 8192, f.bs.local.UnsyncedBytes(), "only the failed suffix should remain dirty")
	require.NoError(t, f.bs.local.Invalidate(ctx, "warm-src", 0, 4096))
	resident, err := f.bs.local.IsRangeResident(ctx, "warm-src", 0, 4096)
	require.NoError(t, err)
	require.False(t, resident, "reader must resolve the published prefix through the manifest")
	got := make([]byte, len(fresh))
	_, err = f.bs.ReadAt(ctx, "warm-src", got, 0)
	require.NoError(t, err)
	require.True(t, bytes.Equal(fresh, got), "committed prefix retained an obsolete interior row: byte 2048=%#x, want %#x", got[2048], fresh[2048])
}
