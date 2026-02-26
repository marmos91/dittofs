package auth

import (
	"testing"

	"github.com/jcmturner/gofork/encoding/asn1"
	gokrbspnego "github.com/jcmturner/gokrb5/v8/spnego"
)

// =============================================================================
// OID Constants Tests
// =============================================================================

func TestOIDConstants(t *testing.T) {
	tests := []struct {
		name     string
		oid      asn1.ObjectIdentifier
		expected []int
	}{
		{
			name:     "OIDNTLMSSP",
			oid:      OIDNTLMSSP,
			expected: []int{1, 3, 6, 1, 4, 1, 311, 2, 2, 10},
		},
		{
			name:     "OIDKerberosV5",
			oid:      OIDKerberosV5,
			expected: []int{1, 2, 840, 113554, 1, 2, 2},
		},
		{
			name:     "OIDMSKerberosV5",
			oid:      OIDMSKerberosV5,
			expected: []int{1, 2, 840, 48018, 1, 2, 2},
		},
		{
			name:     "OIDSPNEGO",
			oid:      OIDSPNEGO,
			expected: []int{1, 3, 6, 1, 5, 5, 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.oid.Equal(asn1.ObjectIdentifier(tt.expected)) {
				t.Errorf("%s = %v, expected %v", tt.name, tt.oid, tt.expected)
			}
		})
	}
}

// =============================================================================
// NegState Constants Tests
// =============================================================================

func TestNegStateConstants(t *testing.T) {
	if NegStateAcceptCompleted != 0 {
		t.Errorf("NegStateAcceptCompleted = %d, expected 0", NegStateAcceptCompleted)
	}
	if NegStateAcceptIncomplete != 1 {
		t.Errorf("NegStateAcceptIncomplete = %d, expected 1", NegStateAcceptIncomplete)
	}
	if NegStateReject != 2 {
		t.Errorf("NegStateReject = %d, expected 2", NegStateReject)
	}
	if NegStateRequestMIC != 3 {
		t.Errorf("NegStateRequestMIC = %d, expected 3", NegStateRequestMIC)
	}
}

// =============================================================================
// Parse Tests
// =============================================================================

func TestParse(t *testing.T) {
	t.Run("ParsesNegTokenInit", func(t *testing.T) {
		// Create a valid NegTokenInit using gokrb5
		ntlmToken := []byte("NTLMSSP\x00test-payload")
		initToken := gokrbspnego.NegTokenInit{
			MechTypes:      []asn1.ObjectIdentifier{OIDNTLMSSP},
			MechTokenBytes: ntlmToken,
		}

		data, err := initToken.Marshal()
		if err != nil {
			t.Fatalf("Failed to marshal test token: %v", err)
		}

		parsed, err := Parse(data)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}

		if parsed.Type != TokenTypeInit {
			t.Errorf("Type = %v, expected TokenTypeInit", parsed.Type)
		}

		if len(parsed.MechTypes) != 1 {
			t.Errorf("MechTypes length = %d, expected 1", len(parsed.MechTypes))
		}

		if !parsed.MechTypes[0].Equal(OIDNTLMSSP) {
			t.Errorf("MechTypes[0] = %v, expected NTLMSSP OID", parsed.MechTypes[0])
		}

		if string(parsed.MechToken) != string(ntlmToken) {
			t.Errorf("MechToken = %v, expected %v", parsed.MechToken, ntlmToken)
		}
	})

	t.Run("ParsesNegTokenResp", func(t *testing.T) {
		// Create a valid NegTokenResp using gokrb5
		responseToken := []byte("response-data")
		respToken := gokrbspnego.NegTokenResp{
			NegState:      asn1.Enumerated(NegStateAcceptIncomplete),
			SupportedMech: OIDNTLMSSP,
			ResponseToken: responseToken,
		}

		data, err := respToken.Marshal()
		if err != nil {
			t.Fatalf("Failed to marshal test token: %v", err)
		}

		parsed, err := Parse(data)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}

		if parsed.Type != TokenTypeResp {
			t.Errorf("Type = %v, expected TokenTypeResp", parsed.Type)
		}

		if parsed.NegState != NegStateAcceptIncomplete {
			t.Errorf("NegState = %v, expected NegStateAcceptIncomplete", parsed.NegState)
		}

		if !parsed.SupportedMech.Equal(OIDNTLMSSP) {
			t.Errorf("SupportedMech = %v, expected NTLMSSP OID", parsed.SupportedMech)
		}
	})

	t.Run("ReturnsErrorForInvalidToken", func(t *testing.T) {
		invalidData := []byte{0xDE, 0xAD, 0xBE, 0xEF}

		_, err := Parse(invalidData)
		if err == nil {
			t.Error("Expected error for invalid token")
		}
	})

	t.Run("ReturnsErrorForTooShort", func(t *testing.T) {
		_, err := Parse([]byte{0x60})
		if err == nil {
			t.Error("Expected error for too short input")
		}
	})

	t.Run("ReturnsErrorForEmpty", func(t *testing.T) {
		_, err := Parse([]byte{})
		if err == nil {
			t.Error("Expected error for empty input")
		}
	})
}

// =============================================================================
// HasMechanism Tests
// =============================================================================

