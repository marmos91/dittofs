package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// newBadgerCommitter builds a badger metadata store with one regular file whose
// PayloadID is returned, so a carve sink commit for that PayloadID projects onto
// a real File row (the only cross-commit shared key). Badger uses SSI, so its
// conflict counter is the disk-speed-independent contention fingerprint the
// memory store fixture cannot provide.
func newBadgerCommitter(t *testing.T) (*metadatabadger.BadgerMetadataStore, metadata.PayloadID) {
	t.Helper()
	ctx := context.Background()
	store, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	const shareName = "s1"
	require.NoError(t, store.CreateShare(ctx, &metadata.Share{Name: shareName}))
	root, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	})
	require.NoError(t, err)
	dir, err := metadata.EncodeFileHandle(root)
	require.NoError(t, err)

	pid := metadata.PayloadID(shareName + "/" + uuid.NewString())
	handle, err := store.GenerateHandle(ctx, shareName, "/f.bin")
	require.NoError(t, err)
	_, id, err := metadata.DecodeFileHandle(handle)
	require.NoError(t, err)

	file := &metadata.File{
		ShareName: shareName,
		Path:      "/f.bin",
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o600, UID: 1000, GID: 1000,
			PayloadID: pid,
		},
	}
	file.ID = id
	require.NoError(t, store.PutFile(ctx, file))
	require.NoError(t, store.SetParent(ctx, handle, dir))
	require.NoError(t, store.SetChild(ctx, dir, "f.bin", handle))
	return store, pid
}

// TestLocalBlockSink_ConcurrentSameFileCommit_NoSSIConflict reproduces the carve
// SSI wall: the within-file carve dispatcher (CarveUploadConcurrency) fires
// several CommitBlock calls for one file concurrently, and each re-projects
// File.Blocks (PutFile) on the SAME File row. Under badger's SSI that read-write
// on a shared row aborts as ErrConflict; enough contention exhausts the retry
// budget and surfaces the conflict to the carver (the client sees EDEADLK).
//
// The commits touch disjoint offsets/hashes/blocks, so a correct implementation
// serializes only the shared File-row write and leaves the conflict counter at
// zero.
func TestLocalBlockSink_ConcurrentSameFileCommit_NoSSIConflict(t *testing.T) {
	ctx := context.Background()
	store, pid := newBadgerCommitter(t)
	sink := localBlockSink{committer: store, commitLocks: &carveCommitLocks{}}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data := []byte{byte(i), byte(i >> 8), 0xab, 0xcd}
			chunk := journal.CarveChunk{
				FileID:     journal.FileID(pid),
				FileOffset: int64(i) * 4096,
				Hash:       journal.ChunkHash(blake3.Sum256(data)),
				Data:       data,
			}
			<-start
			errs[i] = sink.CommitBlock(ctx, []journal.CarveChunk{chunk})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "concurrent CommitBlock %d surfaced an error", i)
	}
	require.Zero(t, store.TransactionConflictsForTest(),
		"concurrent same-file carve commits must not serialize on the File row (SSI conflict)")
}
