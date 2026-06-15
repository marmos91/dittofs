package metadata_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

// TestQuotaFSStatOverlay verifies that GetFilesystemStatisticsForIdentity
// narrows the reported total/available to the caller's per-identity quota,
// using that identity's own usage — what `df` / FSSTAT / SMB FS_FULL_SIZE show.
func TestQuotaFSStatOverlay(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)
	// Use immediate commits so the per-identity usage counter reflects the
	// committed write synchronously (no NFS COMMIT/flush in this unit test).
	fx.service.SetDeferredCommit(false)

	// uid=1000 gets a 10000-byte quota.
	fx.service.SetIdentityQuota(fx.shareName, metadata.IdentityQuota{
		Scope:      metadata.QuotaScopeUser,
		ID:         1000,
		LimitBytes: 10000,
	})

	uc := fx.userContext()

	// Create a file as uid=1000 and grow+commit it to 4000 bytes so the usage
	// counter reflects real usage.
	file, _, err := fx.service.CreateFile(uc, fx.rootHandle, "u.bin", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(file.ShareName, file.ID)
	require.NoError(t, err)
	op, err := fx.service.PrepareWrite(uc, handle, 4000)
	require.NoError(t, err)
	_, err = fx.service.CommitWrite(uc, op)
	require.NoError(t, err)

	// With identity: total clamps to the 10000-byte quota, available = quota - used.
	stats, err := fx.service.GetFilesystemStatisticsForIdentity(uc.Context, handle, uc.Identity)
	require.NoError(t, err)
	require.Equal(t, uint64(10000), stats.TotalBytes, "total should reflect the user quota")
	require.Equal(t, uint64(6000), stats.AvailableBytes, "available should be quota minus the user's usage")

	// A different (unquota'd) user sees the raw volume, not uid=1000's quota.
	other := fx.authContext(2000, 2000)
	ostats, err := fx.service.GetFilesystemStatisticsForIdentity(other.Context, handle, other.Identity)
	require.NoError(t, err)
	require.NotEqual(t, uint64(10000), ostats.TotalBytes, "unquota'd user must not inherit another user's quota")
}
