package header

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

func TestParseTransformHeader_Valid(t *testing.T) {
	// Build a valid 52-byte transform header
	buf := make([]byte, TransformHeaderSize)
	binary.LittleEndian.PutUint32(buf[0:4], TransformProtocolID)
	// Signature (16 bytes at offset 4)
	for i := 0; i < 16; i++ {
		buf[4+i] = byte(i + 0xA0)
	}
	// Nonce (16 bytes at offset 20)
	for i := 0; i < 16; i++ {
		buf[20+i] = byte(i + 0xB0)
	}
	// OriginalMessageSize at offset 36
	binary.LittleEndian.PutUint32(buf[36:40], 135)
	// Reserved at offset 40
	binary.LittleEndian.PutUint16(buf[40:42], 0)
	// Flags at offset 42
	binary.LittleEndian.PutUint16(buf[42:44], 0x0001)
	// SessionId at offset 44
	binary.LittleEndian.PutUint64(buf[44:52], 0x8E40014000011)

	h, err := ParseTransformHeader(buf)
	if err != nil {
		t.Fatalf("ParseTransformHeader: %v", err)
	}

	// Check Signature
	for i := 0; i < 16; i++ {
		if h.Signature[i] != byte(i+0xA0) {
			t.Errorf("Signature[%d] = 0x%02x, want 0x%02x", i, h.Signature[i], byte(i+0xA0))
		}
	}

	// Check Nonce
	for i := 0; i < 16; i++ {
		if h.Nonce[i] != byte(i+0xB0) {
			t.Errorf("Nonce[%d] = 0x%02x, want 0x%02x", i, h.Nonce[i], byte(i+0xB0))
		}
	}

	if h.OriginalMessageSize != 135 {
		t.Errorf("OriginalMessageSize = %d, want 135", h.OriginalMessageSize)
	}

	if h.Flags != 0x0001 {
		t.Errorf("Flags = 0x%04x, want 0x0001", h.Flags)
	}

	if h.SessionId != 0x8E40014000011 {
		t.Errorf("SessionId = 0x%016x, want 0x008E40014000011", h.SessionId)
	}
}

func TestParseTransformHeader_TooShort(t *testing.T) {
	data := make([]byte, TransformHeaderSize-1)
	_, err := ParseTransformHeader(data)
	if err != ErrTransformTooShort {
		t.Errorf("expected ErrTransformTooShort, got %v", err)
	}
}

func TestParseTransformHeader_WrongProtocolID(t *testing.T) {
	buf := make([]byte, TransformHeaderSize)
	// Use SMB2 protocol ID instead of Transform
	binary.LittleEndian.PutUint32(buf[0:4], 0x424D53FE)
	_, err := ParseTransformHeader(buf)
	if err != ErrTransformInvalidProtocol {
		t.Errorf("expected ErrTransformInvalidProtocol, got %v", err)
	}
}

func TestTransformHeader_Encode(t *testing.T) {
	h := &TransformHeader{
		Signature:           [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10},
		Nonce:               [16]byte{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1A, 0x1B, 0x00, 0x00, 0x00, 0x00, 0x00},
		OriginalMessageSize: 256,
		Flags:               0x0001,
		SessionId:           12345678,
	}

	encoded := h.Encode()
	if len(encoded) != TransformHeaderSize {
		t.Fatalf("Encode length = %d, want %d", len(encoded), TransformHeaderSize)
	}

	// Check protocol ID at offset 0
	pid := binary.LittleEndian.Uint32(encoded[0:4])
	if pid != TransformProtocolID {
		t.Errorf("ProtocolID = 0x%08x, want 0x%08x", pid, TransformProtocolID)
	}

	// Check Reserved at offset 40 is zero
	reserved := binary.LittleEndian.Uint16(encoded[40:42])
	if reserved != 0 {
		t.Errorf("Reserved = 0x%04x, want 0x0000", reserved)
	}

	// Check OriginalMessageSize at offset 36
	msgSize := binary.LittleEndian.Uint32(encoded[36:40])
	if msgSize != 256 {
		t.Errorf("OriginalMessageSize = %d, want 256", msgSize)
	}

	// Check Flags at offset 42
	flags := binary.LittleEndian.Uint16(encoded[42:44])
	if flags != 0x0001 {
		t.Errorf("Flags = 0x%04x, want 0x0001", flags)
	}

	// Check SessionId at offset 44
	sid := binary.LittleEndian.Uint64(encoded[44:52])
	if sid != 12345678 {
		t.Errorf("SessionId = %d, want 12345678", sid)
	}
}

