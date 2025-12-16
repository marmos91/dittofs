// Package ntlm implements NTLM authentication for SMB and other protocols.
//
// NTLM (NT LAN Manager) is a challenge-response authentication protocol
// defined in [MS-NLMP]. This package provides:
//   - NTLM message detection and parsing
//   - Challenge (Type 2) message building
//   - Support for guest/anonymous authentication
//
// For production use with credential validation, additional implementation
// of NTLMv2 response verification is required.
package ntlm

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
)

// =============================================================================
// NTLM Message Types
// =============================================================================

// MessageType identifies the three messages in the NTLM handshake.
// [MS-NLMP] Section 2.2.1
type MessageType uint32

const (
	// Negotiate (Type 1) is sent by the client to initiate authentication.
	// Contains client capabilities and optional domain/workstation names.
	Negotiate MessageType = 1

	// Challenge (Type 2) is sent by the server in response to Type 1.
	// Contains the server challenge and negotiated flags.
	Challenge MessageType = 2

	// Authenticate (Type 3) is sent by the client to complete authentication.
	// Contains the challenge response computed from user credentials.
	Authenticate MessageType = 3
)

// =============================================================================
// NTLM Message Structure Constants
// =============================================================================

// Signature is the 8-byte signature that identifies NTLM messages.
// All NTLM messages begin with this signature: "NTLMSSP\0"
// [MS-NLMP] Section 2.2.1
var Signature = []byte{'N', 'T', 'L', 'M', 'S', 'S', 'P', 0}

// NTLM message header offsets (common to all message types)
// [MS-NLMP] Section 2.2.1
const (
	signatureOffset   = 0 // 8 bytes: "NTLMSSP\0"
	messageTypeOffset = 8 // 4 bytes: message type (1, 2, or 3)
	headerSize        = 12
)

// NTLM Type 2 (CHALLENGE) message offsets
// [MS-NLMP] Section 2.2.1.2
const (
	challengeTargetNameLenOffset = 12 // 2 bytes: TargetName length
	challengeTargetNameMaxOffset = 14 // 2 bytes: TargetName max length
	challengeTargetNameOffOffset = 16 // 4 bytes: TargetName buffer offset
	challengeFlagsOffset         = 20 // 4 bytes: NegotiateFlags
	challengeServerChalOffset    = 24 // 8 bytes: ServerChallenge (random)
	challengeReservedOffset      = 32 // 8 bytes: Reserved (must be zero)
	challengeTargetInfoLenOffset = 40 // 2 bytes: TargetInfo length
	challengeTargetInfoMaxOffset = 42 // 2 bytes: TargetInfo max length
	challengeTargetInfoOffOffset = 44 // 4 bytes: TargetInfo buffer offset
	challengeVersionOffset       = 48 // 8 bytes: Version (optional)
	challengeBaseSize            = 56 // Minimum size without payload
)

// NTLM Type 3 (AUTHENTICATE) message offsets
// [MS-NLMP] Section 2.2.1.3
const (
	authLmResponseLenOffset          = 12 // 2 bytes: LmChallengeResponse length
	authLmResponseMaxOffset          = 14 // 2 bytes: LmChallengeResponse max length
	authLmResponseOffOffset          = 16 // 4 bytes: LmChallengeResponse buffer offset
	authNtResponseLenOffset          = 20 // 2 bytes: NtChallengeResponse length
	authNtResponseMaxOffset          = 22 // 2 bytes: NtChallengeResponse max length
	authNtResponseOffOffset          = 24 // 4 bytes: NtChallengeResponse buffer offset
	authDomainNameLenOffset          = 28 // 2 bytes: DomainName length
	authDomainNameMaxOffset          = 30 // 2 bytes: DomainName max length
	authDomainNameOffOffset          = 32 // 4 bytes: DomainName buffer offset
	authUserNameLenOffset            = 36 // 2 bytes: UserName length
	authUserNameMaxOffset            = 38 // 2 bytes: UserName max length
	authUserNameOffOffset            = 40 // 4 bytes: UserName buffer offset
	authWorkstationLenOffset         = 44 // 2 bytes: Workstation length
	authWorkstationMaxOffset         = 46 // 2 bytes: Workstation max length
	authWorkstationOffOffset         = 48 // 4 bytes: Workstation buffer offset
	authEncryptedRandomSessionKeyLen = 52 // 2 bytes: EncryptedRandomSessionKey length
	authEncryptedRandomSessionKeyMax = 54 // 2 bytes: EncryptedRandomSessionKey max length
	authEncryptedRandomSessionKeyOff = 56 // 4 bytes: EncryptedRandomSessionKey buffer offset
	authNegotiateFlagsOffset         = 60 // 4 bytes: NegotiateFlags
	authBaseSize                     = 64 // Minimum size without payload (not including Version)
)

