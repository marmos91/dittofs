package s3

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/marmos91/dittofs/pkg/backup/destination"
)

// hashTeeWriter is the S3-package copy of the destination package's
// hashTeeWriter. Duplicating here (instead of exporting the helper from
// package destination) matches the 02-PATTERNS precedent "duplicate over
// premature refactor" and keeps the destination package free of types that
// don't belong to its public surface.
type hashTeeWriter struct {
	dst io.Writer
	h   hash.Hash
	mw  io.Writer
	n   int64
}

func newHashTeeWriter(dst io.Writer) *hashTeeWriter {
	h := sha256.New()
	return &hashTeeWriter{dst: dst, h: h, mw: io.MultiWriter(dst, h)}
}

func (t *hashTeeWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n, err := t.mw.Write(p)
	t.n += int64(n)
	return n, err
}

func (t *hashTeeWriter) Sum() string { return hex.EncodeToString(t.h.Sum(nil)) }
func (t *hashTeeWriter) Size() int64 { return t.n }

// verifyReader hashes every byte read from r and compares the final
// digest against expected on Close. Mismatch surfaces as
// destination.ErrSHA256Mismatch — D-04 integrity check over ciphertext.
type verifyReader struct {
	r        io.Reader
	h        hash.Hash
	expected string
	done     bool
	sumErr   error
}

func newVerifyReader(r io.Reader, expected string) *verifyReader {
	return &verifyReader{r: r, h: sha256.New(), expected: expected}
}

func (v *verifyReader) Read(p []byte) (int, error) {
	if v.done {
		return 0, io.EOF
	}
	n, err := v.r.Read(p)
	if n > 0 {
		_, _ = v.h.Write(p[:n])
	}
	if errors.Is(err, io.EOF) {
		v.done = true
		v.finalise()
		if v.sumErr != nil {
			return n, v.sumErr
		}
	}
	return n, err
}

// finalise records the digest-mismatch error for later Close propagation.
// An empty expected digest is treated as a mismatch (fail-closed) so a
// malformed or pre-Phase-3 manifest never silently skips integrity
// verification. This matches the fs driver's Mismatch semantics.
func (v *verifyReader) finalise() {
	got := hex.EncodeToString(v.h.Sum(nil))
	if got != v.expected {
		v.sumErr = fmt.Errorf("%w: got %s, want %s", destination.ErrSHA256Mismatch, got, v.expected)
	}
}

// verifyReadCloser wraps the verify+decrypt chain and the underlying S3
// response body. Close drains and closes the body, then returns the
// verify-side error (mismatch) if Read never hit EOF.
type verifyReadCloser struct {
	r    io.Reader
	vr   *verifyReader
	body io.Closer
}

func (v *verifyReadCloser) Read(p []byte) (int, error) { return v.r.Read(p) }

// Close releases the S3 response body and propagates any SHA mismatch
// latched by the verifyReader. If Read never reached EOF (caller closed
// early), the verifyReader drains the body to completion so the digest
// can be computed; failure to drain is treated as a transport error, not
// a mismatch.
func (v *verifyReadCloser) Close() error {
	// Drain remaining bytes to let the verifyReader finalise. The caller
	// may have aborted mid-stream; we still owe it a final integrity
	// check on the bytes that WERE delivered (per D-11 "verifies SHA-256
	// as it streams and returns ErrSHA256Mismatch on close").
	_, drainErr := io.Copy(io.Discard, v.vr)
	closeErr := v.body.Close()
	// Priority: mismatch first (load-bearing invariant), then drain,
	// then close. Drain errors are usually a broken pipe / already-closed
	// body and can mask the real cause.
	if v.vr.sumErr != nil {
		return v.vr.sumErr
	}
	if drainErr != nil && !errors.Is(drainErr, io.EOF) {
		return drainErr
	}
	return closeErr
}
