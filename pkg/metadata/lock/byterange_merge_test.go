package lock

import (
	"context"
	"testing"
)

// Adjacent and overlapping byte-range locks held by one lock-owner on one file
// are a single logical lock (RFC 7530 Section 9.3). AddUnifiedLock must fold a
// new range into the rows it touches instead of stacking fragments, or LOCKT
// reports whichever fragment it scans first and LOCKU over the union leaves the
// unnamed fragments behind.

const mergeHandle = "share-a:merge-file"

func mergeOwnerLock(ownerID string, offset, length uint64, lt LockType) *UnifiedLock {
	return &UnifiedLock{
		Owner:      LockOwner{OwnerID: ownerID, ClientID: "client-1"},
		FileHandle: FileHandle(mergeHandle),
		Offset:     offset,
		Length:     length,
		Type:       lt,
	}
}

// singleLock asserts exactly one lock remains and returns it.
func singleLock(t *testing.T, lm *Manager) *UnifiedLock {
	t.Helper()
	locks := lm.ListUnifiedLocks(mergeHandle)
	if len(locks) != 1 {
		for _, l := range locks {
			t.Logf("lock offset=%d length=%d type=%v owner=%s", l.Offset, l.Length, l.Type, l.Owner.OwnerID)
		}
		t.Fatalf("expected 1 merged lock, got %d", len(locks))
	}
	return locks[0]
}

func TestAddUnifiedLock_MergesSameOwnerRanges(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                   string
		first, second          [2]uint64 // offset, length
		wantOffset, wantLength uint64
	}{
		// The pynfs LOCKMRG case: [25,100) then [50,125) is one lock [25,125).
		{"overlapping", [2]uint64{25, 75}, [2]uint64{50, 75}, 25, 100},
		{"abutting above", [2]uint64{0, 50}, [2]uint64{50, 50}, 0, 100},
		{"abutting below", [2]uint64{50, 50}, [2]uint64{0, 50}, 0, 100},
		{"contained", [2]uint64{0, 100}, [2]uint64{25, 25}, 0, 100},
		{"containing", [2]uint64{25, 25}, [2]uint64{0, 100}, 0, 100},
		{"identical", [2]uint64{10, 10}, [2]uint64{10, 10}, 10, 10},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lm := NewManager()
			for _, r := range [][2]uint64{tc.first, tc.second} {
				if err := lm.AddUnifiedLock(mergeHandle, mergeOwnerLock("owner-1", r[0], r[1], LockTypeExclusive)); err != nil {
					t.Fatalf("AddUnifiedLock(%d,%d): %v", r[0], r[1], err)
				}
			}
			got := singleLock(t, lm)
			if got.Offset != tc.wantOffset || got.Length != tc.wantLength {
				t.Fatalf("merged lock = [%d,%d], want [%d,%d]", got.Offset, got.Length, tc.wantOffset, tc.wantLength)
			}
			if got.Type != LockTypeExclusive {
				t.Fatalf("merged lock type = %v, want exclusive", got.Type)
			}
		})
	}
}

// An unbounded (length 0) range swallows everything above its offset, and the
// merged row must stay unbounded rather than acquire a finite length.
func TestAddUnifiedLock_MergeKeepsUnbounded(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	if err := lm.AddUnifiedLock(mergeHandle, mergeOwnerLock("owner-1", 100, 0, LockTypeExclusive)); err != nil {
		t.Fatalf("AddUnifiedLock(100,EOF): %v", err)
	}
	if err := lm.AddUnifiedLock(mergeHandle, mergeOwnerLock("owner-1", 0, 100, LockTypeExclusive)); err != nil {
		t.Fatalf("AddUnifiedLock(0,100): %v", err)
	}

	got := singleLock(t, lm)
	if got.Offset != 0 || got.Length != 0 {
		t.Fatalf("merged lock = [%d,%d], want [0,EOF]", got.Offset, got.Length)
	}
}

// A new range that bridges the gap between two rows must absorb both, leaving a
// single span rather than merging only the row it happens to meet first. The
// rows go in descending order so the disjoint check below is also asked the
// question the wrong way round, where the existing row starts after the new one.
func TestAddUnifiedLock_MergeBridgesTwoRanges(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	for _, r := range [][2]uint64{{90, 10}, {0, 10}} {
		if err := lm.AddUnifiedLock(mergeHandle, mergeOwnerLock("owner-1", r[0], r[1], LockTypeExclusive)); err != nil {
			t.Fatalf("AddUnifiedLock(%d,%d): %v", r[0], r[1], err)
		}
	}
	if len(lm.ListUnifiedLocks(mergeHandle)) != 2 {
		t.Fatalf("disjoint ranges should not have merged")
	}

	if err := lm.AddUnifiedLock(mergeHandle, mergeOwnerLock("owner-1", 10, 80, LockTypeExclusive)); err != nil {
		t.Fatalf("AddUnifiedLock(10,80): %v", err)
	}

	got := singleLock(t, lm)
	if got.Offset != 0 || got.Length != 100 {
		t.Fatalf("merged lock = [%d,%d], want [0,100]", got.Offset, got.Length)
	}
}

