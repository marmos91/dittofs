package lock

import (
	"sync"
	"testing"
	"time"
)

// ============================================================================
// ConflictsWith - All 4 Conflict Cases
// ============================================================================

func TestConflictsWith_SameOwnerNeverConflicts(t *testing.T) {
	t.Parallel()

	owner := LockOwner{OwnerID: "smb:client1"}

	// Exclusive vs Exclusive from same owner - no conflict
	a := &UnifiedLock{Owner: owner, Offset: 0, Length: 100, Type: LockTypeExclusive}
	b := &UnifiedLock{Owner: owner, Offset: 0, Length: 100, Type: LockTypeExclusive}

	if a.ConflictsWith(b) {
		t.Error("Same owner should never conflict (exclusive vs exclusive)")
	}
}

// --- Case 1: Access Mode Conflicts ---

func TestConflictsWith_AccessMode_DenyReadConflicts(t *testing.T) {
	t.Parallel()

	a := &UnifiedLock{
		Owner:      LockOwner{OwnerID: "smb:client1"},
		AccessMode: AccessModeDenyRead,
		Offset:     0, Length: 100, Type: LockTypeShared,
	}
	b := &UnifiedLock{
		Owner:      LockOwner{OwnerID: "smb:client2"},
		AccessMode: AccessModeDenyWrite, // Non-None access mode
		Offset:     0, Length: 100, Type: LockTypeShared,
	}

	if !a.ConflictsWith(b) {
		t.Error("DenyRead should conflict with non-None access mode")
	}
}

func TestConflictsWith_AccessMode_DenyAllConflicts(t *testing.T) {
	t.Parallel()

	a := &UnifiedLock{
		Owner:      LockOwner{OwnerID: "smb:client1"},
		AccessMode: AccessModeDenyAll,
		Offset:     0, Length: 100, Type: LockTypeShared,
	}
	b := &UnifiedLock{
		Owner:      LockOwner{OwnerID: "smb:client2"},
		AccessMode: AccessModeDenyRead,
		Offset:     0, Length: 100, Type: LockTypeShared,
	}

	if !a.ConflictsWith(b) {
		t.Error("DenyAll should conflict with any non-None access mode")
	}
}

func TestConflictsWith_AccessMode_BothNoneNoConflict(t *testing.T) {
	t.Parallel()

	a := &UnifiedLock{
		Owner:      LockOwner{OwnerID: "smb:client1"},
		AccessMode: AccessModeNone,
		Offset:     0, Length: 100, Type: LockTypeShared,
	}
	b := &UnifiedLock{
		Owner:      LockOwner{OwnerID: "smb:client2"},
		AccessMode: AccessModeNone,
		Offset:     0, Length: 100, Type: LockTypeShared,
	}

	if a.ConflictsWith(b) {
		t.Error("Both AccessModeNone should not conflict")
	}
}

// --- Case 2: OpLock vs OpLock ---

func TestConflictsWith_OpLock_ReadReadNoConflict(t *testing.T) {
	t.Parallel()

	a := &UnifiedLock{
		Owner: LockOwner{OwnerID: "smb:client1"},
		Lease: &OpLock{LeaseKey: [16]byte{1}, LeaseState: LeaseStateRead},
	}
	b := &UnifiedLock{
		Owner: LockOwner{OwnerID: "smb:client2"},
		Lease: &OpLock{LeaseKey: [16]byte{2}, LeaseState: LeaseStateRead},
	}

	if a.ConflictsWith(b) {
		t.Error("Read-Read oplocks should not conflict")
	}
}

func TestConflictsWith_OpLock_ReadWriteConflict(t *testing.T) {
	t.Parallel()

	a := &UnifiedLock{
		Owner: LockOwner{OwnerID: "smb:client1"},
		Lease: &OpLock{LeaseKey: [16]byte{1}, LeaseState: LeaseStateRead},
	}
	b := &UnifiedLock{
		Owner: LockOwner{OwnerID: "smb:client2"},
		Lease: &OpLock{LeaseKey: [16]byte{2}, LeaseState: LeaseStateRead | LeaseStateWrite},
	}

	if !a.ConflictsWith(b) {
		t.Error("Read vs Write oplock should conflict")
	}
}

