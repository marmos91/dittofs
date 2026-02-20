package types

import (
	"bytes"
	"testing"
)

func TestTestStateidArgs_RoundTrip_SingleStateid(t *testing.T) {
	original := TestStateidArgs{
		Stateids: []Stateid4{ValidStateid()},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded TestStateidArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if len(decoded.Stateids) != 1 {
		t.Fatalf("Stateids length: got %d, want 1", len(decoded.Stateids))
	}
	if decoded.Stateids[0].Seqid != original.Stateids[0].Seqid {
		t.Errorf("Seqid: got %d, want %d", decoded.Stateids[0].Seqid, original.Stateids[0].Seqid)
	}
}

func TestTestStateidArgs_RoundTrip_MultipleStateids(t *testing.T) {
	sid1 := ValidStateid()
	sid2 := Stateid4{Seqid: 5, Other: [NFS4_OTHER_SIZE]byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66}}
	sid3 := Stateid4{Seqid: 99}

	original := TestStateidArgs{Stateids: []Stateid4{sid1, sid2, sid3}}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded TestStateidArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if len(decoded.Stateids) != 3 {
		t.Fatalf("Stateids length: got %d, want 3", len(decoded.Stateids))
	}
	for i := range original.Stateids {
		if decoded.Stateids[i].Seqid != original.Stateids[i].Seqid {
			t.Errorf("[%d] Seqid: got %d, want %d", i, decoded.Stateids[i].Seqid, original.Stateids[i].Seqid)
		}
		if decoded.Stateids[i].Other != original.Stateids[i].Other {
			t.Errorf("[%d] Other mismatch", i)
		}
	}
}

func TestTestStateidArgs_RoundTrip_Empty(t *testing.T) {
	original := TestStateidArgs{Stateids: []Stateid4{}}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded TestStateidArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if len(decoded.Stateids) != 0 {
		t.Errorf("Stateids length: got %d, want 0", len(decoded.Stateids))
	}
}

func TestTestStateidRes_RoundTrip_OK(t *testing.T) {
	original := TestStateidRes{
		Status:      NFS4_OK,
		StatusCodes: []uint32{NFS4_OK, NFS4ERR_BAD_STATEID, NFS4ERR_OLD_STATEID},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded TestStateidRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Fatalf("Status: got %d, want %d", decoded.Status, NFS4_OK)
	}
	if len(decoded.StatusCodes) != 3 {
		t.Fatalf("StatusCodes length: got %d, want 3", len(decoded.StatusCodes))
	}
	for i, want := range original.StatusCodes {
		if decoded.StatusCodes[i] != want {
			t.Errorf("StatusCodes[%d]: got %d, want %d", i, decoded.StatusCodes[i], want)
		}
	}
}

func TestTestStateidRes_RoundTrip_Error(t *testing.T) {
	original := TestStateidRes{Status: NFS4ERR_SERVERFAULT}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded TestStateidRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4ERR_SERVERFAULT {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4ERR_SERVERFAULT)
	}
	if len(decoded.StatusCodes) != 0 {
		t.Errorf("StatusCodes should be empty on error, got %d", len(decoded.StatusCodes))
	}
}

func TestTestStateidArgs_String(t *testing.T) {
	args := TestStateidArgs{Stateids: []Stateid4{ValidStateid(), ValidStateid()}}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
