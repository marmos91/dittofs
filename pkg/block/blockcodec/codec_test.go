package blockcodec_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/blockcodec"
)

// fakeSealer is a test Sealer that:
//   - embeds the AAD in the sealed output so Open rejects AAD mismatches
//   - captures each AAD passed to Seal for post-round-trip assertions
//   - XORs the plaintext with 0x42 for opacity
//
// Wire format: [0xFE marker] [aadLen uint8] [aad bytes] [XOR(plaintext, 0x42)]
type fakeSealer struct {
	capturedAADs [][]byte
}

func (s *fakeSealer) Seal(plaintext, aad []byte) ([]byte, error) {
	if len(aad) > 255 {
		return nil, fmt.Errorf("fakeSealer: aad length %d exceeds 255", len(aad))
	}
	aadCopy := make([]byte, len(aad))
	copy(aadCopy, aad)
	s.capturedAADs = append(s.capturedAADs, aadCopy)

	out := make([]byte, 2+len(aad)+len(plaintext))
	out[0] = 0xFE // nonce marker
	out[1] = byte(len(aad))
	copy(out[2:], aad)
	for i, b := range plaintext {
		out[2+len(aad)+i] = b ^ 0x42
	}
	return out, nil
}

func (s *fakeSealer) Open(sealed, aad []byte) ([]byte, error) {
	if len(sealed) < 2 || sealed[0] != 0xFE {
		return nil, errors.New("fakeSealer: invalid nonce marker")
	}
	aadLen := int(sealed[1])
	if len(sealed) < 2+aadLen {
		return nil, errors.New("fakeSealer: sealed data truncated")
	}
	if !bytes.Equal(sealed[2:2+aadLen], aad) {
		return nil, fmt.Errorf("fakeSealer: AAD mismatch: sealed %x, open %x", sealed[2:2+aadLen], aad)
	}
	ciphertext := sealed[2+aadLen:]
	out := make([]byte, len(ciphertext))
	for i, b := range ciphertext {
		out[i] = b ^ 0x42
	}
	return out, nil
}

