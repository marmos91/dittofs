package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/jcmturner/gofork/encoding/asn1"
)

// TestSPNEGORoundTrip simulates the full SPNEGO round-trip as go-smb2 does:
// 1. Client sends NegTokenInit wrapping NTLM NEGOTIATE
// 2. Server sends NegTokenResp wrapping NTLM CHALLENGE
// 3. Client sends NegTokenResp wrapping NTLM AUTHENTICATE
// 4. Server unwraps and validates
func TestSPNEGORoundTrip(t *testing.T) {
	password := "TestPass123!"
	username := "testuser"

	// =====================================================
	// Step 1: Client sends NegTokenInit with NTLM Negotiate
	// =====================================================
	ntlmNegotiate := buildFakeNTLMNegotiate()
	clientInit, err := buildClientNegTokenInit(ntlmNegotiate)
	if err != nil {
		t.Fatalf("Failed to build client NegTokenInit: %v", err)
	}

	// Server receives and extracts NTLM token (simulating extractNTLMToken from handlers package)
	initToken := testExtractNTLMToken(clientInit)
	if !IsValid(initToken) || GetMessageType(initToken) != Negotiate {
		t.Fatalf("Server failed to extract NTLM Negotiate from SPNEGO")
	}
	t.Logf("Step 1: Server extracted NTLM Negotiate (len=%d)", len(initToken))

	// =====================================================
	// Step 2: Server sends Challenge wrapped in NegTokenResp
	// =====================================================
	challengeMsg, serverChallenge := BuildChallenge()
	t.Logf("Step 2: Server challenge: %x", serverChallenge)

	spnegoChallengeResp, err := BuildAcceptIncomplete(OIDNTLMSSP, challengeMsg)
	if err != nil {
		t.Fatalf("Failed to build SPNEGO challenge response: %v", err)
	}

	// =====================================================
	// Step 3: Client receives challenge, builds AUTHENTICATE
	// =====================================================
	// Client unwraps SPNEGO to get raw NTLM challenge
	clientParsed, err := Parse(spnegoChallengeResp)
	if err != nil {
		t.Fatalf("Client failed to parse SPNEGO challenge: %v", err)
	}
	clientChallengeMsg := clientParsed.MechToken
	t.Logf("Step 3a: Client extracted challenge (len=%d)", len(clientChallengeMsg))

	// Verify the extracted challenge matches the original
	if !bytes.Equal(clientChallengeMsg, challengeMsg) {
		t.Fatalf("Extracted challenge doesn't match original!")
	}

	// Client builds NTLM AUTHENTICATE (simulating go-smb2)
	ntlmAuth, clientDomain := buildFakeNTLMAuthenticate(t, username, password, clientChallengeMsg, serverChallenge)

	// Client wraps in NegTokenResp (as go-smb2 does)
	clientResp, err := buildClientNegTokenResp(ntlmAuth)
	if err != nil {
		t.Fatalf("Failed to build client NegTokenResp: %v", err)
	}
	t.Logf("Step 3b: Client built NegTokenResp (len=%d)", len(clientResp))
	t.Logf("Step 3b: First bytes of client response: %x", clientResp[:min(16, len(clientResp))])

	// =====================================================
	// Step 4: Server receives and validates
	// =====================================================
	// Server extracts NTLM AUTHENTICATE from SPNEGO
	extractedAuth := testExtractNTLMToken(clientResp)
	if !IsValid(extractedAuth) {
		t.Logf("Extracted bytes (first 32): %x", extractedAuth[:min(32, len(extractedAuth))])
		t.Fatalf("Server failed to extract valid NTLM AUTHENTICATE from SPNEGO")
	}
	authMsgType := GetMessageType(extractedAuth)
	if authMsgType != Authenticate {
		t.Fatalf("Expected Authenticate type (3), got %d", authMsgType)
	}
	t.Logf("Step 4a: Server extracted NTLM Authenticate (len=%d)", len(extractedAuth))

	// Parse AUTHENTICATE message
	authMsg, err := ParseAuthenticate(extractedAuth)
	if err != nil {
		t.Fatalf("Failed to parse NTLM AUTHENTICATE: %v", err)
	}
	t.Logf("Step 4b: Parsed AUTHENTICATE: username=%q domain=%q ntResponseLen=%d",
		authMsg.Username, authMsg.Domain, len(authMsg.NtChallengeResponse))

	// Validate NTLMv2 response
	ntHash := ComputeNTHash(password)

	// Try the exact same domain logic as completeNTLMAuth
	hostname := decodeUTF16LEBytesTest(extractTargetNameFromChallenge(challengeMsg))
	domainsToTry := []string{
		authMsg.Domain,
		"",
		strings.ToUpper(hostname),
		"WORKGROUP",
	}

	var validated bool
	for _, d := range domainsToTry {
		_, err := ValidateNTLMv2Response(ntHash, authMsg.Username, d, serverChallenge, authMsg.NtChallengeResponse)
		if err == nil {
			t.Logf("Step 4c: Validation PASSED with domain=%q", d)
			validated = true
			break
		}
		t.Logf("Step 4c: Validation failed with domain=%q: %v", d, err)
	}

	if !validated {
		// Additional debug: what domain did the client use?
		t.Logf("DEBUG: Client used domain=%q (go string)", clientDomain)
		t.Logf("DEBUG: Hostname=%q", hostname)

		// Try the client's domain directly
		_, err := ValidateNTLMv2Response(ntHash, authMsg.Username, clientDomain, serverChallenge, authMsg.NtChallengeResponse)
		if err == nil {
			t.Logf("DEBUG: Validation PASSED with exact client domain=%q", clientDomain)
			t.Fatal("Server domain fallback list is missing the client's domain!")
		}

		t.Fatal("NTLMv2 validation FAILED with all domain variants")
	}
}

