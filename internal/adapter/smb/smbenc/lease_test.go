package smbenc

import (
	"encoding/binary"
	"testing"
)

// TestEncodeLeaseV2ResponseContext_HasParentFalse_ZeroesParentKey verifies that
// when hasParent=false the encoder writes a zero ParentLeaseKey on the wire and
// does NOT set SMB2_LEASE_FLAG_PARENT_LEASE_KEY_SET (0x4) in the Flags field —
// even if the caller passes a non-zero parent key. Per MS-SMB2 §2.2.14.2.11,
// parent_lease_key is meaningful only when the flag is set; smbtorture
// v2_flags_parentkey checks both halves of this contract.
func TestEncodeLeaseV2ResponseContext_HasParentFalse_ZeroesParentKey(t *testing.T) {
	leaseKey := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	parentKey := [16]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}

	out := EncodeLeaseV2ResponseContext(leaseKey, 0x05, 0, parentKey, false, 7)

	if len(out) != LeaseV2ContextSize {
		t.Fatalf("len = %d, want %d", len(out), LeaseV2ContextSize)
	}
	flags := binary.LittleEndian.Uint32(out[20:24])
	if flags&LeaseResponseFlagParentKeySet != 0 {
		t.Errorf("Flags = 0x%x, expected PARENT_LEASE_KEY_SET (0x4) cleared", flags)
	}
	for i := 32; i < 48; i++ {
		if out[i] != 0 {
			t.Errorf("ParentLeaseKey byte %d = 0x%x, expected 0", i, out[i])
		}
	}
}

// TestEncodeLeaseV2ResponseContext_HasParentTrue_EchoesParentKey is the
// positive-path counterpart: when hasParent=true the encoder must set the
// flag and serialize the supplied parent key verbatim.
func TestEncodeLeaseV2ResponseContext_HasParentTrue_EchoesParentKey(t *testing.T) {
	leaseKey := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	parentKey := [16]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}

	out := EncodeLeaseV2ResponseContext(leaseKey, 0x05, 0, parentKey, true, 7)

	flags := binary.LittleEndian.Uint32(out[20:24])
	if flags&LeaseResponseFlagParentKeySet == 0 {
		t.Errorf("Flags = 0x%x, expected PARENT_LEASE_KEY_SET (0x4) set", flags)
	}
	for i := 0; i < 16; i++ {
		if out[32+i] != parentKey[i] {
			t.Errorf("ParentLeaseKey byte %d = 0x%x, want 0x%x", i, out[32+i], parentKey[i])
		}
	}
}

// TestEncodeLeaseV2ResponseContext_PreservesCallerBreakInProgressFlag checks
// that the encoder ORs the parent-key flag onto pre-existing flags rather than
// overwriting them — BREAK_IN_PROGRESS (0x2) and PARENT_LEASE_KEY_SET (0x4)
// can coexist on the same response.
func TestEncodeLeaseV2ResponseContext_PreservesCallerBreakInProgressFlag(t *testing.T) {
	leaseKey := [16]byte{1}
	parentKey := [16]byte{2}

	out := EncodeLeaseV2ResponseContext(leaseKey, 0x07, LeaseResponseFlagBreakInProgress, parentKey, true, 1)

	flags := binary.LittleEndian.Uint32(out[20:24])
	if flags&LeaseResponseFlagBreakInProgress == 0 {
		t.Errorf("Flags = 0x%x, expected BREAK_IN_PROGRESS preserved", flags)
	}
	if flags&LeaseResponseFlagParentKeySet == 0 {
		t.Errorf("Flags = 0x%x, expected PARENT_LEASE_KEY_SET set", flags)
	}
}
