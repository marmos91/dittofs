package s3

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// hashOf returns the BLAKE3-256 hash of data as a ContentHash.
func hashOf(t *testing.T, data []byte) blockstore.ContentHash {
	t.Helper()
	sum := blake3.Sum256(data)
	var h blockstore.ContentHash
	copy(h[:], sum[:])
	return h
}

// flipped returns the input with its first byte XOR-flipped.
func flipped(data []byte) []byte {
	out := make([]byte, len(data))
	copy(out, data)
	if len(out) > 0 {
		out[0] ^= 0xFF
	}
	return out
}

// trackingReader wraps a bytes.Reader and counts how many times Read was
// called. Used to assert no body reads happen on header-pre-check failure.
type trackingReader struct {
	r          *bytes.Reader
	readCalls  atomic.Int64
	closeCalls atomic.Int64
}

func newTrackingReader(data []byte) *trackingReader {
	return &trackingReader{r: bytes.NewReader(data)}
}

func (t *trackingReader) Read(p []byte) (int, error) {
	t.readCalls.Add(1)
	return t.r.Read(p)
}

func (t *trackingReader) Close() error {
	t.closeCalls.Add(1)
	return nil
}

// shortReader returns one byte at a time so we exercise the verifier's
// partial-read accumulation path.
type shortReader struct {
	data   []byte
	pos    int
	closed bool
}

func (s *shortReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = s.data[s.pos]
	s.pos++
	return 1, nil
}

func (s *shortReader) Close() error {
	s.closed = true
	return nil
}

// TestVerifyingReader_HappyPath verifies that a stream whose body matches
// expected hash flows through cleanly and returns the bytes intact.
func TestVerifyingReader_HappyPath(t *testing.T) {
	data := []byte("hello, dittofs CAS world — payload bytes for verification")
	expected := hashOf(t, data)

	src := io.NopCloser(bytes.NewReader(data))
	v := newVerifyingReader(src, expected)

	out, err := io.ReadAll(v)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(out, data) {
		t.Fatalf("ReadAll bytes mismatch: got %q, want %q", out, data)
	}
	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestVerifyingReader_BodyTampered verifies that flipping a single byte
// surfaces ErrCASContentMismatch on EOF observation. Note: the buffer
// callers receive on this path is corrupt — callers MUST discard it.
func TestVerifyingReader_BodyTampered(t *testing.T) {
	clean := []byte("authoritative payload — must hash to expected")
	expected := hashOf(t, clean)
	tampered := flipped(clean)

	src := io.NopCloser(bytes.NewReader(tampered))
	v := newVerifyingReader(src, expected)

	_, err := io.ReadAll(v)
	if err == nil {
		t.Fatal("ReadAll: expected ErrCASContentMismatch, got nil")
	}
	if !errors.Is(err, blockstore.ErrCASContentMismatch) {
		t.Fatalf("ReadAll err = %v, want wrapped ErrCASContentMismatch", err)
	}
}

// TestVerifyingReader_EarlyClose verifies that closing before reaching
// EOF is treated as a verification failure (we cannot prove the bytes
// were correct, so we MUST return mismatch).
func TestVerifyingReader_EarlyClose(t *testing.T) {
	data := bytes.Repeat([]byte{0xAB}, 1024)
	expected := hashOf(t, data)

	src := io.NopCloser(bytes.NewReader(data))
	v := newVerifyingReader(src, expected)

	// Read just a few bytes — do NOT drain to EOF.
	buf := make([]byte, 16)
	if _, err := v.Read(buf); err != nil {
		t.Fatalf("Read: %v", err)
	}

	closeErr := v.Close()
	if closeErr == nil {
		t.Fatal("Close: expected ErrCASContentMismatch on early close, got nil")
	}
	if !errors.Is(closeErr, blockstore.ErrCASContentMismatch) {
		t.Fatalf("Close err = %v, want wrapped ErrCASContentMismatch", closeErr)
	}
	if !strings.Contains(closeErr.Error(), "before EOF") {
		t.Errorf("Close err message should mention early EOF, got %q", closeErr.Error())
	}
}

// TestVerifyingReader_PartialReads exercises the byte-at-a-time path so
// that the hasher correctly accumulates state across many small Read calls.
func TestVerifyingReader_PartialReads(t *testing.T) {
	data := []byte("partial reads exercise the hasher state machine")
	expected := hashOf(t, data)

	v := newVerifyingReader(&shortReader{data: data}, expected)

	out, err := io.ReadAll(v)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(out, data) {
		t.Fatalf("bytes: got %q, want %q", out, data)
	}
	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestVerifyingReader_DoubleClose ensures Close is idempotent.
func TestVerifyingReader_DoubleClose(t *testing.T) {
	data := []byte("xyz")
	expected := hashOf(t, data)

	v := newVerifyingReader(io.NopCloser(bytes.NewReader(data)), expected)
	_, _ = io.ReadAll(v)

	if err := v.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := v.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestReadAllVerified_ContentLengthExact verifies the pre-sized path and
// that the verifier observes EOF when the body exactly matches
// ContentLength.
func TestReadAllVerified_ContentLengthExact(t *testing.T) {
	data := bytes.Repeat([]byte("ABCDEFGH"), 4096) // 32 KiB
	expected := hashOf(t, data)
	cl := int64(len(data))

	v := newVerifyingReader(io.NopCloser(bytes.NewReader(data)), expected)
	out, err := readAllVerified(v, &cl, maxBlockReadSize)
	if err != nil {
		t.Fatalf("readAllVerified: %v", err)
	}
	if !bytes.Equal(out, data) {
		t.Fatal("bytes mismatch")
	}
	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestReadAllVerified_ContentLengthMismatch covers the body-tampered path
// through readAllVerified — the buffer is discarded on mismatch.
func TestReadAllVerified_ContentLengthMismatch(t *testing.T) {
	clean := bytes.Repeat([]byte("WXYZ"), 1024) // 4 KiB
	expected := hashOf(t, clean)
	tampered := flipped(clean)
	cl := int64(len(tampered))

	v := newVerifyingReader(io.NopCloser(bytes.NewReader(tampered)), expected)
	out, err := readAllVerified(v, &cl, maxBlockReadSize)
	if err == nil {
		t.Fatal("expected ErrCASContentMismatch, got nil")
	}
	if !errors.Is(err, blockstore.ErrCASContentMismatch) {
		t.Fatalf("err = %v, want wrapped ErrCASContentMismatch", err)
	}
	if out != nil {
		t.Fatalf("expected nil buffer on mismatch, got %d bytes", len(out))
	}
}

// TestReadAllVerified_NoContentLength covers the fallback ReadFrom path.
func TestReadAllVerified_NoContentLength(t *testing.T) {
	data := []byte("body without content-length header")
	expected := hashOf(t, data)

	v := newVerifyingReader(io.NopCloser(bytes.NewReader(data)), expected)
	out, err := readAllVerified(v, nil, maxBlockReadSize)
	if err != nil {
		t.Fatalf("readAllVerified: %v", err)
	}
	if !bytes.Equal(out, data) {
		t.Fatal("bytes mismatch")
	}
	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
