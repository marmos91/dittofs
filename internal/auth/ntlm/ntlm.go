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
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // MD5 is required for NTLM protocol compatibility (HMAC-MD5 in NTLMv2)
	"crypto/rand"
	"crypto/rc4" //nolint:gosec // RC4 required for NTLM KEY_EXCH; only encrypts session key, not message data
	"encoding/binary"
	"os"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
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

	// FlagKeyExch (bit V) indicates key exchange support.
	// When set, the client generates a random session key (ExportedSessionKey)
	// and encrypts it with RC4 using the SessionBaseKey. The encrypted key
	// is sent in the EncryptedRandomSessionKey field of the AUTHENTICATE message.
	// The ExportedSessionKey becomes the SigningKey instead of SessionBaseKey.
	FlagKeyExch NegotiateFlag = 0x40000000

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

	// AvDnsComputerName (0x0003) contains the server's DNS hostname.
	// Example: "fileserver.corp.com"
	AvDnsComputerName AvID = 0x0003

	// AvDnsDomainName (0x0004) contains the DNS domain name.
	// Example: "corp.com"
	AvDnsDomainName AvID = 0x0004

	// AvTimestamp (0x0007) contains the server's FILETIME timestamp.
	// Used by NTLMv2 for replay protection.
	AvTimestamp AvID = 0x0007
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

// BuildChallenge creates an NTLM Type 2 (CHALLENGE) message.
//
// Returns the challenge message and the 8-byte server challenge that was
// embedded in the message. The server challenge must be stored to validate
// the client's NTLMv2 response and derive the session key.
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
func BuildChallenge() (message []byte, serverChallenge [8]byte) {
	// Generate random 8-byte challenge
	// This challenge is used to validate the client's NTLMv2 response
	// and derive the session key for message signing.
	challenge := make([]byte, serverChallengeSize)
	_, _ = rand.Read(challenge)
	copy(serverChallenge[:], challenge)

	// Target name: server hostname for client identification.
	// Windows clients require a non-empty TargetName when FlagRequestTarget is set.
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "DITTOFS"
	}
	targetName := encodeUTF16LE(strings.ToUpper(hostname))

	// Flags for server capabilities.
	// TODO: Implement encryption. We currently advertise Flag128 and
	// Flag56 but don't actually encrypt traffic. This requires
	// implementing session key derivation and RC4/AES encryption per [MS-NLMP].
	flags := FlagUnicode | // Support UTF-16LE strings
		FlagRequestTarget | // We can provide target info
		FlagNTLM | // Support NTLM authentication
		FlagSign | // Support message integrity (signing)
		FlagAlwaysSign | // Include signature (even if dummy)
		FlagTargetTypeServer | // We are a server (not domain controller)
		FlagExtendedSecurity | // Support NTLMv2 session security
		FlagTargetInfo | // Include AV_PAIR list
		FlagKeyExch | // Support session key exchange (required for signing)
		Flag128 | // Support 128-bit encryption
		Flag56 // Support 56-bit encryption (legacy)

	// Build target info with required AV_PAIRs for Windows compatibility.
	// Windows clients need at minimum: NbDomainName, NbComputerName,
	// DnsComputerName, and Timestamp (for NTLMv2 replay protection).
	targetInfo := buildTargetInfo(hostname)

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

	return msg, serverChallenge
}

// BuildMinimalTargetInfo creates a minimal AV_PAIR list with just the terminator.
// This is kept for backward compatibility and testing.
//
// [MS-NLMP] Section 2.2.2.1
func BuildMinimalTargetInfo() []byte {
	return []byte{
		0x00, 0x00, // AvId: AvEOL
		0x00, 0x00, // AvLen: 0
	}
}

