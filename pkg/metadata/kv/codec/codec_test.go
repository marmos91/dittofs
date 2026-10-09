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
