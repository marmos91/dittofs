package types

import (
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
)

// NegotiateContext represents a single SMB2 negotiate context as sent in
// SMB 3.1.1 NEGOTIATE request/response messages.
//
// [MS-SMB2] Section 2.2.3.1
type NegotiateContext struct {
	// ContextType identifies the context type (e.g., NegCtxPreauthIntegrity).
	ContextType uint16

	// Data contains the context-specific payload.
	Data []byte
}

// PreauthIntegrityCaps represents SMB2_PREAUTH_INTEGRITY_CAPABILITIES.
//
// Specifies supported preauth integrity hash algorithms and salt for the
// connection preauth integrity hash computation.
//
// [MS-SMB2] Section 2.2.3.1.1
type PreauthIntegrityCaps struct {
	// HashAlgorithms contains the supported hash algorithm IDs.
	HashAlgorithms []uint16

	// Salt is the random salt value.
	Salt []byte
}

// Encode serializes the PreauthIntegrityCaps to wire format.
//
// Wire format:
//
//	HashAlgorithmCount (2 bytes)
//	SaltLength         (2 bytes)
//	HashAlgorithms     (HashAlgorithmCount * 2 bytes)
//	Salt               (SaltLength bytes)
func (p PreauthIntegrityCaps) Encode() []byte {
	w := smbenc.NewWriter(4 + len(p.HashAlgorithms)*2 + len(p.Salt))
	w.WriteUint16(uint16(len(p.HashAlgorithms)))
	w.WriteUint16(uint16(len(p.Salt)))
	for _, alg := range p.HashAlgorithms {
		w.WriteUint16(alg)
	}
	w.WriteBytes(p.Salt)
	return w.Bytes()
}

// DecodePreauthIntegrityCaps parses SMB2_PREAUTH_INTEGRITY_CAPABILITIES from wire data.
func DecodePreauthIntegrityCaps(data []byte) (PreauthIntegrityCaps, error) {
	r := smbenc.NewReader(data)
	algCount := r.ReadUint16()
	saltLen := r.ReadUint16()
	if r.Err() != nil {
		return PreauthIntegrityCaps{}, fmt.Errorf("preauth integrity caps: %w", r.Err())
	}

	algs := make([]uint16, algCount)
	for i := range algs {
		algs[i] = r.ReadUint16()
	}

	salt := r.ReadBytes(int(saltLen))
	if r.Err() != nil {
		return PreauthIntegrityCaps{}, fmt.Errorf("preauth integrity caps: %w", r.Err())
	}

	return PreauthIntegrityCaps{
		HashAlgorithms: algs,
		Salt:           salt,
	}, nil
}

// EncryptionCaps represents SMB2_ENCRYPTION_CAPABILITIES.
//
// Specifies supported encryption cipher IDs.
//
// [MS-SMB2] Section 2.2.3.1.2
type EncryptionCaps struct {
	// Ciphers contains the supported cipher IDs.
	Ciphers []uint16
}

// Encode serializes the EncryptionCaps to wire format.
//
// Wire format:
//
//	CipherCount (2 bytes)
//	Ciphers     (CipherCount * 2 bytes)
func (e EncryptionCaps) Encode() []byte {
	w := smbenc.NewWriter(2 + len(e.Ciphers)*2)
	w.WriteUint16(uint16(len(e.Ciphers)))
	for _, c := range e.Ciphers {
		w.WriteUint16(c)
	}
	return w.Bytes()
}

// DecodeEncryptionCaps parses SMB2_ENCRYPTION_CAPABILITIES from wire data.
func DecodeEncryptionCaps(data []byte) (EncryptionCaps, error) {
	r := smbenc.NewReader(data)
	cipherCount := r.ReadUint16()
	if r.Err() != nil {
		return EncryptionCaps{}, fmt.Errorf("encryption caps: %w", r.Err())
	}

	ciphers := make([]uint16, cipherCount)
	for i := range ciphers {
		ciphers[i] = r.ReadUint16()
	}
	if r.Err() != nil {
		return EncryptionCaps{}, fmt.Errorf("encryption caps: %w", r.Err())
	}

	return EncryptionCaps{
		Ciphers: ciphers,
	}, nil
}

