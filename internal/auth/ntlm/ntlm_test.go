package ntlm

import (
	"bytes"
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
	msg := BuildChallenge()

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

	t.Run("GeneratesUniqueChallenge", func(t *testing.T) {
		msg2 := BuildChallenge()
		challenge1 := msg[24:32]
		challenge2 := msg2[24:32]

		if bytes.Equal(challenge1, challenge2) {
			t.Error("Two challenges should be different (random)")
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
// Test Helpers
// =============================================================================

// buildTestMessage creates a minimal NTLM message of the given type.
func buildTestMessage(msgType MessageType) []byte {
	msg := make([]byte, 32)
	copy(msg[0:8], Signature)
	binary.LittleEndian.PutUint32(msg[8:12], uint32(msgType))
	return msg
}
