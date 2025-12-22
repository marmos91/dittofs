package ntlm

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"encoding/binary"
	"testing"
)

// =============================================================================
// Signature Tests
// =============================================================================

func TestSignature(t *testing.T) {
	expected := []byte{'N', 'T', 'L', 'M', 'S', 'S', 'P', 0}
	if !bytes.Equal(Signature, expected) {
		t.Errorf("Signature = %v, expected %v", Signature, expected)
	}
}

// =============================================================================
// IsValid Tests
// =============================================================================

func TestIsValid(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		expected bool
	}{
		{
			name:     "ValidNegotiateMessage",
			input:    buildTestMessage(Negotiate),
			expected: true,
		},
		{
			name:     "ValidChallengeMessage",
			input:    buildTestMessage(Challenge),
			expected: true,
		},
		{
			name:     "ValidAuthenticateMessage",
			input:    buildTestMessage(Authenticate),
			expected: true,
		},
		{
			name:     "TooShort",
			input:    []byte{'N', 'T', 'L', 'M'},
			expected: false,
		},
		{
			name:     "WrongSignature",
			input:    []byte{'X', 'X', 'X', 'X', 'X', 'X', 'X', 0, 1, 0, 0, 0},
			expected: false,
		},
		{
			name:     "Empty",
			input:    []byte{},
			expected: false,
		},
		{
			name:     "Nil",
			input:    nil,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsValid(tt.input)
			if result != tt.expected {
				t.Errorf("IsValid(%v) = %v, expected %v", tt.input, result, tt.expected)
			}
		})
	}
}

// =============================================================================
// GetMessageType Tests
// =============================================================================