// NTLM challenge sizes
const (
	serverChallengeSize = 8 // ServerChallenge is always 8 bytes
)

// =============================================================================
// NTLM Negotiate Flags
// =============================================================================

// NegotiateFlag controls authentication behavior and capabilities.
// These flags are exchanged in Type 1, Type 2, and Type 3 messages.
// [MS-NLMP] Section 2.2.2.5
type NegotiateFlag uint32

const (
	// FlagUnicode (bit A) indicates Unicode character set encoding.
	// When set, strings are encoded as UTF-16LE.
	FlagUnicode NegotiateFlag = 0x00000001

	// FlagOEM (bit B) indicates OEM character set encoding.
	// When set, strings use the OEM code page.
	FlagOEM NegotiateFlag = 0x00000002

	// FlagRequestTarget (bit C) requests the server's authentication realm.
	// Server responds with TargetName in Type 2 message.
	FlagRequestTarget NegotiateFlag = 0x00000004

	// FlagSign (bit D) indicates message integrity support.
	// Enables MAC generation for signed messages.
	FlagSign NegotiateFlag = 0x00000010

	// FlagSeal (bit E) indicates message confidentiality support.
	// Enables encryption for sealed messages.
	FlagSeal NegotiateFlag = 0x00000020

	// FlagLMKey (bit G) indicates LAN Manager session key computation.
	// Deprecated; should not be used with NTLMv2.
	FlagLMKey NegotiateFlag = 0x00000080

	// FlagNTLM (bit I) indicates NTLM v1 authentication support.
	// Required for compatibility with older clients.
	FlagNTLM NegotiateFlag = 0x00000200

	// FlagAnonymous (bit K) indicates anonymous authentication.
	// Used when client has no credentials.
	FlagAnonymous NegotiateFlag = 0x00000800

	// FlagDomainSupplied (bit L) indicates domain name is present.
	// Set when Type 1 message contains domain name.
	FlagDomainSupplied NegotiateFlag = 0x00001000

	// FlagWorkstationSupplied (bit M) indicates workstation name is present.
	// Set when Type 1 message contains workstation name.
	FlagWorkstationSupplied NegotiateFlag = 0x00002000

	// FlagAlwaysSign (bit O) requires signing for all messages.
	// Even if signing is not negotiated, dummy signature is included.
	FlagAlwaysSign NegotiateFlag = 0x00008000

	// FlagTargetTypeDomain (bit P) indicates target is a domain.
	// Mutually exclusive with FlagTargetTypeServer.
	FlagTargetTypeDomain NegotiateFlag = 0x00010000

	// FlagTargetTypeServer (bit Q) indicates target is a server.
	// Mutually exclusive with FlagTargetTypeDomain.
	FlagTargetTypeServer NegotiateFlag = 0x00020000

	// FlagExtendedSecurity (bit S) indicates extended session security.
	// Enables NTLMv2 session security.
	FlagExtendedSecurity NegotiateFlag = 0x00080000

	// FlagTargetInfo (bit W) indicates TargetInfo is present.
	// Type 2 message includes AV_PAIR list.
	FlagTargetInfo NegotiateFlag = 0x00800000

	// FlagVersion (bit Y) indicates version field is present.
	// Includes OS version information.
	FlagVersion NegotiateFlag = 0x02000000

	// Flag128 (bit Z) indicates 128-bit encryption support.
	// Required for strong encryption.
	Flag128 NegotiateFlag = 0x20000000

	// Flag56 (bit AA) indicates 56-bit encryption support.
	// Legacy; 128-bit is preferred.
	Flag56 NegotiateFlag = 0x80000000
)

// =============================================================================
// Challenge Target - What Is It?
// =============================================================================

