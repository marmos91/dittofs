package destination

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"testing"
)

// TestHashTee_KnownVector asserts the tee writer produces the canonical
// SHA-256 digest for the "abc" test vector, passes all bytes through to
// the underlying sink, and tracks the byte count.
func TestHashTee_KnownVector(t *testing.T) {
	var buf bytes.Buffer
	tee := newHashTeeWriter(&buf)

	n, err := tee.Write([]byte("abc"))
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != 3 {
		t.Fatalf("Write returned n=%d, want 3", n)
	}
	if buf.String() != "abc" {
		t.Fatalf("underlying sink got %q, want %q", buf.String(), "abc")
	}
	if got, want := tee.Size(), int64(3); got != want {
		t.Fatalf("Size() = %d, want %d", got, want)
	}
	want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := tee.Sum(); got != want {
		t.Fatalf("Sum() = %q, want %q", got, want)
	}
}

// TestHashTee_EmptyInput asserts that a tee writer with no bytes written
// returns the empty-input SHA-256 digest and Size 0.
func TestHashTee_EmptyInput(t *testing.T) {
	var buf bytes.Buffer
	tee := newHashTeeWriter(&buf)
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := tee.Sum(); got != emptySHA256 {
		t.Fatalf("Sum() before Write = %q, want %q", got, emptySHA256)
	}
	if got := tee.Size(); got != 0 {
		t.Fatalf("Size() before Write = %d, want 0", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("underlying sink non-empty: %d bytes", buf.Len())
	}
}

// TestHashTee_StreamMatchesAllAtOnce streams 1 MiB of pseudo-random data
// in small 37-byte chunks through the tee and asserts the resulting digest
// matches the reference sha256.Sum256 over the concatenated bytes.
func TestHashTee_StreamMatchesAllAtOnce(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	const total = 1 << 20 // 1 MiB
	reference := make([]byte, total)
	if _, err := r.Read(reference); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	var buf bytes.Buffer
	tee := newHashTeeWriter(&buf)
	for off := 0; off < total; off += 37 {
		end := off + 37
		if end > total {
			end = total
		}
		n, err := tee.Write(reference[off:end])
		if err != nil {
			t.Fatalf("Write at offset %d: %v", off, err)
		}
		if n != end-off {
			t.Fatalf("short Write at offset %d: n=%d want=%d", off, n, end-off)
		}
	}

	if !bytes.Equal(buf.Bytes(), reference) {
		t.Fatalf("sink did not receive all bytes (got %d of %d)", buf.Len(), total)
	}
	if got, want := tee.Size(), int64(total); got != want {
		t.Fatalf("Size() = %d, want %d", got, want)
	}

	refDigest := sha256.Sum256(reference)
	want := hex.EncodeToString(refDigest[:])
	if got := tee.Sum(); got != want {
		t.Fatalf("Sum() = %q, want %q", got, want)
	}
}

// TestHashTee_ZeroByteWriteNoOp asserts Write(nil) and Write([]byte{}) are
// no-ops: Size stays 0 and Sum() still returns the empty-input digest.
func TestHashTee_ZeroByteWriteNoOp(t *testing.T) {
	var buf bytes.Buffer
	tee := newHashTeeWriter(&buf)

	if n, err := tee.Write(nil); err != nil || n != 0 {
		t.Fatalf("Write(nil) = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := tee.Write([]byte{}); err != nil || n != 0 {
		t.Fatalf("Write([]byte{}) = (%d, %v), want (0, nil)", n, err)
	}
	if got := tee.Size(); got != 0 {
		t.Fatalf("Size() = %d, want 0", got)
	}
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := tee.Sum(); got != emptySHA256 {
		t.Fatalf("Sum() = %q, want %q", got, emptySHA256)
	}
	if buf.Len() != 0 {
		t.Fatalf("underlying sink non-empty: %d bytes", buf.Len())
	}
}
