package storetest

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// LockStoreFactory creates a fresh lock.LockStore for each test. Every metadata
// backend implements lock.LockStore (memory, badger, postgres), so the factory
// can simply return the backend's *MetadataStore. The factory receives
// *testing.T so filesystem-backed stores can use t.TempDir()/t.Cleanup().
type LockStoreFactory func(t *testing.T) lock.LockStore

// RunLockPersistenceSuite is the cross-backend persist->restore round-trip
// conformance net for lock persistence. It mechanically catches the
// field-drop bug class: any identity / range / type / flag / lease /
// delegation field a backend forgets to serialize fails the suite.
//
// Every backend wires this into its *_conformance_test.go via its own
// factory, so a new field on PersistedLock (or a backend that forgets to
// persist an existing one) fails automatically through the reflection guard
// in AllFieldsPreserved.
//
// The suite asserts, for every lock shape:
//   - PutLock -> GetLock(id) preserves every PersistedLock field (deep equal,
//     AcquiredAt compared within tolerance).
//   - ListLocks(LockQuery{ShareName}) recovers the record (the ShareName=""
//     drop class).
//   - ListLocks(LockQuery{ClientID}) + DeleteLocksByClient match on ClientID
//     (the client-leak class).
//   - Stacked / split-fragment records persist under DISTINCT ids (collision
//     class) and all survive a read-back.
func RunLockPersistenceSuite(t *testing.T, factory LockStoreFactory) {
	t.Helper()

	t.Run("AllFieldsPreserved", func(t *testing.T) {
		testLock_AllFieldsPreserved(t, factory)
	})

	t.Run("Shapes", func(t *testing.T) {
		for _, sh := range lockShapes() {
			sh := sh
			t.Run(sh.name, func(t *testing.T) {
				testLock_ShapeRoundTrip(t, factory, sh)
			})
		}
	})

	t.Run("StackedDistinctIDs", func(t *testing.T) {
		testLock_StackedDistinctIDs(t, factory)
	})

	t.Run("SplitFragmentsDistinctIDs", func(t *testing.T) {
		testLock_SplitFragmentsDistinctIDs(t, factory)
	})

	t.Run("ClientCleanup", func(t *testing.T) {
		testLock_ClientCleanup(t, factory)
	})

	t.Run("ZeroByteVsEOFSemantics", func(t *testing.T) {
		testLock_ZeroByteVsEOFSemantics(t, factory)
	})

	t.Run("MaxUint64OffsetLength", func(t *testing.T) {
		testLock_MaxUint64OffsetLength(t, factory)
	})

	t.Run("CleanShutdownMarker", func(t *testing.T) {
		testLock_CleanShutdownMarker(t, factory)
	})
}