// The "target" in NTLM refers to the server or domain that is authenticating
// the client. Think of it as the server saying "Hi, I'm FILESERVER, prove
// you're allowed to access me."
//
// WHY DOES THE TARGET EXIST?
//
// 1. Server Identification
//    The target tells the client WHO is challenging them. Without this,
//    the client wouldn't know which server they're authenticating to.
//
//    Example: "I am FILESERVER in domain CORP"
//
// 2. Security (NTLMv2)
//    In NTLMv2, the TargetInfo is cryptographically included in the client's
//    response hash. This binds the authentication to THIS SPECIFIC SERVER:
//
//    - Prevents reflection attacks: Attacker can't bounce your response
//      back to you pretending to be a different server
//    - Prevents relay attacks: Your response only works for the server
//      that issued the challenge, not any other server
//
// TARGET FIELDS IN THE CHALLENGE MESSAGE
//
// The Type 2 (CHALLENGE) message has two target-related fields:
//
//   ┌─────────────────────────────────────────────────────────────────┐
//   │  TargetName                                                     │
//   │  ───────────                                                    │
//   │  A simple string identifying the server or domain.              │
//   │  Examples: "FILESERVER", "CONTOSO", "WORKGROUP"                 │
//   │  Empty for anonymous/guest authentication.                      │
//   └─────────────────────────────────────────────────────────────────┘
//
//   ┌─────────────────────────────────────────────────────────────────┐
//   │  TargetInfo (AV_PAIR list)                                      │
//   │  ─────────────────────────                                      │
//   │  A list of attribute-value pairs with detailed server info:     │
//   │                                                                 │
//   │    MsvAvNbComputerName  = "FILESERVER"        (NetBIOS name)    │
//   │    MsvAvNbDomainName    = "CORP"              (NetBIOS domain)  │
//   │    MsvAvDnsComputerName = "fileserver.corp.com" (DNS name)      │
//   │    MsvAvTimestamp       = <server time>       (replay protect)  │
//   │    MsvAvEOL             = <end of list>                         │
//   │                                                                 │
//   │  The timestamp is CRITICAL for NTLMv2 - it prevents replay      │
//   │  attacks where an attacker captures and reuses old responses.   │
//   └─────────────────────────────────────────────────────────────────┘
//
// FOR GUEST AUTHENTICATION (this implementation):
// Both fields can be minimal since no credential validation occurs.
// We include just the MsvAvEOL terminator in TargetInfo.

// =============================================================================
// AV_PAIR Constants (TargetInfo Structure)
// =============================================================================

// AvID represents AV_PAIR attribute IDs for the TargetInfo field.
// Each AV_PAIR has: AvId (2 bytes) + AvLen (2 bytes) + Value (AvLen bytes)
// [MS-NLMP] Section 2.2.2.1
type AvID uint16

const (
	// AvEOL (0x0000) marks end of AV_PAIR list.
	// Every TargetInfo MUST end with this terminator.
	AvEOL AvID = 0x0000

	// AvNbComputerName (0x0001) contains the server's NetBIOS name.
	// Example: "FILESERVER"
	AvNbComputerName AvID = 0x0001

	// AvNbDomainName (0x0002) contains the NetBIOS domain name.
	// Example: "CORP" or "WORKGROUP" for standalone servers
	AvNbDomainName AvID = 0x0002
)

// AV_PAIR structure sizes
// Note: These constants are defined for documentation but not used in current implementation.
// They would be used if we implement full AV_PAIR parsing in the future.
const (
	_ = 4 // avPairHeaderSize: AvId (2 bytes) + AvLen (2 bytes)
	_ = 4 // avPairTerminatorLen: MsvAvEOL with AvLen=0 (just the header, no value)
)

// =============================================================================
// NTLM Message Detection
// =============================================================================

// IsValid checks if the buffer starts with the NTLMSSP signature.
// Returns false if the buffer is too short (< 12 bytes) or has wrong signature.
// [MS-NLMP] Section 2.2.1
func IsValid(buf []byte) bool {
	if len(buf) < headerSize {
		return false
	}
	return bytes.Equal(buf[signatureOffset:signatureOffset+8], Signature)
}