// buildFakeNTLMNegotiate builds a minimal NTLM Negotiate message
func buildFakeNTLMNegotiate() []byte {
	msg := make([]byte, 40)
	copy(msg[:8], Signature)
	binary.LittleEndian.PutUint32(msg[8:12], uint32(Negotiate))
	// Flags
	flags := FlagUnicode | FlagRequestTarget | FlagNTLM | FlagSign |
		FlagAlwaysSign | FlagExtendedSecurity | FlagTargetInfo |
		FlagKeyExch | Flag128 | Flag56
	binary.LittleEndian.PutUint32(msg[12:16], uint32(flags))
	return msg
}

// buildFakeNTLMAuthenticate builds an NTLM AUTHENTICATE message mimicking go-smb2.
// Returns the NTLM bytes and the domain string used for NTLMv2 hash computation.
func buildFakeNTLMAuthenticate(t *testing.T, username, password string, challengeMsg []byte, serverChallenge [8]byte) ([]byte, string) {
	t.Helper()

	// Extract TargetName and TargetInfo from challenge (as go-smb2 does)
	targetNameLen := binary.LittleEndian.Uint16(challengeMsg[12:14])
	targetNameOff := binary.LittleEndian.Uint32(challengeMsg[16:20])
	targetName := challengeMsg[targetNameOff : targetNameOff+uint32(targetNameLen)]

	targetInfoLen := binary.LittleEndian.Uint16(challengeMsg[40:42])
	targetInfoOff := binary.LittleEndian.Uint32(challengeMsg[44:48])
	targetInfo := challengeMsg[targetInfoOff : targetInfoOff+uint32(targetInfoLen)]

	// go-smb2: domain = utf16le.EncodeStringToBytes(c.Domain)
	// When empty, domain = targetName
	domain := targetName // empty client domain => use server's TargetName

	domainStr := decodeUTF16LEBytesTest(domain)

	// Compute NTLMv2 hash (go-smb2 style)
	ntHash := ComputeNTHash(password)
	USER := encodeUTF16LEForTest(strings.ToUpper(username))
	h := hmac.New(md5.New, ntHash[:])
	h.Write(USER)
	h.Write(domain)
	ntlmv2Hash := h.Sum(nil)

	// Build client blob
	clientBlob := buildGoSMB2ClientBlob(targetInfo)

	// Build NTProofStr
	h = hmac.New(md5.New, ntlmv2Hash)
	h.Write(serverChallenge[:])
	h.Write(clientBlob)
	ntProofStr := h.Sum(nil)

	// Build NtChallengeResponse
	ntResponse := make([]byte, 16+len(clientBlob))
	copy(ntResponse[:16], ntProofStr)
	copy(ntResponse[16:], clientBlob)

	// Build LM response (24 bytes of zeros for NTLMv2)
	lmResponse := make([]byte, 24)

	// Build AUTHENTICATE message
	domainBytes := domain
	usernameBytes := encodeUTF16LEForTest(username)
	workstationBytes := encodeUTF16LEForTest("")

	// Fixed fields: 88 bytes (64 base + 8 version + 16 MIC)
	off := 88
	msgLen := off + len(domainBytes) + len(usernameBytes) + len(workstationBytes) + len(lmResponse) + len(ntResponse) + 16

	msg := make([]byte, msgLen)
	copy(msg[:8], Signature)
	binary.LittleEndian.PutUint32(msg[8:12], uint32(Authenticate))

	// LM response
	lmOff := off
	binary.LittleEndian.PutUint16(msg[12:14], uint16(len(lmResponse)))
	binary.LittleEndian.PutUint16(msg[14:16], uint16(len(lmResponse)))
	binary.LittleEndian.PutUint32(msg[16:20], uint32(lmOff))
	copy(msg[lmOff:], lmResponse)
	off = lmOff + len(lmResponse)

	// NT response
	ntOff := off
	binary.LittleEndian.PutUint16(msg[20:22], uint16(len(ntResponse)))
	binary.LittleEndian.PutUint16(msg[22:24], uint16(len(ntResponse)))
	binary.LittleEndian.PutUint32(msg[24:28], uint32(ntOff))
	copy(msg[ntOff:], ntResponse)
	off = ntOff + len(ntResponse)

	// Domain
	domainOff := off
	binary.LittleEndian.PutUint16(msg[28:30], uint16(len(domainBytes)))
	binary.LittleEndian.PutUint16(msg[30:32], uint16(len(domainBytes)))
	binary.LittleEndian.PutUint32(msg[32:36], uint32(domainOff))
	copy(msg[domainOff:], domainBytes)
	off = domainOff + len(domainBytes)

	// Username
	usernameOff := off
	binary.LittleEndian.PutUint16(msg[36:38], uint16(len(usernameBytes)))
	binary.LittleEndian.PutUint16(msg[38:40], uint16(len(usernameBytes)))
	binary.LittleEndian.PutUint32(msg[40:44], uint32(usernameOff))
	copy(msg[usernameOff:], usernameBytes)
	off = usernameOff + len(usernameBytes)

	// Workstation (empty)
	binary.LittleEndian.PutUint16(msg[44:46], 0)
	binary.LittleEndian.PutUint16(msg[46:48], 0)
	binary.LittleEndian.PutUint32(msg[48:52], uint32(off))

	// EncryptedRandomSessionKey
	encKeyOff := off
	// Generate random session key and encrypt with RC4(sessionBaseKey)
	sessionBaseKey := computeSessionBaseKey(ntlmv2Hash, ntProofStr)
	exportedSessionKey := make([]byte, 16)
	rand.Read(exportedSessionKey)
	encryptedKey := rc4EncryptBytes(sessionBaseKey, exportedSessionKey)

	binary.LittleEndian.PutUint16(msg[52:54], 16)
	binary.LittleEndian.PutUint16(msg[54:56], 16)
	binary.LittleEndian.PutUint32(msg[56:60], uint32(encKeyOff))
	copy(msg[encKeyOff:], encryptedKey)

	// Flags
	flags := FlagUnicode | FlagRequestTarget | FlagNTLM | FlagSign |
		FlagAlwaysSign | FlagExtendedSecurity | FlagTargetInfo |
		FlagKeyExch | Flag128 | Flag56
	binary.LittleEndian.PutUint32(msg[60:64], uint32(flags))

	return msg, domainStr
}