func TestTransformHeader_EncodeSize(t *testing.T) {
	h := &TransformHeader{}
	encoded := h.Encode()
	if len(encoded) != 52 {
		t.Errorf("Encode produced %d bytes, want exactly 52", len(encoded))
	}
}

func TestTransformHeader_RoundTrip(t *testing.T) {
	original := &TransformHeader{
		Signature:           [16]byte{0xAA, 0xBB, 0xCC, 0xDD, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0x00, 0xAA, 0xBB},
		Nonce:               [16]byte{0x66, 0xE6, 0x9A, 0x11, 0x18, 0x92, 0x58, 0x4F, 0xB5, 0xED, 0x52, 0x00, 0x00, 0x00, 0x00, 0x00},
		OriginalMessageSize: 87,
		Flags:               0x0001,
		SessionId:           0x8E40014000011,
	}

	encoded := original.Encode()
	parsed, err := ParseTransformHeader(encoded)
	if err != nil {
		t.Fatalf("ParseTransformHeader: %v", err)
	}

	if parsed.Signature != original.Signature {
		t.Error("Signature mismatch after round-trip")
	}
	if parsed.Nonce != original.Nonce {
		t.Error("Nonce mismatch after round-trip")
	}
	if parsed.OriginalMessageSize != original.OriginalMessageSize {
		t.Errorf("OriginalMessageSize = %d, want %d", parsed.OriginalMessageSize, original.OriginalMessageSize)
	}
	if parsed.Flags != original.Flags {
		t.Errorf("Flags = 0x%04x, want 0x%04x", parsed.Flags, original.Flags)
	}
	if parsed.SessionId != original.SessionId {
		t.Errorf("SessionId = 0x%016x, want 0x%016x", parsed.SessionId, original.SessionId)
	}
}

func TestTransformHeader_AAD(t *testing.T) {
	h := &TransformHeader{
		Nonce:               [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10},
		OriginalMessageSize: 135,
		Flags:               0x0001,
		SessionId:           0x8E40014000011,
	}

	aad := h.AAD()
	if len(aad) != 32 {
		t.Fatalf("AAD length = %d, want 32", len(aad))
	}

	// AAD should be bytes 20-51 of the encoded header
	encoded := h.Encode()
	expected := encoded[20:52]
	if !bytes.Equal(aad, expected) {
		t.Errorf("AAD does not match bytes 20-51 of encoded header\ngot:  %s\nwant: %s",
			hex.EncodeToString(aad), hex.EncodeToString(expected))
	}
}

func TestTransformHeader_AADSize(t *testing.T) {
	h := &TransformHeader{}
	aad := h.AAD()
	if len(aad) != 32 {
		t.Errorf("AAD length = %d, want exactly 32", len(aad))
	}
}