func TestConflictsWith_OpLock_WriteWriteConflict(t *testing.T) {
	t.Parallel()

	a := &UnifiedLock{
		Owner: LockOwner{OwnerID: "smb:client1"},
		Lease: &OpLock{LeaseKey: [16]byte{1}, LeaseState: LeaseStateRead | LeaseStateWrite},
	}
	b := &UnifiedLock{
		Owner: LockOwner{OwnerID: "smb:client2"},
		Lease: &OpLock{LeaseKey: [16]byte{2}, LeaseState: LeaseStateRead | LeaseStateWrite},
	}

	if !a.ConflictsWith(b) {
		t.Error("Write vs Write oplock should conflict")
	}
}

func TestConflictsWith_OpLock_SameLeaseKeyNoConflict(t *testing.T) {
	t.Parallel()

	sameKey := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	a := &UnifiedLock{
		Owner: LockOwner{OwnerID: "smb:client1"},
		Lease: &OpLock{LeaseKey: sameKey, LeaseState: LeaseStateRead | LeaseStateWrite},
	}
	b := &UnifiedLock{
		Owner: LockOwner{OwnerID: "smb:client2"},
		Lease: &OpLock{LeaseKey: sameKey, LeaseState: LeaseStateRead | LeaseStateWrite},
	}

	if a.ConflictsWith(b) {
		t.Error("Same LeaseKey should never conflict (same caching unit)")
	}
}

func TestConflictsWith_OpLock_HandleOnlyNoConflict(t *testing.T) {
	t.Parallel()

	a := &UnifiedLock{
		Owner: LockOwner{OwnerID: "smb:client1"},
		Lease: &OpLock{LeaseKey: [16]byte{1}, LeaseState: LeaseStateRead | LeaseStateHandle},
	}
	b := &UnifiedLock{
		Owner: LockOwner{OwnerID: "smb:client2"},
		Lease: &OpLock{LeaseKey: [16]byte{2}, LeaseState: LeaseStateRead},
	}

	if a.ConflictsWith(b) {
		t.Error("ReadHandle vs Read should not conflict (no Write involved)")
	}
}

// --- Case 3: OpLock vs Byte-Range ---

func TestConflictsWith_OpLockVsByteRange_WriteOpLockExclusiveLock(t *testing.T) {
	t.Parallel()

	oplock := &UnifiedLock{
		Owner: LockOwner{OwnerID: "smb:client1"},
		Lease: &OpLock{LeaseKey: [16]byte{1}, LeaseState: LeaseStateRead | LeaseStateWrite},
	}
	byteRange := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "nlm:host1:123:abc"},
		Offset: 0, Length: 100,
		Type: LockTypeExclusive,
	}

	if !oplock.ConflictsWith(byteRange) {
		t.Error("Write oplock should conflict with exclusive byte-range lock")
	}
}

func TestConflictsWith_OpLockVsByteRange_ReadOpLockSharedLockNoConflict(t *testing.T) {
	t.Parallel()

	oplock := &UnifiedLock{
		Owner: LockOwner{OwnerID: "smb:client1"},
		Lease: &OpLock{LeaseKey: [16]byte{1}, LeaseState: LeaseStateRead},
	}
	byteRange := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "nlm:host1:123:abc"},
		Offset: 0, Length: 100,
		Type: LockTypeShared,
	}

	if oplock.ConflictsWith(byteRange) {
		t.Error("Read oplock should not conflict with shared byte-range lock")
	}
}

func TestConflictsWith_OpLockVsByteRange_Bidirectional(t *testing.T) {
	t.Parallel()

	oplock := &UnifiedLock{
		Owner: LockOwner{OwnerID: "smb:client1"},
		Lease: &OpLock{LeaseKey: [16]byte{1}, LeaseState: LeaseStateRead | LeaseStateWrite},
	}
	byteRange := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "nlm:host1:123:abc"},
		Offset: 0, Length: 100,
		Type: LockTypeExclusive,
	}

	// Both orderings should give the same result
	ab := oplock.ConflictsWith(byteRange)
	ba := byteRange.ConflictsWith(oplock)

	if ab != ba {
		t.Errorf("ConflictsWith should be symmetric: oplock->byteRange=%v, byteRange->oplock=%v", ab, ba)
	}
}

// --- Case 4: Byte-Range vs Byte-Range ---