// testLock_CleanShutdownMarker pins the clean-shutdown marker round-trip across
// every backend (area-4 H7). The marker drives the grace-entry decision on
// boot, so a backend that fails to round-trip it would either never enter grace
// after a crash (lock-steal window) or always enter grace (90s wedge on every
// restart). The contract this suite enforces on all three backends:
//
//   - SetCleanShutdown(false) then GetCleanShutdown reports false (the boot path
//     clears the marker for the running session immediately after reading it;
//     this also establishes the fail-safe unclean baseline a fresh store has).
//   - SetCleanShutdown(true) then GetCleanShutdown reports true (graceful Close).
//   - Toggling back to false reads false again.
//   - The marker is INDEPENDENT of the server epoch: bumping the epoch must not
//     flip it (a real divergence risk on backends — like postgres — that store
//     both on one singleton row).
//
// The "absent marker defaults to false" property is asserted by the per-backend
// unit tests rather than here, because some backend conformance factories open,
// reset, then gracefully Close() a seed store before handing back a reopened
// one — which legitimately leaves a persisted clean=true marker, not an absent
// one.
func testLock_CleanShutdownMarker(t *testing.T, factory LockStoreFactory) {
	store := factory(t)
	ctx := context.Background()

	// Boot clears the marker for the running session -> false (unclean baseline).
	if err := store.SetCleanShutdown(ctx, false); err != nil {
		t.Fatalf("SetCleanShutdown(false): %v", err)
	}
	got, err := store.GetCleanShutdown(ctx)
	if err != nil {
		t.Fatalf("GetCleanShutdown (after false): %v", err)
	}
	if got {
		t.Fatalf("after SetCleanShutdown(false), GetCleanShutdown = true; unclean marker not persisted")
	}

	// Mark clean (graceful Close) -> true.
	if err := store.SetCleanShutdown(ctx, true); err != nil {
		t.Fatalf("SetCleanShutdown(true): %v", err)
	}
	got, err = store.GetCleanShutdown(ctx)
	if err != nil {
		t.Fatalf("GetCleanShutdown (after true): %v", err)
	}
	if !got {
		t.Fatalf("after SetCleanShutdown(true), GetCleanShutdown = false; clean marker not persisted")
	}

	// Toggle back -> false.
	if err := store.SetCleanShutdown(ctx, false); err != nil {
		t.Fatalf("SetCleanShutdown(false) #2: %v", err)
	}
	// Read-back: backends that silently ignore the false write must fail here.
	got, err = store.GetCleanShutdown(ctx)
	if err != nil {
		t.Fatalf("GetCleanShutdown (after false #2): %v", err)
	}
	if got {
		t.Fatalf("after SetCleanShutdown(false) #2, GetCleanShutdown = true; toggle-to-false not persisted (grace-period wedge class)")
	}

	// Marker must be independent of the server epoch on backends that share a
	// singleton row (postgres): bumping the epoch must not flip the marker.
	if err := store.SetCleanShutdown(ctx, true); err != nil {
		t.Fatalf("SetCleanShutdown(true) pre-epoch: %v", err)
	}
	if _, err := store.IncrementServerEpoch(ctx); err != nil {
		t.Fatalf("IncrementServerEpoch: %v", err)
	}
	got, err = store.GetCleanShutdown(ctx)
	if err != nil {
		t.Fatalf("GetCleanShutdown (after epoch bump): %v", err)
	}
	if !got {
		t.Fatalf("IncrementServerEpoch cleared the clean-shutdown marker; marker and epoch must be independent")
	}
}

// testLock_MaxUint64OffsetLength pins R3-4: NFSv4 expresses an unbounded range
// as Offset/Length = 0xFFFFFFFFFFFFFFFF and SMB allows high-bit offsets. A
// backend storing these in a signed 64-bit column (postgres BIGINT) rejects any
// uint64 > MaxInt64 at PutLock, silently dropping the lock so it is never
// persisted nor recovered. The store must round-trip the full uint64 range.
func testLock_MaxUint64OffsetLength(t *testing.T, factory LockStoreFactory) {
	store := factory(t)
	ctx := context.Background()

	const maxU64 = ^uint64(0) // 0xFFFFFFFFFFFFFFFF, > math.MaxInt64

	lk := &lock.PersistedLock{
		ID:                "max-u64",
		ShareName:         shapeShareName,
		FileID:            shapeFileID,
		OwnerID:           "nfs4:1:deadbeef",
		ClientID:          "nfs4:1",
		LockType:          int(lock.LockTypeExclusive),
		Offset:            maxU64,
		Length:            maxU64,
		IsLegacyByteRange: false,
		AcquiredAt:        time.Unix(1, 0).UTC(),
		ServerEpoch:       1,
	}

	if err := store.PutLock(ctx, lk); err != nil {
		t.Fatalf("PutLock with uint64 max Offset/Length failed (signed-column overflow class): %v", err)
	}

	got, err := store.GetLock(ctx, lk.ID)
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}
	if got.Offset != maxU64 {
		t.Errorf("Offset round-trip: got %d, want %d (uint64 max)", got.Offset, maxU64)
	}
	if got.Length != maxU64 {
		t.Errorf("Length round-trip: got %d, want %d (uint64 max)", got.Length, maxU64)
	}

	// The per-share recovery query must also recover it.
	byShare, err := store.ListLocks(ctx, lock.LockQuery{ShareName: shapeShareName})
	if err != nil {
		t.Fatalf("ListLocks{ShareName}: %v", err)
	}
	if findByID(byShare, lk.ID) == nil {
		t.Fatalf("max-uint64 lock not recovered by per-share query (dropped on persist)")
	}
}

