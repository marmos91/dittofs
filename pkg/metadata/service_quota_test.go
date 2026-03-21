package metadata_test

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Quota Test Helpers
// ============================================================================

// quotaFixture extends testFixture with quota-specific helpers.
type quotaFixture struct {
	t          *testing.T
	service    *metadata.MetadataService
	store      *memory.MemoryMetadataStore
	shareName  string
	rootHandle metadata.FileHandle
}

func newQuotaFixture(t *testing.T) *quotaFixture {
	t.Helper()

	store := memory.NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()
	shareName := "/test"

	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o777,
		UID:  0,
		GID:  0,
	})
	require.NoError(t, err)

	rootHandle, err := metadata.EncodeShareHandle(shareName, rootFile.ID)
	require.NoError(t, err)

	svc := metadata.New()
	svc.SetDeferredCommit(false) // Use immediate commits for test clarity
	err = svc.RegisterStoreForShare(shareName, store)
	require.NoError(t, err)

	return &quotaFixture{
		t:          t,
		service:    svc,
		store:      store,
		shareName:  shareName,
		rootHandle: rootHandle,
	}
}

func (f *quotaFixture) rootContext() *metadata.AuthContext {
	return &metadata.AuthContext{
		Context:    context.Background(),
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID:  metadata.Uint32Ptr(0),
			GID:  metadata.Uint32Ptr(0),
			GIDs: []uint32{0},
		},
		ClientAddr: "127.0.0.1",
	}
}

func (f *quotaFixture) createFileWithSize(name string, size uint64) metadata.FileHandle {
	f.t.Helper()
	ctx := f.rootContext()

	file, err := f.service.CreateFile(ctx, f.rootHandle, name, &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o644,
		UID:  0,
		GID:  0,
	})
	require.NoError(f.t, err)

	handle, err := metadata.EncodeShareHandle(f.shareName, file.ID)
	require.NoError(f.t, err)

	if size > 0 {
		// Set file size via direct store transaction to simulate existing file data
		storeCtx := context.Background()
		err = f.store.WithTransaction(storeCtx, func(tx metadata.Transaction) error {
			storedFile, err := tx.GetFile(storeCtx, handle)
			if err != nil {
				return err
			}
			storedFile.Size = size
			storedFile.Mtime = time.Now()
			storedFile.Ctime = time.Now()
			return tx.PutFile(storeCtx, storedFile)
		})
		require.NoError(f.t, err)
	}

	return handle
}

// ============================================================================
// PrepareWrite Quota Tests
// ============================================================================

func TestPrepareWrite_QuotaExceeded(t *testing.T) {
	t.Parallel()
	f := newQuotaFixture(t)

	// Set quota of 4000 bytes
	f.service.SetQuotaForShare(f.shareName, 4000)

	// Create a file of size 0
	handle := f.createFileWithSize("test.txt", 0)

	// Attempt to write 5000 bytes (exceeds 4000 quota)
	_, err := f.service.PrepareWrite(f.rootContext(), handle, 5000)
	require.Error(t, err)

	var storeErr *metadata.StoreError
	require.ErrorAs(t, err, &storeErr)
	assert.Equal(t, metadata.ErrNoSpace, storeErr.Code)
}

func TestPrepareWrite_QuotaFits(t *testing.T) {
	t.Parallel()
	f := newQuotaFixture(t)

	// Set quota of 4000 bytes
	f.service.SetQuotaForShare(f.shareName, 4000)

	// Create a file of size 0
	handle := f.createFileWithSize("test.txt", 0)

	// Write 3000 bytes (fits within 4000 quota)
	op, err := f.service.PrepareWrite(f.rootContext(), handle, 3000)
	require.NoError(t, err)
	assert.NotNil(t, op)
	assert.Equal(t, uint64(3000), op.NewSize)
}

func TestPrepareWrite_QuotaUnlimited(t *testing.T) {
	t.Parallel()
	f := newQuotaFixture(t)

	// Quota 0 = unlimited (no enforcement)
	// Do not set any quota (default is 0)

	// Create a file of size 0
	handle := f.createFileWithSize("test.txt", 0)

	// Write any amount should succeed with no quota
	op, err := f.service.PrepareWrite(f.rootContext(), handle, 100_000_000)
	require.NoError(t, err)
	assert.NotNil(t, op)
}