func TestConflictsWith_ByteRange_ExclusiveOverlapConflict(t *testing.T) {
	t.Parallel()

	a := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "nlm:host1:1:00"},
		Offset: 0, Length: 100,
		Type: LockTypeExclusive,
	}
	b := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "nlm:host2:2:00"},
		Offset: 50, Length: 100,
		Type: LockTypeExclusive,
	}

	if !a.ConflictsWith(b) {
		t.Error("Overlapping exclusive byte-range locks should conflict")
	}
}

func TestConflictsWith_ByteRange_SharedOverlapNoConflict(t *testing.T) {
	t.Parallel()

	a := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "nlm:host1:1:00"},
		Offset: 0, Length: 100,
		Type: LockTypeShared,
	}
	b := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "nlm:host2:2:00"},
		Offset: 50, Length: 100,
		Type: LockTypeShared,
	}

	if a.ConflictsWith(b) {
		t.Error("Overlapping shared byte-range locks should not conflict")
	}
}

func TestConflictsWith_ByteRange_NoOverlapNoConflict(t *testing.T) {
	t.Parallel()

	a := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "nlm:host1:1:00"},
		Offset: 0, Length: 50,
		Type: LockTypeExclusive,
	}
	b := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "nlm:host2:2:00"},
		Offset: 100, Length: 50,
		Type: LockTypeExclusive,
	}

	if a.ConflictsWith(b) {
		t.Error("Non-overlapping exclusive byte-range locks should not conflict")
	}
}

func TestConflictsWith_ByteRange_SharedExclusiveConflict(t *testing.T) {
	t.Parallel()

	a := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "nlm:host1:1:00"},
		Offset: 0, Length: 100,
		Type: LockTypeShared,
	}
	b := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "nlm:host2:2:00"},
		Offset: 50, Length: 100,
		Type: LockTypeExclusive,
	}

	if !a.ConflictsWith(b) {
		t.Error("Shared + overlapping exclusive should conflict")
	}
}

// ============================================================================
// Cross-Protocol Scenario: NFS delegation broken by SMB write
// ============================================================================

func TestCrossProtocol_NFSDelegationBrokenBySMBWrite(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	cb := &crossProtocolBreakCallback{}
	lm.RegisterBreakCallbacks(cb)

	// NFS client has a Read oplock (delegation equivalent)
	nfsDelegation := &UnifiedLock{
		ID:    "nfs-delegation-1",
		Owner: LockOwner{OwnerID: "nfs4:client1:stateid1", ClientID: "nfs-conn1"},
		Lease: &OpLock{
			LeaseKey:   [16]byte{10, 20, 30},
			LeaseState: LeaseStateRead,
		},
	}
	if err := lm.AddUnifiedLock("shared-file", nfsDelegation); err != nil {
		t.Fatalf("AddUnifiedLock failed: %v", err)
	}

	// SMB client performs write - should trigger break of NFS delegation
	smbWriter := &LockOwner{OwnerID: "smb:session456"}
	err := lm.CheckAndBreakOpLocksForWrite("shared-file", smbWriter)
	if err != nil {
		t.Fatalf("CheckAndBreakOpLocksForWrite failed: %v", err)
	}

	breaks := cb.getBreaks()
	if len(breaks) != 1 {
		t.Fatalf("Expected 1 break (NFS delegation), got %d", len(breaks))
	}
	if breaks[0].ownerID != "nfs4:client1:stateid1" {
		t.Fatalf("Expected NFS owner break, got '%s'", breaks[0].ownerID)
	}
	if breaks[0].breakToState != LeaseStateNone {
		t.Fatalf("Expected break to None for write, got %d", breaks[0].breakToState)
	}
}

// crossProtocolBreakCallback is a test helper for cross-protocol break tests.
type crossProtocolBreakCallback struct {
	mu     sync.Mutex
	breaks []crossBreakEvent
}

type crossBreakEvent struct {
	handleKey    string
	ownerID      string
	breakToState uint32
}

func (c *crossProtocolBreakCallback) OnOpLockBreak(handleKey string, lock *UnifiedLock, breakToState uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.breaks = append(c.breaks, crossBreakEvent{
		handleKey:    handleKey,
		ownerID:      lock.Owner.OwnerID,
		breakToState: breakToState,
	})
}

func (c *crossProtocolBreakCallback) OnByteRangeRevoke(handleKey string, lock *UnifiedLock, reason string) {
	// Not used in this test
}

