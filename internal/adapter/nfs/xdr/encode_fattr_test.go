package xdr

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
)

// goldenFileAttr is a fully-populated fattr3 with distinct, non-trivial values
// in every field so that any byte-offset or endianness regression in
// EncodeFileAttr is detected.
func goldenFileAttr() *types.NFSFileAttr {
	return &types.NFSFileAttr{
		Type:   1,
		Mode:   0o644,
		Nlink:  2,
		UID:    1000,
		GID:    1001,
		Size:   123456789,
		Used:   0xdeadbeefcafe,
		Rdev:   types.SpecData{Major: 7, Minor: 13},
		Fsid:   0x1122334455667788,
		Fileid: 0x99aabbccddeeff00,
		Atime:  types.TimeVal{Seconds: 0x01020304, Nseconds: 0x05060708},
		Mtime:  types.TimeVal{Seconds: 0x11121314, Nseconds: 0x15161718},
		Ctime:  types.TimeVal{Seconds: 0x21222324, Nseconds: 0x25262728},
	}
}

// TestEncodeFileAttr_GoldenBytes asserts byte-for-byte equality against the
// exact wire output captured from the previous reflection-based encoder. The
// fattr3 wire format must never drift, so this golden vector is load-bearing.
func TestEncodeFileAttr_GoldenBytes(t *testing.T) {
	// Captured from the previous reflection-based EncodeFileAttr implementation.
	const golden = "00000001000001a400000002000003e8000003e900000000075bcd150000deadbeefcafe000000070000000d112233445566778899aabbccddeeff00010203040506070811121314151617182122232425262728"

	want, err := hex.DecodeString(golden)
	if err != nil {
		t.Fatalf("decode golden hex: %v", err)
	}

	var buf bytes.Buffer
	if err := EncodeFileAttr(&buf, goldenFileAttr()); err != nil {
		t.Fatalf("EncodeFileAttr: %v", err)
	}

	if got := buf.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("EncodeFileAttr wire mismatch:\n got=%x\nwant=%x", got, want)
	}

	// fattr3 is fixed-size: 5 uint32 + 2 uint64 + specdata3 + 2 uint64 + 3
	// nfstime3 = 84 bytes.
	if buf.Len() != 84 {
		t.Fatalf("EncodeFileAttr length = %d, want 84", buf.Len())
	}
}

func TestEncodeFileAttr_NilReturnsError(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeFileAttr(&buf, nil); err == nil {
		t.Fatal("EncodeFileAttr(nil) = nil error, want error")
	}
}