// Merging is scoped to one owner and one lock type: a different owner's row, and
// a row of the other type, must survive an overlapping acquire untouched.
func TestAddUnifiedLock_MergeScopedToOwnerAndType(t *testing.T) {
	t.Parallel()

	t.Run("other owner untouched", func(t *testing.T) {
		t.Parallel()
		lm := NewManager()
		if err := lm.AddUnifiedLock(mergeHandle, mergeOwnerLock("owner-1", 0, 100, LockTypeShared)); err != nil {
			t.Fatalf("AddUnifiedLock(owner-1): %v", err)
		}
		if err := lm.AddUnifiedLock(mergeHandle, mergeOwnerLock("owner-2", 50, 100, LockTypeShared)); err != nil {
			t.Fatalf("AddUnifiedLock(owner-2): %v", err)
		}
		if got := len(lm.ListUnifiedLocks(mergeHandle)); got != 2 {
			t.Fatalf("expected 2 locks across owners, got %d", got)
		}
	})

	t.Run("other type untouched", func(t *testing.T) {
		t.Parallel()
		lm := NewManager()
		if err := lm.AddUnifiedLock(mergeHandle, mergeOwnerLock("owner-1", 0, 100, LockTypeExclusive)); err != nil {
			t.Fatalf("AddUnifiedLock(exclusive): %v", err)
		}
		if err := lm.AddUnifiedLock(mergeHandle, mergeOwnerLock("owner-1", 50, 100, LockTypeShared)); err != nil {
			t.Fatalf("AddUnifiedLock(shared): %v", err)
		}
		if got := len(lm.ListUnifiedLocks(mergeHandle)); got != 2 {
			t.Fatalf("expected 2 locks across types, got %d", got)
		}
	})

	// Same owner, same range, other type is an upgrade/downgrade, not a merge:
	// the row keeps its identity and flips type.
	t.Run("exact range downgrades in place", func(t *testing.T) {
		t.Parallel()
		lm := NewManager()
		if err := lm.AddUnifiedLock(mergeHandle, mergeOwnerLock("owner-1", 25, 75, LockTypeExclusive)); err != nil {
			t.Fatalf("AddUnifiedLock(exclusive): %v", err)
		}
		if err := lm.AddUnifiedLock(mergeHandle, mergeOwnerLock("owner-1", 25, 75, LockTypeShared)); err != nil {
			t.Fatalf("AddUnifiedLock(shared): %v", err)
		}
		got := singleLock(t, lm)
		if got.Type != LockTypeShared || got.Offset != 25 || got.Length != 75 {
			t.Fatalf("downgraded lock = [%d,%d] type %v, want [25,75] shared", got.Offset, got.Length, got.Type)
		}
	})
}

// Persisted records are keyed by lock ID, and nothing stops a caller reusing one
// ID for two live rows — a lock type change leaves the old row in place, so a
// second row can arrive carrying the same ID. Absorbing one of those two must
// not delete the record the other still depends on, or the survivor is a lock
// that is held in memory and simply gone after a restart.
func TestAddUnifiedLock_MergeKeepsRecordOfSameIDSurvivor(t *testing.T) {
	t.Parallel()

	store := newMockLockStore()
	lm := NewManager()
	lm.SetLockStore(store)
	lm.SetShareName("share-dup")

	add := func(id string, offset, length uint64, lt LockType) {
		t.Helper()
		l := mergeOwnerLock("owner-1", offset, length, lt)
		l.ID = id
		if err := lm.AddUnifiedLock(mergeHandle, l); err != nil {
			t.Fatalf("AddUnifiedLock(%s, [%d,%d)): %v", id, offset, offset+length, err)
		}
	}
	owner := LockOwner{OwnerID: "owner-1", ClientID: "client-1"}

	// One row under "dup", promoted so a later shared row cannot merge with it.
	add("dup", 0, 40, LockTypeShared)
	if _, err := lm.UpgradeLock(mergeHandle, owner, 0, 40); err != nil {
		t.Fatalf("UpgradeLock: %v", err)
	}
	// A second live row reusing that same ID: different type, so no merge and no
	// exact-range update — it is appended alongside.
	add("dup", 20, 40, LockTypeShared)
	if got := len(lm.ListUnifiedLocks(mergeHandle)); got != 2 {
		t.Fatalf("expected 2 rows sharing one ID, got %d", got)
	}

	// Absorb only the shared one. The exclusive row still answers to "dup".
	add("other", 50, 20, LockTypeShared)

	snap, err := store.ListLocks(context.Background(), LockQuery{ShareName: "share-dup"})
	if err != nil {
		t.Fatalf("ListLocks: %v", err)
	}
	stored := map[string]bool{}
	for _, pl := range snap {
		stored[pl.ID] = true
	}
	for id := range liveManagerPersistIDs(lm, mergeHandle) {
		if !stored[id] {
			t.Fatalf("in-memory lock %s has no store record after the merge", id)
		}
	}
}
