package lock

import "testing"

// These tests pin area-5 H-3 / xproto H1+H2: SMB byte-range locks (lm.locks,
// via Manager.Lock) and NLM/NFSv4 byte-range locks (lm.unifiedLocks, via
// Manager.AddUnifiedLock) live in two separate maps. Each acquisition / IO path
// must cross-check the other map so a lock taken via one protocol blocks a
// conflicting lock or write via the other (MS-FSA §2.1.5).

const xHandle = "share-a:file-1"

func smbExclusive(openID string, offset, length uint64) FileLock {
	return FileLock{OpenID: openID, SessionID: 7, Offset: offset, Length: length, Exclusive: true}
}

func nlmLock(ownerID string, offset, length uint64, exclusive bool) *UnifiedLock {
	lt := LockTypeShared
	if exclusive {
		lt = LockTypeExclusive
	}
	return &UnifiedLock{
		Owner:  LockOwner{OwnerID: ownerID},
		Offset: offset, Length: length, Type: lt,
	}
}

// TestCrossStore_NLMLockBlocksSMB: an NLM exclusive byte-range lock must block a
// later overlapping SMB exclusive lock.
func TestCrossStore_NLMLockBlocksSMB(t *testing.T) {
	lm := NewManager()

	if err := lm.AddUnifiedLock(xHandle, nlmLock("nlm:host-a", 0, 100, true)); err != nil {
		t.Fatalf("AddUnifiedLock (NLM) failed: %v", err)
	}

	err := lm.Lock(xHandle, smbExclusive("smb:open-1", 50, 100))
	if err == nil {
		t.Fatal("SMB lock overlapping an existing NLM exclusive lock must be denied")
	}
}

// TestCrossStore_SMBLockBlocksNLM: an SMB exclusive byte-range lock must block a
// later overlapping NLM lock.
func TestCrossStore_SMBLockBlocksNLM(t *testing.T) {
	lm := NewManager()

	if err := lm.Lock(xHandle, smbExclusive("smb:open-1", 0, 100)); err != nil {
		t.Fatalf("SMB Lock failed: %v", err)
	}

	err := lm.AddUnifiedLock(xHandle, nlmLock("nlm:host-a", 50, 100, true))
	if err == nil {
		t.Fatal("NLM lock overlapping an existing SMB exclusive lock must be denied")
	}
}

// TestCrossStore_SharedRangesCoexist: two shared (read) byte-range locks from
// different protocols on the same range must both be granted.
func TestCrossStore_SharedRangesCoexist(t *testing.T) {
	lm := NewManager()

	if err := lm.AddUnifiedLock(xHandle, nlmLock("nlm:host-a", 0, 100, false)); err != nil {
		t.Fatalf("AddUnifiedLock (NLM shared) failed: %v", err)
	}

	// SMB shared lock on the same range.
	smbShared := FileLock{OpenID: "smb:open-1", SessionID: 7, Offset: 0, Length: 100, Exclusive: false}
	if err := lm.Lock(xHandle, smbShared); err != nil {
		t.Fatalf("two cross-protocol shared locks on the same range must coexist, got %v", err)
	}
}

// TestCrossStore_NonOverlappingCoexist: cross-protocol locks on disjoint ranges
// never conflict.
func TestCrossStore_NonOverlappingCoexist(t *testing.T) {
	lm := NewManager()

	if err := lm.AddUnifiedLock(xHandle, nlmLock("nlm:host-a", 0, 100, true)); err != nil {
		t.Fatalf("AddUnifiedLock failed: %v", err)
	}
	if err := lm.Lock(xHandle, smbExclusive("smb:open-1", 200, 100)); err != nil {
		t.Fatalf("disjoint-range SMB lock must be granted, got %v", err)
	}
}

// TestCrossStore_CheckForIO_NLMLockBlocksSMBWrite pins xproto H2: an NLM
// exclusive byte-range lock must block an SMB write to the overlapping range.
func TestCrossStore_CheckForIO_NLMLockBlocksSMBWrite(t *testing.T) {
	lm := NewManager()

	if err := lm.AddUnifiedLock(xHandle, nlmLock("nlm:host-a", 0, 100, true)); err != nil {
		t.Fatalf("AddUnifiedLock failed: %v", err)
	}

	conflict := lm.CheckForIO(xHandle, "smb:open-1", 7, 50, 10, true)
	if conflict == nil {
		t.Fatal("SMB write overlapping an NLM exclusive lock must be blocked (xproto H2)")
	}
}

