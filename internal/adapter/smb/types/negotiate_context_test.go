package types

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// ===== Negotiate Context Constants Tests =====

func TestNegotiateContextConstants(t *testing.T) {
	// Verify constants match MS-SMB2 spec
	if NegCtxPreauthIntegrity != 0x0001 {
		t.Errorf("NegCtxPreauthIntegrity: expected 0x0001, got 0x%04X", NegCtxPreauthIntegrity)
	}
	if NegCtxEncryptionCaps != 0x0002 {
		t.Errorf("NegCtxEncryptionCaps: expected 0x0002, got 0x%04X", NegCtxEncryptionCaps)
	}
	if NegCtxNetnameContextID != 0x0005 {
		t.Errorf("NegCtxNetnameContextID: expected 0x0005, got 0x%04X", NegCtxNetnameContextID)
	}
}

func TestHashAlgorithmConstants(t *testing.T) {
	if HashAlgSHA512 != 0x0001 {
		t.Errorf("HashAlgSHA512: expected 0x0001, got 0x%04X", HashAlgSHA512)
	}
}

func TestCipherConstants(t *testing.T) {
	if CipherAES128CCM != 0x0001 {
		t.Errorf("CipherAES128CCM: expected 0x0001, got 0x%04X", CipherAES128CCM)
	}
	if CipherAES128GCM != 0x0002 {
		t.Errorf("CipherAES128GCM: expected 0x0002, got 0x%04X", CipherAES128GCM)
	}
	if CipherAES256CCM != 0x0003 {
		t.Errorf("CipherAES256CCM: expected 0x0003, got 0x%04X", CipherAES256CCM)
	}
	if CipherAES256GCM != 0x0004 {
		t.Errorf("CipherAES256GCM: expected 0x0004, got 0x%04X", CipherAES256GCM)
	}
}

// ===== PreauthIntegrityCaps Tests =====

func TestPreauthIntegrityCapsEncodeDecode(t *testing.T) {
	salt := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10,
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
		0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F, 0x20}

	original := PreauthIntegrityCaps{
		HashAlgorithms: []uint16{HashAlgSHA512},
		Salt:           salt,
	}

	encoded := original.Encode()
	if encoded == nil {
		t.Fatal("Encode returned nil")
	}

	decoded, err := DecodePreauthIntegrityCaps(encoded)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}

	if len(decoded.HashAlgorithms) != 1 {
		t.Fatalf("expected 1 hash algorithm, got %d", len(decoded.HashAlgorithms))
	}
	if decoded.HashAlgorithms[0] != HashAlgSHA512 {
		t.Errorf("expected SHA512 (0x0001), got 0x%04X", decoded.HashAlgorithms[0])
	}
	if !bytes.Equal(decoded.Salt, salt) {
		t.Errorf("salt mismatch: got %v", decoded.Salt)
	}
}

func TestPreauthIntegrityCapsMultipleAlgorithms(t *testing.T) {
	original := PreauthIntegrityCaps{
		HashAlgorithms: []uint16{HashAlgSHA512, 0x0002}, // SHA-512 + hypothetical
		Salt:           []byte{0xAA, 0xBB},
	}

	encoded := original.Encode()
	decoded, err := DecodePreauthIntegrityCaps(encoded)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}

	if len(decoded.HashAlgorithms) != 2 {
		t.Fatalf("expected 2 hash algorithms, got %d", len(decoded.HashAlgorithms))
	}
	if decoded.HashAlgorithms[0] != HashAlgSHA512 {
		t.Errorf("first algorithm: expected 0x0001, got 0x%04X", decoded.HashAlgorithms[0])
	}
	if decoded.HashAlgorithms[1] != 0x0002 {
		t.Errorf("second algorithm: expected 0x0002, got 0x%04X", decoded.HashAlgorithms[1])
	}
}

// ===== EncryptionCaps Tests =====

