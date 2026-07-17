package metadata_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/require"
)

// countingStore wraps the memory store and counts GetFile calls, keyed by the
// requested handle. It embeds *memory.MemoryMetadataStore so it satisfies the
// full metadata.Store interface while overriding only GetFile — the Service
// holds it via the Store interface (RegisterStoreForShare), so the override is
// actually dispatched. Note: it observes store.GetFile only; parent reads that
// went through tx.GetFile inside a transaction would not be counted here. The
// create path deliberately does not read the parent inode inside its
// transaction (#1573), so every parent read is a store.GetFile and is caught.
type countingStore struct {
	*memory.MemoryMetadataStore
	total  atomic.Int64
	perKey map[string]int64 // guarded by seq: create path is single-goroutine
}

func (c *countingStore) GetFile(ctx context.Context, h metadata.FileHandle) (*metadata.File, error) {
	c.total.Add(1)
	c.perKey[string(h)]++
	return c.MemoryMetadataStore.GetFile(ctx, h)
}

// TestCreateFile_ParentGetFileDedup pins the parent-inode read dedup (#1737):
// a single CreateFile must load the parent handle exactly once, not three times.
func TestCreateFile_ParentGetFileDedup(t *testing.T) {
	t.Parallel()

	inner := memory.NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()
	shareName := "/test"

	rootFile, err := inner.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o777,
		UID:  0,
		GID:  0,
	})
	require.NoError(t, err)
	rootHandle, err := metadata.EncodeShareHandle(shareName, rootFile.ID)
	require.NoError(t, err)

	cs := &countingStore{MemoryMetadataStore: inner, perKey: make(map[string]int64)}
	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare(shareName, cs))

	rootCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID:  metadata.Uint32Ptr(0),
			GID:  metadata.Uint32Ptr(0),
			GIDs: []uint32{0},
		},
		ClientAddr: "127.0.0.1",
	}

	// A real subdirectory to create children into.
	dir, _, err := svc.CreateDirectory(rootCtx, rootHandle, "sub", &metadata.FileAttr{Mode: 0o777})
	require.NoError(t, err)
	dirHandle, err := metadata.EncodeShareHandle(shareName, dir.ID)
	require.NoError(t, err)

	// Reset counters; measure exactly one CreateFile into the subdirectory.
	cs.total.Store(0)
	cs.perKey = make(map[string]int64)

	_, _, err = svc.CreateFile(rootCtx, dirHandle, "child.txt", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)

	parentReads := cs.perKey[string(dirHandle)]
	t.Logf("CreateFile: total GetFile=%d, parent GetFile=%d", cs.total.Load(), parentReads)

	// After the dedup the parent inode is loaded exactly once. Before #1737 it
	// was loaded three times (createEntry + CheckParentCreateAccess +
	// checkWritePermission). Guard against silent regression.
	require.Equal(t, int64(1), parentReads,
		"parent inode must be loaded exactly once per CreateFile (was 3 before #1737)")
}

// BenchmarkCreateFile creates children in one directory via the fixture,
// reporting ns/op and allocs/op as a secondary before/after signal.
func BenchmarkCreateFile(b *testing.B) {
	inner := memory.NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()
	shareName := "/test"

	rootFile, err := inner.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o777,
	})
	require.NoError(b, err)
	rootHandle, err := metadata.EncodeShareHandle(shareName, rootFile.ID)
	require.NoError(b, err)

	svc := metadata.New()
	require.NoError(b, svc.RegisterStoreForShare(shareName, inner))

	rootCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID:  metadata.Uint32Ptr(0),
			GID:  metadata.Uint32Ptr(0),
			GIDs: []uint32{0},
		},
		ClientAddr: "127.0.0.1",
	}
	dir, _, err := svc.CreateDirectory(rootCtx, rootHandle, "bench", &metadata.FileAttr{Mode: 0o777})
	require.NoError(b, err)
	dirHandle, err := metadata.EncodeShareHandle(shareName, dir.ID)
	require.NoError(b, err)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		name := "f" + itoa(i)
		if _, _, err := svc.CreateFile(rootCtx, dirHandle, name, &metadata.FileAttr{Mode: 0o644}); err != nil {
			b.Fatal(err)
		}
	}
}

// itoa is a tiny allocation-light int->string for unique bench names.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
