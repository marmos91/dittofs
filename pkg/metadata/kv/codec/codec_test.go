package codec

import (
	"encoding/hex"
	"errors"
	"testing"
)

func TestGoldenAndRoundTrip(t *testing.T) {
	b := NewWriter(1).Uint64(0x0102).Bytes([]byte("ab")).Encode()
	if got, want := hex.EncodeToString(b), "01"+"0000000000000102"+"02"+"6162"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	r, v, err := NewReader(b, 1)
	if err != nil || v != 1 {
		t.Fatalf("version %d, %v", v, err)
	}
	if n, s := r.Uint64(), r.Bytes(); n != 0x0102 || string(s) != "ab" || r.Done() != nil {
		t.Fatalf("got %d %q %v", n, s, r.Done())
	}
}

func TestRefusal(t *testing.T) {
	b := NewWriter(1).Uint64(7).Encode()
	if _, _, err := NewReader(b, 0); !errors.Is(err, ErrVersion) {
		t.Errorf("unknown version: %v", err)
	}
	if _, _, err := NewReader(nil, 1); !errors.Is(err, ErrTruncated) {
		t.Errorf("empty: %v", err)
	}
	r, _, _ := NewReader(b[:5], 1)
	r.Uint64()
	if !errors.Is(r.Done(), ErrTruncated) {
		t.Errorf("truncated: %v", r.Done())
	}
	r, _, _ = NewReader(append(b, 0), 1)
	r.Uint64()
	if !errors.Is(r.Done(), ErrTrailing) {
		t.Errorf("trailing: %v", r.Done())
	}
}

func TestPrimitives(t *testing.T) {
	b := NewWriter(2).Uint32(9).Uint8(7).Bool(true).String("é").Fixed([]byte{1, 2}).Len(3).Encode()
	if got, want := hex.EncodeToString(b), "02"+"00000009"+"07"+"01"+"02c3a9"+"0102"+"03"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	r, _, _ := NewReader(b, 2)
	if r.Uint32() != 9 || r.Uint8() != 7 || !r.Bool() || r.String() != "é" || string(r.Fixed(2)) != "\x01\x02" {
		t.Fatal("round trip")
	}
	// The count claims 3 items with no bytes left for them.
	if r.Len() != 0 || !errors.Is(r.Done(), ErrTruncated) {
		t.Errorf("count past the end: %v", r.Done())
	}
	r, _, _ = NewReader([]byte{1, 2}, 1)
	if r.Bool(); !errors.Is(r.Done(), ErrTruncated) {
		t.Errorf("bool 2: %v", r.Done())
	}
}