// lockShape is one entry in the persist matrix: a fully-formed PersistedLock
// plus the invariant assertions specific to its semantics.
type lockShape struct {
	name string
	lock *lock.PersistedLock
}

const (
	shapeShareName = "persist-suite-share"
	shapeFileID    = "persist-suite-share:file-1"
)

// lockShapes enumerates every field-carrying PersistedLock variant the
// persistence layer must round-trip. Each shape stamps ShareName + ClientID so
// the per-share recovery query and the client-cleanup path are exercised for
// every shape, not just byte-range locks.
func lockShapes() []lockShape {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	leaseKey := bytes16(0xA0)
	parentLeaseKey := bytes16(0xB0)

	return []lockShape{
		{
			name: "SMB_ByteRange_Exclusive_Legacy",
			lock: &lock.PersistedLock{
				ID:                "smb-br-excl",
				ShareName:         shapeShareName,
				FileID:            shapeFileID,
				OwnerID:           "smb:7:open-1",
				ClientID:          "smb:7",
				LockType:          int(lock.LockTypeExclusive),
				Offset:            100,
				Length:            50,
				IsLegacyByteRange: true,
				AccessMode:        int(lock.AccessModeNone),
				AcquiredAt:        base,
				ServerEpoch:       3,
			},
		},
		{
			name: "SMB_ByteRange_Shared_Legacy",
			lock: &lock.PersistedLock{
				ID:                "smb-br-shared",
				ShareName:         shapeShareName,
				FileID:            shapeFileID,
				OwnerID:           "smb:7:open-1",
				ClientID:          "smb:7",
				LockType:          int(lock.LockTypeShared),
				Offset:            100,
				Length:            50,
				IsLegacyByteRange: true,
				AccessMode:        int(lock.AccessModeDenyWrite),
				AcquiredAt:        base,
				ServerEpoch:       3,
			},
		},
		{
			name: "SMB_ZeroByte_NotEOF",
			lock: &lock.PersistedLock{
				ID:                "smb-zerobyte",
				ShareName:         shapeShareName,
				FileID:            shapeFileID,
				OwnerID:           "smb:7:open-zb",
				ClientID:          "smb:7",
				LockType:          int(lock.LockTypeExclusive),
				Offset:            10,
				Length:            0,
				IsZeroByte:        true, // must NOT restore as unbounded
				IsLegacyByteRange: true,
				AcquiredAt:        base,
				ServerEpoch:       3,
			},
		},
		{
			name: "SMB_ByteRange_ToEOF",
			lock: &lock.PersistedLock{
				ID:                "smb-eof",
				ShareName:         shapeShareName,
				FileID:            shapeFileID,
				OwnerID:           "smb:7:open-eof",
				ClientID:          "smb:7",
				LockType:          int(lock.LockTypeExclusive),
				Offset:            10,
				Length:            0,
				IsZeroByte:        false, // unbounded to-EOF
				IsLegacyByteRange: true,
				AcquiredAt:        base,
				ServerEpoch:       3,
			},
		},
		{
			name: "NLM_Unified_ByteRange",
			lock: &lock.PersistedLock{
				ID:          "nlm-br",
				ShareName:   shapeShareName, // stamped by manager even though producer leaves it ""
				FileID:      shapeFileID,
				OwnerID:     "nlm:client-x:pid123",
				ClientID:    "nlm:client-x",
				LockType:    int(lock.LockTypeExclusive),
				Offset:      0,
				Length:      4096,
				AcquiredAt:  base,
				ServerEpoch: 3,
			},
		},
		{
			name: "NFSv4_Unified_ByteRange",
			lock: &lock.PersistedLock{
				ID:          "nfs4-br",
				ShareName:   shapeShareName,
				FileID:      shapeFileID,
				OwnerID:     "nfs4:1:deadbeef",
				ClientID:    "nfs4:1",
				LockType:    int(lock.LockTypeShared),
				Offset:      8192,
				Length:      0, // to-EOF
				AcquiredAt:  base,
				ServerEpoch: 3,
			},
		},
		{
			name: "Lease_V2_Breaking",
			lock: &lock.PersistedLock{
				ID:                  "lease-v2",
				ShareName:           shapeShareName,
				FileID:              shapeFileID,
				OwnerID:             "smb:9:lease",
				ClientID:            "smb:9",
				LockType:            int(lock.LockTypeExclusive),
				AcquiredAt:          base,
				ServerEpoch:         3,
				LeaseKey:            leaseKey,
				LeaseState:          lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle,
				LeaseEpoch:          7,
				BreakToState:        lock.LeaseStateRead | lock.LeaseStateHandle,
				BreakingToRequired:  lock.LeaseStateRead,
				Breaking:            true,
				ParentLeaseKey:      parentLeaseKey,
				IsDirectory:         true,
				IsTraditionalOplock: false,
			},
		},
		{
			name: "TraditionalOplock",
			lock: &lock.PersistedLock{
				ID:                  "trad-oplock",
				ShareName:           shapeShareName,
				FileID:              shapeFileID,
				OwnerID:             "smb:11:oplock",
				ClientID:            "smb:11",
				LockType:            int(lock.LockTypeExclusive),
				AcquiredAt:          base,
				ServerEpoch:         3,
				LeaseKey:            bytes16(0xC0),
				LeaseState:          lock.LeaseStateRead | lock.LeaseStateWrite,
				LeaseEpoch:          1,
				IsTraditionalOplock: true,
			},
		},
		{
			name: "Delegation_Write_Recalled",
			lock: &lock.PersistedLock{
				ID:                    "deleg-write",
				ShareName:             shapeShareName,
				FileID:                shapeFileID,
				OwnerID:               "nfs4:2:deleg",
				ClientID:              "nfs4:2",
				LockType:              int(lock.LockTypeExclusive),
				AcquiredAt:            base,
				ServerEpoch:           3,
				DelegationID:          "deleg-uuid-1",
				DelegType:             int(lock.DelegTypeWrite),
				DelegBreaking:         true,
				DelegRecalled:         true,
				DelegRevoked:          false,
				DelegNotificationMask: 0xDEAD,
				IsDirectory:           true,
			},
		},
	}
}