// TestCrossStore_CheckForIO_NLMSharedBlocksSMBWrite: a shared (read) NLM lock
// blocks writes from a different protocol (mandatory-lock semantics), but not
// reads.
func TestCrossStore_CheckForIO_NLMSharedBlocksSMBWrite(t *testing.T) {
	lm := NewManager()

	if err := lm.AddUnifiedLock(xHandle, nlmLock("nlm:host-a", 0, 100, false)); err != nil {
		t.Fatalf("AddUnifiedLock failed: %v", err)
	}

	if c := lm.CheckForIO(xHandle, "smb:open-1", 7, 50, 10, true); c == nil {
		t.Fatal("SMB write overlapping an NLM shared lock must be blocked")
	}
	if c := lm.CheckForIO(xHandle, "smb:open-1", 7, 50, 10, false); c != nil {
		t.Fatal("SMB read overlapping an NLM shared lock must be allowed")
	}
}

// TestCrossStore_LeaseDoesNotBlockByteRange: a whole-file lease in unifiedLocks
// is a caching primitive resolved via the break path, not a byte-range lock,
// and must not block a cross-protocol byte-range acquisition.
func TestCrossStore_LeaseDoesNotBlockByteRange(t *testing.T) {
	lm := NewManager()

	lease := &UnifiedLock{
		Owner: LockOwner{OwnerID: "smb:open-lease"},
		Type:  LockTypeExclusive,
		Lease: &OpLock{},
	}
	if err := lm.AddUnifiedLock(xHandle, lease); err != nil {
		t.Fatalf("AddUnifiedLock (lease) failed: %v", err)
	}

	if err := lm.Lock(xHandle, smbExclusive("smb:open-1", 0, 100)); err != nil {
		t.Fatalf("a whole-file lease must not block a byte-range lock, got %v", err)
	}
}

// TestCrossStore_TestUnifiedLockSeesSMBLock pins the NLM/NFSv4 TEST (LOCKT)
// path: TestUnifiedLock must report a conflict when an overlapping SMB
// byte-range lock exists, matching what AddUnifiedLock would enforce. Without
// the cross-store scan a LOCKT would falsely report the range grantable.
func TestCrossStore_TestUnifiedLockSeesSMBLock(t *testing.T) {
	lm := NewManager()

	if err := lm.Lock(xHandle, smbExclusive("smb:open-1", 0, 100)); err != nil {
		t.Fatalf("SMB Lock failed: %v", err)
	}

	conflict := lm.TestUnifiedLock(xHandle, nlmLock("nlm:host-a", 50, 10, true))
	if conflict == nil {
		t.Fatal("NLM/NFSv4 TEST must see an overlapping SMB byte-range lock (xproto H1)")
	}

	// A disjoint NLM range must report grantable.
	if c := lm.TestUnifiedLock(xHandle, nlmLock("nlm:host-a", 500, 10, true)); c != nil {
		t.Fatalf("disjoint NLM range must be grantable, got conflict %+v", c)
	}
}

// TestCrossStore_ZeroByteSMBLockDoesNotBlockNLM: an SMB2 zero-byte lock has no
// real byte range and must never block a cross-protocol NLM lock.
func TestCrossStore_ZeroByteSMBLockDoesNotBlockNLM(t *testing.T) {
	lm := NewManager()

	zb := FileLock{OpenID: "smb:open-1", SessionID: 7, Offset: 10, Length: 0, Exclusive: true, IsZeroByte: true}
	if err := lm.Lock(xHandle, zb); err != nil {
		t.Fatalf("SMB zero-byte Lock failed: %v", err)
	}

	if err := lm.AddUnifiedLock(xHandle, nlmLock("nlm:host-a", 0, 100, true)); err != nil {
		t.Fatalf("a zero-byte SMB lock must not block an NLM lock, got %v", err)
	}
}

// TestCrossStore_TestLockAgreesWithLock: the TestLock preview must report the
// same cross-protocol conflict that Lock would enforce.
func TestCrossStore_TestLockAgreesWithLock(t *testing.T) {
	lm := NewManager()

	if err := lm.AddUnifiedLock(xHandle, nlmLock("nlm:host-a", 0, 100, true)); err != nil {
		t.Fatalf("AddUnifiedLock failed: %v", err)
	}

	conflict, err := lm.TestLock(xHandle, smbExclusive("smb:open-1", 50, 10))
	if err != nil {
		t.Fatalf("TestLock returned error: %v", err)
	}
	if conflict == nil {
		t.Fatal("TestLock must report the cross-protocol conflict that Lock would enforce")
	}
}
