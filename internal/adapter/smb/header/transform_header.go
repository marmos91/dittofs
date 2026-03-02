package header

import (
	"encoding/binary"
	"errors"
)

// Transform header constants.
const (
	// TransformHeaderSize is the fixed size of an SMB2 TRANSFORM_HEADER (52 bytes).
	// [MS-SMB2] Section 2.2.41
	TransformHeaderSize = 52

	// TransformProtocolID is the protocol identifier for transform headers.
	// Little-endian representation of 0xFD 'S' 'M' 'B'.
	TransformProtocolID uint32 = 0x424D53FD
)

// Transform header errors.
var (
	// ErrTransformTooShort indicates the data is too short for a transform header.
	ErrTransformTooShort = errors.New("message too short for SMB2 transform header")

	// ErrTransformInvalidProtocol indicates the transform header has wrong protocol ID.
	ErrTransformInvalidProtocol = errors.New("invalid SMB2 transform header protocol ID")

	// ErrTransformInvalidReserved indicates the Reserved field (offset 40-41) is not zero.
	ErrTransformInvalidReserved = errors.New("invalid SMB2 transform header: Reserved field must be 0")

	// ErrTransformInvalidFlags indicates the Flags field (offset 42-43) has an invalid value.
	// Per MS-SMB2, this field must be 0x0001 (Encrypted).
	ErrTransformInvalidFlags = errors.New("invalid SMB2 transform header: Flags must be 0x0001")
)

// TransformHeader represents the SMB2 TRANSFORM_HEADER used for message encryption.
//
// Wire layout (52 bytes):
//
//	Offset  Size  Field
//	------  ----  -------------------
//	0       4     ProtocolId (0xFD534D42 LE, not stored in struct)
//	4       16    Signature (AEAD authentication tag)
//	20      16    Nonce (CCM: first 11 bytes used, GCM: first 12 bytes used)
//	36      4     OriginalMessageSize (size of unencrypted SMB2 message)
//	40      2     Reserved (must be 0)
//	42      2     Flags (0x0001 = Encrypted for 3.1.1, EncryptionAlgorithm for 3.0)
//	44      8     SessionId
//
// The AAD (Additional Authenticated Data) for AEAD operations is the 32 bytes
// starting at offset 20 (Nonce through SessionId, inclusive). This is the
// transform header excluding the ProtocolId and Signature fields.
//
// Reference: [MS-SMB2] Section 2.2.41
type TransformHeader struct {
	// Signature contains the AEAD authentication tag (16 bytes).
	Signature [16]byte

	// Nonce is a 16-byte field containing the AEAD nonce.
	// For CCM, only the first 11 bytes are the actual nonce; the rest is zero padding.
	// For GCM, only the first 12 bytes are the actual nonce; the rest is zero padding.
	Nonce [16]byte

	// OriginalMessageSize is the size of the unencrypted SMB2 message in bytes.
	OriginalMessageSize uint32

	// Flags is always 0x0001.
	// For SMB 3.1.1: Flags field with value 0x0001 meaning "Encrypted".
	// For SMB 3.0/3.0.2: EncryptionAlgorithm field with value 0x0001 meaning AES-128-CCM.
	Flags uint16

	// SessionId identifies the session whose keys are used for encryption/decryption.
	SessionId uint64
}

// ParseTransformHeader parses a transform header from wire format data.
//
// The data must be at least 52 bytes and must start with the transform protocol ID
// (0xFD 'S' 'M' 'B'). Returns an error if the data is too short or has the wrong
// protocol ID.
func ParseTransformHeader(data []byte) (*TransformHeader, error) {
	if len(data) < TransformHeaderSize {
		return nil, ErrTransformTooShort
	}

	protocolID := binary.LittleEndian.Uint32(data[0:4])
	if protocolID != TransformProtocolID {
		return nil, ErrTransformInvalidProtocol
	}

	reserved := binary.LittleEndian.Uint16(data[40:42])
	if reserved != 0 {
		return nil, ErrTransformInvalidReserved
	}

	flags := binary.LittleEndian.Uint16(data[42:44])
	if flags != 0x0001 {
		return nil, ErrTransformInvalidFlags
	}

	h := &TransformHeader{
		OriginalMessageSize: binary.LittleEndian.Uint32(data[36:40]),
		Flags:               flags,
		SessionId:           binary.LittleEndian.Uint64(data[44:52]),
	}
	copy(h.Signature[:], data[4:20])
	copy(h.Nonce[:], data[20:36])

	return h, nil
}

// Encode serializes the transform header to its 52-byte wire format.
//
// The ProtocolId is always written as 0xFD 'S' 'M' 'B' (TransformProtocolID).
// The Reserved field at offset 40-41 is always set to zero.
func (h *TransformHeader) Encode() []byte {
	buf := make([]byte, TransformHeaderSize)
	binary.LittleEndian.PutUint32(buf[0:4], TransformProtocolID)
	copy(buf[4:20], h.Signature[:])
	copy(buf[20:36], h.Nonce[:])
	binary.LittleEndian.PutUint32(buf[36:40], h.OriginalMessageSize)
	binary.LittleEndian.PutUint16(buf[40:42], 0) // Reserved
	binary.LittleEndian.PutUint16(buf[42:44], h.Flags)
	binary.LittleEndian.PutUint64(buf[44:52], h.SessionId)
	return buf
}

// AAD returns the 32-byte Additional Authenticated Data for AEAD operations.
//
// The AAD is bytes 20-51 of the encoded transform header:
//
//	Nonce(16) + OriginalMessageSize(4) + Reserved(2) + Flags(2) + SessionId(8) = 32 bytes
//
// This is the transform header excluding the ProtocolId and Signature fields,
// as specified in [MS-SMB2] Section 3.1.4.3.
func (h *TransformHeader) AAD() []byte {
	aad := make([]byte, 32)
	copy(aad[0:16], h.Nonce[:])
	binary.LittleEndian.PutUint32(aad[16:20], h.OriginalMessageSize)
	binary.LittleEndian.PutUint16(aad[20:22], 0) // Reserved
	binary.LittleEndian.PutUint16(aad[22:24], h.Flags)
	binary.LittleEndian.PutUint64(aad[24:32], h.SessionId)
	return aad
}

// IsTransformMessage checks if the data starts with a transform header protocol ID.
// Use this for protocol detection before calling ParseTransformHeader.
func IsTransformMessage(data []byte) bool {
	return len(data) >= 4 && binary.LittleEndian.Uint32(data[0:4]) == TransformProtocolID
}
