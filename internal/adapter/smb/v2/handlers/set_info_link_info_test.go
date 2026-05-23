package handlers

import (
	"bytes"
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

// encodeFileLinkInfoWire serialises a FILE_LINK_INFORMATION blob in MS-FSCC
// 2.4.21.2 wire format for use by tests:
//
//	+0   1B  ReplaceIfExists
//	+1   7B  Reserved
//	+8   8B  RootDirectory
//	+16  4B  FileNameLength (bytes)
//	+20  N   FileName (UTF-16LE)
func encodeFileLinkInfoWire(t *testing.T, replaceIfExists bool, rootDir [8]byte, fileName string) []byte {
	t.Helper()
	utf16Buf := utf16.Encode([]rune(fileName))
	nameBytes := make([]byte, 2*len(utf16Buf))
	for i, w := range utf16Buf {
		binary.LittleEndian.PutUint16(nameBytes[2*i:], w)
	}
	buf := make([]byte, 20+len(nameBytes))
	if replaceIfExists {
		buf[0] = 1
	}
	copy(buf[8:16], rootDir[:])
	binary.LittleEndian.PutUint32(buf[16:20], uint32(len(nameBytes)))
	copy(buf[20:], nameBytes)
	return buf
}

// TestDecodeFileLinkInfo_Basic round-trips the FILE_LINK_INFORMATION wire
// format and pins MS-FSCC 2.4.21.2: ReplaceIfExists, RootDirectory, and
// the UTF-16LE FileName field land in the decoded struct verbatim.
func TestDecodeFileLinkInfo_Basic(t *testing.T) {
	t.Parallel()

	rootDir := [8]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x00, 0x11}
	buf := encodeFileLinkInfoWire(t, true, rootDir, "newlink.txt")

	info, err := DecodeFileLinkInfo(buf)
	if err != nil {
		t.Fatalf("DecodeFileLinkInfo: %v", err)
	}
	if !info.ReplaceIfExists {
		t.Errorf("ReplaceIfExists = false, want true")
	}
	if !bytes.Equal(info.RootDirectory[:], rootDir[:]) {
		t.Errorf("RootDirectory = %x, want %x", info.RootDirectory, rootDir)
	}
	if info.FileName != "newlink.txt" {
		t.Errorf("FileName = %q, want %q", info.FileName, "newlink.txt")
	}
}

// TestDecodeFileLinkInfo_ZeroRootDir pins the zero-RootDirectory case which
// the handler interprets as "FileName is a path from the share root".
func TestDecodeFileLinkInfo_ZeroRootDir(t *testing.T) {
	t.Parallel()

	buf := encodeFileLinkInfoWire(t, false, [8]byte{}, "subdir\\link.dat")
	info, err := DecodeFileLinkInfo(buf)
	if err != nil {
		t.Fatalf("DecodeFileLinkInfo: %v", err)
	}
	if info.ReplaceIfExists {
		t.Errorf("ReplaceIfExists = true, want false")
	}
	for i, b := range info.RootDirectory {
		if b != 0 {
			t.Errorf("RootDirectory[%d] = 0x%02x, want 0", i, b)
		}
	}
	if info.FileName != "subdir\\link.dat" {
		t.Errorf("FileName = %q (backslash should be decoded verbatim — normalization happens in handler)", info.FileName)
	}
}

// TestDecodeFileLinkInfo_TooShort asserts that buffers smaller than the
// 20-byte fixed header are rejected with a descriptive error rather than
// causing an index panic.
func TestDecodeFileLinkInfo_TooShort(t *testing.T) {
	t.Parallel()

	cases := []int{0, 1, 19}
	for _, n := range cases {
		_, err := DecodeFileLinkInfo(make([]byte, n))
		if err == nil {
			t.Errorf("DecodeFileLinkInfo(%d-byte buf): expected error, got nil", n)
		}
	}
}

// TestDecodeFileLinkInfo_FileNameLengthTooLarge asserts that an
// out-of-bounds FileNameLength does not over-read the underlying buffer
// (defence against malformed client input).
func TestDecodeFileLinkInfo_FileNameLengthTooLarge(t *testing.T) {
	t.Parallel()

	buf := make([]byte, 22) // header + 2 bytes of name budget
	binary.LittleEndian.PutUint32(buf[16:20], 1024)
	if _, err := DecodeFileLinkInfo(buf); err == nil {
		t.Errorf("expected error for FileNameLength=1024 with only 2 name bytes available")
	}
}