func TestPrepareWrite_QuotaTruncateAllowed(t *testing.T) {
	t.Parallel()
	f := newQuotaFixture(t)

	// Set quota of 4000 bytes
	f.service.SetQuotaForShare(f.shareName, 4000)

	// Create a file that is 5000 bytes (over quota -- could happen if quota is set after file creation)
	handle := f.createFileWithSize("big.txt", 5000)

	// Truncate from 5000 to 1000 -- should always be allowed even at/over quota
	op, err := f.service.PrepareWrite(f.rootContext(), handle, 1000)
	require.NoError(t, err)
	assert.NotNil(t, op)
	assert.Equal(t, uint64(1000), op.NewSize)
}

// ============================================================================
// GetFilesystemStatistics Quota Tests
// ============================================================================

func TestGetFilesystemStatistics_QuotaTotalBytes(t *testing.T) {
	t.Parallel()
	f := newQuotaFixture(t)

	// Set quota of 10000 bytes
	f.service.SetQuotaForShare(f.shareName, 10000)

	stats, err := f.service.GetFilesystemStatistics(context.Background(), f.rootHandle)
	require.NoError(t, err)

	// TotalBytes should be the quota value
	assert.Equal(t, uint64(10000), stats.TotalBytes)
}

func TestGetFilesystemStatistics_QuotaAvailableBytes(t *testing.T) {
	t.Parallel()
	f := newQuotaFixture(t)

	// Set quota of 10000 bytes
	f.service.SetQuotaForShare(f.shareName, 10000)

	// Create some files to consume 3000 bytes
	f.createFileWithSize("file1.txt", 1000)
	f.createFileWithSize("file2.txt", 2000)

	stats, err := f.service.GetFilesystemStatistics(context.Background(), f.rootHandle)
	require.NoError(t, err)

	// AvailableBytes should be quota - used = 10000 - 3000 = 7000
	assert.Equal(t, uint64(10000), stats.TotalBytes)
	assert.Equal(t, uint64(3000), stats.UsedBytes)
	assert.Equal(t, uint64(7000), stats.AvailableBytes)
}

func TestGetFilesystemStatistics_QuotaOverQuota(t *testing.T) {
	t.Parallel()
	f := newQuotaFixture(t)

	// Create files first, then set a quota that is less than used
	f.createFileWithSize("big1.txt", 3000)
	f.createFileWithSize("big2.txt", 3000)

	// Set quota of 4000 bytes (used=6000 > quota=4000)
	f.service.SetQuotaForShare(f.shareName, 4000)

	stats, err := f.service.GetFilesystemStatistics(context.Background(), f.rootHandle)
	require.NoError(t, err)

	// AvailableBytes should be 0 (not negative)
	assert.Equal(t, uint64(4000), stats.TotalBytes)
	assert.Equal(t, uint64(0), stats.AvailableBytes)
}

func TestGetFilesystemStatistics_NoQuota(t *testing.T) {
	t.Parallel()
	f := newQuotaFixture(t)

	// No quota set (default 0 = unlimited)

	stats, err := f.service.GetFilesystemStatistics(context.Background(), f.rootHandle)
	require.NoError(t, err)

	// Should use store defaults (large value, not the quota)
	assert.True(t, stats.TotalBytes > 0, "TotalBytes should be non-zero with no quota")
	// Should not be capped by any quota overlay
}

func TestPrepareWrite_ZeroByteCreationAtQuota(t *testing.T) {
	t.Parallel()
	f := newQuotaFixture(t)

	// Set quota and fill it
	f.service.SetQuotaForShare(f.shareName, 1000)
	f.createFileWithSize("fill.txt", 1000)

	// Creating a zero-byte file should succeed -- PrepareWrite is not called for zero-byte creates
	// But if PrepareWrite IS called with newSize=0, it should succeed
	handle := f.createFileWithSize("empty.txt", 0)

	// PrepareWrite with newSize=0 (zero-byte write) should succeed even at quota
	op, err := f.service.PrepareWrite(f.rootContext(), handle, 0)
	require.NoError(t, err)
	assert.NotNil(t, op)
}
