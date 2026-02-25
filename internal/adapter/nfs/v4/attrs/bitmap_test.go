package attrs

import (
	"bytes"
	"testing"
)

// ============================================================================
// EncodeBitmap4 / DecodeBitmap4 Roundtrip Tests
// ============================================================================

func TestBitmap4RoundtripEmpty(t *testing.T) {
	// Empty bitmap (0 words) should roundtrip correctly
	var buf bytes.Buffer
	original := []uint32{}

	if err := EncodeBitmap4(&buf, original); err != nil {
		t.Fatalf("EncodeBitmap4 failed: %v", err)
	}

	decoded, err := DecodeBitmap4(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("DecodeBitmap4 failed: %v", err)
	}

	if len(decoded) != 0 {
		t.Errorf("decoded bitmap length = %d, want 0", len(decoded))
	}
}

func TestBitmap4RoundtripSingleWord(t *testing.T) {
	var buf bytes.Buffer
	original := []uint32{0x0000001F} // bits 0-4 set

	if err := EncodeBitmap4(&buf, original); err != nil {
		t.Fatalf("EncodeBitmap4 failed: %v", err)
	}

	decoded, err := DecodeBitmap4(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("DecodeBitmap4 failed: %v", err)
	}

	if len(decoded) != 1 {
		t.Fatalf("decoded bitmap length = %d, want 1", len(decoded))
	}
	if decoded[0] != original[0] {
		t.Errorf("decoded[0] = 0x%08x, want 0x%08x", decoded[0], original[0])
	}
}

func TestBitmap4RoundtripMultiWord(t *testing.T) {
	var buf bytes.Buffer
	original := []uint32{0xDEADBEEF, 0x12345678, 0xCAFEBABE}

	if err := EncodeBitmap4(&buf, original); err != nil {
		t.Fatalf("EncodeBitmap4 failed: %v", err)
	}

	decoded, err := DecodeBitmap4(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("DecodeBitmap4 failed: %v", err)
	}

	if len(decoded) != 3 {
		t.Fatalf("decoded bitmap length = %d, want 3", len(decoded))
	}
	for i := range original {
		if decoded[i] != original[i] {
			t.Errorf("decoded[%d] = 0x%08x, want 0x%08x", i, decoded[i], original[i])
		}
	}
}

func TestDecodeBitmap4RejectsTooLarge(t *testing.T) {
	// Craft a bitmap with numWords = 9 (exceeds limit of 8)
	var buf bytes.Buffer
	buf.Write([]byte{0, 0, 0, 9}) // numWords = 9

	_, err := DecodeBitmap4(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("expected error for numWords > 8, got nil")
	}
}

// ============================================================================
// IsBitSet Tests
// ============================================================================

func TestIsBitSetInRange(t *testing.T) {
	bitmap := []uint32{0x00000002} // bit 1 set
	if !IsBitSet(bitmap, 1) {
		t.Error("IsBitSet(bitmap, 1) = false, want true")
	}
	if IsBitSet(bitmap, 0) {
		t.Error("IsBitSet(bitmap, 0) = true, want false")
	}
}

func TestIsBitSetBit31(t *testing.T) {
	bitmap := []uint32{0x80000000} // bit 31 set (highest bit of word 0)
	if !IsBitSet(bitmap, 31) {
		t.Error("IsBitSet(bitmap, 31) = false, want true")
	}
}

func TestIsBitSetBit32(t *testing.T) {
	// bit 32 is the lowest bit of word 1
	bitmap := []uint32{0x00000000, 0x00000001}
	if !IsBitSet(bitmap, 32) {
		t.Error("IsBitSet(bitmap, 32) = false, want true")
	}
	if IsBitSet(bitmap, 31) {
		t.Error("IsBitSet(bitmap, 31) = true, want false")
	}
}

func TestIsBitSetBit63(t *testing.T) {
	bitmap := []uint32{0x00000000, 0x80000000}
	if !IsBitSet(bitmap, 63) {
		t.Error("IsBitSet(bitmap, 63) = false, want true")
	}
}

func TestIsBitSetOutOfRange(t *testing.T) {
	bitmap := []uint32{0xFFFFFFFF} // only word 0
	// Bit 32 is in word 1, which doesn't exist
	if IsBitSet(bitmap, 32) {
		t.Error("IsBitSet(bitmap, 32) = true for 1-word bitmap, want false")
	}
	// Bit 64 is in word 2
	if IsBitSet(bitmap, 64) {
		t.Error("IsBitSet(bitmap, 64) = true for 1-word bitmap, want false")
	}
}

