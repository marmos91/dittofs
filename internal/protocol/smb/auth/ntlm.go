// Package auth implements SMB authentication mechanisms.
package auth

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
)

// NTLM message type constants
const (
	NTLMNegotiate    uint32 = 1
	NTLMChallenge    uint32 = 2
	NTLMAuthenticate uint32 = 3
)

// NTLMSSP signature
var NTLMSSPSignature = []byte{'N', 'T', 'L', 'M', 'S', 'S', 'P', 0}

// NTLM negotiate flags
const (
	NTLMNegotiateUnicode              uint32 = 0x00000001
	NTLMNegotiateOEM                  uint32 = 0x00000002
	NTLMRequestTarget                 uint32 = 0x00000004
	NTLMNegotiateSign                 uint32 = 0x00000010
	NTLMNegotiateSeal                 uint32 = 0x00000020
	NTLMNegotiateLMKey                uint32 = 0x00000080
	NTLMNegotiateNTLM                 uint32 = 0x00000200
	NTLMNegotiateAnonymous            uint32 = 0x00000800
	NTLMNegotiateDomainSupplied       uint32 = 0x00001000
	NTLMNegotiateWorkstationSupplied  uint32 = 0x00002000
	NTLMNegotiateAlwaysSign           uint32 = 0x00008000
	NTLMTargetTypeDomain              uint32 = 0x00010000
	NTLMTargetTypeServer              uint32 = 0x00020000
	NTLMNegotiateExtendedSecurity     uint32 = 0x00080000
	NTLMNegotiateTargetInfo           uint32 = 0x00800000
	NTLMNegotiateVersion              uint32 = 0x02000000
	NTLMNegotiate128                  uint32 = 0x20000000
	NTLMNegotiate56                   uint32 = 0x80000000
)

// IsNTLMSSP checks if the buffer starts with the NTLMSSP signature.
func IsNTLMSSP(buf []byte) bool {
	if len(buf) < 12 {
		return false
	}
	return bytes.Equal(buf[:8], NTLMSSPSignature)
}

// GetNTLMMessageType returns the NTLM message type from a buffer.
func GetNTLMMessageType(buf []byte) uint32 {
	if len(buf) < 12 {
		return 0
	}
	return binary.LittleEndian.Uint32(buf[8:12])
}

// BuildNTLMChallenge creates an NTLM Type 2 (CHALLENGE) message for anonymous authentication.
// This is a minimal challenge that allows guest/anonymous sessions.
func BuildNTLMChallenge() []byte {
	// Generate random 8-byte challenge
	challenge := make([]byte, 8)
	rand.Read(challenge)

	// Target name: empty for anonymous
	targetName := []byte{}

	// Flags for anonymous/guest support
	flags := NTLMNegotiateUnicode |
		NTLMRequestTarget |
		NTLMNegotiateNTLM |
		NTLMNegotiateAlwaysSign |
		NTLMTargetTypeServer |
		NTLMNegotiateExtendedSecurity |
		NTLMNegotiateTargetInfo |
		NTLMNegotiate128 |
		NTLMNegotiate56

	// Build Type 2 message
	// Structure:
	// - Signature (8 bytes): NTLMSSP\0
	// - MessageType (4 bytes): 2
	// - TargetNameFields (8 bytes): Len/MaxLen/Offset
	// - NegotiateFlags (4 bytes)
	// - ServerChallenge (8 bytes)
	// - Reserved (8 bytes)
	// - TargetInfoFields (8 bytes): Len/MaxLen/Offset
	// - [Version (8 bytes) - optional]
	// - TargetName (variable)
	// - TargetInfo (variable)

	// Minimal target info (just end marker)
	targetInfo := buildMinimalTargetInfo()

	// Calculate offsets
	baseSize := 56 // Up to TargetInfoFields
	targetNameOffset := baseSize
	targetInfoOffset := targetNameOffset + len(targetName)

	// Build message
	msg := make([]byte, targetInfoOffset+len(targetInfo))

	// Signature
	copy(msg[0:8], NTLMSSPSignature)
	// MessageType
	binary.LittleEndian.PutUint32(msg[8:12], NTLMChallenge)
	// TargetNameFields
	binary.LittleEndian.PutUint16(msg[12:14], uint16(len(targetName)))   // Len
	binary.LittleEndian.PutUint16(msg[14:16], uint16(len(targetName)))   // MaxLen
	binary.LittleEndian.PutUint32(msg[16:20], uint32(targetNameOffset))  // Offset
	// NegotiateFlags
	binary.LittleEndian.PutUint32(msg[20:24], flags)
	// ServerChallenge
	copy(msg[24:32], challenge)
	// Reserved (8 bytes of zeros)
	// TargetInfoFields
	binary.LittleEndian.PutUint16(msg[40:42], uint16(len(targetInfo)))   // Len
	binary.LittleEndian.PutUint16(msg[42:44], uint16(len(targetInfo)))   // MaxLen
	binary.LittleEndian.PutUint32(msg[44:48], uint32(targetInfoOffset))  // Offset
	// Version (optional, 8 bytes) - we skip it for simplicity
	// Note: baseSize includes space for this but we leave it as zeros

	// Copy variable data
	copy(msg[targetNameOffset:], targetName)
	copy(msg[targetInfoOffset:], targetInfo)

	return msg
}

// buildMinimalTargetInfo creates a minimal AV_PAIR list with just the terminator.
func buildMinimalTargetInfo() []byte {
	// AV_PAIR structure:
	// AvId (2 bytes) + AvLen (2 bytes) + Value (variable)
	// MsvAvEOL (0x0000) terminates the list

	// Just the terminator: AvId=0, AvLen=0
	return []byte{0x00, 0x00, 0x00, 0x00}
}