// TestPlaintextRoundTrip builds a two-chunk plaintext block, parses it, and
// verifies that hashes, locators, and body bytes all round-trip correctly.
func TestPlaintextRoundTrip(t *testing.T) {
	var buf bytes.Buffer

	var hash1 block.ContentHash
	hash1[0] = 0xAA
	hash1[1] = 0xBB

	var hash2 block.ContentHash
	hash2[0] = 0xCC
	hash2[31] = 0xDD

	wire1 := []byte("chunk-one-bytes")
	wire2 := []byte("chunk-two-bytes-longer-payload-here")

	b, err := blockcodec.NewBuilder(&buf, "block-001", nil)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	loc1, err := b.Add(hash1, wire1)
	if err != nil {
		t.Fatalf("Add(hash1): %v", err)
	}

	loc2, err := b.Add(hash2, wire2)
	if err != nil {
		t.Fatalf("Add(hash2): %v", err)
	}

	total, err := b.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if int64(buf.Len()) != total {
		t.Errorf("Finish total=%d != buf.Len()=%d", total, buf.Len())
	}

	raw := buf.Bytes()

	blockID, records, err := blockcodec.Parse(raw, nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if blockID != "block-001" {
		t.Errorf("blockID=%q want %q", blockID, "block-001")
	}
	if len(records) != 2 {
		t.Fatalf("len(records)=%d want 2", len(records))
	}

	// Record 0
	if records[0].Hash != hash1 {
		t.Errorf("records[0].Hash mismatch")
	}
	if records[0].WireOffset != loc1.WireOffset {
		t.Errorf("records[0].WireOffset=%d want %d", records[0].WireOffset, loc1.WireOffset)
	}
	if records[0].WireLength != loc1.WireLength {
		t.Errorf("records[0].WireLength=%d want %d", records[0].WireLength, loc1.WireLength)
	}
	got1 := raw[records[0].WireOffset : records[0].WireOffset+records[0].WireLength]
	if !bytes.Equal(got1, wire1) {
		t.Errorf("body1=%q want %q", got1, wire1)
	}

	// Record 1
	if records[1].Hash != hash2 {
		t.Errorf("records[1].Hash mismatch")
	}
	if records[1].WireOffset != loc2.WireOffset {
		t.Errorf("records[1].WireOffset=%d want %d", records[1].WireOffset, loc2.WireOffset)
	}
	if records[1].WireLength != loc2.WireLength {
		t.Errorf("records[1].WireLength=%d want %d", records[1].WireLength, loc2.WireLength)
	}
	got2 := raw[records[1].WireOffset : records[1].WireOffset+records[1].WireLength]
	if !bytes.Equal(got2, wire2) {
		t.Errorf("body2=%q want %q", got2, wire2)
	}
}

// TestEmptyBody verifies that a zero-length wire body is framed and parsed
// without error, and the returned locator reports Length==0.
func TestEmptyBody(t *testing.T) {
	var buf bytes.Buffer

	var hash block.ContentHash
	hash[0] = 0xCC

	b, err := blockcodec.NewBuilder(&buf, "b", nil)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	loc, err := b.Add(hash, nil)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if loc.WireLength != 0 {
		t.Errorf("loc.WireLength=%d want 0", loc.WireLength)
	}

	if _, err := b.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	blockID, records, err := blockcodec.Parse(buf.Bytes(), nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if blockID != "b" {
		t.Errorf("blockID=%q want %q", blockID, "b")
	}
	if len(records) != 1 {
		t.Fatalf("len(records)=%d want 1", len(records))
	}
	if records[0].Hash != hash {
		t.Errorf("records[0].Hash mismatch")
	}
	if records[0].WireLength != 0 {
		t.Errorf("records[0].WireLength=%d want 0", records[0].WireLength)
	}
}

// TestLargeChunk verifies that a chunk larger than any typical block target
// (one-chunk block) round-trips correctly.
func TestLargeChunk(t *testing.T) {
	const size = 16 * 1024 * 1024 // 16 MiB, well above any block target

	wire := make([]byte, size)
	for i := range wire {
		wire[i] = byte(i & 0xFF)
	}

	var hash block.ContentHash
	hash[0] = 0xDD

	var buf bytes.Buffer
	b, err := blockcodec.NewBuilder(&buf, "large-block", nil)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	loc, err := b.Add(hash, wire)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if loc.WireLength != int64(size) {
		t.Errorf("loc.WireLength=%d want %d", loc.WireLength, size)
	}

	if _, err := b.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	raw := buf.Bytes()
	blockID, records, err := blockcodec.Parse(raw, nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if blockID != "large-block" {
		t.Errorf("blockID=%q want %q", blockID, "large-block")
	}
	if len(records) != 1 {
		t.Fatalf("len(records)=%d want 1", len(records))
	}
	if records[0].Hash != hash {
		t.Errorf("records[0].Hash mismatch")
	}
	if records[0].WireLength != int64(size) {
		t.Errorf("records[0].WireLength=%d want %d", records[0].WireLength, size)
	}
	got := raw[records[0].WireOffset : records[0].WireOffset+records[0].WireLength]
	if !bytes.Equal(got, wire) {
		t.Errorf("large chunk body mismatch (first diff at byte %d)", firstDiff(got, wire))
	}
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return len(a)
}

// TestSealedRoundTrip builds a sealed block, verifies that parsing without a
// Sealer fails (headers are opaque), then parses with the Sealer and verifies
// hashes and locators recover correctly. Bodies are always plaintext-visible at
// WireOffset/WireLength.
//
// It also asserts that buildAAD produces blockID_bytes || uvarint(recordIndex)
// by inspecting the AAD captured by fakeSealer for record 0.
func TestSealedRoundTrip(t *testing.T) {
	sealer := &fakeSealer{}

	var hash1 block.ContentHash
	hash1[0] = 0xAA
	hash1[1] = 0xBB

	wire1 := []byte("sealed-chunk-payload")

	var buf bytes.Buffer
	b, err := blockcodec.NewBuilder(&buf, "enc-block", sealer)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	loc, err := b.Add(hash1, wire1)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if _, err := b.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	raw := buf.Bytes()

	// Sealed block parsed without a Sealer must error (headers are opaque).
	if _, _, err := blockcodec.Parse(raw, nil); err == nil {
		t.Error("Parse of sealed block without Sealer should have failed")
	}

	// With the Sealer the block round-trips.
	blockID, records, err := blockcodec.Parse(raw, sealer)
	if err != nil {
		t.Fatalf("Parse with sealer: %v", err)
	}
	if blockID != "enc-block" {
		t.Errorf("blockID=%q want %q", blockID, "enc-block")
	}
	if len(records) != 1 {
		t.Fatalf("len(records)=%d want 1", len(records))
	}
	if records[0].Hash != hash1 {
		t.Errorf("records[0].Hash mismatch")
	}
	if records[0].WireOffset != loc.WireOffset {
		t.Errorf("records[0].WireOffset=%d want %d", records[0].WireOffset, loc.WireOffset)
	}
	if records[0].WireLength != loc.WireLength {
		t.Errorf("records[0].WireLength=%d want %d", records[0].WireLength, loc.WireLength)
	}
	// Bodies live in plaintext-visible region (after the sealed header).
	got := raw[records[0].WireOffset : records[0].WireOffset+records[0].WireLength]
	if !bytes.Equal(got, wire1) {
		t.Errorf("body=%q want %q", got, wire1)
	}

	// Assert buildAAD("enc-block", 0) == []byte("enc-block") || uvarint(0).
	// uvarint(0) encodes as a single 0x00 byte.
	if len(sealer.capturedAADs) < 1 {
		t.Fatal("fakeSealer captured no AADs during Build")
	}
	wantAAD := append([]byte("enc-block"), 0x00) // blockID bytes || uvarint(0)
	if !bytes.Equal(sealer.capturedAADs[0], wantAAD) {
		t.Errorf("record 0 AAD = %x, want %x", sealer.capturedAADs[0], wantAAD)
	}
}

// TestTruncated verifies that truncating a valid block at various offsets
// returns a structural error and never panics.
func TestTruncated(t *testing.T) {
	// Build a reference block.
	var buf bytes.Buffer
	b, err := blockcodec.NewBuilder(&buf, "block", nil)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	var hash block.ContentHash
	if _, err := b.Add(hash, []byte("body data here")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := b.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	raw := buf.Bytes()

	cuts := []int{0, 1, 2, 4, len(raw) / 2, len(raw) - 1}
	for _, n := range cuts {
		t.Run(fmt.Sprintf("truncate_at_%d", n), func(t *testing.T) {
			_, _, err := blockcodec.Parse(raw[:n], nil)
			if err == nil {
				t.Errorf("expected error for truncation at %d, got nil", n)
			}
		})
	}
}

// TestParseRejectsOverlongLength verifies that a crafted block whose wireLen
// uvarint encodes math.MaxUint64 is rejected with an error and does not panic.
// Before the bounds guard, int(wireLen) wraps to -1 on amd64, bypasses the
// readN guard (remaining >= 0 is never < -1), and the slice expression
// buf[pos : pos+(-1)] panics. This test must fail (panic) before the fix.
func TestParseRejectsOverlongLength(t *testing.T) {
	// Construct a syntactically plausible plaintext block:
	//   magic(4) | flags=0x00 | blockID "test" (uvarint(4) + 4 bytes) |
	//   hash(32 zero bytes) | wireLen = MaxUint64 (10-byte uvarint) | (no body)
	var buf []byte
	buf = append(buf, 'D', 'F', 'B', '1') // magic
	buf = append(buf, 0x00)               // flags = plaintext
	// blockID = "test" — length 4 as uvarint(4) = 0x04
	buf = append(buf, 0x04, 't', 'e', 's', 't')
	// hash: 32 zero bytes
	buf = append(buf, make([]byte, 32)...)
	// wireLen = math.MaxUint64 = 0xFFFFFFFFFFFFFFFF encoded as uvarint:
	//   9 bytes of 0xFF (7-bit groups with continuation) + 0x01 (final bit)
	buf = append(buf, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01)
	// No body follows — Parse must return a structural error, never panic.

	_, _, err := blockcodec.Parse(buf, nil)
	if err == nil {
		t.Fatal("Parse with wireLen=math.MaxUint64 must return an error, got nil")
	}
}

// TestMagicMismatch verifies that wrong magic bytes and wrong flags produce errors.
func TestMagicMismatch(t *testing.T) {
	t.Run("wrong_magic", func(t *testing.T) {
		data := []byte{'X', 'Y', 'Z', 'W', 0x00, 0x00} // bad magic + flags + empty blockID
		_, _, err := blockcodec.Parse(data, nil)
		if err == nil {
			t.Error("expected error for wrong magic, got nil")
		}
	})

	t.Run("sealed_flag_no_sealer", func(t *testing.T) {
		// Build a valid plaintext block then flip the sealed flag bit.
		var buf bytes.Buffer
		b, err := blockcodec.NewBuilder(&buf, "b", nil)
		if err != nil {
			t.Fatalf("NewBuilder: %v", err)
		}
		b.Finish() //nolint:errcheck
		raw := make([]byte, buf.Len())
		copy(raw, buf.Bytes())
		raw[4] |= 0x01 // set bit0 = sealed
		_, _, err = blockcodec.Parse(raw, nil)
		if err == nil {
			t.Error("expected error for sealed block without Sealer, got nil")
		}
	})
}