func (c *crossProtocolBreakCallback) OnAccessConflict(handleKey string, existingLock *UnifiedLock, requestedMode AccessMode) {
	// Not used in this test
}

func (c *crossProtocolBreakCallback) getBreaks() []crossBreakEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]crossBreakEvent, len(c.breaks))
	copy(result, c.breaks)
	return result
}

// ============================================================================
// Existing Cross-Protocol Translation Tests (kept from original)
// ============================================================================

func TestTranslateToNLMHolder_WriteLease(t *testing.T) {
	leaseKey := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	lease := &UnifiedLock{
		ID: "lease1",
		Owner: LockOwner{
			OwnerID:   "smb:client123",
			ClientID:  "conn1",
			ShareName: "/export",
		},
		FileHandle: "file1",
		Offset:     0,
		Length:     0,
		Type:       LockTypeExclusive,
		AcquiredAt: time.Now(),
		Lease: &OpLock{
			LeaseKey:   leaseKey,
			LeaseState: LeaseStateRead | LeaseStateWrite,
			Epoch:      1,
		},
	}

	holder := TranslateToNLMHolder(lease)

	// Verify CallerName
	if holder.CallerName != "smb:client123" {
		t.Errorf("Expected CallerName 'smb:client123', got '%s'", holder.CallerName)
	}

	// Verify Svid is 0 for SMB
	if holder.Svid != 0 {
		t.Errorf("Expected Svid 0 for SMB lease, got %d", holder.Svid)
	}

	// Verify OH is first 8 bytes of LeaseKey
	expectedOH := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	if len(holder.OH) != 8 {
		t.Errorf("Expected OH length 8, got %d", len(holder.OH))
	}
	for i, b := range expectedOH {
		if holder.OH[i] != b {
			t.Errorf("OH byte %d: expected %d, got %d", i, b, holder.OH[i])
		}
	}

	// Verify Offset is 0 (whole file)
	if holder.Offset != 0 {
		t.Errorf("Expected Offset 0 for lease, got %d", holder.Offset)
	}

	// Verify Length is max uint64 (whole file)
	if holder.Length != ^uint64(0) {
		t.Errorf("Expected Length max uint64 for lease, got %d", holder.Length)
	}

	// Verify Exclusive is true for Write lease
	if !holder.Exclusive {
		t.Error("Expected Exclusive=true for Write lease")
	}
}

func TestTranslateToNLMHolder_ReadOnlyLease(t *testing.T) {
	leaseKey := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	lease := &UnifiedLock{
		ID: "lease2",
		Owner: LockOwner{
			OwnerID:   "smb:client456",
			ClientID:  "conn2",
			ShareName: "/export",
		},
		FileHandle: "file1",
		Offset:     0,
		Length:     0,
		Type:       LockTypeShared,
		AcquiredAt: time.Now(),
		Lease: &OpLock{
			LeaseKey:   leaseKey,
			LeaseState: LeaseStateRead, // Read-only lease
			Epoch:      1,
		},
	}

	holder := TranslateToNLMHolder(lease)

	// Verify Exclusive is false for Read-only lease
	if holder.Exclusive {
		t.Error("Expected Exclusive=false for Read-only lease")
	}
}

func TestTranslateToNLMHolder_PanicsOnNonLease(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("Expected panic for non-lease lock")
		}
	}()

	// Create a byte-range lock (not a lease)
	lock := &UnifiedLock{
		ID: "lock1",
		Owner: LockOwner{
			OwnerID: "nlm:host1:123:abc",
		},
		FileHandle: "file1",
		Offset:     0,
		Length:     100,
		Type:       LockTypeExclusive,
		// Lease is nil
	}

	TranslateToNLMHolder(lock) // Should panic
}

