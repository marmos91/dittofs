package xdr

import (
	"encoding/binary"
	"testing"
)

// ============================================================================
// DecodeMapping Tests
// ============================================================================

func TestDecodeMapping_Valid(t *testing.T) {
	data := make([]byte, MappingSize)
	binary.BigEndian.PutUint32(data[0:4], 100003)  // prog = NFS
	binary.BigEndian.PutUint32(data[4:8], 3)       // vers = 3
	binary.BigEndian.PutUint32(data[8:12], 6)      // prot = TCP
	binary.BigEndian.PutUint32(data[12:16], 12049) // port = 12049

	m, err := DecodeMapping(data)
	if err != nil {
		t.Fatalf("DecodeMapping failed: %v", err)
	}

	if m.Prog != 100003 {
		t.Errorf("Prog = %d, want 100003", m.Prog)
	}
	if m.Vers != 3 {
		t.Errorf("Vers = %d, want 3", m.Vers)
	}
	if m.Prot != 6 {
		t.Errorf("Prot = %d, want 6", m.Prot)
	}
	if m.Port != 12049 {
		t.Errorf("Port = %d, want 12049", m.Port)
	}
}

func TestDecodeMapping_Truncated(t *testing.T) {
	// Only 12 bytes instead of required 16
	data := make([]byte, 12)
	binary.BigEndian.PutUint32(data[0:4], 100003)
	binary.BigEndian.PutUint32(data[4:8], 3)
	binary.BigEndian.PutUint32(data[8:12], 6)

	_, err := DecodeMapping(data)
	if err == nil {
		t.Fatal("DecodeMapping should fail with truncated input")
	}
}

func TestDecodeMapping_Empty(t *testing.T) {
	_, err := DecodeMapping(nil)
	if err == nil {
		t.Fatal("DecodeMapping should fail with nil input")
	}

	_, err = DecodeMapping([]byte{})
	if err == nil {
		t.Fatal("DecodeMapping should fail with empty input")
	}
}

// ============================================================================
// EncodeMapping Round-trip Tests
// ============================================================================

func TestEncodeMapping_Roundtrip(t *testing.T) {
	original := &Mapping{
		Prog: 100005,
		Vers: 3,
		Prot: 17,
		Port: 12049,
	}

	encoded := EncodeMapping(original)
	if len(encoded) != MappingSize {
		t.Fatalf("EncodeMapping produced %d bytes, want %d", len(encoded), MappingSize)
	}

	decoded, err := DecodeMapping(encoded)
	if err != nil {
		t.Fatalf("DecodeMapping failed on round-trip: %v", err)
	}

	if decoded.Prog != original.Prog {
		t.Errorf("Prog = %d, want %d", decoded.Prog, original.Prog)
	}
	if decoded.Vers != original.Vers {
		t.Errorf("Vers = %d, want %d", decoded.Vers, original.Vers)
	}
	if decoded.Prot != original.Prot {
		t.Errorf("Prot = %d, want %d", decoded.Prot, original.Prot)
	}
	if decoded.Port != original.Port {
		t.Errorf("Port = %d, want %d", decoded.Port, original.Port)
	}
}

func TestEncodeMapping_ZeroValues(t *testing.T) {
	m := &Mapping{Prog: 0, Vers: 0, Prot: 0, Port: 0}
	encoded := EncodeMapping(m)
	decoded, err := DecodeMapping(encoded)
	if err != nil {
		t.Fatalf("DecodeMapping failed: %v", err)
	}
	if decoded.Prog != 0 || decoded.Vers != 0 || decoded.Prot != 0 || decoded.Port != 0 {
		t.Errorf("Round-trip of zero mapping failed: got %+v", decoded)
	}
}

func TestEncodeMapping_MaxValues(t *testing.T) {
	m := &Mapping{
		Prog: 0xFFFFFFFF,
		Vers: 0xFFFFFFFF,
		Prot: 0xFFFFFFFF,
		Port: 0xFFFFFFFF,
	}
	encoded := EncodeMapping(m)
	decoded, err := DecodeMapping(encoded)
	if err != nil {
		t.Fatalf("DecodeMapping failed: %v", err)
	}
	if decoded.Prog != 0xFFFFFFFF || decoded.Vers != 0xFFFFFFFF ||
		decoded.Prot != 0xFFFFFFFF || decoded.Port != 0xFFFFFFFF {
		t.Errorf("Round-trip of max mapping failed: got %+v", decoded)
	}
}

// ============================================================================
// EncodeDumpResponse Tests
// ============================================================================