// GetMessageType returns the NTLM message type from a buffer.
// Returns 0 if the buffer is too short or doesn't have a valid NTLM signature.
// Valid return values are: Negotiate (1), Challenge (2), Authenticate (3)
// [MS-NLMP] Section 2.2.1
func GetMessageType(buf []byte) MessageType {
	if len(buf) < headerSize {
		return 0
	}
	return MessageType(binary.LittleEndian.Uint32(buf[messageTypeOffset : messageTypeOffset+4]))
}

// =============================================================================
// NTLM Message Building
// =============================================================================

// BuildChallenge creates an NTLM Type 2 (CHALLENGE) message for guest authentication.
//
// This function builds a minimal challenge message that allows any client to
// authenticate as a guest. No credential validation is performed.
//
// The returned message has the following structure:
//
//	Offset  Size  Field              Value/Description
//	------  ----  ----------------   ----------------------------------
//	0       8     Signature          "NTLMSSP\0"
//	8       4     MessageType        2 (CHALLENGE)
//	12      8     TargetNameFields   Empty target name (Len=0)
//	20      4     NegotiateFlags     Server capabilities
//	24      8     ServerChallenge    Random 8-byte challenge
//	32      8     Reserved           Zero
//	40      8     TargetInfoFields   Minimal AV_PAIR list
//	48      8     Version            Zero (not populated)
//	56      var   Payload            TargetInfo terminator
//
// [MS-NLMP] Section 2.2.1.2
func BuildChallenge() []byte {
	// Generate random 8-byte challenge
	// This challenge would normally be used to validate the client's response,
	// but for guest authentication we don't verify it.
	// TODO: Verify the challenge for production credential validation
	challenge := make([]byte, serverChallengeSize)
	_, _ = rand.Read(challenge)

	// Target name: empty for anonymous/guest authentication
	targetName := []byte{}

	// Flags for guest/anonymous support
	// These flags indicate our server capabilities to the client.
	// TODO: Implement encryption. We currently advertise Flag128 and
	// Flag56 but don't actually encrypt traffic. This requires
	// implementing session key derivation and RC4/AES encryption per [MS-NLMP].
	flags := FlagUnicode | // Support UTF-16LE strings
		FlagRequestTarget | // We can provide target info
		FlagNTLM | // Support NTLM authentication
		FlagAlwaysSign | // Include signature (even if dummy)
		FlagTargetTypeServer | // We are a server (not domain controller)
		FlagExtendedSecurity | // Support NTLMv2 session security
		FlagTargetInfo | // Include AV_PAIR list
		Flag128 | // Support 128-bit encryption
		Flag56 // Support 56-bit encryption (legacy)

	// Build minimal target info (just the terminator)
	targetInfo := BuildMinimalTargetInfo()

	// Calculate payload offsets
	// Payload starts immediately after the fixed fields (56 bytes)
	targetNameOffset := challengeBaseSize
	targetInfoOffset := targetNameOffset + len(targetName)

	// Allocate message buffer
	msg := make([]byte, targetInfoOffset+len(targetInfo))

	// Write fixed fields using named offsets for clarity

	// Signature: "NTLMSSP\0" at offset 0
	copy(msg[signatureOffset:signatureOffset+8], Signature)

	// MessageType: 2 (CHALLENGE) at offset 8
	binary.LittleEndian.PutUint32(
		msg[messageTypeOffset:messageTypeOffset+4],
		uint32(Challenge),
	)

	// TargetNameFields at offset 12
	binary.LittleEndian.PutUint16(
		msg[challengeTargetNameLenOffset:challengeTargetNameLenOffset+2],
		uint16(len(targetName)),
	)
	binary.LittleEndian.PutUint16(
		msg[challengeTargetNameMaxOffset:challengeTargetNameMaxOffset+2],
		uint16(len(targetName)),
	)
	binary.LittleEndian.PutUint32(
		msg[challengeTargetNameOffOffset:challengeTargetNameOffOffset+4],
		uint32(targetNameOffset),
	)

	// NegotiateFlags at offset 20
	binary.LittleEndian.PutUint32(
		msg[challengeFlagsOffset:challengeFlagsOffset+4],
		uint32(flags),
	)

	// ServerChallenge at offset 24
	copy(msg[challengeServerChalOffset:challengeServerChalOffset+8], challenge)

	// Reserved at offset 32: already zero (from make())

	// TargetInfoFields at offset 40
	binary.LittleEndian.PutUint16(
		msg[challengeTargetInfoLenOffset:challengeTargetInfoLenOffset+2],
		uint16(len(targetInfo)),
	)
	binary.LittleEndian.PutUint16(
		msg[challengeTargetInfoMaxOffset:challengeTargetInfoMaxOffset+2],
		uint16(len(targetInfo)),
	)
	binary.LittleEndian.PutUint32(
		msg[challengeTargetInfoOffOffset:challengeTargetInfoOffOffset+4],
		uint32(targetInfoOffset),
	)

	// Version at offset 48: left as zero (optional field)

	// Copy variable-length payload
	copy(msg[targetNameOffset:], targetName)
	copy(msg[targetInfoOffset:], targetInfo)

	return msg
}

