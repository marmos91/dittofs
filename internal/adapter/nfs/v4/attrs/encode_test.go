package attrs

import (
	"bytes"
	"encoding/binary"
	"sync"
	"testing"

	v4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// ============================================================================
// Test Helper: Mock PseudoFSAttrSource
// ============================================================================

type mockPseudoNode struct {
	handle   []byte
	fsidMaj  uint64
	fsidMin  uint64
	fileID   uint64
	changeID uint64
	fileType uint32
}

func (m *mockPseudoNode) GetHandle() []byte         { return m.handle }
func (m *mockPseudoNode) GetFSID() (uint64, uint64) { return m.fsidMaj, m.fsidMin }
func (m *mockPseudoNode) GetFileID() uint64         { return m.fileID }
func (m *mockPseudoNode) GetChangeID() uint64       { return m.changeID }
func (m *mockPseudoNode) GetType() uint32           { return m.fileType }

func newMockNode() *mockPseudoNode {
	return &mockPseudoNode{
		handle:   []byte("pseudofs:/"),
		fsidMaj:  0,
		fsidMin:  1,
		fileID:   1,
		changeID: 42,
		fileType: v4types.NF4DIR,
	}
}

// ============================================================================
// SupportedAttrs Tests
// ============================================================================

func TestSupportedAttrsNonEmpty(t *testing.T) {
	bitmap := SupportedAttrs()
	if len(bitmap) == 0 {
		t.Fatal("SupportedAttrs() returned empty bitmap")
	}
}

func TestSupportedAttrsHasMandatoryBits(t *testing.T) {
	bitmap := SupportedAttrs()

	mandatoryBits := []uint32{
		FATTR4_SUPPORTED_ATTRS,
		FATTR4_TYPE,
		FATTR4_FH_EXPIRE_TYPE,
		FATTR4_CHANGE,
		FATTR4_SIZE,
		FATTR4_LINK_SUPPORT,
		FATTR4_SYMLINK_SUPPORT,
		FATTR4_NAMED_ATTR,
		FATTR4_FSID,
		FATTR4_UNIQUE_HANDLES,
		FATTR4_LEASE_TIME,
		FATTR4_RDATTR_ERROR,
	}

	for _, bit := range mandatoryBits {
		if !IsBitSet(bitmap, bit) {
			t.Errorf("SupportedAttrs() missing mandatory bit %d", bit)
		}
	}
}

func TestSupportedAttrsHasRecommendedBits(t *testing.T) {
	bitmap := SupportedAttrs()

	recommendedBits := []uint32{
		FATTR4_FILEHANDLE,
		FATTR4_FILEID,
		FATTR4_MODE,
		FATTR4_NUMLINKS,
		FATTR4_OWNER,
		FATTR4_OWNER_GROUP,
		FATTR4_SPACE_USED,
		FATTR4_TIME_ACCESS,
		FATTR4_TIME_MODIFY,
		FATTR4_MOUNTED_ON_FILEID,
	}

	for _, bit := range recommendedBits {
		if !IsBitSet(bitmap, bit) {
			t.Errorf("SupportedAttrs() missing recommended bit %d", bit)
		}
	}
}

// ============================================================================
// EncodePseudoFSAttrs Tests
// ============================================================================

func TestEncodePseudoFSAttrsEmptyRequest(t *testing.T) {
	var buf bytes.Buffer
	node := newMockNode()

	err := EncodePseudoFSAttrs(&buf, []uint32{}, node)
	if err != nil {
		t.Fatalf("EncodePseudoFSAttrs failed: %v", err)
	}

	// Should contain: empty bitmap (1 uint32 = 0) + empty opaque data (1 uint32 = 0)
	data := buf.Bytes()

	// Read response bitmap word count
	reader := bytes.NewReader(data)
	var numWords uint32
	if err := binary.Read(reader, binary.BigEndian, &numWords); err != nil {
		t.Fatalf("read numWords: %v", err)
	}
	if numWords != 0 {
		t.Errorf("response bitmap numWords = %d, want 0", numWords)
	}

	// Read opaque data length
	var opaqueLen uint32
	if err := binary.Read(reader, binary.BigEndian, &opaqueLen); err != nil {
		t.Fatalf("read opaqueLen: %v", err)
	}
	if opaqueLen != 0 {
		t.Errorf("attr data length = %d, want 0", opaqueLen)
	}
}

func TestEncodePseudoFSAttrsTypeRequested(t *testing.T) {
	var buf bytes.Buffer
	node := newMockNode()

	// Request only TYPE (bit 1)
	var requested []uint32
	SetBit(&requested, FATTR4_TYPE)

	err := EncodePseudoFSAttrs(&buf, requested, node)
	if err != nil {
		t.Fatalf("EncodePseudoFSAttrs failed: %v", err)
	}

	// Parse response
	reader := bytes.NewReader(buf.Bytes())

	// Read response bitmap
	responseBitmap, err := DecodeBitmap4(reader)
	if err != nil {
		t.Fatalf("decode response bitmap: %v", err)
	}

	// TYPE bit should be set in response
	if !IsBitSet(responseBitmap, FATTR4_TYPE) {
		t.Error("FATTR4_TYPE not set in response bitmap")
	}

	// No other bits should be set
	for bit := uint32(0); bit < uint32(len(responseBitmap)*32); bit++ {
		if bit == FATTR4_TYPE {
			continue
		}
		if IsBitSet(responseBitmap, bit) {
			t.Errorf("unexpected bit %d set in response bitmap", bit)
		}
	}

	// Read opaque data length
	var opaqueLen uint32
	if err := binary.Read(reader, binary.BigEndian, &opaqueLen); err != nil {
		t.Fatalf("read opaqueLen: %v", err)
	}

	// TYPE is a uint32 = 4 bytes
	if opaqueLen != 4 {
		t.Fatalf("attr data length = %d, want 4", opaqueLen)
	}

	// Read the TYPE value
	var fileType uint32
	if err := binary.Read(reader, binary.BigEndian, &fileType); err != nil {
		t.Fatalf("read file type: %v", err)
	}
	if fileType != v4types.NF4DIR {
		t.Errorf("file type = %d, want %d (NF4DIR)", fileType, v4types.NF4DIR)
	}
}

func TestEncodePseudoFSAttrsFSIDRequested(t *testing.T) {
	var buf bytes.Buffer
	node := &mockPseudoNode{
		handle:   []byte("test"),
		fsidMaj:  42,
		fsidMin:  7,
		fileID:   1,
		changeID: 1,
		fileType: v4types.NF4DIR,
	}

	// Request only FSID (bit 8)
	var requested []uint32
	SetBit(&requested, FATTR4_FSID)

	err := EncodePseudoFSAttrs(&buf, requested, node)
	if err != nil {
		t.Fatalf("EncodePseudoFSAttrs failed: %v", err)
	}

	// Parse response
	reader := bytes.NewReader(buf.Bytes())

	// Read response bitmap
	responseBitmap, err := DecodeBitmap4(reader)
	if err != nil {
		t.Fatalf("decode response bitmap: %v", err)
	}

	if !IsBitSet(responseBitmap, FATTR4_FSID) {
		t.Error("FATTR4_FSID not set in response bitmap")
	}

	// Read opaque data length
	var opaqueLen uint32
	if err := binary.Read(reader, binary.BigEndian, &opaqueLen); err != nil {
		t.Fatalf("read opaqueLen: %v", err)
	}

	// FSID is two uint64s = 16 bytes
	if opaqueLen != 16 {
		t.Fatalf("attr data length = %d, want 16", opaqueLen)
	}

	// Read the FSID major and minor
	var major, minor uint64
	if err := binary.Read(reader, binary.BigEndian, &major); err != nil {
		t.Fatalf("read fsid major: %v", err)
	}
	if err := binary.Read(reader, binary.BigEndian, &minor); err != nil {
		t.Fatalf("read fsid minor: %v", err)
	}

	if major != 42 {
		t.Errorf("fsid major = %d, want 42", major)
	}
	if minor != 7 {
		t.Errorf("fsid minor = %d, want 7", minor)
	}
}

func TestEncodePseudoFSAttrsUnsupportedBitNotInResponse(t *testing.T) {
	var buf bytes.Buffer
	node := newMockNode()

	// Request a bit we do NOT support (bit 62 = some hypothetical future attr)
	var requested []uint32
	SetBit(&requested, 62) // Not in SupportedAttrs

	err := EncodePseudoFSAttrs(&buf, requested, node)
	if err != nil {
		t.Fatalf("EncodePseudoFSAttrs failed: %v", err)
	}

	// Parse response
	reader := bytes.NewReader(buf.Bytes())

	// Read response bitmap
	responseBitmap, err := DecodeBitmap4(reader)
	if err != nil {
		t.Fatalf("decode response bitmap: %v", err)
	}

	// Bit 62 should NOT be in response (not supported)
	if IsBitSet(responseBitmap, 62) {
		t.Error("unsupported bit 62 should not be in response bitmap")
	}

	// No bits should be set (our request had no supported bits)
	for bit := uint32(0); bit < uint32(len(responseBitmap)*32); bit++ {
		if IsBitSet(responseBitmap, bit) {
			t.Errorf("unexpected bit %d set in response bitmap", bit)
		}
	}

	// Read opaque data length - should be 0
	var opaqueLen uint32
	if err := binary.Read(reader, binary.BigEndian, &opaqueLen); err != nil {
		t.Fatalf("read opaqueLen: %v", err)
	}
	if opaqueLen != 0 {
		t.Errorf("attr data length = %d, want 0 (no supported attrs requested)", opaqueLen)
	}
}

func TestEncodePseudoFSAttrsMultipleAttributes(t *testing.T) {
	var buf bytes.Buffer
	node := newMockNode()

	// Request TYPE + CHANGE + SIZE (bits 1, 3, 4)
	var requested []uint32
	SetBit(&requested, FATTR4_TYPE)
	SetBit(&requested, FATTR4_CHANGE)
	SetBit(&requested, FATTR4_SIZE)

	err := EncodePseudoFSAttrs(&buf, requested, node)
	if err != nil {
		t.Fatalf("EncodePseudoFSAttrs failed: %v", err)
	}

	// Parse response
	reader := bytes.NewReader(buf.Bytes())

	// Read response bitmap
	responseBitmap, err := DecodeBitmap4(reader)
	if err != nil {
		t.Fatalf("decode response bitmap: %v", err)
	}

	// All three bits should be set
	if !IsBitSet(responseBitmap, FATTR4_TYPE) {
		t.Error("FATTR4_TYPE not set in response")
	}
	if !IsBitSet(responseBitmap, FATTR4_CHANGE) {
		t.Error("FATTR4_CHANGE not set in response")
	}
	if !IsBitSet(responseBitmap, FATTR4_SIZE) {
		t.Error("FATTR4_SIZE not set in response")
	}

	// Read opaque data length
	// TYPE=4 bytes + CHANGE=8 bytes + SIZE=8 bytes = 20 bytes
	var opaqueLen uint32
	if err := binary.Read(reader, binary.BigEndian, &opaqueLen); err != nil {
		t.Fatalf("read opaqueLen: %v", err)
	}
	if opaqueLen != 20 {
		t.Fatalf("attr data length = %d, want 20 (4+8+8)", opaqueLen)
	}

	// Read TYPE (uint32 = NF4DIR = 2)
	var fileType uint32
	if err := binary.Read(reader, binary.BigEndian, &fileType); err != nil {
		t.Fatalf("read type: %v", err)
	}
	if fileType != v4types.NF4DIR {
		t.Errorf("type = %d, want %d", fileType, v4types.NF4DIR)
	}

	// Read CHANGE (uint64 = 42)
	var change uint64
	if err := binary.Read(reader, binary.BigEndian, &change); err != nil {
		t.Fatalf("read change: %v", err)
	}
	if change != 42 {
		t.Errorf("change = %d, want 42", change)
	}

	// Read SIZE (uint64 = 0 for directory)
	var size uint64
	if err := binary.Read(reader, binary.BigEndian, &size); err != nil {
		t.Fatalf("read size: %v", err)
	}
	if size != 0 {
		t.Errorf("size = %d, want 0", size)
	}
}

// TestNonZeroFileID verifies the wire-level guard that no fileid of 0 is ever
// emitted. A fileid of 0 is illegal per RFC 7530 and kernel-panics the macOS
// NFSv4.1 client (regression test for the pseudo-fs root fileid=0 crash).
func TestNonZeroFileID(t *testing.T) {
	if got := nonZeroFileID(0); got != 1 {
		t.Errorf("nonZeroFileID(0) = %d, want 1", got)
	}
	for _, id := range []uint64{1, 2, 42, 1 << 40, ^uint64(0)} {
		if got := nonZeroFileID(id); got != id {
			t.Errorf("nonZeroFileID(%d) = %d, want %d (must pass through non-zero)", id, got, id)
		}
	}
}

// TestEncodePseudoFSAttrsFileIDNeverZero ensures a node whose GetFileID() yields
// 0 is still encoded with a non-zero FATTR4_FILEID on the wire, so a buggy or
// uninitialized source can never trigger the macOS fileid-0 panic.
func TestEncodePseudoFSAttrsFileIDNeverZero(t *testing.T) {
	var buf bytes.Buffer
	node := &mockPseudoNode{
		handle:   []byte("pseudofs:/"),
		fsidMaj:  0,
		fsidMin:  1,
		fileID:   0, // deliberately zero
		changeID: 1,
		fileType: v4types.NF4DIR,
	}

	var requested []uint32
	SetBit(&requested, FATTR4_FILEID)
	SetBit(&requested, FATTR4_MOUNTED_ON_FILEID)

	if err := EncodePseudoFSAttrs(&buf, requested, node); err != nil {
		t.Fatalf("EncodePseudoFSAttrs failed: %v", err)
	}

	reader := bytes.NewReader(buf.Bytes())
	responseBitmap, err := DecodeBitmap4(reader)
	if err != nil {
		t.Fatalf("decode response bitmap: %v", err)
	}
	if !IsBitSet(responseBitmap, FATTR4_FILEID) {
		t.Fatal("FATTR4_FILEID not set in response bitmap")
	}
	if !IsBitSet(responseBitmap, FATTR4_MOUNTED_ON_FILEID) {
		t.Fatal("FATTR4_MOUNTED_ON_FILEID not set in response bitmap")
	}

	var opaqueLen uint32
	if err := binary.Read(reader, binary.BigEndian, &opaqueLen); err != nil {
		t.Fatalf("read opaqueLen: %v", err)
	}
	// FILEID + MOUNTED_ON_FILEID = two uint64s, encoded in ascending bit order.
	if opaqueLen != 16 {
		t.Fatalf("attr data length = %d, want 16 (two uint64s)", opaqueLen)
	}
	var fileID, mountedOnFileID uint64
	if err := binary.Read(reader, binary.BigEndian, &fileID); err != nil {
		t.Fatalf("read fileid: %v", err)
	}
	if err := binary.Read(reader, binary.BigEndian, &mountedOnFileID); err != nil {
		t.Fatalf("read mounted_on_fileid: %v", err)
	}
	if fileID == 0 {
		t.Error("FATTR4_FILEID encoded as 0 (illegal; crashes macOS client)")
	}
	if mountedOnFileID == 0 {
		t.Error("FATTR4_MOUNTED_ON_FILEID encoded as 0 (illegal; crashes macOS client)")
	}
}

// TestSetLeaseTime_NoDataRace exercises concurrent SetLeaseTime writes against
// FATTR4_LEASE_TIME encode reads. Run with -race to detect the data race that
// existed before leaseTimeSeconds was made atomic.
func TestSetLeaseTime_NoDataRace(t *testing.T) {
	// Restore original value after test.
	orig := GetLeaseTime()
	t.Cleanup(func() { SetLeaseTime(orig) })

	const goroutines = 8
	ready := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Half goroutines write.
	for i := 0; i < goroutines; i++ {
		go func(v uint32) {
			defer wg.Done()
			<-ready
			SetLeaseTime(v)
		}(uint32(30 + i))
	}

	// Half goroutines read via the encode path (exercises a read site).
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-ready
			node := newMockNode()
			var buf bytes.Buffer
			var req []uint32
			SetBit(&req, FATTR4_LEASE_TIME)
			_ = EncodePseudoFSAttrs(&buf, req, node)
		}()
	}

	close(ready)
	wg.Wait()
}

// TestSetIdentityMapper_NoDataRace exercises concurrent SetIdentityMapper writes
// against FATTR4_OWNER encode reads (resolveOwnerString). Run with -race to detect
// the data race that existed before identityMapper became an atomic.Pointer.
func TestSetIdentityMapper_NoDataRace(t *testing.T) {
	// Restore nil after test.
	t.Cleanup(func() { SetIdentityMapper(nil) })

	const goroutines = 8
	ready := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Half goroutines swap the mapper.
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-ready
			SetIdentityMapper(nil)
		}()
	}

	// Half goroutines read via OWNER encoding (calls resolveOwnerString).
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-ready
			node := newMockNode()
			var buf bytes.Buffer
			var req []uint32
			SetBit(&req, FATTR4_OWNER)
			SetBit(&req, FATTR4_OWNER_GROUP)
			_ = EncodePseudoFSAttrs(&buf, req, node)
		}()
	}

	close(ready)
	wg.Wait()
}
