package types

import (
	"bytes"
	"testing"
)

func TestLayoutCommitArgs_RoundTrip_AllOptionals(t *testing.T) {
	original := LayoutCommitArgs{
		Offset:            0,
		Length:            1048576,
		Reclaim:           false,
		Stateid:           ValidStateid(),
		NewOffsetPresent:  true,
		NewOffset:         524288,
		TimeModifyPresent: true,
		TimeModify:        NFS4Time{Seconds: 1700000000, Nseconds: 123456789},
		LayoutUpdateType:  LAYOUT4_NFSV4_1_FILES,
		LayoutUpdate:      []byte{0x01, 0x02, 0x03},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded LayoutCommitArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Offset != original.Offset {
		t.Errorf("Offset: got %d, want %d", decoded.Offset, original.Offset)
	}
	if decoded.Length != original.Length {
		t.Errorf("Length: got %d, want %d", decoded.Length, original.Length)
	}
	if decoded.Reclaim != original.Reclaim {
		t.Errorf("Reclaim: got %t, want %t", decoded.Reclaim, original.Reclaim)
	}
	if decoded.NewOffsetPresent != true {
		t.Fatal("NewOffsetPresent should be true")
	}
	if decoded.NewOffset != 524288 {
		t.Errorf("NewOffset: got %d, want 524288", decoded.NewOffset)
	}
	if decoded.TimeModifyPresent != true {
		t.Fatal("TimeModifyPresent should be true")
	}
	if decoded.TimeModify.Seconds != 1700000000 {
		t.Errorf("TimeModify.Seconds: got %d, want 1700000000", decoded.TimeModify.Seconds)
	}
	if decoded.TimeModify.Nseconds != 123456789 {
		t.Errorf("TimeModify.Nseconds: got %d, want 123456789", decoded.TimeModify.Nseconds)
	}
	if decoded.LayoutUpdateType != LAYOUT4_NFSV4_1_FILES {
		t.Errorf("LayoutUpdateType: got %d, want %d", decoded.LayoutUpdateType, LAYOUT4_NFSV4_1_FILES)
	}
	if !bytes.Equal(decoded.LayoutUpdate, original.LayoutUpdate) {
		t.Errorf("LayoutUpdate: got %x, want %x", decoded.LayoutUpdate, original.LayoutUpdate)
	}
}

func TestLayoutCommitArgs_RoundTrip_NoOptionals(t *testing.T) {
	original := LayoutCommitArgs{
		Offset:            4096,
		Length:            8192,
		Reclaim:           true,
		Stateid:           ValidStateid(),
		NewOffsetPresent:  false,
		TimeModifyPresent: false,
		LayoutUpdateType:  LAYOUT4_BLOCK_VOLUME,
		LayoutUpdate:      []byte{},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded LayoutCommitArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.NewOffsetPresent != false {
		t.Error("NewOffsetPresent should be false")
	}
	if decoded.TimeModifyPresent != false {
		t.Error("TimeModifyPresent should be false")
	}
	if decoded.Reclaim != true {
		t.Error("Reclaim should be true")
	}
}

func TestLayoutCommitRes_RoundTrip_OK_WithSize(t *testing.T) {
	original := LayoutCommitRes{
		Status:         NFS4_OK,
		NewSizePresent: true,
		NewSize:        2097152,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded LayoutCommitRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Fatalf("Status: got %d, want OK", decoded.Status)
	}
	if decoded.NewSizePresent != true {
		t.Fatal("NewSizePresent should be true")
	}
	if decoded.NewSize != 2097152 {
		t.Errorf("NewSize: got %d, want 2097152", decoded.NewSize)
	}
}

func TestLayoutCommitRes_RoundTrip_OK_NoSize(t *testing.T) {
	original := LayoutCommitRes{
		Status:         NFS4_OK,
		NewSizePresent: false,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded LayoutCommitRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Fatalf("Status: got %d, want OK", decoded.Status)
	}
	if decoded.NewSizePresent != false {
		t.Error("NewSizePresent should be false")
	}
}

func TestLayoutCommitRes_RoundTrip_Error(t *testing.T) {
	original := LayoutCommitRes{Status: NFS4ERR_NOTSUPP}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded LayoutCommitRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4ERR_NOTSUPP {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4ERR_NOTSUPP)
	}
}

func TestLayoutCommitArgs_String(t *testing.T) {
	args := LayoutCommitArgs{Offset: 100, Length: 200}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