// BuildMinimalTargetInfo creates a minimal AV_PAIR list with just the terminator.
//
// AV_PAIR structures are used in the TargetInfo field of Type 2 messages.
// Each AV_PAIR has the format:
//
//	Offset  Size  Field   Description
//	------  ----  ------  ----------------------------------
//	0       2     AvId    Attribute ID (see MsvAv* constants)
//	2       2     AvLen   Length of Value field
//	4       var   Value   Attribute value (AvLen bytes)
//
// The list is terminated by MsvAvEOL (AvId=0, AvLen=0).
//
// For guest authentication, we only include the terminator.
// Production implementations would include:
//   - AvNbDomainName: NetBIOS domain name
//   - AvNbComputerName: NetBIOS computer name
//   - MsvAvTimestamp: FILETIME timestamp (for NTLMv2)
//
// [MS-NLMP] Section 2.2.2.1
func BuildMinimalTargetInfo() []byte {
	// Minimal AV_PAIR list: just the terminator
	// MsvAvEOL (0x0000) with AvLen=0
	return []byte{
		0x00, 0x00, // AvId: AvEOL
		0x00, 0x00, // AvLen: 0
	}
}

// =============================================================================
// NTLM Authenticate Message Parsing
// =============================================================================

// AuthenticateMessage contains parsed fields from an NTLM Type 3 message.
//
// This structure holds the client's authentication response including:
//   - Username and domain for user lookup
//   - Challenge responses for credential validation (if implementing NTLMv2)
//   - Workstation name for logging/auditing
//
// [MS-NLMP] Section 2.2.1.3
type AuthenticateMessage struct {
	// LmChallengeResponse contains the LM response to the server challenge.
	// For NTLMv2, this is typically empty or contains LMv2 response.
	LmChallengeResponse []byte

	// NtChallengeResponse contains the NT response to the server challenge.
	// For NTLMv2, this includes the NTProofStr and client blob.
	NtChallengeResponse []byte

	// Domain is the authentication domain.
	// May be empty for local authentication.
	Domain string

	// Username is the account name.
	// This is the key for looking up the user in DittoFS UserStore.
	Username string

	// Workstation is the client workstation name.
	// Used for logging and auditing.
	Workstation string

	// NegotiateFlags contains the negotiated flags.
	NegotiateFlags NegotiateFlag

	// IsAnonymous indicates if this is an anonymous authentication request.
	// Set when FlagAnonymous is present in NegotiateFlags.
	IsAnonymous bool
}

