package destination

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
)

// hashTeeWriter wraps an underlying writer with a SHA-256 hasher. Every
// byte written passes through to dst and updates the digest in one pass.
//
// Per Phase 3 CONTEXT.md D-04, SHA-256 is computed over the CIPHERTEXT
// bytes written to storage — so the tee wraps the destination sink and
// sits OUTSIDE the encryptWriter in the pipeline:
//
//	plaintext → encryptWriter → hashTeeWriter → storage
//	                           ↑ Sum() → manifest.SHA256
//
// This matches the tee pattern in pkg/metadata/store/badger/backup.go:203
// (`io.MultiWriter(w, crc)`), swapping the CRC32 hasher for SHA-256 so
// operators can verify storage integrity without loading the key.
type hashTeeWriter struct {
	dst io.Writer
	h   hash.Hash
	mw  io.Writer
	n   int64
}

// newHashTeeWriter returns a tee writer forwarding writes to dst while
// maintaining a parallel SHA-256 digest.
func newHashTeeWriter(dst io.Writer) *hashTeeWriter {
	h := sha256.New()
	return &hashTeeWriter{
		dst: dst,
		h:   h,
		mw:  io.MultiWriter(dst, h),
	}
}

// HashTeeWriter is the exported handle to the SHA-256 tee — driver
// packages (e.g. destination/fs, destination/s3) wrap the destination
// sink with this and read back the hex digest plus byte count via Sum()
// and Size() when populating manifest.SHA256 and manifest.SizeBytes.
//
// The unexported hashTeeWriter keeps fields package-private; exposing it
// as *HashTeeWriter gives drivers a concrete pointer type they can hold
// without importing the internal struct.
type HashTeeWriter = hashTeeWriter

// NewHashTeeWriter is the exported constructor for drivers (D-04
// integration point). Returns a *HashTeeWriter whose Write method
// tees to dst while updating a SHA-256 hasher; call Sum() and Size()
// after the final Write to populate manifest.SHA256 and SizeBytes.
func NewHashTeeWriter(dst io.Writer) *HashTeeWriter {
	return newHashTeeWriter(dst)
}

// Write forwards p to the underlying sink and updates the SHA-256 digest.
// A zero-length write is a no-op (does not touch the hash or byte count).
func (t *hashTeeWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n, err := t.mw.Write(p)
	t.n += int64(n)
	return n, err
}

// Sum returns the hex-encoded SHA-256 digest over every byte written.
// Format matches manifest.Manifest.SHA256 (lowercase hex, 64 characters).
func (t *hashTeeWriter) Sum() string { return hex.EncodeToString(t.h.Sum(nil)) }

// Size returns the cumulative byte count successfully forwarded.
func (t *hashTeeWriter) Size() int64 { return t.n }
