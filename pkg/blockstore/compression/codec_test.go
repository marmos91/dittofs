package compression

import (
	"bytes"
	"crypto/rand"
	"io"
	"strings"
	"testing"
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
