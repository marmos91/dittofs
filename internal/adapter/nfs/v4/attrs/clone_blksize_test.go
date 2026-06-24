package attrs

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestSupportedAttrsAdvertisesCloneBlksize asserts FATTR4_CLONE_BLKSIZE (bit 77,
// word 2) is advertised in the server's supported-attrs set so clients learn the
// CLONE alignment.
func TestSupportedAttrsAdvertisesCloneBlksize(t *testing.T) {
	if !IsBitSet(SupportedAttrs(), FATTR4_CLONE_BLKSIZE) {
		t.Fatalf("FATTR4_CLONE_BLKSIZE (bit %d) not advertised in SupportedAttrs()", FATTR4_CLONE_BLKSIZE)
	}
}

// TestEncodeCloneBlksize asserts a GETATTR for FATTR4_CLONE_BLKSIZE encodes the
// advertised block size as a uint32. Uses the pseudo-fs encoder (no metadata
// needed); the real-file encoder shares the same CloneBlockSize value.
func TestEncodeCloneBlksize(t *testing.T) {
	var buf bytes.Buffer
	node := newMockNode()

	var requested []uint32
	SetBit(&requested, FATTR4_CLONE_BLKSIZE)

	if err := EncodePseudoFSAttrs(&buf, requested, node); err != nil {
		t.Fatalf("EncodePseudoFSAttrs failed: %v", err)
	}

	reader := bytes.NewReader(buf.Bytes())
	responseBitmap, err := DecodeBitmap4(reader)
	if err != nil {
		t.Fatalf("decode response bitmap: %v", err)
	}
	if !IsBitSet(responseBitmap, FATTR4_CLONE_BLKSIZE) {
		t.Fatal("FATTR4_CLONE_BLKSIZE not set in response bitmap")
	}

	var opaqueLen uint32
	if err := binary.Read(reader, binary.BigEndian, &opaqueLen); err != nil {
		t.Fatalf("read opaqueLen: %v", err)
	}
	if opaqueLen != 4 {
		t.Fatalf("attr data length = %d, want 4 (uint32)", opaqueLen)
	}

	var got uint32
	if err := binary.Read(reader, binary.BigEndian, &got); err != nil {
		t.Fatalf("read clone_blksize: %v", err)
	}
	if got != CloneBlockSize {
		t.Errorf("clone_blksize = %d, want %d", got, CloneBlockSize)
	}
}
