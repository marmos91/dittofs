package types

import (
	"bytes"
	"testing"
)

// TestSequenceArgs_RoundTrip tests encode/decode with session ID, slot 0, cache=true.
func TestSequenceArgs_RoundTrip(t *testing.T) {
	original := &SequenceArgs{
		SessionID:     ValidSessionId(),
		SequenceID:    1,
		SlotID:        0,
		HighestSlotID: 15,
		CacheThis:     true,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &SequenceArgs{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.SessionID != original.SessionID {
		t.Errorf("SessionID = %s, want %s", decoded.SessionID.String(), original.SessionID.String())
	}
	if decoded.SequenceID != original.SequenceID {
		t.Errorf("SequenceID = %d, want %d", decoded.SequenceID, original.SequenceID)
	}
	if decoded.SlotID != original.SlotID {
		t.Errorf("SlotID = %d, want %d", decoded.SlotID, original.SlotID)
	}
	if decoded.HighestSlotID != original.HighestSlotID {
		t.Errorf("HighestSlotID = %d, want %d", decoded.HighestSlotID, original.HighestSlotID)
	}
	if decoded.CacheThis != true {
		t.Errorf("CacheThis = %t, want true", decoded.CacheThis)
	}

	// Verify String() doesn't panic
	s := original.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

// TestSequenceArgs_RoundTrip_NoCaching tests with cache=false.
func TestSequenceArgs_RoundTrip_NoCaching(t *testing.T) {
	original := &SequenceArgs{
		SessionID:     ValidSessionId(),
		SequenceID:    42,
		SlotID:        5,
		HighestSlotID: 31,
		CacheThis:     false,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &SequenceArgs{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.CacheThis != false {
		t.Errorf("CacheThis = %t, want false", decoded.CacheThis)
	}
	if decoded.SequenceID != 42 {
		t.Errorf("SequenceID = %d, want 42", decoded.SequenceID)
	}
	if decoded.SlotID != 5 {
		t.Errorf("SlotID = %d, want 5", decoded.SlotID)
	}
}

// TestSequenceRes_RoundTrip tests a success response with status flags.
func TestSequenceRes_RoundTrip(t *testing.T) {
	original := &SequenceRes{
		Status:              NFS4_OK,
		SessionID:           ValidSessionId(),
		SequenceID:          1,
		SlotID:              0,
		HighestSlotID:       15,
		TargetHighestSlotID: 31,
		StatusFlags:         SEQ4_STATUS_CB_PATH_DOWN | SEQ4_STATUS_LEASE_MOVED,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &SequenceRes{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Errorf("Status = %d, want %d", decoded.Status, NFS4_OK)
	}
	if decoded.SessionID != original.SessionID {
		t.Errorf("SessionID = %s, want %s", decoded.SessionID.String(), original.SessionID.String())
	}
	if decoded.SequenceID != original.SequenceID {
		t.Errorf("SequenceID = %d, want %d", decoded.SequenceID, original.SequenceID)
	}
	if decoded.SlotID != original.SlotID {
		t.Errorf("SlotID = %d, want %d", decoded.SlotID, original.SlotID)
	}
	if decoded.HighestSlotID != original.HighestSlotID {
		t.Errorf("HighestSlotID = %d, want %d", decoded.HighestSlotID, original.HighestSlotID)
	}
	if decoded.TargetHighestSlotID != original.TargetHighestSlotID {
		t.Errorf("TargetHighestSlotID = %d, want %d", decoded.TargetHighestSlotID, original.TargetHighestSlotID)
	}
	if decoded.StatusFlags != original.StatusFlags {
		t.Errorf("StatusFlags = 0x%x, want 0x%x", decoded.StatusFlags, original.StatusFlags)
	}

	// Verify String() doesn't panic
	s := decoded.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

// TestSequenceRes_RoundTrip_Error tests an error response (status-only).
func TestSequenceRes_RoundTrip_Error(t *testing.T) {
	original := &SequenceRes{
		Status: NFS4ERR_BADSESSION,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &SequenceRes{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.Status != NFS4ERR_BADSESSION {
		t.Errorf("Status = %d, want %d", decoded.Status, NFS4ERR_BADSESSION)
	}
	// Session ID should be zero (not decoded)
	var zeroSession SessionId4
	if decoded.SessionID != zeroSession {
		t.Errorf("SessionID = %s, want zero (error case)", decoded.SessionID.String())
	}

	// Verify error String() format
	s := decoded.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

// TestSequenceRes_RoundTrip_WithStatusFlags tests multiple SEQ4_STATUS flags encode correctly.
func TestSequenceRes_RoundTrip_WithStatusFlags(t *testing.T) {
	// Combine multiple status flags
	flags := uint32(SEQ4_STATUS_CB_PATH_DOWN |
		SEQ4_STATUS_EXPIRED_ALL_STATE_REVOKED |
		SEQ4_STATUS_RECALLABLE_STATE_REVOKED |
		SEQ4_STATUS_BACKCHANNEL_FAULT)

	original := &SequenceRes{
		Status:              NFS4_OK,
		SessionID:           ValidSessionId(),
		SequenceID:          100,
		SlotID:              3,
		HighestSlotID:       7,
		TargetHighestSlotID: 15,
		StatusFlags:         flags,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &SequenceRes{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.StatusFlags != flags {
		t.Errorf("StatusFlags = 0x%08x, want 0x%08x", decoded.StatusFlags, flags)
	}

	// Verify individual flag bits
	if decoded.StatusFlags&SEQ4_STATUS_CB_PATH_DOWN == 0 {
		t.Error("SEQ4_STATUS_CB_PATH_DOWN not set")
	}
	if decoded.StatusFlags&SEQ4_STATUS_EXPIRED_ALL_STATE_REVOKED == 0 {
		t.Error("SEQ4_STATUS_EXPIRED_ALL_STATE_REVOKED not set")
	}
	if decoded.StatusFlags&SEQ4_STATUS_RECALLABLE_STATE_REVOKED == 0 {
		t.Error("SEQ4_STATUS_RECALLABLE_STATE_REVOKED not set")
	}
	if decoded.StatusFlags&SEQ4_STATUS_BACKCHANNEL_FAULT == 0 {
		t.Error("SEQ4_STATUS_BACKCHANNEL_FAULT not set")
	}
	// This flag should NOT be set
	if decoded.StatusFlags&SEQ4_STATUS_LEASE_MOVED != 0 {
		t.Error("SEQ4_STATUS_LEASE_MOVED unexpectedly set")
	}
}
