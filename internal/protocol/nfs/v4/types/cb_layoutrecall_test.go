package types

import (
	"bytes"
	"testing"
)

func TestCbLayoutRecallArgs_RoundTrip_File(t *testing.T) {
	fh := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	original := CbLayoutRecallArgs{
		LayoutType:  LAYOUT4_NFSV4_1_FILES,
		IOMode:      2, // LAYOUTIOMODE4_RW
		Changed:     true,
		RecallType:  LAYOUTRECALL4_FILE,
		FileOffset:  1024,
		FileLength:  4096,
		FileStateid: ValidStateid(),
		FileFH:      fh,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbLayoutRecallArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.LayoutType != LAYOUT4_NFSV4_1_FILES {
		t.Errorf("LayoutType: got %d, want %d", decoded.LayoutType, LAYOUT4_NFSV4_1_FILES)
	}
	if decoded.IOMode != 2 {
		t.Errorf("IOMode: got %d, want 2", decoded.IOMode)
	}
	if decoded.Changed != true {
		t.Errorf("Changed: got %t, want true", decoded.Changed)
	}
	if decoded.RecallType != LAYOUTRECALL4_FILE {
		t.Errorf("RecallType: got %d, want %d", decoded.RecallType, LAYOUTRECALL4_FILE)
	}
	if decoded.FileOffset != 1024 {
		t.Errorf("FileOffset: got %d, want 1024", decoded.FileOffset)
	}
	if decoded.FileLength != 4096 {
		t.Errorf("FileLength: got %d, want 4096", decoded.FileLength)
	}
	if decoded.FileStateid.Seqid != original.FileStateid.Seqid {
		t.Errorf("FileStateid.Seqid: got %d, want %d", decoded.FileStateid.Seqid, original.FileStateid.Seqid)
	}
	if !bytes.Equal(decoded.FileFH, fh) {
		t.Errorf("FileFH: got %x, want %x", decoded.FileFH, fh)
	}
}

func TestCbLayoutRecallArgs_RoundTrip_Fsid(t *testing.T) {
	original := CbLayoutRecallArgs{
		LayoutType: LAYOUT4_NFSV4_1_FILES,
		IOMode:     1, // LAYOUTIOMODE4_READ
		Changed:    false,
		RecallType: LAYOUTRECALL4_FSID,
		FsidMajor:  0xDEADBEEF,
		FsidMinor:  0xCAFEBABE,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbLayoutRecallArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.RecallType != LAYOUTRECALL4_FSID {
		t.Errorf("RecallType: got %d, want %d", decoded.RecallType, LAYOUTRECALL4_FSID)
	}
	if decoded.FsidMajor != 0xDEADBEEF {
		t.Errorf("FsidMajor: got 0x%x, want 0xDEADBEEF", decoded.FsidMajor)
	}
	if decoded.FsidMinor != 0xCAFEBABE {
		t.Errorf("FsidMinor: got 0x%x, want 0xCAFEBABE", decoded.FsidMinor)
	}
}

func TestCbLayoutRecallArgs_RoundTrip_All(t *testing.T) {
	original := CbLayoutRecallArgs{
		LayoutType: LAYOUT4_BLOCK_VOLUME,
		IOMode:     2, // LAYOUTIOMODE4_RW
		Changed:    true,
		RecallType: LAYOUTRECALL4_ALL,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbLayoutRecallArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.RecallType != LAYOUTRECALL4_ALL {
		t.Errorf("RecallType: got %d, want %d", decoded.RecallType, LAYOUTRECALL4_ALL)
	}
	if decoded.LayoutType != LAYOUT4_BLOCK_VOLUME {
		t.Errorf("LayoutType: got %d, want %d", decoded.LayoutType, LAYOUT4_BLOCK_VOLUME)
	}
	if decoded.Changed != true {
		t.Errorf("Changed: got %t, want true", decoded.Changed)
	}
}

func TestCbLayoutRecallRes_RoundTrip(t *testing.T) {
	for _, status := range []uint32{NFS4_OK, NFS4ERR_NOMATCHING_LAYOUT, NFS4ERR_DELAY} {
		original := CbLayoutRecallRes{Status: status}

		var buf bytes.Buffer
		if err := original.Encode(&buf); err != nil {
			t.Fatalf("Encode status=%d: %v", status, err)
		}

		var decoded CbLayoutRecallRes
		if err := decoded.Decode(&buf); err != nil {
			t.Fatalf("Decode status=%d: %v", status, err)
		}

		if decoded.Status != status {
			t.Errorf("Status: got %d, want %d", decoded.Status, status)
		}
	}
}

func TestCbLayoutRecallArgs_String(t *testing.T) {
	args := CbLayoutRecallArgs{RecallType: LAYOUTRECALL4_FILE}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