// testLock_AllFieldsPreserved is the reflection guard. It builds a
// kitchen-sink PersistedLock with EVERY exported field set to a distinct
// non-zero sentinel, round-trips it through PutLock/GetLock, and asserts deep
// equality on every field. A backend that forgets to serialize any field —
// including a field added in the future — fails here automatically.
func testLock_AllFieldsPreserved(t *testing.T, factory LockStoreFactory) {
	store := factory(t)
	ctx := context.Background()

	want := kitchenSinkLock()

	// Fail loudly if a future field is left zero in the fixture: the guard is
	// only meaningful if every field is non-zero before the round-trip.
	if zero := firstZeroExportedField(want); zero != "" {
		t.Fatalf("kitchenSinkLock leaves PersistedLock.%s at its zero value; "+
			"add a non-zero sentinel so the field-preservation guard covers it", zero)
	}

	if err := store.PutLock(ctx, want); err != nil {
		t.Fatalf("PutLock: %v", err)
	}

	got, err := store.GetLock(ctx, want.ID)
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}

	assertLockEqual(t, want, got)
}

// kitchenSinkLock returns a PersistedLock with every exported field set to a
// distinct non-zero value. LeaseKey/ParentLeaseKey carry valid 16-byte keys so
// IsLease() classification stays stable across the round-trip.
//
// NOTE: a lease and a delegation never legitimately coexist on one record
// (ToPersistedLock guards the invariant), but the persistence layer must still
// faithfully store and return whatever bytes it was handed — that is precisely
// the property a field-drop guard verifies. Setting both here maximizes field
// coverage without asking the persistence layer to interpret semantics.
func kitchenSinkLock() *lock.PersistedLock {
	return &lock.PersistedLock{
		ID:                    "kitchen-sink",
		ShareName:             "kitchen-share",
		FileID:                "kitchen-share:file-ks",
		OwnerID:               "proto:owner:ks",
		ClientID:              "proto:client:ks",
		LockType:              int(lock.LockTypeExclusive),
		Offset:                4096,
		Length:                8192,
		IsZeroByte:            true,
		IsLegacyByteRange:     true,
		AccessMode:            int(lock.AccessModeDenyAll),
		AcquiredAt:            time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		ServerEpoch:           42,
		LeaseKey:              bytes16(0x11),
		LeaseState:            lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle,
		LeaseEpoch:            9,
		BreakToState:          lock.LeaseStateRead | lock.LeaseStateHandle,
		BreakingToRequired:    lock.LeaseStateRead,
		Breaking:              true,
		BreakStarted:          time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
		ParentLeaseKey:        bytes16(0x22),
		IsDirectory:           true,
		IsTraditionalOplock:   true,
		DelegationID:          "deleg-ks",
		DelegType:             int(lock.DelegTypeWrite),
		DelegBreaking:         true,
		DelegRecalled:         true,
		DelegRevoked:          true,
		DelegNotificationMask: 0xCAFE,
	}
}

