package nfs

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/stretchr/testify/require"
)

// Byte-range lock merging lives in the lock manager, which NLM and NFSv4 share,
// so the two protocols must give one answer about the same file. These pin the
// NLM side of that seam: abutting and overlapping ranges from one owner become a
// single lock, and unlocking the union releases all of it.

func newMergeNLMService() (*nlmService, *lock.Manager) {
	lm := lock.NewManager()
	return newNLMService(lm, graceFileChecker{}), lm
}

func TestNLM_AbuttingLocksMergeIntoOneRange(t *testing.T) {
	svc, lm := newMergeNLMService()

	handle := []byte("share-a:merge-file")
	owner := lock.LockOwner{OwnerID: "nlm:client-1", ClientID: "client-1", ShareName: "share-a"}

	// [25,100) then [50,125): overlapping, so one lock spanning [25,125).
	for _, r := range [][2]uint64{{25, 75}, {50, 75}} {
		_, err := svc.LockFileNLM(context.Background(), handle, owner, r[0], r[1], true, false)
		require.NoErrorf(t, err, "LockFileNLM(%d,%d)", r[0], r[1])
	}

	locks := lm.ListUnifiedLocks(string(handle))
	require.Len(t, locks, 1, "same-owner overlapping NLM locks must coalesce into one range")
	require.Equal(t, uint64(25), locks[0].Offset)
	require.Equal(t, uint64(100), locks[0].Length)

	// The merged range is one logical lock, so unlocking the union clears it.
	require.NoError(t, svc.UnlockFileNLM(context.Background(), handle, owner.OwnerID, 25, 100))
	require.Empty(t, lm.ListUnifiedLocks(string(handle)),
		"unlocking the merged union must leave nothing behind")
}

func TestNLM_OtherOwnerRangeNotAbsorbed(t *testing.T) {
	svc, lm := newMergeNLMService()

	handle := []byte("share-a:merge-file")
	first := lock.LockOwner{OwnerID: "nlm:client-1", ClientID: "client-1", ShareName: "share-a"}
	second := lock.LockOwner{OwnerID: "nlm:client-2", ClientID: "client-2", ShareName: "share-a"}

	_, err := svc.LockFileNLM(context.Background(), handle, first, 0, 100, false, false)
	require.NoError(t, err)
	_, err = svc.LockFileNLM(context.Background(), handle, second, 100, 100, false, false)
	require.NoError(t, err)

	require.Len(t, lm.ListUnifiedLocks(string(handle)), 2,
		"merging is per lock-owner: another owner's abutting range must survive intact")
}
