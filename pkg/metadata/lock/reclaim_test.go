package lock

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mockClientRecoveryStore implements ClientRecoveryStore for principal-check
// tests. Only ListClientRecovery is exercised by reclaim; the rest are stubs.
type mockClientRecoveryStore struct {
	recs []*V4ClientRecoveryRecord
}

func (m *mockClientRecoveryStore) PutClientRecovery(ctx context.Context, rec *V4ClientRecoveryRecord) error {
	m.recs = append(m.recs, rec)
	return nil
}

func (m *mockClientRecoveryStore) DeleteClientRecovery(ctx context.Context, clientIDString string) error {
	return nil
}

func (m *mockClientRecoveryStore) ListClientRecovery(ctx context.Context) ([]*V4ClientRecoveryRecord, error) {
	return m.recs, nil
}

func (m *mockClientRecoveryStore) RecordReclaimComplete(ctx context.Context, clientIDString string) error {
	return nil
}

// newGraceManagerWithStore builds a Manager in an active grace period backed by
// a fresh mock lock store, sharing the given share name.
func newGraceManagerWithStore(t *testing.T, expected []string) (*Manager, *mockLockStore) {
	t.Helper()
	store := newMockLockStore()
	gpm := NewGracePeriodManager(time.Hour, nil)
	lm := NewManagerWithGracePeriod(gpm)
	lm.SetLockStore(store)
	lm.SetShareName("share-a")
	lm.EnterGracePeriod(expected)
	require.True(t, lm.IsInGracePeriod(), "grace must be active after EnterGracePeriod")
	return lm, store
}

func persistLease(t *testing.T, store *mockLockStore, id, clientID string, leaseKey [16]byte) {
	t.Helper()
	require.NoError(t, store.PutLock(context.Background(), &PersistedLock{
		ID:         id,
		ShareName:  "share-a",
		FileID:     "share-a:file-1",
		ClientID:   clientID,
		LeaseKey:   leaseKey[:],
		LeaseState: LeaseStateRead | LeaseStateHandle,
	}))
}

// TestReclaimLease_RejectsWrongClientID pins the lease-stealing guard: a client
// that did not own the persisted lease must not be able to reclaim it. Before
// the fix reclaimLeaseImpl ignored the caller identity and returned the wrong
// client's lease.
func TestReclaimLease_RejectsWrongClientID(t *testing.T) {
	ctx := context.Background()
	lm, store := newGraceManagerWithStore(t, []string{"client-a"})

	leaseKey := [16]byte{1, 2, 3, 4}
	persistLease(t, store, "lease-1", "client-a", leaseKey)

	lock, err := lm.ReclaimLease(ctx, leaseKey, LeaseStateRead, false, "client-b")
	require.Error(t, err, "reclaim by a non-owning client must be rejected")
	require.Nil(t, lock, "no lock may be returned to a lease-stealing client")
}

// TestReclaimLease_AcceptsMatchingClientID is the positive control: the owning
// client reclaims its own lease successfully.
func TestReclaimLease_AcceptsMatchingClientID(t *testing.T) {
	ctx := context.Background()
	lm, store := newGraceManagerWithStore(t, []string{"client-a"})

	leaseKey := [16]byte{1, 2, 3, 4}
	persistLease(t, store, "lease-1", "client-a", leaseKey)

	lock, err := lm.ReclaimLease(ctx, leaseKey, LeaseStateRead, false, "client-a")
	require.NoError(t, err, "owning client must reclaim its own lease")
	require.NotNil(t, lock)
	require.True(t, lock.Reclaim, "reclaimed lock must carry Reclaim=true")
}

// TestReclaimLease_NoLockStore_NoFileHandle_ReturnsError pins the orphaned-stub
// fix: with no lock store and no in-memory lease, reclaim must return an error
// rather than fabricating a dangling UnifiedLock that nothing can clean up.
func TestReclaimLease_NoLockStore_NoFileHandle_ReturnsError(t *testing.T) {
	ctx := context.Background()
	gpm := NewGracePeriodManager(time.Hour, nil)
	lm := NewManagerWithGracePeriod(gpm)
	lm.EnterGracePeriod([]string{"client-a"})
	require.True(t, lm.IsInGracePeriod())

	leaseKey := [16]byte{9, 9, 9}
	lock, err := lm.ReclaimLease(ctx, leaseKey, LeaseStateRead, false, "client-a")
	require.Error(t, err, "no lock store + no in-memory lease must error, not return a stub")
	require.Nil(t, lock, "no orphan stub may be returned")
	require.Contains(t, err.Error(), "cannot reclaim",
		"error must explain the missing-handle condition")
}

// TestReclaimLease_NoLockStore_InMemoryLeaseFound_MarksReclaim covers the
// no-lock-store path when the lease already lives in memory: it is marked
// reclaimed and returned.
func TestReclaimLease_NoLockStore_InMemoryLeaseFound_MarksReclaim(t *testing.T) {
	ctx := context.Background()
	gpm := NewGracePeriodManager(time.Hour, nil)
	lm := NewManagerWithGracePeriod(gpm)
	lm.EnterGracePeriod([]string{"client-a"})
	require.True(t, lm.IsInGracePeriod())

	leaseKey := [16]byte{4, 5, 6}
	handleKey := "share-a:file-1"
	require.NoError(t, lm.AddUnifiedLock(handleKey, &UnifiedLock{
		ID:         "ul-1",
		Owner:      LockOwner{OwnerID: "smb:s1:f1", ClientID: "client-a"},
		FileHandle: FileHandle(handleKey),
		Lease:      &OpLock{LeaseKey: leaseKey, LeaseState: LeaseStateRead | LeaseStateHandle},
	}))

	lock, err := lm.ReclaimLease(ctx, leaseKey, LeaseStateRead, false, "client-a")
	require.NoError(t, err, "an in-memory lease must reclaim without a lock store")
	require.NotNil(t, lock)
	require.True(t, lock.Reclaim, "reclaimed in-memory lock must carry Reclaim=true")
}