// testLock_ShapeRoundTrip persists one matrix shape, asserts full-field
// preservation via GetLock, and asserts the per-share recovery query
// (ListLocks{ShareName}) finds it (the ShareName="" drop class).
func testLock_ShapeRoundTrip(t *testing.T, factory LockStoreFactory, sh lockShape) {
	store := factory(t)
	ctx := context.Background()

	if err := store.PutLock(ctx, sh.lock); err != nil {
		t.Fatalf("PutLock: %v", err)
	}

	// GetLock by ID preserves every field.
	got, err := store.GetLock(ctx, sh.lock.ID)
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}
	assertLockEqual(t, sh.lock, got)

	// Per-share recovery query must find it (the ShareName="" class — R2).
	byShare, err := store.ListLocks(ctx, lock.LockQuery{ShareName: sh.lock.ShareName})
	if err != nil {
		t.Fatalf("ListLocks{ShareName}: %v", err)
	}
	found := findByID(byShare, sh.lock.ID)
	if found == nil {
		t.Fatalf("ListLocks{ShareName:%q} did not recover lock %q (ShareName drop class)",
			sh.lock.ShareName, sh.lock.ID)
	}
	assertLockEqual(t, sh.lock, found)

	// Lease/delegation classification must survive the round-trip.
	if got.IsLease() != sh.lock.IsLease() {
		t.Errorf("IsLease() classification changed: got %v, want %v",
			got.IsLease(), sh.lock.IsLease())
	}
}

