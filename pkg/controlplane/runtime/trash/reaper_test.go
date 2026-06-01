package trash

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recycleSized creates a file, forces an explicit byte size onto it via the
// store, then RemoveFile's it so it lands in the bin carrying that size.
//
// CreateFile cannot set Size: ApplyCreateDefaults zeroes it for regular files
// (no data-plane write runs in these unit tests). So we stamp Size directly
// through the store's GetFile/PutFile — the same direct-store technique the
// service uses in clearStamp for fields SetFileAttributes does not expose. The
// recycle move preserves the attr, so Entry.Size is non-zero and the max-size
// eviction test can assert on it deterministically.
func (tt *trashTest) recycleSized(name string, size uint64) {
	tt.t.Helper()
	_, err := tt.deps.svc.CreateFile(tt.ctx, tt.deps.rootHandle, name, &metadata.FileAttr{Mode: 0o644})
	require.NoError(tt.t, err)

	store, err := tt.deps.svc.GetStoreForShare(tt.deps.shareName)
	require.NoError(tt.t, err)
	handle, err := tt.deps.svc.GetChild(tt.ctx.Context, tt.deps.rootHandle, name)
	require.NoError(tt.t, err)
	file, err := store.GetFile(tt.ctx.Context, handle)
	require.NoError(tt.t, err)
	file.Size = size
	require.NoError(tt.t, store.PutFile(tt.ctx.Context, file))

	_, err = tt.deps.svc.RemoveFile(tt.ctx, tt.deps.rootHandle, name)
	require.NoError(tt.t, err)
}

func TestReapDeletesExpiredEntries(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	tt.recycle("old.txt")

	cfg := Config{Enabled: true, RetentionDays: 7}
	// now is 8 days after the entry was recycled: past the 7-day cutoff.
	now := time.Now().Add(8 * 24 * time.Hour)

	removed, err := tt.svc.reapShareAt(tt.ctx, tt.deps.shareName, cfg, now)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)

	// Bin is empty and the file's blocks were freed.
	entries, err := tt.svc.List(tt.ctx, tt.deps.shareName)
	require.NoError(t, err)
	assert.Empty(t, entries)
	assert.Len(t, tt.deps.freed, 1, "expired entry should free its blocks")
}

func TestReapKeepsUnexpired(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	tt.recycle("new.txt")

	cfg := Config{Enabled: true, RetentionDays: 7}
	// Recycled just now, well inside the 7-day window.
	now := time.Now()

	removed, err := tt.svc.reapShareAt(tt.ctx, tt.deps.shareName, cfg, now)
	require.NoError(t, err)
	assert.Equal(t, 0, removed)

	entries, err := tt.svc.List(tt.ctx, tt.deps.shareName)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "new.txt", entries[0].OriginalPath)
	assert.Empty(t, tt.deps.freed)
}

func TestMaxSizeEvictsOldestFirst(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	// Recycle the oldest entry first; the second sorts later by DeletedAt.
	tt.recycleSized("old.bin", 1000)
	time.Sleep(2 * time.Millisecond) // ensure a strictly later DeletedAt
	tt.recycleSized("new.bin", 1000)

	// Total is 2000 bytes; cap of 1500 forces eviction of the single oldest
	// entry, leaving 1000 bytes (<= cap).
	removed, err := tt.svc.evictToCap(tt.ctx, tt.deps.shareName, 1500)
	require.NoError(t, err)
	assert.Equal(t, 1, removed, "exactly the oldest entry should be evicted")

	entries, err := tt.svc.List(tt.ctx, tt.deps.shareName)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the newer entry must survive")
	assert.Equal(t, "new.bin", entries[0].OriginalPath)
	assert.Len(t, tt.deps.freed, 1, "evicted entry should free its blocks")
}

func TestEvictToCapNoopWhenUnderCap(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	tt.recycleSized("a.bin", 100)
	tt.recycleSized("b.bin", 100)

	removed, err := tt.svc.evictToCap(tt.ctx, tt.deps.shareName, 1000)
	require.NoError(t, err)
	assert.Equal(t, 0, removed)

	entries, err := tt.svc.List(tt.ctx, tt.deps.shareName)
	require.NoError(t, err)
	assert.Len(t, entries, 2)
	assert.Empty(t, tt.deps.freed)
}

func TestStopIsIdempotent(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	// Two Stop calls must not panic on a closed channel.
	tt.svc.Stop()
	tt.svc.Stop()
}