func TestEncodeDumpResponse_Empty(t *testing.T) {
	result := EncodeDumpResponse(nil)

	// Empty list should be just the terminator: uint32(0) = 4 bytes
	if len(result) != 4 {
		t.Fatalf("Empty dump response should be 4 bytes, got %d", len(result))
	}

	terminator := binary.BigEndian.Uint32(result[0:4])
	if terminator != 0 {
		t.Errorf("Terminator = %d, want 0", terminator)
	}
}

func TestEncodeDumpResponse_SingleMapping(t *testing.T) {
	mappings := []*Mapping{
		{Prog: 100003, Vers: 3, Prot: 6, Port: 12049},
	}

	result := EncodeDumpResponse(mappings)

	// Expected: 4 (discriminant) + 16 (mapping) + 4 (terminator) = 24 bytes
	expectedLen := 4 + MappingSize + 4
	if len(result) != expectedLen {
		t.Fatalf("Single mapping dump response should be %d bytes, got %d", expectedLen, len(result))
	}

	// Check discriminant = 1 (value follows)
	disc := binary.BigEndian.Uint32(result[0:4])
	if disc != 1 {
		t.Errorf("First discriminant = %d, want 1", disc)
	}

	// Check mapping data
	decoded, err := DecodeMapping(result[4:20])
	if err != nil {
		t.Fatalf("DecodeMapping from dump failed: %v", err)
	}
	if decoded.Prog != 100003 || decoded.Vers != 3 || decoded.Prot != 6 || decoded.Port != 12049 {
		t.Errorf("Decoded mapping = %+v, want {100003, 3, 6, 12049}", decoded)
	}

	// Check terminator = 0
	term := binary.BigEndian.Uint32(result[20:24])
	if term != 0 {
		t.Errorf("Terminator = %d, want 0", term)
	}
}

func TestEncodeDumpResponse_ThreeMappings(t *testing.T) {
	mappings := []*Mapping{
		{Prog: 100003, Vers: 3, Prot: 6, Port: 12049},
		{Prog: 100005, Vers: 3, Prot: 6, Port: 12049},
		{Prog: 100021, Vers: 4, Prot: 17, Port: 12049},
	}

	result := EncodeDumpResponse(mappings)

	// Expected: 3 * (4 + 16) + 4 = 64 bytes
	expectedLen := 3*(4+MappingSize) + 4
	if len(result) != expectedLen {
		t.Fatalf("Three mapping dump response should be %d bytes, got %d", expectedLen, len(result))
	}

	// Walk through the linked list
	offset := 0
	for i, expected := range mappings {
		disc := binary.BigEndian.Uint32(result[offset : offset+4])
		if disc != 1 {
			t.Errorf("Entry %d: discriminant = %d, want 1", i, disc)
		}
		offset += 4

		decoded, err := DecodeMapping(result[offset : offset+MappingSize])
		if err != nil {
			t.Fatalf("Entry %d: DecodeMapping failed: %v", i, err)
		}
		if decoded.Prog != expected.Prog || decoded.Vers != expected.Vers ||
			decoded.Prot != expected.Prot || decoded.Port != expected.Port {
			t.Errorf("Entry %d: got %+v, want %+v", i, decoded, expected)
		}
		offset += MappingSize
	}

	// Check terminator
	term := binary.BigEndian.Uint32(result[offset : offset+4])
	if term != 0 {
		t.Errorf("Terminator = %d, want 0", term)
	}
}

// ============================================================================
// EncodeGetportResponse Tests
// ============================================================================

func TestEncodeGetportResponse(t *testing.T) {
	tests := []struct {
		name string
		port uint32
	}{
		{"zero (not registered)", 0},
		{"standard NFS port", 2049},
		{"DittoFS port", 12049},
		{"max port", 65535},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EncodeGetportResponse(tt.port)
			if len(result) != 4 {
				t.Fatalf("EncodeGetportResponse should produce 4 bytes, got %d", len(result))
			}

			decoded := binary.BigEndian.Uint32(result)
			if decoded != tt.port {
				t.Errorf("Decoded port = %d, want %d", decoded, tt.port)
			}
		})
	}
}

// ============================================================================
// EncodeBoolResponse Tests
// ============================================================================

func TestEncodeBoolResponse_True(t *testing.T) {
	result := EncodeBoolResponse(true)
	if len(result) != 4 {
		t.Fatalf("EncodeBoolResponse should produce 4 bytes, got %d", len(result))
	}

	val := binary.BigEndian.Uint32(result)
	if val != 1 {
		t.Errorf("True encoded as %d, want 1", val)
	}
}

func TestEncodeBoolResponse_False(t *testing.T) {
	result := EncodeBoolResponse(false)
	if len(result) != 4 {
		t.Fatalf("EncodeBoolResponse should produce 4 bytes, got %d", len(result))
	}

	val := binary.BigEndian.Uint32(result)
	if val != 0 {
		t.Errorf("False encoded as %d, want 0", val)
	}
}
