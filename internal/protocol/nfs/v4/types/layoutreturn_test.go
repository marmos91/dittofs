package types

import (
	"bytes"
	"testing"
)

func TestLayoutReturnArgs_RoundTrip_File(t *testing.T) {
	original := LayoutReturnArgs{
		Reclaim:    false,
		LayoutType: LAYOUT4_NFSV4_1_FILES,
		IOMode:     LAYOUTIOMODE4_RW,
		ReturnType: LAYOUTRETURN4_FILE,
		Offset:     0,
		Length:     1048576,
		Stateid:    ValidStateid(),
		Body:       []byte{0xde, 0xad},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded LayoutReturnArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Reclaim != original.Reclaim {
		t.Errorf("Reclaim: got %t, want %t", decoded.Reclaim, original.Reclaim)
	}
	if decoded.ReturnType != LAYOUTRETURN4_FILE {
		t.Errorf("ReturnType: got %d, want %d", decoded.ReturnType, LAYOUTRETURN4_FILE)
	}
	if decoded.Offset != original.Offset {
		t.Errorf("Offset: got %d, want %d", decoded.Offset, original.Offset)
	}
	if decoded.Length != original.Length {
		t.Errorf("Length: got %d, want %d", decoded.Length, original.Length)
	}
	if decoded.Stateid.Seqid != original.Stateid.Seqid {
		t.Errorf("Stateid.Seqid: got %d, want %d", decoded.Stateid.Seqid, original.Stateid.Seqid)
	}
	if !bytes.Equal(decoded.Body, original.Body) {
		t.Errorf("Body: got %x, want %x", decoded.Body, original.Body)
	}
}

func TestLayoutReturnArgs_RoundTrip_FSID(t *testing.T) {
	original := LayoutReturnArgs{
		Reclaim:    false,
		LayoutType: LAYOUT4_NFSV4_1_FILES,
		IOMode:     LAYOUTIOMODE4_READ,
		ReturnType: LAYOUTRETURN4_FSID,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded LayoutReturnArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.ReturnType != LAYOUTRETURN4_FSID {
		t.Errorf("ReturnType: got %d, want %d", decoded.ReturnType, LAYOUTRETURN4_FSID)
	}
}

func TestLayoutReturnArgs_RoundTrip_All(t *testing.T) {
	original := LayoutReturnArgs{
		Reclaim:    true,
		LayoutType: LAYOUT4_BLOCK_VOLUME,
		IOMode:     LAYOUTIOMODE4_RW,
		ReturnType: LAYOUTRETURN4_ALL,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded LayoutReturnArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.ReturnType != LAYOUTRETURN4_ALL {
		t.Errorf("ReturnType: got %d, want %d", decoded.ReturnType, LAYOUTRETURN4_ALL)
	}
	if decoded.Reclaim != true {
		t.Errorf("Reclaim: got %t, want true", decoded.Reclaim)
	}
}

func TestLayoutReturnRes_RoundTrip_OK_WithStateid(t *testing.T) {
	original := LayoutReturnRes{
		Status:         NFS4_OK,
		StateidPresent: true,
		Stateid:        ValidStateid(),
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded LayoutReturnRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Fatalf("Status: got %d, want OK", decoded.Status)
	}
	if decoded.StateidPresent != true {
		t.Fatal("StateidPresent should be true")
	}
	if decoded.Stateid.Seqid != original.Stateid.Seqid {
		t.Errorf("Stateid.Seqid: got %d, want %d", decoded.Stateid.Seqid, original.Stateid.Seqid)
	}
}

func TestLayoutReturnRes_RoundTrip_OK_NoStateid(t *testing.T) {
	original := LayoutReturnRes{
		Status:         NFS4_OK,
		StateidPresent: false,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded LayoutReturnRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Fatalf("Status: got %d, want OK", decoded.Status)
	}
	if decoded.StateidPresent != false {
		t.Error("StateidPresent should be false")
	}
}

func TestLayoutReturnRes_RoundTrip_Error(t *testing.T) {
	original := LayoutReturnRes{Status: NFS4ERR_NOMATCHING_LAYOUT}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded LayoutReturnRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4ERR_NOMATCHING_LAYOUT {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4ERR_NOMATCHING_LAYOUT)
	}
}

func TestLayoutReturnArgs_String(t *testing.T) {
	args := LayoutReturnArgs{ReturnType: LAYOUTRETURN4_ALL}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
	if !bytes.Contains([]byte(s), []byte("ALL")) {
		t.Errorf("String() should contain ALL: %s", s)
	}
}
