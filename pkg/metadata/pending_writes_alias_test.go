package metadata

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func pendingAliasHandles() []FileHandle {
	id := "00112233-4455-6677-8899-aabbccddeeff"
	return []FileHandle{
		FileHandle("/pending:" + id),
		FileHandle("/pending:" + strings.ToUpper(id)),
		FileHandle("/pending:" + strings.ReplaceAll(id, "-", "")),
	}
}

func TestPendingWritesHandleAliasesShareState(t *testing.T) {
	aliases := pendingAliasHandles()
	for _, writer := range aliases {
		t.Run(string(writer), func(t *testing.T) {
			tr := NewPendingWritesTracker()
			file := &File{ID: uuid.MustParse("00112233-4455-6677-8899-aabbccddeeff"), ShareName: "/pending", FileAttr: FileAttr{Type: FileTypeRegular, Size: 32}}
			stamp := time.Unix(123, 0)
			tr.SetCachedFile(writer, file)
			tr.RecordWrite(writer, &WriteOperation{Handle: writer, NewSize: 64, PayloadID: "payload", NewMtime: stamp, PreWriteAttr: &file.FileAttr}, false)
			tr.RecordWrite(aliases[0], &WriteOperation{Handle: aliases[0], NewSize: 128, PayloadID: "payload", NewMtime: stamp}, true)
			require.Equal(t, 1, tr.Count(), "one inode must have one deferred-write entry")
			for _, reader := range aliases {
				size, ok := tr.GetPendingSize(reader)
				require.True(t, ok)
				require.Equal(t, uint64(128), size)
				pending, ok := tr.GetPending(reader)
				require.True(t, ok)
				require.Equal(t, PayloadID("payload"), pending.PayloadID)
				require.True(t, pending.ClearSetuidSetgid)
				require.Equal(t, file, tr.GetCachedFile(reader))
				require.Same(t, tr.GetFlushLock(writer), tr.GetFlushLock(reader))
			}

			frozen := stamp.Add(time.Second)
			require.True(t, tr.UpdatePendingMtime(aliases[2], frozen))
			pending, ok := tr.PopPending(aliases[1])
			require.True(t, ok)
			require.Equal(t, frozen, pending.LastMtime)
			require.Zero(t, tr.Count())
			tr.RestorePending(aliases[2], pending)
			require.Equal(t, []FileHandle{aliases[0]}, tr.PendingHandles())

			tr.InvalidateCache(aliases[1])
			for _, reader := range aliases {
				require.Nil(t, tr.GetCachedFile(reader))
				size, ok := tr.GetPendingSize(reader)
				require.True(t, ok, "cache invalidation must retain actual pending writes")
				require.Equal(t, uint64(128), size)
			}
			all := tr.PendingHandles()
			require.Len(t, all, 1)
			require.Equal(t, aliases[0], all[0])
			drained, ok := tr.PopPending(all[0])
			require.True(t, ok)
			require.Equal(t, uint64(128), drained.MaxSize)
			require.Zero(t, tr.Count())

			tr.SetCachedFile(writer, file)
			tr.InvalidateCache(aliases[0])
			require.Zero(t, tr.Count(), "cache-only alias entries must also be removed")
		})
	}
}

func TestPendingWritesCanonicalizationIsLocalToTracker(t *testing.T) {
	aliases := pendingAliasHandles()
	require.NotEqual(t, handleKey(aliases[0]), handleKey(aliases[1]), "other consumers retain their existing opaque handle keys")
	for _, handle := range []FileHandle{nil, FileHandle("not-a-handle"), FileHandle("/pending:invalid-uuid")} {
		require.Equal(t, string(handle), pendingWriteKey(handle))
		tr := NewPendingWritesTracker()
		tr.RecordWrite(handle, &WriteOperation{Handle: handle, NewSize: 7}, false)
		state, ok := tr.PopPending(handle)
		require.True(t, ok)
		require.Equal(t, uint64(7), state.MaxSize)
	}
}

func TestInvalidateWriteCacheWaitsForAliasFlush(t *testing.T) {
	svc := New()
	aliases := pendingAliasHandles()
	file := &File{FileAttr: FileAttr{Type: FileTypeRegular, Size: 64}}
	svc.PrewarmWriteCache(aliases[1], file)
	mu := svc.pendingWrites.GetFlushLock(aliases[1])
	mu.Lock()
	locked := true
	defer func() {
		if locked {
			mu.Unlock()
		}
	}()
	state, ok := svc.pendingWrites.PopPending(aliases[1])
	require.True(t, ok)

	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		close(started)
		svc.InvalidateWriteCache(aliases[0])
		close(done)
	}()
	<-started
	finished := false
	select {
	case <-done:
		finished = true
		t.Error("invalidation returned while the cache-only flush still owned its stripe")
	case <-time.After(100 * time.Millisecond):
		// Bounded exclusion check: invalidation must wait for the held mutex.
	}

	// Complete the cache-only flush's pop -> republish sequence under its lock.
	svc.pendingWrites.SetCachedFile(aliases[1], state.CachedFile)
	mu.Unlock()
	locked = false
	if !finished {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("invalidation did not finish after the flush released its stripe")
		}
	}
	for _, handle := range aliases {
		require.Nil(t, svc.pendingWrites.GetCachedFile(handle), "a completed flush must not resurrect attributes after invalidation")
	}
}
