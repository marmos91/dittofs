package compression

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

func codecRoundTrip(t *testing.T, c codec, plaintext []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	enc, err := c.EncodeStream(&compressed)
	if err != nil {
		t.Fatalf("EncodeStream: %v", err)
	}
	if _, err := enc.Write(plaintext); err != nil {
		t.Fatalf("encoder Write: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("encoder Close: %v", err)
	}
	dec, err := c.DecodeStream(bytes.NewReader(compressed.Bytes()))
	if err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}
	out, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("decoder ReadAll: %v", err)
	}
	if err := dec.Close(); err != nil {
		t.Fatalf("decoder Close: %v", err)
	}
	if !bytes.Equal(out, plaintext) {
		t.Fatalf("round-trip mismatch: got %d bytes, want %d bytes", len(out), len(plaintext))
	}
	return compressed.Bytes()
}

func TestCodec_RoundTrip(t *testing.T) {
	rng := bytes.Repeat([]byte("Lorem ipsum dolor sit amet, "), 1024) // 27 KiB-ish text
	zeros := make([]byte, 1<<20)
	randBuf := make([]byte, 1<<20)
	if _, err := rand.Read(randBuf); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"single_byte", []byte{42}},
		{"text_27k", rng},
		{"zeros_1mib", zeros},
		{"random_1mib", randBuf},
	}
	for _, algo := range []Algo{AlgoZstd, AlgoLZ4} {
		c, err := newCodec(algo)
		if err != nil {
			t.Fatalf("newCodec(%v): %v", algo, err)
		}
		for _, tc := range cases {
			t.Run(algo.String()+"/"+tc.name, func(t *testing.T) {
				codecRoundTrip(t, c, tc.body)
			})
		}
	}
}

func TestCodec_PooledReuse(t *testing.T) {
	// Drive the pool through many sequential encodes to confirm reset()
	// works and no encoder leaks bytes between successive callers.
	c, err := newCodec(AlgoZstd)
	if err != nil {
		t.Fatal(err)
	}
	bodies := [][]byte{
		[]byte("hello"),
		bytes.Repeat([]byte{0}, 4096),
		[]byte(strings.Repeat("abc", 1000)),
	}
	for i := range 20 {
		body := bodies[i%len(bodies)]
		codecRoundTrip(t, c, body)
	}
}

func TestNewCodec_UnknownAlgo(t *testing.T) {
	_, err := newCodec(Algo(0xff))
	if err == nil {
		t.Fatal("expected error for unknown algo")
	}
}

// failWriter is an io.Writer that accepts up to n bytes, then returns err
// on the Write call that exhausts the budget. It simulates a downstream
// flush failure during encoder Close.
type failWriter struct {
	n   int
	err error
}

func (f *failWriter) Write(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, f.err
	}
	take := len(p)
	if take > f.n {
		take = f.n
	}
	f.n -= take
	if f.n == 0 {
		return take, f.err
	}
	return take, nil
}

// TestZstdEncoderHandle_CloseError_DoesNotPoisonPool deterministically
// asserts that an encoder whose Close fails (downstream write error) is NOT
// returned to the shared pool. It swaps the package pool for an empty one,
// captures the single encoder handed out by EncodeStream, forces a Close
// failure, then Gets from the same pool and confirms the broken encoder
// pointer is not recycled.
//
// Before the fix the Close path called Put unconditionally, so the swapped
// pool would hand the very same (poisoned) *zstd.Encoder back on the next
// Get and the test fails. After the fix the broken encoder is dropped and a
// fresh one is constructed by New.
func TestZstdEncoderHandle_CloseError_DoesNotPoisonPool(t *testing.T) {
	// Swap in an empty pool whose New always builds a fresh encoder. Save
	// only the New func (copying the whole sync.Pool by value is illegal).
	savedNew := zstdEncoderPool.New
	t.Cleanup(func() { zstdEncoderPool = &sync.Pool{New: savedNew} })
	zstdEncoderPool = &sync.Pool{New: savedNew}

	c, err := newCodec(AlgoZstd)
	if err != nil {
		t.Fatal(err)
	}

	// Accept only a few bytes, then reject. A 1 MiB payload guarantees the
	// encoder must write past the budget during compression and/or the
	// Close-emitted frame trailer, so Close reports the downstream error.
	fw := &failWriter{n: 8, err: errors.New("simulated write failure")}
	wc, err := c.EncodeStream(fw)
	if err != nil {
		t.Fatal(err)
	}
	broken := wc.(*zstdEncoderHandle).enc
	if _, err := wc.Write(bytes.Repeat([]byte("x"), 1<<20)); err != nil {
		// A buffered write failure here is acceptable; Close drives the guard.
		_ = err
	}
	if closeErr := wc.Close(); closeErr == nil {
		t.Fatal("expected Close to surface the downstream write failure, got nil")
	}

	// The poisoned encoder must not be in the pool. Drain a few times to be
	// robust against sync.Pool's per-P local/shared placement.
	for i := 0; i < 8; i++ {
		got := zstdEncoderPool.Get().(*zstd.Encoder)
		if got == broken {
			t.Fatalf("poisoned zstd encoder was returned to the pool (iter %d)", i)
		}
	}

	// Sanity: the codec still round-trips correctly afterwards.
	codecRoundTrip(t, c, []byte("post-error round-trip payload"))
}

// TestLZ4EncoderHandle_CloseError_DoesNotPoisonPool is the lz4 counterpart.
func TestLZ4EncoderHandle_CloseError_DoesNotPoisonPool(t *testing.T) {
	savedNew := lz4WriterPool.New
	t.Cleanup(func() { lz4WriterPool = &sync.Pool{New: savedNew} })
	lz4WriterPool = &sync.Pool{New: savedNew}

	c, err := newCodec(AlgoLZ4)
	if err != nil {
		t.Fatal(err)
	}

	fw := &failWriter{n: 8, err: errors.New("simulated write failure")}
	wc, err := c.EncodeStream(fw)
	if err != nil {
		t.Fatal(err)
	}
	broken := wc.(*lz4EncoderHandle).w
	if _, err := wc.Write(bytes.Repeat([]byte("y"), 1<<20)); err != nil {
		_ = err
	}
	if closeErr := wc.Close(); closeErr == nil {
		t.Fatal("expected Close to surface the downstream write failure, got nil")
	}

	for i := 0; i < 8; i++ {
		got := lz4WriterPool.Get().(*lz4.Writer)
		if got == broken {
			t.Fatalf("poisoned lz4 writer was returned to the pool (iter %d)", i)
		}
	}

	codecRoundTrip(t, c, []byte("post-error round-trip payload lz4"))
}

func TestCodec_CompressibleShrinks(t *testing.T) {
	// Smoke check: both codecs must shrink a highly-redundant payload.
	body := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), 4096)
	for _, algo := range []Algo{AlgoZstd, AlgoLZ4} {
		c, err := newCodec(algo)
		if err != nil {
			t.Fatal(err)
		}
		compressed := codecRoundTrip(t, c, body)
		if len(compressed) >= len(body)/2 {
			t.Fatalf("%v: expected >2x compression on redundant text, got %d -> %d", algo, len(body), len(compressed))
		}
	}
}