func computeSessionBaseKey(ntlmv2Hash, ntProofStr []byte) []byte {
	h := hmac.New(md5.New, ntlmv2Hash)
	h.Write(ntProofStr)
	return h.Sum(nil)
}

func rc4EncryptBytes(key, data []byte) []byte {
	// Simple RC4 implementation
	s := make([]byte, 256)
	for i := range s {
		s[i] = byte(i)
	}
	j := 0
	for i := 0; i < 256; i++ {
		j = (j + int(s[i]) + int(key[i%len(key)])) % 256
		s[i], s[j] = s[j], s[i]
	}
	result := make([]byte, len(data))
	ii, jj := 0, 0
	for k := 0; k < len(data); k++ {
		ii = (ii + 1) % 256
		jj = (jj + int(s[ii])) % 256
		s[ii], s[jj] = s[jj], s[ii]
		result[k] = data[k] ^ s[(int(s[ii])+int(s[jj]))%256]
	}
	return result
}

func encodeUTF16LEForTest(s string) []byte {
	if s == "" {
		return nil
	}
	encoded := utf16.Encode([]rune(s))
	b := make([]byte, len(encoded)*2)
	for i, v := range encoded {
		binary.LittleEndian.PutUint16(b[i*2:], v)
	}
	return b
}

func decodeUTF16LEBytesTest(b []byte) string {
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}
	runes := make([]rune, len(b)/2)
	for i := 0; i < len(b); i += 2 {
		runes[i/2] = rune(binary.LittleEndian.Uint16(b[i : i+2]))
	}
	return string(runes)
}