func TestIsBitSetEmptyBitmap(t *testing.T) {
	var bitmap []uint32
	if IsBitSet(bitmap, 0) {
		t.Error("IsBitSet(empty, 0) = true, want false")
	}
}

// ============================================================================
// SetBit Tests
// ============================================================================

func TestSetBitExtendsSlice(t *testing.T) {
	bitmap := []uint32{0x00000001} // 1-word bitmap

	// Setting bit 64 should extend to 3 words
	SetBit(&bitmap, 64)

	if len(bitmap) != 3 {
		t.Fatalf("after SetBit(64), len(bitmap) = %d, want 3", len(bitmap))
	}

	// bit 64 is word 2, bit 0
	if bitmap[2] != 0x00000001 {
		t.Errorf("bitmap[2] = 0x%08x, want 0x00000001", bitmap[2])
	}

	// Original bit 0 should still be set
	if bitmap[0] != 0x00000001 {
		t.Errorf("bitmap[0] = 0x%08x, want 0x00000001 (original value preserved)", bitmap[0])
	}

	// Word 1 should be zero (no bits set there)
	if bitmap[1] != 0x00000000 {
		t.Errorf("bitmap[1] = 0x%08x, want 0x00000000", bitmap[1])
	}
}

func TestSetBitInExistingWord(t *testing.T) {
	bitmap := []uint32{0x00000001}
	SetBit(&bitmap, 1)

	if bitmap[0] != 0x00000003 {
		t.Errorf("bitmap[0] = 0x%08x, want 0x00000003", bitmap[0])
	}
}

func TestSetBitOnEmptyBitmap(t *testing.T) {
	var bitmap []uint32
	SetBit(&bitmap, 0)

	if len(bitmap) != 1 {
		t.Fatalf("len(bitmap) = %d, want 1", len(bitmap))
	}
	if bitmap[0] != 0x00000001 {
		t.Errorf("bitmap[0] = 0x%08x, want 0x00000001", bitmap[0])
	}
}

// ============================================================================
// ClearBit Tests
// ============================================================================

func TestClearBit(t *testing.T) {
	bitmap := []uint32{0x00000003} // bits 0 and 1 set
	ClearBit(bitmap, 0)

	if bitmap[0] != 0x00000002 {
		t.Errorf("after ClearBit(0), bitmap[0] = 0x%08x, want 0x00000002", bitmap[0])
	}
}

func TestClearBitOutOfRange(t *testing.T) {
	bitmap := []uint32{0xFFFFFFFF}
	// Should be no-op for out-of-range bit
	ClearBit(bitmap, 32)
	if bitmap[0] != 0xFFFFFFFF {
		t.Errorf("ClearBit(32) modified bitmap[0] = 0x%08x, want 0xFFFFFFFF", bitmap[0])
	}
}

// ============================================================================
// Intersect Tests
// ============================================================================

func TestIntersectOverlapping(t *testing.T) {
	a := []uint32{0xFF00FF00}
	b := []uint32{0x0F0F0F0F}
	result := Intersect(a, b)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	expected := uint32(0x0F000F00)
	if result[0] != expected {
		t.Errorf("result[0] = 0x%08x, want 0x%08x", result[0], expected)
	}
}

func TestIntersectDisjoint(t *testing.T) {
	a := []uint32{0xFF000000}
	b := []uint32{0x000000FF}
	result := Intersect(a, b)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if result[0] != 0 {
		t.Errorf("result[0] = 0x%08x, want 0x00000000", result[0])
	}
}

func TestIntersectDifferentLengths(t *testing.T) {
	// request is 2 words, supported is 1 word
	request := []uint32{0xFFFFFFFF, 0xFFFFFFFF}
	supported := []uint32{0x0000000F}
	result := Intersect(request, supported)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1 (min of 2, 1)", len(result))
	}
	if result[0] != 0x0000000F {
		t.Errorf("result[0] = 0x%08x, want 0x0000000F", result[0])
	}
}

func TestIntersectBothEmpty(t *testing.T) {
	result := Intersect([]uint32{}, []uint32{})
	if len(result) != 0 {
		t.Errorf("len(result) = %d, want 0", len(result))
	}
}

func TestIntersectOneEmpty(t *testing.T) {
	result := Intersect([]uint32{0xFFFFFFFF}, []uint32{})
	if len(result) != 0 {
		t.Errorf("len(result) = %d, want 0", len(result))
	}
}