// buildTargetInfo creates a complete AV_PAIR list required by Windows clients.
//
// Windows NTLM clients require the following AV_PAIRs in the TargetInfo:
//   - MsvAvNbDomainName: NetBIOS domain name (WORKGROUP for standalone)
//   - MsvAvNbComputerName: NetBIOS server name
//   - MsvAvDnsComputerName: DNS hostname
//   - MsvAvDnsDomainName: DNS domain name
//   - MsvAvTimestamp: FILETIME timestamp (critical for NTLMv2)
//   - MsvAvEOL: terminator
//
// Without these fields, Windows clients reject the NTLM challenge and
// disconnect immediately after receiving the Type 2 message.
//
// [MS-NLMP] Section 2.2.2.1
func buildTargetInfo(hostname string) []byte {
	domain := "WORKGROUP"
	nbName := strings.ToUpper(hostname)
	dnsName := strings.ToLower(hostname)

	// Encode all values as UTF-16LE
	nbDomainBytes := encodeUTF16LE(domain)
	nbComputerBytes := encodeUTF16LE(nbName)
	dnsComputerBytes := encodeUTF16LE(dnsName)
	dnsDomainBytes := encodeUTF16LE("local")

	// Timestamp: Windows FILETIME (100-nanosecond intervals since 1601-01-01)
	// Go epoch is 1970-01-01, offset is 116444736000000000 (100ns intervals)
	const epochDiff = 116444736000000000
	ft := uint64(time.Now().UnixNano()/100) + epochDiff
	timestampBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(timestampBytes, ft)

	// Build AV_PAIR list
	var buf []byte
	buf = append(buf, buildAvPair(AvNbDomainName, nbDomainBytes)...)
	buf = append(buf, buildAvPair(AvNbComputerName, nbComputerBytes)...)
	buf = append(buf, buildAvPair(AvDnsComputerName, dnsComputerBytes)...)
	buf = append(buf, buildAvPair(AvDnsDomainName, dnsDomainBytes)...)
	buf = append(buf, buildAvPair(AvTimestamp, timestampBytes)...)
	// Terminator
	buf = append(buf, 0x00, 0x00, 0x00, 0x00)

	return buf
}

// buildAvPair creates a single AV_PAIR structure.
func buildAvPair(id AvID, value []byte) []byte {
	pair := make([]byte, 4+len(value))
	binary.LittleEndian.PutUint16(pair[0:2], uint16(id))
	binary.LittleEndian.PutUint16(pair[2:4], uint16(len(value)))
	copy(pair[4:], value)
	return pair
}