// NetnameContext represents SMB2_NETNAME_NEGOTIATE_CONTEXT_ID.
//
// Contains the server name as a UTF-16LE string. This context is sent by
// the client only; the server does not include it in the response.
//
// [MS-SMB2] Section 2.2.3.1.4
type NetnameContext struct {
	// NetName is the server name (decoded from UTF-16LE).
	NetName string
}

// DecodeNetnameContext parses SMB2_NETNAME_NEGOTIATE_CONTEXT_ID from wire data.
// The data is a UTF-16LE encoded string without null terminator.
func DecodeNetnameContext(data []byte) (NetnameContext, error) {
	if len(data) == 0 {
		return NetnameContext{}, nil
	}
	if len(data)%2 != 0 {
		return NetnameContext{}, fmt.Errorf("netname context: odd data length %d", len(data))
	}

	// Decode UTF-16LE to string (ASCII subset for server names)
	runes := make([]rune, len(data)/2)
	for i := range runes {
		runes[i] = rune(binary.LittleEndian.Uint16(data[i*2:]))
	}
	return NetnameContext{
		NetName: string(runes),
	}, nil
}

// ParseNegotiateContextList parses a list of negotiate contexts from wire data.
// Contexts are 8-byte aligned relative to the start of the context list.
//
// [MS-SMB2] Section 2.2.3.1
func ParseNegotiateContextList(data []byte, count int) ([]NegotiateContext, error) {
	if count == 0 {
		return nil, nil
	}

	contexts := make([]NegotiateContext, 0, count)
	offset := 0

	for i := range count {
		// Each context header: ContextType(2) + DataLength(2) + Reserved(4) = 8 bytes
		if offset+8 > len(data) {
			return nil, fmt.Errorf("negotiate context %d: insufficient data for header at offset %d", i, offset)
		}

		contextType := binary.LittleEndian.Uint16(data[offset:])
		dataLength := binary.LittleEndian.Uint16(data[offset+2:])
		// Reserved at offset+4 (4 bytes), skip

		headerEnd := offset + 8
		if headerEnd+int(dataLength) > len(data) {
			return nil, fmt.Errorf("negotiate context %d: insufficient data for payload at offset %d (need %d, have %d)",
				i, headerEnd, dataLength, len(data)-headerEnd)
		}

		ctxData := make([]byte, int(dataLength))
		copy(ctxData, data[headerEnd:headerEnd+int(dataLength)])

		contexts = append(contexts, NegotiateContext{
			ContextType: contextType,
			Data:        ctxData,
		})

		// Advance past header + data
		offset = headerEnd + int(dataLength)

		// Pad to 8-byte alignment for next context (not after last)
		if i < count-1 && offset%8 != 0 {
			offset += 8 - (offset % 8)
		}
	}

	return contexts, nil
}

// EncodeNegotiateContextList encodes a list of negotiate contexts to wire format
// with 8-byte alignment padding between contexts (not after the last one).
func EncodeNegotiateContextList(contexts []NegotiateContext) []byte {
	if len(contexts) == 0 {
		return nil
	}

	w := smbenc.NewWriter(256)
	for i, ctx := range contexts {
		// Context header: ContextType(2) + DataLength(2) + Reserved(4)
		w.WriteUint16(ctx.ContextType)
		w.WriteUint16(uint16(len(ctx.Data)))
		w.WriteUint32(0) // Reserved
		w.WriteBytes(ctx.Data)

		// Pad to 8-byte alignment between contexts (not after last)
		if i < len(contexts)-1 {
			w.Pad(8)
		}
	}

	return w.Bytes()
}
