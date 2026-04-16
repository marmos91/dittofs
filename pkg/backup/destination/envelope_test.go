package destination

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// randKey returns a fresh 32-byte AES-256 key.
func randKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, aes256KeyLen)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return k
}

// randBytes returns n cryptographically-random bytes.
func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if n > 0 {
		if _, err := rand.Read(b); err != nil {
			t.Fatalf("rand.Read: %v", err)
		}
	}
	return b
}

// encryptAll runs plaintext through NewEncryptWriter with the given frame
// size and returns the ciphertext envelope.
func encryptAll(t *testing.T, key, plaintext []byte, frameSize int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewEncryptWriter(&buf, key, frameSize)
	if err != nil {
		t.Fatalf("NewEncryptWriter: %v", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

// decryptAll reads through NewDecryptReader until EOF.
func decryptAll(t *testing.T, key, ciphertext []byte) ([]byte, error) {
	t.Helper()
	r, err := NewDecryptReader(bytes.NewReader(ciphertext), key)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestEnvelope_RoundTrip_Sizes(t *testing.T) {
	key := randKey(t)
	const frameSize = 1024
	sizes := []int{0, 1, 1024, frameSize - 1, frameSize, frameSize + 1, 3*frameSize + 5}

	for _, n := range sizes {
		n := n
		t.Run("", func(t *testing.T) {
			plaintext := randBytes(t, n)
			ct := encryptAll(t, key, plaintext, frameSize)
			got, err := decryptAll(t, key, ct)
			if err != nil {
				t.Fatalf("decrypt size=%d: %v", n, err)
			}
			if !bytes.Equal(got, plaintext) {
				t.Fatalf("size=%d: roundtrip mismatch (len got=%d want=%d)", n, len(got), n)
			}
		})
	}
}

func TestEnvelope_DefaultFrameSize(t *testing.T) {
	// Pass frameSize=0 to opt into the 4 MiB default. Write 5 MiB so at
	// least one full default frame + a tail is exercised.
	key := randKey(t)
	plaintext := randBytes(t, 5*1024*1024)
	ct := encryptAll(t, key, plaintext, 0)
	got, err := decryptAll(t, key, ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("roundtrip mismatch: len got=%d want=%d", len(got), len(plaintext))
	}
	// Header encodes the frame size explicitly; confirm the default was used.
	frameSize := binary.BigEndian.Uint32(ct[5:9])
	if frameSize != defaultFrameSize {
		t.Fatalf("header frame_size = %d, want default %d", frameSize, defaultFrameSize)
	}
}

func TestEnvelope_CloseRequired(t *testing.T) {
	// Write bytes but skip Close — no final-tagged frame is emitted, so the
	// decoder must reject the stream.
	key := randKey(t)
	var buf bytes.Buffer
	w, err := NewEncryptWriter(&buf, key, 1024)
	if err != nil {
		t.Fatalf("NewEncryptWriter: %v", err)
	}
	if _, err := w.Write(randBytes(t, 100)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Intentionally skip w.Close()
	_, err = decryptAll(t, key, buf.Bytes())
	if !errors.Is(err, ErrDecryptFailed) {
		t.Fatalf("err = %v, want errors.Is ErrDecryptFailed", err)
	}
}

func TestEnvelope_WrongKey(t *testing.T) {
	keyA := randKey(t)
	keyB := randKey(t)
	ct := encryptAll(t, keyA, randBytes(t, 4096), 1024)
	_, err := decryptAll(t, keyB, ct)
	if !errors.Is(err, ErrDecryptFailed) {
		t.Fatalf("err = %v, want errors.Is ErrDecryptFailed", err)
	}
}

func TestEnvelope_Truncation_MidFrame(t *testing.T) {
	key := randKey(t)
	const frameSize = 1024
	ct := encryptAll(t, key, randBytes(t, 3*frameSize), frameSize)
	// Cut the ciphertext in the middle of the first frame body. Header is 9
	// bytes; nonce is 12; ct_len is 4; frame body starts at offset 25.
	truncated := ct[:25+50]
	_, err := decryptAll(t, key, truncated)
	if !errors.Is(err, ErrDecryptFailed) {
		t.Fatalf("err = %v, want errors.Is ErrDecryptFailed", err)
	}
}

func TestEnvelope_Truncation_MissingFinal(t *testing.T) {
	// Encrypt several frames, then strip the last (final-tagged) frame off
	// the ciphertext entirely. The reader should reach EOF without having
	// seen a `final` AAD and reject.
	key := randKey(t)
	const frameSize = 16
	// Produce at least 3 data frames + 1 final by writing 3*frameSize bytes
	// (exact multiple → final frame is zero-length but still present).
	ct := encryptAll(t, key, randBytes(t, 3*frameSize), frameSize)

	// Each frame on the wire is nonce(12) + ct_len(4) + ciphertext+tag.
	// Walk the frames, keep only the first N-1 frames, drop the final.
	framesStart := envelopeHeaderLen
	type frame struct{ off, end int }
	var frames []frame
	off := framesStart
	for off < len(ct) {
		if off+gcmNonceSize+4 > len(ct) {
			t.Fatalf("unexpected short stream at offset %d", off)
		}
		ctLen := binary.BigEndian.Uint32(ct[off+gcmNonceSize : off+gcmNonceSize+4])
		bodyLen := gcmNonceSize + 4 + int(ctLen)
		frames = append(frames, frame{off: off, end: off + bodyLen})
		off += bodyLen
	}
	if len(frames) < 2 {
		t.Fatalf("expected at least 2 frames (data + final), got %d", len(frames))
	}
	// Truncate to drop the final frame — retain header + all data frames.
	truncated := ct[:frames[len(frames)-1].off]
	_, err := decryptAll(t, key, truncated)
	if !errors.Is(err, ErrDecryptFailed) {
		t.Fatalf("err = %v, want errors.Is ErrDecryptFailed", err)
	}
}

func TestEnvelope_BadMagic(t *testing.T) {
	key := randKey(t)
	// Header with bad magic 0xDEADBEEF.
	hdr := make([]byte, envelopeHeaderLen)
	binary.BigEndian.PutUint32(hdr[0:4], 0xDEADBEEF)
	hdr[4] = envelopeVersion
	binary.BigEndian.PutUint32(hdr[5:9], 1024)

	_, err := NewDecryptReader(bytes.NewReader(hdr), key)
	if !errors.Is(err, ErrDecryptFailed) {
		t.Fatalf("err = %v, want errors.Is ErrDecryptFailed", err)
	}
}

func TestEnvelope_BadVersion(t *testing.T) {
	key := randKey(t)
	hdr := make([]byte, envelopeHeaderLen)
	binary.BigEndian.PutUint32(hdr[0:4], envelopeMagic)
	hdr[4] = 2 // unsupported version
	binary.BigEndian.PutUint32(hdr[5:9], 1024)

	_, err := NewDecryptReader(bytes.NewReader(hdr), key)
	if !errors.Is(err, ErrDecryptFailed) {
		t.Fatalf("err = %v, want errors.Is ErrDecryptFailed", err)
	}
}

func TestEnvelope_BadKeyLength(t *testing.T) {
	var buf bytes.Buffer
	_, err := NewEncryptWriter(&buf, make([]byte, 16), 0)
	if !errors.Is(err, ErrInvalidKeyMaterial) {
		t.Fatalf("NewEncryptWriter err = %v, want errors.Is ErrInvalidKeyMaterial", err)
	}
	_, err = NewDecryptReader(bytes.NewReader(make([]byte, envelopeHeaderLen)), make([]byte, 16))
	if !errors.Is(err, ErrInvalidKeyMaterial) {
		t.Fatalf("NewDecryptReader err = %v, want errors.Is ErrInvalidKeyMaterial", err)
	}
}

func TestEnvelope_FrameReorder(t *testing.T) {
	// Small frame size so 3 frames fit in a modest payload. Swap two
	// data frames; counter-in-AAD must detect the reorder.
	key := randKey(t)
	const frameSize = 16
	// 3 data frames (48 bytes → exact multiple emits a zero-length final)
	plaintext := randBytes(t, 3*frameSize)
	ct := encryptAll(t, key, plaintext, frameSize)

	// Parse frame boundaries. Each frame: nonce(12) + ct_len(4) + body.
	off := envelopeHeaderLen
	type boundary struct{ off, end int }
	var frames []boundary
	for off < len(ct) {
		ctLen := binary.BigEndian.Uint32(ct[off+gcmNonceSize : off+gcmNonceSize+4])
		bodyLen := gcmNonceSize + 4 + int(ctLen)
		frames = append(frames, boundary{off: off, end: off + bodyLen})
		off += bodyLen
	}
	if len(frames) < 3 {
		t.Fatalf("expected at least 3 frames, got %d", len(frames))
	}
	// Swap frame 0 and frame 1 bytes. Clone ct into a mutable buffer.
	tampered := append([]byte(nil), ct...)
	f0 := append([]byte(nil), tampered[frames[0].off:frames[0].end]...)
	f1 := append([]byte(nil), tampered[frames[1].off:frames[1].end]...)
	// Both frames are the same length (data frames with full frameSize
	// payload → same ct_len). Assert so the swap is byte-aligned.
	if len(f0) != len(f1) {
		t.Fatalf("frame 0 and 1 have different lengths: %d vs %d", len(f0), len(f1))
	}
	copy(tampered[frames[0].off:], f1)
	copy(tampered[frames[1].off:], f0)

	_, err := decryptAll(t, key, tampered)
	if !errors.Is(err, ErrDecryptFailed) {
		t.Fatalf("err = %v, want errors.Is ErrDecryptFailed (counter-in-AAD must catch reorder)", err)
	}
}

func TestEnvelope_Deterministic_OnlyNonce(t *testing.T) {
	key := randKey(t)
	plaintext := randBytes(t, 2048)
	ct1 := encryptAll(t, key, plaintext, 1024)
	ct2 := encryptAll(t, key, plaintext, 1024)

	if bytes.Equal(ct1, ct2) {
		t.Fatal("ciphertexts are identical — nonces must be random per frame")
	}

	p1, err := decryptAll(t, key, ct1)
	if err != nil {
		t.Fatalf("decrypt ct1: %v", err)
	}
	p2, err := decryptAll(t, key, ct2)
	if err != nil {
		t.Fatalf("decrypt ct2: %v", err)
	}
	if !bytes.Equal(p1, plaintext) || !bytes.Equal(p2, plaintext) {
		t.Fatalf("roundtrip mismatch")
	}
}
