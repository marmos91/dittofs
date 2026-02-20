package types

import (
	"bytes"
	"testing"
)

func TestCbSequenceArgs_RoundTrip(t *testing.T) {
	// With referring call lists
	otherSession := SessionId4{
		0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11,
		0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99,
	}
	original := CbSequenceArgs{
		SessionID:     ValidSessionId(),
		SequenceID:    5,
		SlotID:        2,
		HighestSlotID: 7,
		CacheThis:     true,
		ReferringCallLists: []ReferringCallTriple{
			{
				SessionID: otherSession,
				ReferringCalls: []ReferringCall4{
					{SequenceID: 10, SlotID: 3},
					{SequenceID: 11, SlotID: 4},
				},
			},
		},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbSequenceArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.SessionID != original.SessionID {
		t.Errorf("SessionID: got %v, want %v", decoded.SessionID, original.SessionID)
	}
	if decoded.SequenceID != original.SequenceID {
		t.Errorf("SequenceID: got %d, want %d", decoded.SequenceID, original.SequenceID)
	}
	if decoded.SlotID != original.SlotID {
		t.Errorf("SlotID: got %d, want %d", decoded.SlotID, original.SlotID)
	}
	if decoded.HighestSlotID != original.HighestSlotID {
		t.Errorf("HighestSlotID: got %d, want %d", decoded.HighestSlotID, original.HighestSlotID)
	}
	if decoded.CacheThis != original.CacheThis {
		t.Errorf("CacheThis: got %t, want %t", decoded.CacheThis, original.CacheThis)
	}
	if len(decoded.ReferringCallLists) != 1 {
		t.Fatalf("ReferringCallLists length: got %d, want 1", len(decoded.ReferringCallLists))
	}
	if decoded.ReferringCallLists[0].SessionID != otherSession {
		t.Errorf("Referring SessionID: got %v, want %v",
			decoded.ReferringCallLists[0].SessionID, otherSession)
	}
	if len(decoded.ReferringCallLists[0].ReferringCalls) != 2 {
		t.Fatalf("ReferringCalls length: got %d, want 2",
			len(decoded.ReferringCallLists[0].ReferringCalls))
	}
	if decoded.ReferringCallLists[0].ReferringCalls[0].SequenceID != 10 {
		t.Errorf("ReferringCall[0].SequenceID: got %d, want 10",
			decoded.ReferringCallLists[0].ReferringCalls[0].SequenceID)
	}
	if decoded.ReferringCallLists[0].ReferringCalls[1].SlotID != 4 {
		t.Errorf("ReferringCall[1].SlotID: got %d, want 4",
			decoded.ReferringCallLists[0].ReferringCalls[1].SlotID)
	}
}

func TestCbSequenceArgs_RoundTrip_NoReferringCalls(t *testing.T) {
	original := CbSequenceArgs{
		SessionID:          ValidSessionId(),
		SequenceID:         1,
		SlotID:             0,
		HighestSlotID:      15,
		CacheThis:          false,
		ReferringCallLists: nil,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbSequenceArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.SequenceID != 1 {
		t.Errorf("SequenceID: got %d, want 1", decoded.SequenceID)
	}
	if decoded.CacheThis != false {
		t.Errorf("CacheThis: got %t, want false", decoded.CacheThis)
	}
	if len(decoded.ReferringCallLists) != 0 {
		t.Errorf("ReferringCallLists: got %d, want 0", len(decoded.ReferringCallLists))
	}
}

func TestCbSequenceRes_RoundTrip_OK(t *testing.T) {
	original := CbSequenceRes{
		Status:              NFS4_OK,
		SessionID:           ValidSessionId(),
		SequenceID:          5,
		SlotID:              2,
		HighestSlotID:       7,
		TargetHighestSlotID: 15,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbSequenceRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4_OK)
	}
	if decoded.SessionID != original.SessionID {
		t.Errorf("SessionID mismatch")
	}
	if decoded.SequenceID != 5 {
		t.Errorf("SequenceID: got %d, want 5", decoded.SequenceID)
	}
	if decoded.SlotID != 2 {
		t.Errorf("SlotID: got %d, want 2", decoded.SlotID)
	}
	if decoded.HighestSlotID != 7 {
		t.Errorf("HighestSlotID: got %d, want 7", decoded.HighestSlotID)
	}
	if decoded.TargetHighestSlotID != 15 {
		t.Errorf("TargetHighestSlotID: got %d, want 15", decoded.TargetHighestSlotID)
	}
}

func TestCbSequenceRes_RoundTrip_Error(t *testing.T) {
	original := CbSequenceRes{Status: NFS4ERR_BADSESSION}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbSequenceRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4ERR_BADSESSION {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4ERR_BADSESSION)
	}
}

func TestCbSequenceArgs_String(t *testing.T) {
	args := CbSequenceArgs{SessionID: ValidSessionId(), SequenceID: 1}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

func TestCbSequenceRes_String(t *testing.T) {
	res := CbSequenceRes{Status: NFS4_OK, SessionID: ValidSessionId(), SequenceID: 1}
	s := res.String()
	if s == "" {
		t.Error("String() returned empty")
	}

	resErr := CbSequenceRes{Status: NFS4ERR_BADSESSION}
	s2 := resErr.String()
	if s2 == "" {
		t.Error("String() returned empty for error case")
	}
}
