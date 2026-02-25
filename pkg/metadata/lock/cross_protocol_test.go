package lock

import (
	"testing"
	"time"
)

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