// testLock_StackedDistinctIDs verifies two stacked records (same
// owner/offset/length/type, distinct IDs — the SMB shared-lock stacking case)
// both persist and both read back. A backend keyed only on
// owner/offset/length would collapse them into one.
func testLock_StackedDistinctIDs(t *testing.T, factory LockStoreFactory) {
	store := factory(t)
	ctx := context.Background()

	mk := func(id string) *lock.PersistedLock {
		return &lock.PersistedLock{
			ID:                id,
			ShareName:         shapeShareName,
			FileID:            shapeFileID,
			OwnerID:           "smb:7:open-1",
			ClientID:          "smb:7",
			LockType:          int(lock.LockTypeShared),
			Offset:            100,
			Length:            50,
			IsLegacyByteRange: true,
			AcquiredAt:        time.Unix(1, 0).UTC(),
			ServerEpoch:       1,
		}
	}
	a, b := mk("stack-1"), mk("stack-2")
	if err := store.PutLock(ctx, a); err != nil {
		t.Fatalf("PutLock(a): %v", err)
	}
	if err := store.PutLock(ctx, b); err != nil {
		t.Fatalf("PutLock(b): %v", err)
	}

	got, err := store.ListLocks(ctx, lock.LockQuery{ShareName: shapeShareName})
	if err != nil {
		t.Fatalf("ListLocks: %v", err)
	}
	if findByID(got, "stack-1") == nil || findByID(got, "stack-2") == nil {
		t.Fatalf("both stacked records must persist under distinct ids; got ids %v", idsOf(got))
	}
}

// testLock_SplitFragmentsDistinctIDs verifies the split-fragment case: take a
// unified byte-range lock, run SplitLock to fragment it, persist both
// fragments under distinct IDs, and assert both read back. SplitLock that
// cloned the original ID verbatim would let the second PutLock overwrite the
// first (store keyed by ID) and silently drop a byte-range on restart — R1.
func testLock_SplitFragmentsDistinctIDs(t *testing.T, factory LockStoreFactory) {
	store := factory(t)
	ctx := context.Background()

	original := &lock.UnifiedLock{
		ID: "split-original",
		Owner: lock.LockOwner{
			OwnerID:   "nlm:client-z",
			ClientID:  "nlm:client-z",
			ShareName: shapeShareName,
		},
		FileHandle: lock.FileHandle(shapeFileID),
		Offset:     0,
		Length:     100,
		Type:       lock.LockTypeExclusive,
	}

	// Unlock the middle [40,60) -> fragments [0,40) and [60,100).
	fragments := lock.SplitLock(original, 40, 20)
	if len(fragments) != 2 {
		t.Fatalf("SplitLock yielded %d fragments, want 2", len(fragments))
	}
	if fragments[0].ID == fragments[1].ID {
		t.Fatalf("SplitLock produced colliding fragment IDs %q — split-loss bug class (R1)",
			fragments[0].ID)
	}

	for _, frag := range fragments {
		if err := store.PutLock(ctx, lock.ToPersistedLock(frag, 1)); err != nil {
			t.Fatalf("PutLock(fragment %s): %v", frag.ID, err)
		}
	}

	got, err := store.ListLocks(ctx, lock.LockQuery{ShareName: shapeShareName})
	if err != nil {
		t.Fatalf("ListLocks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("both split fragments must persist under distinct ids; got %d records: %v",
			len(got), idsOf(got))
	}

	var ranges [][2]uint64
	for _, r := range got {
		ranges = append(ranges, [2]uint64{r.Offset, r.Length})
	}
	if !containsRange(ranges, 0, 40) || !containsRange(ranges, 60, 40) {
		t.Fatalf("restored fragments must be [0,40) and [60,100); got %v", ranges)
	}
}