func TestParsedToken_HasMechanism(t *testing.T) {
	parsed := &ParsedToken{
		Type:      TokenTypeInit,
		MechTypes: []asn1.ObjectIdentifier{OIDNTLMSSP, OIDKerberosV5},
	}

	t.Run("ReturnsTrueForPresentMech", func(t *testing.T) {
		if !parsed.HasMechanism(OIDNTLMSSP) {
			t.Error("Should have NTLMSSP mechanism")
		}
		if !parsed.HasMechanism(OIDKerberosV5) {
			t.Error("Should have Kerberos mechanism")
		}
	})

	t.Run("ReturnsFalseForAbsentMech", func(t *testing.T) {
		unknownOID := asn1.ObjectIdentifier{1, 2, 3, 4, 5}
		if parsed.HasMechanism(unknownOID) {
			t.Error("Should not have unknown mechanism")
		}
	})
}

func TestParsedToken_HasNTLM(t *testing.T) {
	t.Run("ReturnsTrueWhenPresent", func(t *testing.T) {
		parsed := &ParsedToken{
			Type:      TokenTypeInit,
			MechTypes: []asn1.ObjectIdentifier{OIDNTLMSSP},
		}
		if !parsed.HasNTLM() {
			t.Error("Should have NTLM")
		}
	})

	t.Run("ReturnsFalseWhenAbsent", func(t *testing.T) {
		parsed := &ParsedToken{
			Type:      TokenTypeInit,
			MechTypes: []asn1.ObjectIdentifier{OIDKerberosV5},
		}
		if parsed.HasNTLM() {
			t.Error("Should not have NTLM")
		}
	})
}

func TestParsedToken_HasKerberos(t *testing.T) {
	t.Run("ReturnsTrueForStandardKerberos", func(t *testing.T) {
		parsed := &ParsedToken{
			Type:      TokenTypeInit,
			MechTypes: []asn1.ObjectIdentifier{OIDKerberosV5},
		}
		if !parsed.HasKerberos() {
			t.Error("Should have Kerberos (standard OID)")
		}
	})

	t.Run("ReturnsTrueForMSKerberos", func(t *testing.T) {
		parsed := &ParsedToken{
			Type:      TokenTypeInit,
			MechTypes: []asn1.ObjectIdentifier{OIDMSKerberosV5},
		}
		if !parsed.HasKerberos() {
			t.Error("Should have Kerberos (MS OID)")
		}
	})

	t.Run("ReturnsFalseWhenAbsent", func(t *testing.T) {
		parsed := &ParsedToken{
			Type:      TokenTypeInit,
			MechTypes: []asn1.ObjectIdentifier{OIDNTLMSSP},
		}
		if parsed.HasKerberos() {
			t.Error("Should not have Kerberos")
		}
	})
}

// =============================================================================
// BuildResponse Tests
// =============================================================================

func TestBuildResponse(t *testing.T) {
	t.Run("BuildsValidResponse", func(t *testing.T) {
		responseToken := []byte("test-response")
		data, err := BuildResponse(NegStateAcceptIncomplete, OIDNTLMSSP, responseToken)
		if err != nil {
			t.Fatalf("BuildResponse failed: %v", err)
		}

		// Should be parseable
		parsed, err := Parse(data)
		if err != nil {
			t.Fatalf("Failed to parse built response: %v", err)
		}

		if parsed.Type != TokenTypeResp {
			t.Errorf("Type = %v, expected TokenTypeResp", parsed.Type)
		}

		if parsed.NegState != NegStateAcceptIncomplete {
			t.Errorf("NegState = %v, expected NegStateAcceptIncomplete", parsed.NegState)
		}
	})
}

func TestBuildAcceptIncomplete(t *testing.T) {
	responseToken := []byte("challenge-data")
	data, err := BuildAcceptIncomplete(OIDNTLMSSP, responseToken)
	if err != nil {
		t.Fatalf("BuildAcceptIncomplete failed: %v", err)
	}

	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	if parsed.NegState != NegStateAcceptIncomplete {
		t.Errorf("NegState = %v, expected NegStateAcceptIncomplete", parsed.NegState)
	}
}

func TestBuildAcceptComplete(t *testing.T) {
	data, err := BuildAcceptComplete(OIDNTLMSSP, nil)
	if err != nil {
		t.Fatalf("BuildAcceptComplete failed: %v", err)
	}

	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	if parsed.NegState != NegStateAcceptCompleted {
		t.Errorf("NegState = %v, expected NegStateAcceptCompleted", parsed.NegState)
	}
}

func TestBuildReject(t *testing.T) {
	data, err := BuildReject()
	if err != nil {
		t.Fatalf("BuildReject failed: %v", err)
	}

	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	if parsed.NegState != NegStateReject {
		t.Errorf("NegState = %v, expected NegStateReject", parsed.NegState)
	}
}

// =============================================================================
// Error Types Tests
// =============================================================================

func TestErrorTypes(t *testing.T) {
	// Just verify error types exist and are distinct
	errors := []error{
		ErrInvalidToken,
		ErrUnsupportedMech,
		ErrNoMechToken,
		ErrUnexpectedToken,
	}

	for i, err1 := range errors {
		if err1 == nil {
			t.Errorf("Error at index %d should not be nil", i)
		}
		for j, err2 := range errors {
			if i != j && err1 == err2 {
				t.Errorf("Errors at index %d and %d should be different", i, j)
			}
		}
	}
}
