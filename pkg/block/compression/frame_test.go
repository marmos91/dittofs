package compression

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"
)

func TestEncodeDecodeFrame_RoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		algo     Algo
		origSize uint64
		body     []byte
	}{
		{"zstd_small", AlgoZstd, 16, []byte{0xde, 0xad, 0xbe, 0xef}},
		{"lz4_small", AlgoLZ4, 1024, bytes.Repeat([]byte{0x42}, 32)},
		{"zstd_empty_body", AlgoZstd, 0, nil},
		{"zstd_large_size", AlgoZstd, 1 << 30, []byte("x")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire := encodeFrame(tc.algo, tc.origSize, tc.body)
			if !hasFrameMagic(wire) {
				t.Fatalf("encoded frame missing magic prefix: % x", wire[:8])
			}
			algo, size, body, framed, err := tryDecodeFrame(wire)
			if err != nil {
				t.Fatalf("decode error: %v", err)
			}
			if !framed {
				t.Fatal("expected framed=true")
			}
			if algo != tc.algo {
				t.Fatalf("algo: got %v want %v", algo, tc.algo)
			}
			if size != tc.origSize {
				t.Fatalf("origSize: got %d want %d", size, tc.origSize)
			}
			if !bytes.Equal(body, tc.body) {
				t.Fatalf("body: got % x want % x", body, tc.body)
			}
		})
	}
}

func TestTryDecodeFrame_NotFramed(t *testing.T) {
	cases := [][]byte{
		nil,
		{},
		[]byte("hello"),
		[]byte("DFCM"),               // too short
		[]byte("DFCMQ\x01\x00"),      // wrong magic
		bytes.Repeat([]byte{0}, 256), // random-ish, no magic
	}
	for i, b := range cases {
		_, _, _, framed, err := tryDecodeFrame(b)
		if err != nil {
			t.Fatalf("case %d: unexpected err: %v", i, err)
		}
		if framed {
			t.Fatalf("case %d: framed=true for non-framed input", i)
		}
	}
}

func TestTryDecodeFrame_UnknownAlgo(t *testing.T) {
	wire := append([]byte{}, FrameMagic[:]...)
	wire = append(wire, 0x7f) // unknown algo byte
	wire = append(wire, 0x00) // uvarint=0
	_, _, _, framed, err := tryDecodeFrame(wire)
	if !framed {
		t.Fatal("framed should be true (magic present)")
	}
	if !errors.Is(err, ErrUnsupportedCompressionAlgo) {
		t.Fatalf("err: got %v want wraps ErrUnsupportedCompressionAlgo", err)
	}
}

func TestTryDecodeFrame_TruncatedUvarint(t *testing.T) {
	// magic + zstd algo + a multi-byte uvarint that is cut short.
	wire := append([]byte{}, FrameMagic[:]...)
	wire = append(wire, byte(AlgoZstd))
	// 0x80 with no continuation -> binary.Uvarint returns n<=0
	wire = append(wire, 0x80)
	_, _, _, framed, err := tryDecodeFrame(wire)
	if !framed {
		t.Fatal("framed should be true (magic present)")
	}
	if !errors.Is(err, ErrCompressedFrameCorrupt) {
		t.Fatalf("err: got %v want wraps ErrCompressedFrameCorrupt", err)
	}
}

func TestFrameHeaderSize_MatchesEncoding(t *testing.T) {
	// origSize=0 fits in a single uvarint byte: header is exactly fixed+1.
	wire := encodeFrame(AlgoZstd, 0, nil)
	want := FrameHeaderFixedSize + 1
	if len(wire) != want {
		t.Fatalf("frame size for empty body: got %d want %d", len(wire), want)
	}
	// Sanity: uvarint encoding of 0 is one byte 0x00.
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], 0)
	if n != 1 || buf[0] != 0x00 {
		t.Fatalf("unexpected uvarint(0): n=%d buf[0]=0x%02x", n, buf[0])
	}
}

func TestParsePolicy_Default(t *testing.T) {
	p, err := ParsePolicy(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	if p.Algo != AlgoZstd {
		t.Fatalf("default algo: got %v want zstd", p.Algo)
	}
}

func TestParsePolicy_Empty(t *testing.T) {
	p, err := ParsePolicy(nil)
	if err != nil {
		t.Fatalf("ParsePolicy(nil): %v", err)
	}
	if p.Algo != AlgoZstd {
		t.Fatalf("nil-raw default algo: got %v want zstd", p.Algo)
	}
}

func TestParsePolicy_Explicit(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Algo
	}{
		{`{"algo":"zstd"}`, AlgoZstd},
		{`{"algo":"lz4"}`, AlgoLZ4},
	} {
		p, err := ParsePolicy(json.RawMessage(tc.in))
		if err != nil {
			t.Fatalf("ParsePolicy(%q): %v", tc.in, err)
		}
		if p.Algo != tc.want {
			t.Fatalf("algo for %q: got %v want %v", tc.in, p.Algo, tc.want)
		}
	}
}

func TestParsePolicy_UnknownAlgo(t *testing.T) {
	_, err := ParsePolicy(json.RawMessage(`{"algo":"snappy"}`))
	if !errors.Is(err, ErrUnsupportedCompressionAlgo) {
		t.Fatalf("err: got %v want wraps ErrUnsupportedCompressionAlgo", err)
	}
}

func TestParsePolicy_BadJSON(t *testing.T) {
	_, err := ParsePolicy(json.RawMessage(`{`))
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}

func TestParsePolicy_RejectsNonObject(t *testing.T) {
	for _, in := range []string{`null`, `"zstd"`, `["zstd"]`, `42`, `true`} {
		if _, err := ParsePolicy(json.RawMessage(in)); err == nil {
			t.Errorf("ParsePolicy(%q): expected error, got nil", in)
		}
	}
}