// testLock_ClientCleanup verifies the ClientID is persisted in a form that the
// per-client recovery query and DeleteLocksByClient match — the leak class
// (R3): without it a disconnecting client's persisted rows survive forever and
// resurrect on the next restart.
func testLock_ClientCleanup(t *testing.T, factory LockStoreFactory) {
	store := factory(t)
	ctx := context.Background()

	const clientA, clientB = "smb:7", "smb:8"
	locks := []*lock.PersistedLock{
		{ID: "c-a1", ShareName: shapeShareName, FileID: shapeFileID, OwnerID: "o1", ClientID: clientA, IsLegacyByteRange: true, AcquiredAt: time.Unix(1, 0).UTC()},
		{ID: "c-a2", ShareName: shapeShareName, FileID: shapeFileID, OwnerID: "o2", ClientID: clientA, IsLegacyByteRange: true, AcquiredAt: time.Unix(1, 0).UTC()},
		{ID: "c-b1", ShareName: shapeShareName, FileID: shapeFileID, OwnerID: "o3", ClientID: clientB, IsLegacyByteRange: true, AcquiredAt: time.Unix(1, 0).UTC()},
	}
	for _, lk := range locks {
		if err := store.PutLock(ctx, lk); err != nil {
			t.Fatalf("PutLock(%s): %v", lk.ID, err)
		}
	}

	// Per-client recovery query matches on the persisted ClientID.
	byClient, err := store.ListLocks(ctx, lock.LockQuery{ClientID: clientA})
	if err != nil {
		t.Fatalf("ListLocks{ClientID}: %v", err)
	}
	if len(byClient) != 2 {
		t.Fatalf("ListLocks{ClientID:%q} = %d, want 2 (client-id drop class)", clientA, len(byClient))
	}

	// DeleteLocksByClient removes exactly clientA's rows.
	n, err := store.DeleteLocksByClient(ctx, clientA)
	if err != nil {
		t.Fatalf("DeleteLocksByClient: %v", err)
	}
	if n != 2 {
		t.Errorf("DeleteLocksByClient returned %d, want 2", n)
	}

	remaining, err := store.ListLocks(ctx, lock.LockQuery{ShareName: shapeShareName})
	if err != nil {
		t.Fatalf("ListLocks (post-delete): %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != "c-b1" {
		t.Fatalf("after DeleteLocksByClient(%q) only c-b1 must remain; got %v", clientA, idsOf(remaining))
	}
}

// testLock_ZeroByteVsEOFSemantics asserts the IsZeroByte discriminator survives
// the round-trip so the restored lock is treated correctly by the conflict
// logic that distinguishes them: a restored zero-byte lock is NOT unbounded; a
// restored Length==0/!IsZeroByte lock IS unbounded (to-EOF). The check is done
// via the in-memory FileLock conflict path after reconstruction, which is the
// code that actually consumes the flag.
func testLock_ZeroByteVsEOFSemantics(t *testing.T, factory LockStoreFactory) {
	store := factory(t)
	ctx := context.Background()

	zb := &lock.PersistedLock{
		ID: "sem-zerobyte", ShareName: shapeShareName, FileID: shapeFileID,
		OwnerID: "smb:7:open-zb", ClientID: "smb:7",
		LockType: int(lock.LockTypeExclusive), Offset: 10, Length: 0,
		IsZeroByte: true, IsLegacyByteRange: true, AcquiredAt: time.Unix(1, 0).UTC(),
	}
	eof := &lock.PersistedLock{
		ID: "sem-eof", ShareName: shapeShareName, FileID: shapeFileID + "-2",
		OwnerID: "smb:7:open-eof", ClientID: "smb:7",
		LockType: int(lock.LockTypeExclusive), Offset: 10, Length: 0,
		IsZeroByte: false, IsLegacyByteRange: true, AcquiredAt: time.Unix(1, 0).UTC(),
	}
	for _, lk := range []*lock.PersistedLock{zb, eof} {
		if err := store.PutLock(ctx, lk); err != nil {
			t.Fatalf("PutLock(%s): %v", lk.ID, err)
		}
	}

	gotZB, err := store.GetLock(ctx, zb.ID)
	if err != nil {
		t.Fatalf("GetLock(zerobyte): %v", err)
	}
	if !gotZB.IsZeroByte {
		t.Fatalf("restored zero-byte lock lost IsZeroByte — would be treated as unbounded to-EOF")
	}

	gotEOF, err := store.GetLock(ctx, eof.ID)
	if err != nil {
		t.Fatalf("GetLock(eof): %v", err)
	}
	if gotEOF.IsZeroByte {
		t.Fatalf("restored to-EOF lock gained IsZeroByte — would stop being unbounded")
	}

	// Reconstruct into a fresh manager and assert the conflict semantics differ:
	// the zero-byte lock guards only its single offset; the to-EOF lock guards
	// everything from its offset onward.
	mgr := lock.NewManager()
	if err := mgr.RestoreLocks([]*lock.PersistedLock{gotZB, gotEOF}); err != nil {
		t.Fatalf("RestoreLocks: %v", err)
	}

	// A different open writing at offset 1000 on the zero-byte file must NOT
	// conflict (zero-byte lock is bounded to its single point).
	if c := mgr.CheckForIO(zb.FileID, "open-other", 99, 1000, 10, true); c != nil {
		t.Errorf("restored zero-byte lock wrongly blocked IO far past its offset (treated as unbounded)")
	}
	// The same write at offset 1000 on the to-EOF file MUST conflict.
	if c := mgr.CheckForIO(eof.FileID, "open-other", 99, 1000, 10, true); c == nil {
		t.Errorf("restored to-EOF lock failed to block IO past its offset (lost unbounded semantics)")
	}
}

// ============================================================================
// Helpers
// ============================================================================

// acquiredAtTolerance bounds AcquiredAt drift across a round-trip. Postgres
// timestamptz is microsecond-resolution and may not preserve monotonic-clock
// or sub-microsecond components; everything coarser must match exactly.
const acquiredAtTolerance = time.Millisecond

// assertLockEqual asserts every PersistedLock field matches, comparing
// AcquiredAt within tolerance and everything else for exact equality. It uses
// reflection so a newly added field is compared automatically without editing
// this helper.
func assertLockEqual(t *testing.T, want, got *lock.PersistedLock) {
	t.Helper()
	if got == nil {
		t.Fatalf("got nil lock, want %+v", want)
	}

	wv := reflect.ValueOf(*want)
	gv := reflect.ValueOf(*got)
	ty := wv.Type()

	for i := 0; i < ty.NumField(); i++ {
		f := ty.Field(i)
		wfv := wv.Field(i)
		gfv := gv.Field(i)

		if f.Type == reflect.TypeOf(time.Time{}) {
			wt := wfv.Interface().(time.Time)
			gt := gfv.Interface().(time.Time)
			if d := wt.Sub(gt); d < -acquiredAtTolerance || d > acquiredAtTolerance {
				t.Errorf("field %s: time drift %v exceeds tolerance (want %v, got %v)", f.Name, d, wt, gt)
			}
			continue
		}

		if !reflect.DeepEqual(wfv.Interface(), gfv.Interface()) {
			t.Errorf("field %s dropped/changed in round-trip: want %v, got %v", f.Name, wfv.Interface(), gfv.Interface())
		}
	}
}

// firstZeroExportedField returns the name of the first exported field left at
// its zero value, or "" if all are non-zero. Used to guarantee the
// kitchen-sink fixture actually exercises every field.
func firstZeroExportedField(pl *lock.PersistedLock) string {
	v := reflect.ValueOf(*pl)
	ty := v.Type()
	for i := 0; i < ty.NumField(); i++ {
		if v.Field(i).IsZero() {
			return ty.Field(i).Name
		}
	}
	return ""
}

func bytes16(seed byte) []byte {
	b := make([]byte, 16)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

func findByID(locks []*lock.PersistedLock, id string) *lock.PersistedLock {
	for _, lk := range locks {
		if lk.ID == id {
			return lk
		}
	}
	return nil
}

func idsOf(locks []*lock.PersistedLock) []string {
	ids := make([]string, 0, len(locks))
	for _, lk := range locks {
		ids = append(ids, lk.ID)
	}
	return ids
}

func containsRange(ranges [][2]uint64, offset, length uint64) bool {
	for _, r := range ranges {
		if r[0] == offset && r[1] == length {
			return true
		}
	}
	return false
}