// TestReclaimLeaseWithPrincipal_RejectsWrongPrincipal pins the NFSv4 principal
// guard: a reclaiming client whose principal differs from the one recorded at
// confirm time must NOT reclaim the prior state.
func TestReclaimLeaseWithPrincipal_RejectsWrongPrincipal(t *testing.T) {
	ctx := context.Background()
	lm, store := newGraceManagerWithStore(t, []string{"nfs4:1"})
	lm.SetClientRecoveryStore(&mockClientRecoveryStore{recs: []*V4ClientRecoveryRecord{
		{ClientIDString: "nfs4:1", Principal: "user@REALM"},
	}})

	leaseKey := [16]byte{2, 4, 6, 8}
	persistLease(t, store, "lease-1", "nfs4:1", leaseKey)

	lock, err := lm.ReclaimLeaseWithPrincipal(ctx, leaseKey, LeaseStateRead, false, "nfs4:1", "attacker@REALM")
	require.Error(t, err, "principal mismatch must be rejected")
	require.Nil(t, lock)
}

// TestReclaimLeaseWithPrincipal_AcceptsMatchingPrincipal is the positive
// control: the matching principal reclaims successfully.
func TestReclaimLeaseWithPrincipal_AcceptsMatchingPrincipal(t *testing.T) {
	ctx := context.Background()
	lm, store := newGraceManagerWithStore(t, []string{"nfs4:1"})
	lm.SetClientRecoveryStore(&mockClientRecoveryStore{recs: []*V4ClientRecoveryRecord{
		{ClientIDString: "nfs4:1", Principal: "user@REALM"},
	}})

	leaseKey := [16]byte{2, 4, 6, 8}
	persistLease(t, store, "lease-1", "nfs4:1", leaseKey)

	lock, err := lm.ReclaimLeaseWithPrincipal(ctx, leaseKey, LeaseStateRead, false, "nfs4:1", "user@REALM")
	require.NoError(t, err, "matching principal must reclaim successfully")
	require.NotNil(t, lock)
	require.True(t, lock.Reclaim)
}

// TestReclaimLeaseWithPrincipal_SkipsCheckWhenPrincipalEmpty verifies an empty
// incoming principal skips the principal check entirely (SMB and AUTH_SYS paths
// with no RPCSEC_GSS principal still reclaim).
func TestReclaimLeaseWithPrincipal_SkipsCheckWhenPrincipalEmpty(t *testing.T) {
	ctx := context.Background()
	lm, store := newGraceManagerWithStore(t, []string{"nfs4:1"})
	lm.SetClientRecoveryStore(&mockClientRecoveryStore{recs: []*V4ClientRecoveryRecord{
		{ClientIDString: "nfs4:1", Principal: "user@REALM"},
	}})

	leaseKey := [16]byte{2, 4, 6, 8}
	persistLease(t, store, "lease-1", "nfs4:1", leaseKey)

	lock, err := lm.ReclaimLeaseWithPrincipal(ctx, leaseKey, LeaseStateRead, false, "nfs4:1", "")
	require.NoError(t, err, "empty incoming principal must skip the principal check")
	require.NotNil(t, lock)
	require.True(t, lock.Reclaim)
}

// TestReclaimLeaseWithPrincipal_SkipsCheckWhenRecordPrincipalEmpty verifies that
// a recovery record with no recorded principal does not reject any reclaim:
// there is nothing to compare against (e.g. AUTH_SYS confirm with no principal),
// so the check must be skipped rather than spuriously denying recovery.
func TestReclaimLeaseWithPrincipal_SkipsCheckWhenRecordPrincipalEmpty(t *testing.T) {
	ctx := context.Background()
	lm, store := newGraceManagerWithStore(t, []string{"nfs4:1"})
	lm.SetClientRecoveryStore(&mockClientRecoveryStore{recs: []*V4ClientRecoveryRecord{
		{ClientIDString: "nfs4:1", Principal: ""},
	}})

	leaseKey := [16]byte{2, 4, 6, 8}
	persistLease(t, store, "lease-1", "nfs4:1", leaseKey)

	lock, err := lm.ReclaimLeaseWithPrincipal(ctx, leaseKey, LeaseStateRead, false, "nfs4:1", "anyone@REALM")
	require.NoError(t, err, "an empty recorded principal must skip the principal check")
	require.NotNil(t, lock)
	require.True(t, lock.Reclaim)
}

// TestReclaimLeaseWithPrincipal_NoRecoveryRecord verifies that when no recovery
// record exists for the lease's ClientID, the principal check finds nothing to
// compare and lets the reclaim proceed (the clientID guard already gated entry).
func TestReclaimLeaseWithPrincipal_NoRecoveryRecord(t *testing.T) {
	ctx := context.Background()
	lm, store := newGraceManagerWithStore(t, []string{"nfs4:1"})
	lm.SetClientRecoveryStore(&mockClientRecoveryStore{recs: []*V4ClientRecoveryRecord{
		{ClientIDString: "some-other-client", Principal: "other@REALM"},
	}})

	leaseKey := [16]byte{2, 4, 6, 8}
	persistLease(t, store, "lease-1", "nfs4:1", leaseKey)

	lock, err := lm.ReclaimLeaseWithPrincipal(ctx, leaseKey, LeaseStateRead, false, "nfs4:1", "user@REALM")
	require.NoError(t, err, "no recovery record for the clientID must not block reclaim")
	require.NotNil(t, lock)
	require.True(t, lock.Reclaim)
}