func TestTranslateByteRangeLockToNLMHolder_NLMFormat(t *testing.T) {
	lock := &UnifiedLock{
		ID: "lock1",
		Owner: LockOwner{
			OwnerID:   "nlm:hostname1:12345:deadbeef",
			ClientID:  "conn1",
			ShareName: "/export",
		},
		FileHandle: "file1",
		Offset:     1024,
		Length:     4096,
		Type:       LockTypeExclusive,
		AcquiredAt: time.Now(),
	}

	holder := TranslateByteRangeLockToNLMHolder(lock)

	// Verify CallerName extracted from NLM format
	if holder.CallerName != "hostname1" {
		t.Errorf("Expected CallerName 'hostname1', got '%s'", holder.CallerName)
	}

	// Verify Svid parsed from NLM format
	if holder.Svid != 12345 {
		t.Errorf("Expected Svid 12345, got %d", holder.Svid)
	}

	// Verify OH parsed from hex
	expectedOH := []byte{0xde, 0xad, 0xbe, 0xef}
	if len(holder.OH) != len(expectedOH) {
		t.Errorf("Expected OH length %d, got %d", len(expectedOH), len(holder.OH))
	}
	for i, b := range expectedOH {
		if holder.OH[i] != b {
			t.Errorf("OH byte %d: expected 0x%02x, got 0x%02x", i, b, holder.OH[i])
		}
	}

	// Verify offset and length preserved
	if holder.Offset != 1024 {
		t.Errorf("Expected Offset 1024, got %d", holder.Offset)
	}
	if holder.Length != 4096 {
		t.Errorf("Expected Length 4096, got %d", holder.Length)
	}

	// Verify exclusive flag
	if !holder.Exclusive {
		t.Error("Expected Exclusive=true for exclusive lock")
	}
}

func TestTranslateByteRangeLockToNLMHolder_SharedLock(t *testing.T) {
	lock := &UnifiedLock{
		ID: "lock1",
		Owner: LockOwner{
			OwnerID: "nlm:host1:1:00",
		},
		FileHandle: "file1",
		Offset:     0,
		Length:     100,
		Type:       LockTypeShared,
	}

	holder := TranslateByteRangeLockToNLMHolder(lock)

	if holder.Exclusive {
		t.Error("Expected Exclusive=false for shared lock")
	}
}

func TestTranslateSMBConflictReason_ExclusiveLock(t *testing.T) {
	lock := &UnifiedLock{
		ID: "lock1",
		Owner: LockOwner{
			OwnerID: "nlm:fileserver:123:abc",
		},
		FileHandle: "file1",
		Offset:     0,
		Length:     1024,
		Type:       LockTypeExclusive,
	}

	reason := TranslateSMBConflictReason(lock)

	expected := "NFS client 'nlm:fileserver' holds exclusive lock on bytes 0-1024"
	if reason != expected {
		t.Errorf("Expected '%s', got '%s'", expected, reason)
	}
}

func TestTranslateSMBConflictReason_SharedLock(t *testing.T) {
	lock := &UnifiedLock{
		ID: "lock1",
		Owner: LockOwner{
			OwnerID: "nlm:client1:456:def",
		},
		FileHandle: "file1",
		Offset:     0,
		Length:     0, // To EOF
		Type:       LockTypeShared,
	}

	reason := TranslateSMBConflictReason(lock)

	expected := "NFS client 'nlm:client1' holds shared lock on bytes 0 to end of file"
	if reason != expected {
		t.Errorf("Expected '%s', got '%s'", expected, reason)
	}
}

func TestTranslateSMBConflictReason_WholeFileLock(t *testing.T) {
	lock := &UnifiedLock{
		ID: "lock1",
		Owner: LockOwner{
			OwnerID: "nlm:host:1:00",
		},
		FileHandle: "file1",
		Offset:     0,
		Length:     ^uint64(0), // Max = whole file
		Type:       LockTypeExclusive,
	}

	reason := TranslateSMBConflictReason(lock)

	expected := "NFS client 'nlm:host' holds exclusive lock on entire file"
	if reason != expected {
		t.Errorf("Expected '%s', got '%s'", expected, reason)
	}
}

func TestTranslateNFSConflictReason_WriteLease(t *testing.T) {
	leaseKey := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	lease := &UnifiedLock{
		ID: "lease1",
		Owner: LockOwner{
			OwnerID: "smb:smbclient1",
		},
		FileHandle: "file1",
		Lease: &OpLock{
			LeaseKey:   leaseKey,
			LeaseState: LeaseStateRead | LeaseStateWrite,
		},
	}

	reason := TranslateNFSConflictReason(lease)

	expected := "SMB client 'smb:smbclient1' holds Write lease (RW)"
	if reason != expected {
		t.Errorf("Expected '%s', got '%s'", expected, reason)
	}
}