func extractTargetNameFromChallenge(challengeMsg []byte) []byte {
	targetNameLen := binary.LittleEndian.Uint16(challengeMsg[12:14])
	targetNameOff := binary.LittleEndian.Uint32(challengeMsg[16:20])
	return challengeMsg[targetNameOff : targetNameOff+uint32(targetNameLen)]
}

// testExtractNTLMToken simulates the handlers.extractNTLMToken function.
// Attempts SPNEGO parsing, falls back to finding NTLMSSP signature.
func testExtractNTLMToken(securityBuffer []byte) []byte {
	if len(securityBuffer) == 0 {
		return securityBuffer
	}

	if len(securityBuffer) >= 2 && (securityBuffer[0] == 0x60 || securityBuffer[0] == 0xa0 || securityBuffer[0] == 0xa1) {
		parsed, err := Parse(securityBuffer)
		if err != nil {
			// Fallback: scan for NTLMSSP signature
			sig := []byte{'N', 'T', 'L', 'M', 'S', 'S', 'P', 0}
			if i := bytes.Index(securityBuffer, sig); i >= 0 {
				return securityBuffer[i:]
			}
			return securityBuffer
		}
		if len(parsed.MechToken) > 0 {
			return parsed.MechToken
		}
	}

	return securityBuffer
}

// buildClientNegTokenInit wraps an NTLM message in SPNEGO NegTokenInit (like go-smb2)
func buildClientNegTokenInit(ntlmMsg []byte) ([]byte, error) {
	type negTokenInit struct {
		MechTypes []asn1.ObjectIdentifier `asn1:"explicit,optional,tag:0"`
		MechToken []byte                  `asn1:"explicit,optional,tag:2"`
	}
	type initialContextToken struct {
		ThisMech asn1.ObjectIdentifier
		Init     []negTokenInit `asn1:"optional,explicit,tag:0"`
	}

	bs, err := asn1.Marshal(initialContextToken{
		ThisMech: asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 2},
		Init: []negTokenInit{{
			MechTypes: []asn1.ObjectIdentifier{{1, 3, 6, 1, 4, 1, 311, 2, 2, 10}},
			MechToken: ntlmMsg,
		}},
	})
	if err != nil {
		return nil, err
	}
	bs[0] = 0x60 // APPLICATION 0 tag
	return bs, nil
}

// buildClientNegTokenResp wraps an NTLM AUTHENTICATE message in SPNEGO NegTokenResp (like go-smb2)
func buildClientNegTokenResp(ntlmMsg []byte) ([]byte, error) {
	type negTokenResp struct {
		NegState      asn1.Enumerated `asn1:"optional,explicit,tag:0"`
		ResponseToken []byte          `asn1:"optional,explicit,tag:2"`
	}
	type wrapper struct {
		Resp []negTokenResp `asn1:"optional,explicit,tag:1"`
	}

	bs, err := asn1.Marshal(wrapper{
		Resp: []negTokenResp{{
			NegState:      1, // accept-incomplete
			ResponseToken: ntlmMsg,
		}},
	})
	if err != nil {
		return nil, err
	}

	// Strip outer SEQUENCE tag+length (as go-smb2 does)
	skip := 1
	if bs[skip] < 128 {
		skip += 1
	} else {
		skip += int(bs[skip]) - 128 + 1
	}

	return bs[skip:], nil
}