func TestEncryptionCapsEncodeDecode(t *testing.T) {
	original := EncryptionCaps{
		Ciphers: []uint16{CipherAES128CCM, CipherAES128GCM},
	}

	encoded := original.Encode()
	if encoded == nil {
		t.Fatal("Encode returned nil")
	}

	decoded, err := DecodeEncryptionCaps(encoded)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}

	if len(decoded.Ciphers) != 2 {
		t.Fatalf("expected 2 ciphers, got %d", len(decoded.Ciphers))
	}
	if decoded.Ciphers[0] != CipherAES128CCM {
		t.Errorf("first cipher: expected 0x0001, got 0x%04X", decoded.Ciphers[0])
	}
	if decoded.Ciphers[1] != CipherAES128GCM {
		t.Errorf("second cipher: expected 0x0002, got 0x%04X", decoded.Ciphers[1])
	}
}

func TestEncryptionCapsAllCiphers(t *testing.T) {
	original := EncryptionCaps{
		Ciphers: []uint16{CipherAES128CCM, CipherAES128GCM, CipherAES256CCM, CipherAES256GCM},
	}

	encoded := original.Encode()
	decoded, err := DecodeEncryptionCaps(encoded)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}

	if len(decoded.Ciphers) != 4 {
		t.Fatalf("expected 4 ciphers, got %d", len(decoded.Ciphers))
	}
}

// ===== NetnameContext Tests =====

func TestNetnameContextDecode(t *testing.T) {
	// Encode "server1" as UTF-16LE
	name := "server1"
	utf16 := make([]byte, len(name)*2)
	for i, c := range name {
		binary.LittleEndian.PutUint16(utf16[i*2:], uint16(c))
	}

	decoded, err := DecodeNetnameContext(utf16)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}

	if decoded.NetName != "server1" {
		t.Errorf("expected 'server1', got %q", decoded.NetName)
	}
}

func TestNetnameContextDecodeEmpty(t *testing.T) {
	decoded, err := DecodeNetnameContext([]byte{})
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if decoded.NetName != "" {
		t.Errorf("expected empty string, got %q", decoded.NetName)
	}
}

// ===== NegotiateContext List Tests =====

func TestParseNegotiateContextListSingle(t *testing.T) {
	// Build a single preauth integrity context
	pic := PreauthIntegrityCaps{
		HashAlgorithms: []uint16{HashAlgSHA512},
		Salt:           make([]byte, 32),
	}
	picData := pic.Encode()

	// Build context header: ContextType(2) + DataLength(2) + Reserved(4) + Data
	buf := make([]byte, 8+len(picData))
	binary.LittleEndian.PutUint16(buf[0:], NegCtxPreauthIntegrity)
	binary.LittleEndian.PutUint16(buf[2:], uint16(len(picData)))
	// Reserved at 4:8 = 0
	copy(buf[8:], picData)

	contexts, err := ParseNegotiateContextList(buf, 1)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(contexts) != 1 {
		t.Fatalf("expected 1 context, got %d", len(contexts))
	}
	if contexts[0].ContextType != NegCtxPreauthIntegrity {
		t.Errorf("expected type 0x0001, got 0x%04X", contexts[0].ContextType)
	}
	if !bytes.Equal(contexts[0].Data, picData) {
		t.Error("context data mismatch")
	}
}

func TestParseNegotiateContextListMultiple(t *testing.T) {
	// Build two contexts with 8-byte alignment between them
	pic := PreauthIntegrityCaps{
		HashAlgorithms: []uint16{HashAlgSHA512},
		Salt:           make([]byte, 32),
	}
	picData := pic.Encode()

	enc := EncryptionCaps{
		Ciphers: []uint16{CipherAES128GCM},
	}
	encData := enc.Encode()

	// Context 1: header(8) + data
	ctx1Len := 8 + len(picData)
	// Pad to 8-byte boundary for next context
	padded1 := ctx1Len
	if padded1%8 != 0 {
		padded1 += 8 - (padded1 % 8)
	}
	// Context 2: header(8) + data
	totalLen := padded1 + 8 + len(encData)

	buf := make([]byte, totalLen)
	// Context 1
	binary.LittleEndian.PutUint16(buf[0:], NegCtxPreauthIntegrity)
	binary.LittleEndian.PutUint16(buf[2:], uint16(len(picData)))
	copy(buf[8:], picData)
	// Context 2 (at padded offset)
	binary.LittleEndian.PutUint16(buf[padded1:], NegCtxEncryptionCaps)
	binary.LittleEndian.PutUint16(buf[padded1+2:], uint16(len(encData)))
	copy(buf[padded1+8:], encData)

	contexts, err := ParseNegotiateContextList(buf, 2)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(contexts) != 2 {
		t.Fatalf("expected 2 contexts, got %d", len(contexts))
	}
	if contexts[0].ContextType != NegCtxPreauthIntegrity {
		t.Errorf("context 0: expected type 0x0001, got 0x%04X", contexts[0].ContextType)
	}
	if contexts[1].ContextType != NegCtxEncryptionCaps {
		t.Errorf("context 1: expected type 0x0002, got 0x%04X", contexts[1].ContextType)
	}
}