func TestGetMessageType(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		expected MessageType
	}{
		{
			name:     "NegotiateMessage",
			input:    buildTestMessage(Negotiate),
			expected: Negotiate,
		},
		{
			name:     "ChallengeMessage",
			input:    buildTestMessage(Challenge),
			expected: Challenge,
		},
		{
			name:     "AuthenticateMessage",
			input:    buildTestMessage(Authenticate),
			expected: Authenticate,
		},
		{
			name:     "TooShort",
			input:    []byte{'N', 'T', 'L', 'M', 'S', 'S', 'P', 0},
			expected: 0,
		},
		{
			name:     "Empty",
			input:    []byte{},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetMessageType(tt.input)
			if result != tt.expected {
				t.Errorf("GetMessageType() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

// =============================================================================
// BuildChallenge Tests
// =============================================================================

func TestBuildChallenge(t *testing.T) {
	msg, serverChallenge := BuildChallenge()

	t.Run("HasCorrectSignature", func(t *testing.T) {
		if !bytes.Equal(msg[0:8], Signature) {
			t.Error("Challenge message should start with NTLMSSP signature")
		}
	})

	t.Run("HasCorrectMessageType", func(t *testing.T) {
		msgType := GetMessageType(msg)
		if msgType != Challenge {
			t.Errorf("Message type = %d, expected %d (Challenge)", msgType, Challenge)
		}
	})

	t.Run("HasMinimumSize", func(t *testing.T) {
		// Minimum challenge message is 56 bytes base + payload
		if len(msg) < 56 {
			t.Errorf("Challenge message too short: %d bytes", len(msg))
		}
	})

	t.Run("HasServerChallenge", func(t *testing.T) {
		// Server challenge is at offset 24, 8 bytes
		challenge := msg[24:32]

		// Challenge should not be all zeros (random)
		allZero := true
		for _, b := range challenge {
			if b != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			t.Error("Server challenge should be random, not all zeros")
		}
	})

	t.Run("ReturnsMatchingServerChallenge", func(t *testing.T) {
		// The returned serverChallenge should match what's in the message
		challengeInMsg := msg[24:32]
		if !bytes.Equal(challengeInMsg, serverChallenge[:]) {
			t.Error("Returned serverChallenge should match challenge in message")
		}
	})

	t.Run("GeneratesUniqueChallenge", func(t *testing.T) {
		msg2, serverChallenge2 := BuildChallenge()
		challenge1 := msg[24:32]
		challenge2 := msg2[24:32]

		if bytes.Equal(challenge1, challenge2) {
			t.Error("Two challenges should be different (random)")
		}
		if bytes.Equal(serverChallenge[:], serverChallenge2[:]) {
			t.Error("Two returned server challenges should be different (random)")
		}
	})

	t.Run("HasExpectedFlags", func(t *testing.T) {
		flags := binary.LittleEndian.Uint32(msg[20:24])

		expectedFlags := []struct {
			flag NegotiateFlag
			name string
		}{
			{FlagUnicode, "Unicode"},
			{FlagRequestTarget, "RequestTarget"},
			{FlagNTLM, "NTLM"},
			{FlagAlwaysSign, "AlwaysSign"},
			{FlagTargetTypeServer, "TargetTypeServer"},
			{FlagExtendedSecurity, "ExtendedSecurity"},
			{FlagTargetInfo, "TargetInfo"},
			{Flag128, "128-bit"},
			{Flag56, "56-bit"},
		}

		for _, ef := range expectedFlags {
			if flags&uint32(ef.flag) == 0 {
				t.Errorf("Expected flag %s (0x%x) to be set", ef.name, ef.flag)
			}
		}
	})
}

// =============================================================================
// BuildMinimalTargetInfo Tests
// =============================================================================

func TestBuildMinimalTargetInfo(t *testing.T) {
	info := BuildMinimalTargetInfo()

	t.Run("HasCorrectLength", func(t *testing.T) {
		// Minimal target info is just the EOL terminator (4 bytes)
		if len(info) != 4 {
			t.Errorf("TargetInfo length = %d, expected 4", len(info))
		}
	})

	t.Run("EndsWithEOL", func(t *testing.T) {
		// AvId should be 0 (EOL)
		avId := binary.LittleEndian.Uint16(info[0:2])
		if AvID(avId) != AvEOL {
			t.Errorf("AvId = %d, expected %d (AvEOL)", avId, AvEOL)
		}

		// AvLen should be 0
		avLen := binary.LittleEndian.Uint16(info[2:4])
		if avLen != 0 {
			t.Errorf("AvLen = %d, expected 0", avLen)
		}
	})
}

// =============================================================================
// MessageType Tests
// =============================================================================

func TestMessageTypeConstants(t *testing.T) {
	if Negotiate != 1 {
		t.Errorf("Negotiate = %d, expected 1", Negotiate)
	}
	if Challenge != 2 {
		t.Errorf("Challenge = %d, expected 2", Challenge)
	}
	if Authenticate != 3 {
		t.Errorf("Authenticate = %d, expected 3", Authenticate)
	}
}

// =============================================================================
// NegotiateFlag Tests
// =============================================================================

func TestNegotiateFlagConstants(t *testing.T) {
	tests := []struct {
		name     string
		flag     NegotiateFlag
		expected uint32
	}{
		{"FlagUnicode", FlagUnicode, 0x00000001},
		{"FlagOEM", FlagOEM, 0x00000002},
		{"FlagRequestTarget", FlagRequestTarget, 0x00000004},
		{"FlagSign", FlagSign, 0x00000010},
		{"FlagSeal", FlagSeal, 0x00000020},
		{"FlagNTLM", FlagNTLM, 0x00000200},
		{"FlagAnonymous", FlagAnonymous, 0x00000800},
		{"FlagExtendedSecurity", FlagExtendedSecurity, 0x00080000},
		{"FlagTargetInfo", FlagTargetInfo, 0x00800000},
		{"Flag128", Flag128, 0x20000000},
		{"Flag56", Flag56, 0x80000000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if uint32(tt.flag) != tt.expected {
				t.Errorf("%s = 0x%x, expected 0x%x", tt.name, tt.flag, tt.expected)
			}
		})
	}
}

// =============================================================================
// AvID Tests
// =============================================================================

func TestAvIDConstants(t *testing.T) {
	if AvEOL != 0x0000 {
		t.Errorf("AvEOL = 0x%x, expected 0x0000", AvEOL)
	}
	if AvNbComputerName != 0x0001 {
		t.Errorf("AvNbComputerName = 0x%x, expected 0x0001", AvNbComputerName)
	}
	if AvNbDomainName != 0x0002 {
		t.Errorf("AvNbDomainName = 0x%x, expected 0x0002", AvNbDomainName)
	}
}

// =============================================================================
// NTLMv2 Authentication Tests
// =============================================================================

func TestComputeNTHash(t *testing.T) {
	// Test that the NT hash implementation produces consistent results
	// and matches known reference values.
	// NT Hash = MD4(UTF16LE(password))

	t.Run("EmptyPassword", func(t *testing.T) {
		// Empty password produces the well-known "empty NT hash"
		ntHash := ComputeNTHash("")
		expected := "31d6cfe0d16ae931b73c59d7e0c089c0"
		result := bytesToHex(ntHash[:])
		if result != expected {
			t.Errorf("ComputeNTHash(\"\") = %s, expected %s", result, expected)
		}
	})

	t.Run("ConsistentResults", func(t *testing.T) {
		// Same password should produce same hash
		hash1 := ComputeNTHash("testpassword")
		hash2 := ComputeNTHash("testpassword")
		if !bytes.Equal(hash1[:], hash2[:]) {
			t.Error("Same password should produce same NT hash")
		}
	})

	t.Run("DifferentPasswordsDifferentHashes", func(t *testing.T) {
		hash1 := ComputeNTHash("password1")
		hash2 := ComputeNTHash("password2")
		if bytes.Equal(hash1[:], hash2[:]) {
			t.Error("Different passwords should produce different NT hashes")
		}
	})

	t.Run("CaseSensitive", func(t *testing.T) {
		hash1 := ComputeNTHash("Password")
		hash2 := ComputeNTHash("password")
		if bytes.Equal(hash1[:], hash2[:]) {
			t.Error("NT hash should be case-sensitive")
		}
	})

	t.Run("UnicodeSupport", func(t *testing.T) {
		// NT hash supports Unicode passwords
		hash := ComputeNTHash("пароль") // Russian for "password"
		// Should produce a valid 16-byte hash
		if len(hash) != 16 {
			t.Errorf("NT hash should be 16 bytes, got %d", len(hash))
		}
		// Should not be all zeros
		allZero := true
		for _, b := range hash {
			if b != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			t.Error("NT hash should not be all zeros for non-empty password")
		}
	})
}

func TestComputeNTLMv2Hash(t *testing.T) {
	// The NTLMv2 hash is: HMAC-MD5(NT_Hash, UPPERCASE(username) + domain)
	// Using UTF-16LE encoding for the concatenated string

	t.Run("ConsistentResults", func(t *testing.T) {
		ntHash := ComputeNTHash("password")
		hash1 := ComputeNTLMv2Hash(ntHash, "user", "DOMAIN")
		hash2 := ComputeNTLMv2Hash(ntHash, "user", "DOMAIN")

		if !bytes.Equal(hash1[:], hash2[:]) {
			t.Error("Same inputs should produce same NTLMv2 hash")
		}
	})

	t.Run("CaseInsensitiveUsername", func(t *testing.T) {
		ntHash := ComputeNTHash("password")
		hash1 := ComputeNTLMv2Hash(ntHash, "user", "DOMAIN")
		hash2 := ComputeNTLMv2Hash(ntHash, "USER", "DOMAIN")
		hash3 := ComputeNTLMv2Hash(ntHash, "User", "DOMAIN")

		if !bytes.Equal(hash1[:], hash2[:]) || !bytes.Equal(hash1[:], hash3[:]) {
			t.Error("Username should be case-insensitive (uppercased internally)")
		}
	})

	t.Run("CaseSensitiveDomain", func(t *testing.T) {
		ntHash := ComputeNTHash("password")
		hash1 := ComputeNTLMv2Hash(ntHash, "user", "DOMAIN")
		hash2 := ComputeNTLMv2Hash(ntHash, "user", "domain")

		if bytes.Equal(hash1[:], hash2[:]) {
			t.Error("Domain should be case-sensitive")
		}
	})

	t.Run("DifferentPasswordsDifferentHashes", func(t *testing.T) {
		ntHash1 := ComputeNTHash("password1")
		ntHash2 := ComputeNTHash("password2")
		hash1 := ComputeNTLMv2Hash(ntHash1, "user", "DOMAIN")
		hash2 := ComputeNTLMv2Hash(ntHash2, "user", "DOMAIN")

		if bytes.Equal(hash1[:], hash2[:]) {
			t.Error("Different passwords should produce different NTLMv2 hashes")
		}
	})

	t.Run("DifferentUsersDifferentHashes", func(t *testing.T) {
		ntHash := ComputeNTHash("password")
		hash1 := ComputeNTLMv2Hash(ntHash, "user1", "DOMAIN")
		hash2 := ComputeNTLMv2Hash(ntHash, "user2", "DOMAIN")

		if bytes.Equal(hash1[:], hash2[:]) {
			t.Error("Different users should produce different NTLMv2 hashes")
		}
	})
}

func TestValidateNTLMv2Response(t *testing.T) {
	t.Run("ResponseTooShort", func(t *testing.T) {
		ntHash := ComputeNTHash("password")
		serverChallenge := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
		shortResponse := make([]byte, 20) // Too short, needs at least 24 bytes

		_, err := ValidateNTLMv2Response(ntHash, "user", "DOMAIN", serverChallenge, shortResponse)
		if err != ErrResponseTooShort {
			t.Errorf("Expected ErrResponseTooShort, got %v", err)
		}
	})

	t.Run("InvalidResponse", func(t *testing.T) {
		ntHash := ComputeNTHash("password")
		serverChallenge := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
		invalidResponse := make([]byte, 32) // Long enough but invalid

		_, err := ValidateNTLMv2Response(ntHash, "user", "DOMAIN", serverChallenge, invalidResponse)
		if err != ErrAuthenticationFailed {
			t.Errorf("Expected ErrAuthenticationFailed, got %v", err)
		}
	})

	t.Run("ValidResponseProducesSessionKey", func(t *testing.T) {
		// This test validates the complete flow:
		// 1. Server generates challenge
		// 2. Client builds NTLMv2 response with correct credentials
		// 3. Server validates and derives session key

		password := "test123"
		username := "testuser"
		domain := "TESTDOMAIN"
		serverChallenge := [8]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}

		// Compute NT hash (both client and server have this)
		ntHash := ComputeNTHash(password)

		// Simulate client building NTLMv2 response
		// The client blob contains timestamp and other data
		clientBlob := buildTestClientBlob()

		// Compute expected NTProofStr (what the client sends)
		ntlmv2Hash := ComputeNTLMv2Hash(ntHash, username, domain)
		ntProofStr := computeNTProofStr(ntlmv2Hash, serverChallenge, clientBlob)

		// Build complete NT response: NTProofStr + ClientBlob
		ntResponse := make([]byte, len(ntProofStr)+len(clientBlob))
		copy(ntResponse[:16], ntProofStr)
		copy(ntResponse[16:], clientBlob)

		// Server validates and gets session key
		sessionKey, err := ValidateNTLMv2Response(ntHash, username, domain, serverChallenge, ntResponse)
		if err != nil {
			t.Fatalf("ValidateNTLMv2Response failed: %v", err)
		}

		// Session key should not be all zeros
		allZero := true
		for _, b := range sessionKey {
			if b != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			t.Error("Session key should not be all zeros")
		}
	})

	t.Run("WrongPasswordFails", func(t *testing.T) {
		password := "correctpassword"
		wrongPassword := "wrongpassword"
		username := "testuser"
		domain := "TESTDOMAIN"
		serverChallenge := [8]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}

		// Client uses correct password
		correctNTHash := ComputeNTHash(password)
		clientBlob := buildTestClientBlob()
		ntlmv2Hash := ComputeNTLMv2Hash(correctNTHash, username, domain)
		ntProofStr := computeNTProofStr(ntlmv2Hash, serverChallenge, clientBlob)

		ntResponse := make([]byte, len(ntProofStr)+len(clientBlob))
		copy(ntResponse[:16], ntProofStr)
		copy(ntResponse[16:], clientBlob)

		// Server uses wrong password (NT hash)
		wrongNTHash := ComputeNTHash(wrongPassword)
		_, err := ValidateNTLMv2Response(wrongNTHash, username, domain, serverChallenge, ntResponse)
		if err != ErrAuthenticationFailed {
			t.Errorf("Expected ErrAuthenticationFailed for wrong password, got %v", err)
		}
	})

	t.Run("WrongServerChallengeFails", func(t *testing.T) {
		password := "test123"
		username := "testuser"
		domain := "TESTDOMAIN"
		correctChallenge := [8]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
		wrongChallenge := [8]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

		ntHash := ComputeNTHash(password)
		clientBlob := buildTestClientBlob()
		ntlmv2Hash := ComputeNTLMv2Hash(ntHash, username, domain)
		// Client computed proof with correct challenge
		ntProofStr := computeNTProofStr(ntlmv2Hash, correctChallenge, clientBlob)

		ntResponse := make([]byte, len(ntProofStr)+len(clientBlob))
		copy(ntResponse[:16], ntProofStr)
		copy(ntResponse[16:], clientBlob)

		// Server validates with wrong challenge
		_, err := ValidateNTLMv2Response(ntHash, username, domain, wrongChallenge, ntResponse)
		if err != ErrAuthenticationFailed {
			t.Errorf("Expected ErrAuthenticationFailed for wrong challenge, got %v", err)
		}
	})
}

// =============================================================================
// Test Helpers
// =============================================================================

// buildTestMessage creates a minimal NTLM message of the given type.
func buildTestMessage(msgType MessageType) []byte {
	msg := make([]byte, 32)
	copy(msg[0:8], Signature)
	binary.LittleEndian.PutUint32(msg[8:12], uint32(msgType))
	return msg
}

// bytesToHex converts a byte slice to a hex string.
func bytesToHex(b []byte) string {
	result := make([]byte, len(b)*2)
	const hexChars = "0123456789abcdef"
	for i, v := range b {
		result[i*2] = hexChars[v>>4]
		result[i*2+1] = hexChars[v&0x0f]
	}
	return string(result)
}

// buildTestClientBlob creates a minimal client blob for testing.
// In real NTLMv2, this contains timestamp, nonce, and target info.
func buildTestClientBlob() []byte {
	// Minimal client blob structure:
	// - RespType (1 byte): 0x01
	// - HiRespType (1 byte): 0x01
	// - Reserved1 (2 bytes): 0x0000
	// - Reserved2 (4 bytes): 0x00000000
	// - TimeStamp (8 bytes): any value
	// - ChallengeFromClient (8 bytes): random
	// - Reserved3 (4 bytes): 0x00000000
	// - AvPairs (4+ bytes): at minimum MsvAvEOL (AvId=0, AvLen=0)
	blob := make([]byte, 32)
	blob[0] = 0x01                                    // RespType
	blob[1] = 0x01                                    // HiRespType
	binary.LittleEndian.PutUint64(blob[8:16], 123)    // TimeStamp
	copy(blob[16:24], []byte{1, 2, 3, 4, 5, 6, 7, 8}) // ClientChallenge
	// AvPairs at 28: MsvAvEOL (AvId=0, AvLen=0) = 4 bytes of zeros
	return blob
}

// computeNTProofStr computes the NTProofStr for testing.
// This simulates what the client does during authentication.
func computeNTProofStr(ntlmv2Hash [16]byte, serverChallenge [8]byte, clientBlob []byte) []byte {
	mac := hmac.New(md5.New, ntlmv2Hash[:])
	mac.Write(serverChallenge[:])
	mac.Write(clientBlob)
	return mac.Sum(nil)
}