// encodeUTF16LE encodes a string as UTF-16LE bytes.
func encodeUTF16LE(s string) []byte {
	encoded := utf16.Encode([]rune(s))
	b := make([]byte, len(encoded)*2)
	for i, v := range encoded {
		binary.LittleEndian.PutUint16(b[i*2:], v)
	}
	return b
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

	// EncryptedRandomSessionKey contains the encrypted session key when KEY_EXCH is negotiated.
	// If KEY_EXCH flag is set, this is decrypted with RC4 using SessionBaseKey
	// to obtain the ExportedSessionKey, which is then used for signing.
	EncryptedRandomSessionKey []byte

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

	// Parse EncryptedRandomSessionKey (used when KEY_EXCH flag is set)
	keyLen := binary.LittleEndian.Uint16(buf[authEncryptedRandomSessionKeyLen : authEncryptedRandomSessionKeyLen+2])
	keyOff := binary.LittleEndian.Uint32(buf[authEncryptedRandomSessionKeyOff : authEncryptedRandomSessionKeyOff+4])
	if keyLen > 0 && int(keyOff)+int(keyLen) <= len(buf) {
		msg.EncryptedRandomSessionKey = make([]byte, keyLen)
		copy(msg.EncryptedRandomSessionKey, buf[keyOff:keyOff+uint32(keyLen)])
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

	// ErrAuthenticationFailed is returned when NTLMv2 validation fails.
	ErrAuthenticationFailed Error = "ntlm: authentication failed"

	// ErrResponseTooShort is returned when NT response is too short.
	ErrResponseTooShort Error = "ntlm: response too short"
)

// =============================================================================
// NTLMv2 Authentication
// =============================================================================

// ComputeNTHash computes the NT hash from a password.
//
// The NT hash is computed as: MD4(UTF16LE(password))
//
// This is the fundamental credential used in NTLM authentication.
// The NT hash should be stored securely (it's equivalent to a password).
//
// [MS-NLMP] Section 3.3.1
//
// This function delegates to models.ComputeNTHash to avoid code duplication.
func ComputeNTHash(password string) [16]byte {
	return models.ComputeNTHash(password)
}

// ComputeNTLMv2Hash computes the NTLMv2 response key.
//
// The NTLMv2 hash is computed as:
//
//	HMAC-MD5(NT_Hash, UPPERCASE(username) + domain)
//
// where username and domain are encoded as UTF-16LE.
//
// [MS-NLMP] Section 3.3.2
func ComputeNTLMv2Hash(ntHash [16]byte, username, domain string) [16]byte {
	// Uppercase username, keep domain as-is
	userUpper := strings.ToUpper(username)

	// Convert to UTF-16LE
	combined := userUpper + domain
	utf16Combined := utf16.Encode([]rune(combined))
	combinedBytes := make([]byte, len(utf16Combined)*2)
	for i, r := range utf16Combined {
		binary.LittleEndian.PutUint16(combinedBytes[i*2:], r)
	}

	// Compute HMAC-MD5
	mac := hmac.New(md5.New, ntHash[:])
	mac.Write(combinedBytes)

	var ntlmv2Hash [16]byte
	copy(ntlmv2Hash[:], mac.Sum(nil))
	return ntlmv2Hash
}

// ValidateNTLMv2Response validates the client's NTLMv2 response and returns the session key.
//
// The NTLMv2 response structure is:
//   - NTProofStr (16 bytes): HMAC-MD5(NTLMv2Hash, ServerChallenge + ClientBlob)
//   - ClientBlob (variable): Contains timestamp, nonce, and target info
//
// Validation process:
//  1. Extract NTProofStr (first 16 bytes) and ClientBlob (rest) from response
//  2. Compute expected NTProofStr = HMAC-MD5(NTLMv2Hash, ServerChallenge + ClientBlob)
//  3. Compare with provided NTProofStr
//  4. If match, compute session key = HMAC-MD5(NTLMv2Hash, NTProofStr)
//
// Returns the 16-byte session key on success, or error on failure.
//
// [MS-NLMP] Section 3.3.2
func ValidateNTLMv2Response(
	ntHash [16]byte,
	username, domain string,
	serverChallenge [8]byte,
	ntResponse []byte,
) ([16]byte, error) {
	var sessionKey [16]byte

	// NTLMv2 response must be at least 16 bytes (NTProofStr) + some client blob
	if len(ntResponse) < 24 {
		return sessionKey, ErrResponseTooShort
	}

	// Extract NTProofStr (first 16 bytes) and ClientBlob (rest)
	ntProofStr := ntResponse[:16]
	clientBlob := ntResponse[16:]

	// Compute NTLMv2 hash
	ntlmv2Hash := ComputeNTLMv2Hash(ntHash, username, domain)

	// Compute expected NTProofStr
	// NTProofStr = HMAC-MD5(NTLMv2Hash, ServerChallenge + ClientBlob)
	mac := hmac.New(md5.New, ntlmv2Hash[:])
	mac.Write(serverChallenge[:])
	mac.Write(clientBlob)
	expectedNTProofStr := mac.Sum(nil)

	// Compare NTProofStr (constant time comparison for security)
	if !hmac.Equal(ntProofStr, expectedNTProofStr) {
		return sessionKey, ErrAuthenticationFailed
	}

	// Compute session key
	// SessionKey = HMAC-MD5(NTLMv2Hash, NTProofStr)
	mac = hmac.New(md5.New, ntlmv2Hash[:])
	mac.Write(ntProofStr)
	copy(sessionKey[:], mac.Sum(nil))

	return sessionKey, nil
}

// DeriveSigningKey derives the final signing key from the session base key.
//
// When the NTLMSSP_NEGOTIATE_KEY_EXCH (0x40000000) flag is negotiated:
//   - The client generates a random 16-byte ExportedSessionKey
//   - The client encrypts it with RC4 using SessionBaseKey
//   - The encrypted key is sent in EncryptedRandomSessionKey
//   - Server decrypts to obtain ExportedSessionKey
//   - ExportedSessionKey is used for message signing
//
// When KEY_EXCH is NOT negotiated:
//   - SessionBaseKey is used directly for signing
//
// This function handles both cases transparently.
//
// Parameters:
//   - sessionBaseKey: The session key from ValidateNTLMv2Response
//   - flags: NegotiateFlags from the AUTHENTICATE message
//   - encryptedKey: EncryptedRandomSessionKey from AUTHENTICATE message (may be nil)
//
// Returns the signing key to use for message signing.
func DeriveSigningKey(sessionBaseKey [16]byte, flags NegotiateFlag, encryptedKey []byte) [16]byte {
	// If KEY_EXCH is not negotiated, use SessionBaseKey directly
	if (flags & FlagKeyExch) == 0 {
		return sessionBaseKey
	}

	// KEY_EXCH is negotiated - decrypt the ExportedSessionKey
	if len(encryptedKey) != 16 {
		// Invalid encrypted key length, fall back to session base key
		// This shouldn't happen with a well-formed AUTHENTICATE message
		return sessionBaseKey
	}

	// Decrypt EncryptedRandomSessionKey using RC4 with SessionBaseKey
	// RC4 is symmetric, so encryption and decryption use the same operation
	cipher, err := rc4.NewCipher(sessionBaseKey[:])
	if err != nil {
		// RC4 cipher creation failed (shouldn't happen with 16-byte key)
		return sessionBaseKey
	}

	var exportedSessionKey [16]byte
	cipher.XORKeyStream(exportedSessionKey[:], encryptedKey)

	return exportedSessionKey
}