func TestEncodeNegotiateContextListRoundtrip(t *testing.T) {
	contexts := []NegotiateContext{
		{
			ContextType: NegCtxPreauthIntegrity,
			Data: PreauthIntegrityCaps{
				HashAlgorithms: []uint16{HashAlgSHA512},
				Salt:           make([]byte, 32),
			}.Encode(),
		},
		{
			ContextType: NegCtxEncryptionCaps,
			Data: EncryptionCaps{
				Ciphers: []uint16{CipherAES128CCM, CipherAES128GCM},
			}.Encode(),
		},
	}

	encoded := EncodeNegotiateContextList(contexts)
	if encoded == nil {
		t.Fatal("Encode returned nil")
	}

	decoded, err := ParseNegotiateContextList(encoded, 2)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	if len(decoded) != 2 {
		t.Fatalf("expected 2 contexts, got %d", len(decoded))
	}
	if decoded[0].ContextType != NegCtxPreauthIntegrity {
		t.Errorf("context 0: expected type 0x0001, got 0x%04X", decoded[0].ContextType)
	}
	if decoded[1].ContextType != NegCtxEncryptionCaps {
		t.Errorf("context 1: expected type 0x0002, got 0x%04X", decoded[1].ContextType)
	}
}

func TestEncodeNegotiateContextListAlignment(t *testing.T) {
	// Create contexts where data length causes non-8-byte alignment
	contexts := []NegotiateContext{
		{
			ContextType: NegCtxPreauthIntegrity,
			Data:        []byte{0x01, 0x02, 0x03}, // 3 bytes -> header(8)+3 = 11 bytes, need pad to 16
		},
		{
			ContextType: NegCtxEncryptionCaps,
			Data:        []byte{0x04, 0x05}, // 2 bytes -> header(8)+2 = 10, no pad (last)
		},
	}

	encoded := EncodeNegotiateContextList(contexts)
	// First context: 8 (header) + 3 (data) = 11, padded to 16
	// Second context: 8 (header) + 2 (data) = 10, no padding (last)
	// Total: 16 + 10 = 26
	if len(encoded) != 26 {
		t.Errorf("expected 26 bytes, got %d", len(encoded))
	}

	// Verify padding bytes are zero
	if encoded[11] != 0 || encoded[12] != 0 || encoded[13] != 0 || encoded[14] != 0 || encoded[15] != 0 {
		t.Errorf("padding bytes should be zero: %v", encoded[11:16])
	}
}

func TestParseNegotiateContextListUnknownType(t *testing.T) {
	// Context with unknown type should be parsed but not cause error
	buf := make([]byte, 12)                        // header(8) + data(4)
	binary.LittleEndian.PutUint16(buf[0:], 0xFFFF) // Unknown type
	binary.LittleEndian.PutUint16(buf[2:], 4)      // 4 bytes data
	buf[8] = 0xDE
	buf[9] = 0xAD
	buf[10] = 0xBE
	buf[11] = 0xEF

	contexts, err := ParseNegotiateContextList(buf, 1)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(contexts) != 1 {
		t.Fatalf("expected 1 context, got %d", len(contexts))
	}
	if contexts[0].ContextType != 0xFFFF {
		t.Errorf("expected type 0xFFFF, got 0x%04X", contexts[0].ContextType)
	}
}

func TestParseNegotiateContextListEmpty(t *testing.T) {
	contexts, err := ParseNegotiateContextList([]byte{}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(contexts) != 0 {
		t.Errorf("expected 0 contexts, got %d", len(contexts))
	}
}

func TestEncodeNegotiateContextListEmpty(t *testing.T) {
	encoded := EncodeNegotiateContextList(nil)
	if len(encoded) != 0 {
		t.Errorf("expected empty, got %d bytes", len(encoded))
	}
}