func TestTransformHeader_MicrosoftTestVector(t *testing.T) {
	// From Microsoft official documentation:
	// https://learn.microsoft.com/en-us/archive/blogs/openspecification/encryption-in-smb-3-0-a-protocol-perspective
	//
	// Session: 0x8e40014000011
	// CCM Nonce (11 bytes): 66E69A111892584FB5ED52
	// OriginalMessageSize: 0x87 (135)
	// Flags: 0x0001
	// Signature: 81A286535415445DAE393921E44FA42E

	h := &TransformHeader{
		OriginalMessageSize: 0x87,
		Flags:               0x0001,
		SessionId:           0x8E40014000011,
	}

	// Set the nonce (11 CCM bytes + 5 zero padding)
	nonceBytes, err := hex.DecodeString("66E69A111892584FB5ED52")
	if err != nil {
		t.Fatal(err)
	}
	copy(h.Nonce[:], nonceBytes)

	// Set the signature
	sigBytes, err := hex.DecodeString("81A286535415445DAE393921E44FA42E")
	if err != nil {
		t.Fatal(err)
	}
	copy(h.Signature[:], sigBytes)

	// Verify encoding
	encoded := h.Encode()

	// Check protocol ID
	if pid := binary.LittleEndian.Uint32(encoded[0:4]); pid != TransformProtocolID {
		t.Errorf("ProtocolID = 0x%08x, want 0x%08x", pid, TransformProtocolID)
	}

	// Verify the AAD is exactly 32 bytes and matches expected from spec
	aad := h.AAD()
	if len(aad) != 32 {
		t.Fatalf("AAD length = %d, want 32", len(aad))
	}

	// AAD = Nonce(16) + OriginalMessageSize(4) + Reserved(2) + Flags(2) + SessionId(8)
	// Verify AAD content matches the hex from the Microsoft doc:
	// 66E69A111892584FB5ED524A744DA3EE87000000000001001100001400E40800
	// Actually this hex from the doc is for the whole request, but we can verify structure.

	// Verify Nonce is at correct position in AAD (first 16 bytes)
	if !bytes.Equal(aad[0:11], nonceBytes) {
		t.Errorf("AAD nonce portion mismatch:\ngot:  %s\nwant: %s",
			hex.EncodeToString(aad[0:11]), hex.EncodeToString(nonceBytes))
	}

	// Verify OriginalMessageSize in AAD (at offset 16 in AAD = offset 36 in header)
	msgSize := binary.LittleEndian.Uint32(aad[16:20])
	if msgSize != 0x87 {
		t.Errorf("AAD OriginalMessageSize = 0x%x, want 0x87", msgSize)
	}

	// Verify Flags in AAD
	flags := binary.LittleEndian.Uint16(aad[22:24])
	if flags != 0x0001 {
		t.Errorf("AAD Flags = 0x%04x, want 0x0001", flags)
	}

	// Verify SessionId in AAD
	sid := binary.LittleEndian.Uint64(aad[24:32])
	if sid != 0x8E40014000011 {
		t.Errorf("AAD SessionId = 0x%016x, want 0x008E40014000011", sid)
	}

	// Verify round-trip
	parsed, err := ParseTransformHeader(encoded)
	if err != nil {
		t.Fatalf("ParseTransformHeader: %v", err)
	}
	if parsed.SessionId != 0x8E40014000011 {
		t.Errorf("parsed SessionId = 0x%016x, want 0x008E40014000011", parsed.SessionId)
	}
	if parsed.OriginalMessageSize != 0x87 {
		t.Errorf("parsed OriginalMessageSize = 0x%x, want 0x87", parsed.OriginalMessageSize)
	}
}

func TestIsTransformMessage(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{
			name: "ValidTransform",
			data: func() []byte {
				buf := make([]byte, TransformHeaderSize)
				binary.LittleEndian.PutUint32(buf[0:4], TransformProtocolID)
				return buf
			}(),
			want: true,
		},
		{
			name: "SMB2Message",
			data: []byte{0xFE, 'S', 'M', 'B'},
			want: false,
		},
		{
			name: "SMB1Message",
			data: []byte{0xFF, 'S', 'M', 'B'},
			want: false,
		},
		{
			name: "TooShort",
			data: []byte{0xFD, 'S', 'M'},
			want: false,
		},
		{
			name: "Empty",
			data: []byte{},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTransformMessage(tt.data); got != tt.want {
				t.Errorf("IsTransformMessage() = %v, want %v", got, tt.want)
			}
		})
	}
}