// ParseAuthenticate parses an NTLM Type 3 (AUTHENTICATE) message.
//
// This function extracts the authentication fields from a Type 3 message:
//   - Username and domain for user lookup
//   - Challenge responses for potential credential validation
//   - Workstation name for logging
//
// Note: This implementation extracts the fields but does not validate
// the NTLMv2 responses. For full credential validation, the server would
// need to:
//  1. Store the ServerChallenge from the Type 2 message
//  2. Compute the expected NTProofStr using the user's NT hash
//  3. Compare with the client's NtChallengeResponse
//
// [MS-NLMP] Section 2.2.1.3
func ParseAuthenticate(buf []byte) (*AuthenticateMessage, error) {
	if len(buf) < authBaseSize {
		return nil, ErrMessageTooShort
	}

	if !IsValid(buf) {
		return nil, ErrInvalidSignature
	}

	if GetMessageType(buf) != Authenticate {
		return nil, ErrWrongMessageType
	}

	msg := &AuthenticateMessage{}

	// Parse NegotiateFlags
	msg.NegotiateFlags = NegotiateFlag(binary.LittleEndian.Uint32(buf[authNegotiateFlagsOffset : authNegotiateFlagsOffset+4]))
	msg.IsAnonymous = (msg.NegotiateFlags & FlagAnonymous) != 0

	// Parse LmChallengeResponse
	lmLen := binary.LittleEndian.Uint16(buf[authLmResponseLenOffset : authLmResponseLenOffset+2])
	lmOff := binary.LittleEndian.Uint32(buf[authLmResponseOffOffset : authLmResponseOffOffset+4])
	if lmLen > 0 && int(lmOff)+int(lmLen) <= len(buf) {
		msg.LmChallengeResponse = make([]byte, lmLen)
		copy(msg.LmChallengeResponse, buf[lmOff:lmOff+uint32(lmLen)])
	}

	// Parse NtChallengeResponse
	ntLen := binary.LittleEndian.Uint16(buf[authNtResponseLenOffset : authNtResponseLenOffset+2])
	ntOff := binary.LittleEndian.Uint32(buf[authNtResponseOffOffset : authNtResponseOffOffset+4])
	if ntLen > 0 && int(ntOff)+int(ntLen) <= len(buf) {
		msg.NtChallengeResponse = make([]byte, ntLen)
		copy(msg.NtChallengeResponse, buf[ntOff:ntOff+uint32(ntLen)])
	}

	// Determine if strings are Unicode (UTF-16LE) or OEM
	isUnicode := (msg.NegotiateFlags & FlagUnicode) != 0

	// Parse DomainName
	domainLen := binary.LittleEndian.Uint16(buf[authDomainNameLenOffset : authDomainNameLenOffset+2])
	domainOff := binary.LittleEndian.Uint32(buf[authDomainNameOffOffset : authDomainNameOffOffset+4])
	if domainLen > 0 && int(domainOff)+int(domainLen) <= len(buf) {
		msg.Domain = decodeString(buf[domainOff:domainOff+uint32(domainLen)], isUnicode)
	}

	// Parse UserName
	userLen := binary.LittleEndian.Uint16(buf[authUserNameLenOffset : authUserNameLenOffset+2])
	userOff := binary.LittleEndian.Uint32(buf[authUserNameOffOffset : authUserNameOffOffset+4])
	if userLen > 0 && int(userOff)+int(userLen) <= len(buf) {
		msg.Username = decodeString(buf[userOff:userOff+uint32(userLen)], isUnicode)
	}

	// Parse Workstation
	wsLen := binary.LittleEndian.Uint16(buf[authWorkstationLenOffset : authWorkstationLenOffset+2])
	wsOff := binary.LittleEndian.Uint32(buf[authWorkstationOffOffset : authWorkstationOffOffset+4])
	if wsLen > 0 && int(wsOff)+int(wsLen) <= len(buf) {
		msg.Workstation = decodeString(buf[wsOff:wsOff+uint32(wsLen)], isUnicode)
	}

	return msg, nil
}

// decodeString decodes a string from either UTF-16LE (Unicode) or OEM encoding.
func decodeString(buf []byte, isUnicode bool) string {
	if isUnicode {
		// UTF-16LE decoding
		if len(buf)%2 != 0 {
			buf = buf[:len(buf)-1] // Truncate odd byte
		}
		runes := make([]rune, len(buf)/2)
		for i := 0; i < len(buf); i += 2 {
			runes[i/2] = rune(binary.LittleEndian.Uint16(buf[i : i+2]))
		}
		return string(runes)
	}
	// OEM encoding - treat as ASCII/Latin-1
	return string(buf)
}

// =============================================================================
// NTLM Errors
// =============================================================================

// Error types for NTLM message parsing.
type Error string

func (e Error) Error() string { return string(e) }

const (
	// ErrMessageTooShort is returned when the buffer is too small for the message type.
	ErrMessageTooShort Error = "ntlm: message too short"

	// ErrInvalidSignature is returned when the NTLMSSP signature is missing or invalid.
	ErrInvalidSignature Error = "ntlm: invalid signature"

	// ErrWrongMessageType is returned when parsing a message of unexpected type.
	ErrWrongMessageType Error = "ntlm: wrong message type"
)