func TestTranslateNFSConflictReason_HandleLease(t *testing.T) {
	leaseKey := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	lease := &UnifiedLock{
		ID: "lease1",
		Owner: LockOwner{
			OwnerID: "smb:smbclient2",
		},
		FileHandle: "file1",
		Lease: &OpLock{
			LeaseKey:   leaseKey,
			LeaseState: LeaseStateRead | LeaseStateHandle,
		},
	}

	reason := TranslateNFSConflictReason(lease)

	expected := "SMB client 'smb:smbclient2' holds Handle lease (RH)"
	if reason != expected {
		t.Errorf("Expected '%s', got '%s'", expected, reason)
	}
}

func TestTranslateNFSConflictReason_ReadOnlyLease(t *testing.T) {
	leaseKey := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	lease := &UnifiedLock{
		ID: "lease1",
		Owner: LockOwner{
			OwnerID: "smb:reader",
		},
		FileHandle: "file1",
		Lease: &OpLock{
			LeaseKey:   leaseKey,
			LeaseState: LeaseStateRead,
		},
	}

	reason := TranslateNFSConflictReason(lease)

	expected := "SMB client 'smb:reader' holds Read lease (R)"
	if reason != expected {
		t.Errorf("Expected '%s', got '%s'", expected, reason)
	}
}

func TestExtractClientID_NLMFormat(t *testing.T) {
	tests := []struct {
		ownerID  string
		expected string
	}{
		{"nlm:hostname:123:abc", "nlm:hostname"},
		{"nlm:client1:456:deadbeef", "nlm:client1"},
		{"nlm:x:0:0", "nlm:x"},
	}

	for _, tt := range tests {
		result := extractClientID(tt.ownerID)
		if result != tt.expected {
			t.Errorf("extractClientID(%s): expected '%s', got '%s'", tt.ownerID, tt.expected, result)
		}
	}
}

func TestExtractClientID_SMBFormat(t *testing.T) {
	tests := []struct {
		ownerID  string
		expected string
	}{
		{"smb:client1", "smb:client1"},
		{"smb:session123", "smb:session123"},
	}

	for _, tt := range tests {
		result := extractClientID(tt.ownerID)
		if result != tt.expected {
			t.Errorf("extractClientID(%s): expected '%s', got '%s'", tt.ownerID, tt.expected, result)
		}
	}
}

func TestExtractClientID_EmptyAndOther(t *testing.T) {
	// Empty returns "unknown"
	if result := extractClientID(""); result != "unknown" {
		t.Errorf("extractClientID(''): expected 'unknown', got '%s'", result)
	}

	// Unknown format returns full owner ID
	if result := extractClientID("other:format"); result != "other:format" {
		t.Errorf("extractClientID('other:format'): expected 'other:format', got '%s'", result)
	}
}

func TestParseNLMOwnerID_Complete(t *testing.T) {
	callerName, svid, oh := parseNLMOwnerID("nlm:myhost:9999:0102030405")

	if callerName != "myhost" {
		t.Errorf("Expected callerName 'myhost', got '%s'", callerName)
	}
	if svid != 9999 {
		t.Errorf("Expected svid 9999, got %d", svid)
	}
	expectedOH := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	if len(oh) != len(expectedOH) {
		t.Errorf("Expected OH length %d, got %d", len(expectedOH), len(oh))
	}
	for i, b := range expectedOH {
		if oh[i] != b {
			t.Errorf("OH byte %d: expected 0x%02x, got 0x%02x", i, b, oh[i])
		}
	}
}

func TestParseNLMOwnerID_Incomplete(t *testing.T) {
	// Only protocol and caller name
	callerName, svid, oh := parseNLMOwnerID("nlm:hostname")

	if callerName != "hostname" {
		t.Errorf("Expected callerName 'hostname', got '%s'", callerName)
	}
	if svid != 0 {
		t.Errorf("Expected svid 0 for incomplete format, got %d", svid)
	}
	if len(oh) != 0 {
		t.Errorf("Expected empty OH for incomplete format, got %v", oh)
	}
}

func TestParseNLMOwnerID_NonNLM(t *testing.T) {
	callerName, svid, oh := parseNLMOwnerID("smb:client1")

	if callerName != "smb:client1" {
		t.Errorf("Expected callerName 'smb:client1', got '%s'", callerName)
	}
	if svid != 0 {
		t.Errorf("Expected svid 0 for non-NLM format, got %d", svid)
	}
	if len(oh) != 0 {
		t.Errorf("Expected empty OH for non-NLM format, got %v", oh)
	}
}
