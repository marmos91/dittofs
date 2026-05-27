package backup

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestEnvelope_RoundTrip(t *testing.T) {
	payload := []byte("hello backup payload")
	raw := writeValidEnvelope(t, "badger", payload)

	r := bytes.NewReader(raw)
	engineTag, pr, crc, err := ReadHeader(r)
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if engineTag != "badger" {
		t.Fatalf("engine tag: got %q, want %q", engineTag, "badger")
	}

	gotPayload := make([]byte, len(payload))
	if _, err := io.ReadFull(pr, gotPayload); err != nil {
		t.Fatalf("ReadFull payload: %v", err)
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Fatalf("payload mismatch: got %q, want %q", gotPayload, payload)
	}

	if err := VerifyCRC(r, crc); err != nil {
		t.Fatalf("VerifyCRC: %v", err)
	}
}

// writeValidEnvelope produces a complete, valid envelope with the given
// engine tag and payload into a buffer and returns the raw bytes.
func writeValidEnvelope(t *testing.T, engineTag string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	ew, err := NewWriter(&buf, engineTag)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err = ew.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err = ew.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return buf.Bytes()
}

func TestEnvelope_BadMagic(t *testing.T) {
	raw := writeValidEnvelope(t, "test", []byte("data"))
	raw[0] = 0xFF // corrupt magic

	_, _, _, err := ReadHeader(bytes.NewReader(raw))
	if !errors.Is(err, ErrBadMagic) {
		t.Fatalf("expected ErrBadMagic, got %v", err)
	}
}

func TestEnvelope_BadVersion(t *testing.T) {
	raw := writeValidEnvelope(t, "test", []byte("data"))
	// Version is at offset 4..8 (uint32 LE after 4-byte magic).
	raw[4] = 99 // corrupt version

	_, _, _, err := ReadHeader(bytes.NewReader(raw))
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("expected ErrUnsupportedVersion, got %v", err)
	}
}

func TestEnvelope_Truncated(t *testing.T) {
	raw := writeValidEnvelope(t, "test", []byte("data"))
	// Truncate mid-header (only 3 bytes of magic).
	raw = raw[:3]

	_, _, _, err := ReadHeader(bytes.NewReader(raw))
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("expected ErrTruncated, got %v", err)
	}
}

func TestEnvelope_BitFlip(t *testing.T) {
	payload := []byte("important backup data here")
	raw := writeValidEnvelope(t, "badger", payload)

	// Flip a bit in the payload region (after the header).
	// Header is: 4 (magic) + 4 (version) + 2 (engine_len) + 6 ("badger") = 16 bytes.
	payloadStart := 4 + 4 + 2 + len("badger")
	raw[payloadStart+2] ^= 0x01

	r := bytes.NewReader(raw)
	_, pr, crc, err := ReadHeader(r)
	if err != nil {
		t.Fatalf("ReadHeader on corrupted stream: %v", err)
	}

	// Drain payload through the tee reader.
	gotPayload := make([]byte, len(payload))
	if _, err := io.ReadFull(pr, gotPayload); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}

	// CRC should fail.
	err = VerifyCRC(r, crc)
	if !errors.Is(err, ErrCRCMismatch) {
		t.Fatalf("expected ErrCRCMismatch, got %v", err)
	}
}

func TestEnvelope_EngineMismatch(t *testing.T) {
	err := VerifyEngine("badger", "memory")
	if !errors.Is(err, ErrEngineMismatch) {
		t.Fatalf("expected ErrEngineMismatch, got %v", err)
	}

	// Same engine should pass.
	if err := VerifyEngine("badger", "badger"); err != nil {
		t.Fatalf("same engine should not error: %v", err)
	}
}
