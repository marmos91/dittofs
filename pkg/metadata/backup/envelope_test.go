package backup

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestEnvelope_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte("hello backup payload")

	// Write
	ew, err := NewWriter(&buf, "badger")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := ew.Write(payload); err != nil {
		t.Fatalf("Write payload: %v", err)
	}
	if err := ew.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	// Read
	raw := buf.Bytes()
	r := bytes.NewReader(raw)
	engineTag, payloadReader, crc, err := ReadHeader(r)
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if engineTag != "badger" {
		t.Fatalf("engine tag: got %q, want %q", engineTag, "badger")
	}

	got, err := io.ReadAll(payloadReader)
	if err != nil {
		t.Fatalf("ReadAll payload: %v", err)
	}
	// The payload reader includes the trailing CRC in its reads because
	// it tees from the original reader. We need to separate them: the
	// payload is len(payload) bytes, and the remaining 4 bytes are CRC.
	// Actually, ReadAll on the tee reader reads everything remaining
	// including the CRC bytes. We need to split.
	if len(got) < len(payload) {
		t.Fatalf("payload too short: got %d bytes, want >= %d", len(got), len(payload))
	}
	actualPayload := got[:len(payload)]
	if !bytes.Equal(actualPayload, payload) {
		t.Fatalf("payload mismatch: got %q, want %q", actualPayload, payload)
	}

	// The remaining bytes were fed into the CRC accumulator by the tee
	// reader, but we need to verify CRC from the original reader.
	// Since ReadAll consumed everything (including CRC bytes through the
	// tee), the CRC accumulator has seen the CRC bytes too. We need a
	// different approach: re-read from scratch with proper separation.

	// --- re-do with proper payload-length-aware read ---
	r2 := bytes.NewReader(raw)
	engineTag2, pr2, crc2, err := ReadHeader(r2)
	if err != nil {
		t.Fatalf("ReadHeader (2): %v", err)
	}
	if engineTag2 != "badger" {
		t.Fatalf("engine tag (2): got %q", engineTag2)
	}

	// Read exactly the payload bytes through the tee reader.
	gotPayload := make([]byte, len(payload))
	if _, err := io.ReadFull(pr2, gotPayload); err != nil {
		t.Fatalf("ReadFull payload (2): %v", err)
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Fatalf("payload mismatch (2): got %q, want %q", gotPayload, payload)
	}

	// Verify trailing CRC from the original reader (not the tee).
	if err := VerifyCRC(r2, crc2); err != nil {
		t.Fatalf("VerifyCRC: %v", err)
	}

	_ = crc // silence unused from first attempt
}

func TestEnvelope_BadMagic(t *testing.T) {
	var buf bytes.Buffer
	ew, err := NewWriter(&buf, "test")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	ew.Write([]byte("data"))
	ew.Finish()

	raw := buf.Bytes()
	raw[0] = 0xFF // corrupt magic

	_, _, _, err = ReadHeader(bytes.NewReader(raw))
	if !errors.Is(err, ErrBadMagic) {
		t.Fatalf("expected ErrBadMagic, got %v", err)
	}
}

func TestEnvelope_BadVersion(t *testing.T) {
	var buf bytes.Buffer
	ew, err := NewWriter(&buf, "test")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	ew.Write([]byte("data"))
	ew.Finish()

	raw := buf.Bytes()
	// Version is at offset 4..8 (uint32 LE after 4-byte magic).
	raw[4] = 99 // corrupt version

	_, _, _, err = ReadHeader(bytes.NewReader(raw))
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("expected ErrUnsupportedVersion, got %v", err)
	}
}

func TestEnvelope_Truncated(t *testing.T) {
	var buf bytes.Buffer
	ew, err := NewWriter(&buf, "test")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	ew.Write([]byte("data"))
	ew.Finish()

	// Truncate mid-header (only 3 bytes of magic).
	raw := buf.Bytes()[:3]

	_, _, _, err = ReadHeader(bytes.NewReader(raw))
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("expected ErrTruncated, got %v", err)
	}
}

func TestEnvelope_BitFlip(t *testing.T) {
	payload := []byte("important backup data here")
	var buf bytes.Buffer

	ew, err := NewWriter(&buf, "badger")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := ew.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := ew.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	raw := buf.Bytes()
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
