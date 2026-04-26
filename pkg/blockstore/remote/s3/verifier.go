package s3

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// verifyingReader wraps an io.ReadCloser and feeds every byte through a
// BLAKE3 hasher. On EOF, the accumulated hash is compared to expected;
// mismatch surfaces ErrCASContentMismatch. The wrapper is single-use and
// not goroutine-safe.
//
// Per D-18: zero extra body allocation — the verifier sees bytes once as
// they flow through the reader, and the hasher state is constant-cost
// regardless of body size. Per INV-06, on mismatch the caller MUST
// discard whatever bytes were read so corrupt bytes never reach upstream.
type verifyingReader struct {
	src      io.ReadCloser
	hasher   *blake3.Hasher
	expected blockstore.ContentHash
	// done indicates the reader has observed io.EOF and the hash check
	// has been performed. Used by Close to detect early-close (caller
	// abandoned mid-stream); we treat that as a verification failure
	// because we cannot prove the bytes were correct.
	done bool
	// hashOK is true once checkHash has passed. Set on EOF; consulted
	// by Close to decide whether the underlying close error should be
	// returned, or whether the close is itself a failure path.
	hashOK bool
	// closed guards against double-close. The src.Close() is always
	// called exactly once.
	closed bool
}

// newVerifyingReader constructs a verifier over src. expected is the
// hash the body MUST match. The caller is responsible for ensuring
// the underlying src is closed via verifyingReader.Close.
func newVerifyingReader(src io.ReadCloser, expected blockstore.ContentHash) *verifyingReader {
	return &verifyingReader{
		src:      src,
		hasher:   blake3.New(blockstore.HashSize, nil),
		expected: expected,
	}
}

// Read implements io.Reader. Bytes flowing through the reader are fed
// to the BLAKE3 hasher. On io.EOF the hash is checked; if it does not
// match, ErrCASContentMismatch is returned in place of io.EOF and the
// caller's buffer holds whatever bytes were last read (which the
// caller MUST discard).
func (v *verifyingReader) Read(p []byte) (int, error) {
	n, err := v.src.Read(p)
	if n > 0 {
		// blake3.Hasher.Write does not error per the package docs; ignore.
		_, _ = v.hasher.Write(p[:n])
	}
	if errors.Is(err, io.EOF) {
		v.done = true
		if mismatch := v.checkHash(); mismatch != nil {
			return n, mismatch
		}
		v.hashOK = true
	}
	return n, err
}

// Close closes the underlying ReadCloser. If the caller closed before
// reaching EOF (so the hash was never verified), Close returns
// ErrCASContentMismatch — we MUST treat unverified bytes as untrusted.
// If the underlying close itself errors, that error is returned only
// when the hash check passed (or no read happened at all).
//
// On early-close (!v.done) we drain a bounded amount from src before
// closing so Go's HTTP/1.1 connection pool can reuse the TCP
// connection (mid-stream Close otherwise invalidates it). The drain
// is capped at maxBodyDrainBytes to bound the worst case.
func (v *verifyingReader) Close() error {
	if v.closed {
		return nil
	}
	v.closed = true
	if !v.done {
		// Best-effort drain — errors are irrelevant since we're about to
		// close the body anyway and treat the read as failed.
		_, _ = io.CopyN(io.Discard, v.src, maxBodyDrainBytes)
	}
	closeErr := v.src.Close()
	if !v.done {
		// Caller didn't read to EOF — verification was not completed.
		// Surface as mismatch (untrusted) rather than silently OK.
		return fmt.Errorf("%w: stream closed before EOF (verification incomplete)",
			blockstore.ErrCASContentMismatch)
	}
	return closeErr
}

// maxBodyDrainBytes caps the per-Close drain when the caller abandons
// the response body before EOF. The constant matches the standard
// pattern used in the Go stdlib (net/http) for keep-alive reuse.
const maxBodyDrainBytes = 1 << 14 // 16 KiB

// checkHash compares the accumulated BLAKE3 sum to expected. Returns
// a wrapped ErrCASContentMismatch on mismatch.
func (v *verifyingReader) checkHash() error {
	var got blockstore.ContentHash
	sum := v.hasher.Sum(nil)
	copy(got[:], sum)
	if got != v.expected {
		return fmt.Errorf("%w: got %s, want %s",
			blockstore.ErrCASContentMismatch, got.CASKey(), v.expected.CASKey())
	}
	return nil
}

// readAllVerified reads the verifyingReader to completion into a single
// buffer. When contentLength is known and positive, the buffer is
// pre-sized exactly; otherwise fallback is used (capped at maxBlockReadSize).
//
// Per INV-06: on ErrCASContentMismatch (surfaced by the verifier on EOF
// or by a header pre-check upstream), the partially-filled buffer is
// discarded by returning a nil slice. Bad bytes never escape.
func readAllVerified(r *verifyingReader, contentLength *int64, fallbackSize int64) ([]byte, error) {
	if contentLength != nil && *contentLength > 0 {
		data := make([]byte, *contentLength)
		_, err := io.ReadFull(r, data)
		if err != nil {
			if errors.Is(err, blockstore.ErrCASContentMismatch) {
				return nil, err
			}
			return nil, fmt.Errorf("read s3 object body: %w", err)
		}
		// io.ReadFull does not surface io.EOF when the read exactly
		// fills the buffer; force a final zero-byte read so the verifier
		// observes EOF and runs the hash check.
		var oneByte [1]byte
		_, peekErr := r.Read(oneByte[:])
		if peekErr == nil {
			// More bytes than ContentLength promised — corrupt response.
			return nil, fmt.Errorf("s3 object body exceeded content-length")
		}
		if errors.Is(peekErr, blockstore.ErrCASContentMismatch) {
			return nil, peekErr
		}
		// Any non-EOF, non-mismatch error: surface as I/O failure.
		if !errors.Is(peekErr, io.EOF) {
			return nil, fmt.Errorf("read s3 object body trailer: %w", peekErr)
		}
		return data, nil
	}

	buf := bytes.NewBuffer(make([]byte, 0, fallbackSize))
	_, err := buf.ReadFrom(r)
	if err != nil {
		if errors.Is(err, blockstore.ErrCASContentMismatch) {
			return nil, err
		}
		return nil, fmt.Errorf("read s3 object body: %w", err)
	}
	return buf.Bytes(), nil
}
