package encryption

import (
	"bytes"
	"errors"
	"testing"
)

func TestFrame_RoundTrip(t *testing.T) {
	masterID := "01234567-89ab-cdef-0123-456789abcdef"
	wrappedKey := bytes.Repeat([]byte{0xA1}, 60)
	nonce := bytes.Repeat([]byte{0xB2}, 12)
	ciphertext := bytes.Repeat([]byte{0xC3}, 256)

	wire, err := encodeFrame(AEADAES256GCM, masterID, wrappedKey, nonce, ciphertext)
	if err != nil {
		t.Fatalf("encodeFrame: %v", err)
	}
	view, framed, err := tryDecodeFrame(wire)
	if err != nil {
		t.Fatalf("tryDecodeFrame: %v", err)
	}
	if !framed {
		t.Fatal("framed=false on freshly encoded wire")
	}
	if view.aead != AEADAES256GCM {
		t.Errorf("aead: got %v want %v", view.aead, AEADAES256GCM)
	}
	if view.masterKeyID != masterID {
		t.Errorf("masterKeyID: got %q want %q", view.masterKeyID, masterID)
	}
	if !bytes.Equal(view.wrappedKey, wrappedKey) {
		t.Error("wrappedKey mismatch")
	}
	if !bytes.Equal(view.nonce, nonce) {
		t.Error("nonce mismatch")
	}
	if !bytes.Equal(view.ciphertext, ciphertext) {
		t.Error("ciphertext mismatch")
	}
}

func TestFrame_UnframedReturnsFalse(t *testing.T) {
	view, framed, err := tryDecodeFrame([]byte("plain bytes that do not begin with the DFENC magic"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if framed {
		t.Fatal("framed=true on unframed bytes")
	}
	if view.aead != 0 {
		t.Fatal("non-zero view returned for unframed input")
	}
}

func TestFrame_BadVersion(t *testing.T) {
	wire, err := encodeFrame(AEADAES256GCM, "id", bytes.Repeat([]byte{0x1}, 16), bytes.Repeat([]byte{0x2}, 12), bytes.Repeat([]byte{0x3}, 32))
	if err != nil {
		t.Fatalf("encodeFrame: %v", err)
	}
	wire[len(FrameMagic)] = 0xFE
	_, framed, err := tryDecodeFrame(wire)
	if !framed {
		t.Fatal("framed=false for bad version")
	}
	if !errors.Is(err, ErrEncryptedFrameCorrupt) {
		t.Fatalf("bad version: want ErrEncryptedFrameCorrupt, got %v", err)
	}
}

func TestFrame_BadAEAD(t *testing.T) {
	wire, err := encodeFrame(AEADAES256GCM, "id", bytes.Repeat([]byte{0x1}, 16), bytes.Repeat([]byte{0x2}, 12), bytes.Repeat([]byte{0x3}, 32))
	if err != nil {
		t.Fatalf("encodeFrame: %v", err)
	}
	wire[len(FrameMagic)+1] = 0x7F
	_, _, err = tryDecodeFrame(wire)
	if !errors.Is(err, ErrUnsupportedAEAD) {
		t.Fatalf("bad aead: want ErrUnsupportedAEAD, got %v", err)
	}
}

func TestFrame_Truncated(t *testing.T) {
	wire, err := encodeFrame(AEADAES256GCM, "id", bytes.Repeat([]byte{0x1}, 16), bytes.Repeat([]byte{0x2}, 12), bytes.Repeat([]byte{0x3}, 32))
	if err != nil {
		t.Fatalf("encodeFrame: %v", err)
	}
	// Truncate to just past the fixed header.
	truncated := wire[:frameHeaderFixedSize+1]
	_, framed, err := tryDecodeFrame(truncated)
	if !framed {
		t.Fatal("framed=false for truncated wire")
	}
	if !errors.Is(err, ErrEncryptedFrameCorrupt) {
		t.Fatalf("truncated: want ErrEncryptedFrameCorrupt, got %v", err)
	}
}

func TestFrame_OversizeWrappedKey(t *testing.T) {
	_, err := encodeFrame(AEADAES256GCM, "id", bytes.Repeat([]byte{0x1}, MaxWrappedBlockKeySize+1), bytes.Repeat([]byte{0x2}, 12), []byte{0x3})
	if err == nil {
		t.Fatal("expected oversize wrappedKey rejection")
	}
}

func TestFrame_NonceBoundaries(t *testing.T) {
	_, err := encodeFrame(AEADAES256GCM, "id", []byte{0x1}, nil, []byte{0x3})
	if err == nil {
		t.Fatal("empty nonce should be rejected")
	}
	_, err = encodeFrame(AEADAES256GCM, "id", []byte{0x1}, bytes.Repeat([]byte{0x2}, MaxNonceSize+1), []byte{0x3})
	if err == nil {
		t.Fatal("oversize nonce should be rejected")
	}
}
