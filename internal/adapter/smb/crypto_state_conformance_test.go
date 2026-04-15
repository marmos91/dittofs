package smb

import (
	"bytes"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// TestPreauthHashConformance_NegotiateRequest replays the MS-SMB2 official test
// vector NEGOTIATE request bytes from the Microsoft blog post appendix and
// verifies that chainHash produces the expected hash value.
//
// Source: https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-3-1-1-pre-authentication-integrity-in-windows-10
func TestPreauthHashConformance_NegotiateRequest(t *testing.T) {
	cs := NewConnectionCryptoState()

	// Negotiate request packet from MS-SMB2 test vector.
	// Starts with FE 53 4D 42 (SMB2 protocol ID) -- no NetBIOS framing.
	negotiateReq, err := hex.DecodeString(
		"FE534D4240000100000000000000800000000000000000000100000000000000" +
			"FFFE000000000000000000000000000000000000000000000000000000000000" +
			"24000500000000003F000000ECD86F326276024F9F7752B89BB33F3A70000000" +
			"020000000202100200030203110300000100260000000000010020000100FA49" +
			"E6578F1F3A9F4CD3E9CC14A67AA884B3D05844E0E5A118225C15887F32FF0000" +
			"0200060000000000020002000100")
	if err != nil {
		t.Fatalf("Failed to decode negotiate request hex: %v", err)
	}

	// Verify the test vector starts with SMB2 protocol ID
	if len(negotiateReq) < 4 {
		t.Fatal("Negotiate request too short")
	}
	if binary.LittleEndian.Uint32(negotiateReq[0:4]) != types.SMB2ProtocolID {
		t.Fatalf("Negotiate request does not start with SMB2 protocol ID: got %x", negotiateReq[0:4])
	}

	cs.UpdatePreauthHash(negotiateReq)
	hash := cs.GetPreauthHash()

	// Expected hash after NEGOTIATE request (from MS-SMB2 test vector)
	expected, err := hex.DecodeString(
		"DD94EFC5321BB618A2E208BA8920D2F422992526947A409B5037DE1E0FE8C736" +
			"2B8C47122594CDE0CE26AA9DFC8BCDBDE0621957672623351A7540F1E54A0426")
	if err != nil {
		t.Fatalf("Failed to decode expected hash hex: %v", err)
	}

	if !bytes.Equal(hash[:], expected) {
		t.Errorf("NEGOTIATE request hash mismatch\ngot:  %x\nwant: %x", hash[:], expected)
	}
}

// TestPreauthHashConformance_ChainOrder verifies the complete preauth hash chain:
//
//  1. Connection hash updated with NEGOTIATE request
//  2. Connection hash updated with NEGOTIATE response
//  3. InitSessionPreauthHash for a session (from connection hash)
//  4. Per-session hash updated with SESSION_SETUP request (via stash+init)
//  5. Per-session hash updated with SESSION_SETUP response (MORE_PROCESSING)
//  6. Per-session hash updated with final SESSION_SETUP request
//
// Assert intermediate and final values are non-zero and consistent with
// manual SHA-512 computation.
func TestPreauthHashConformance_ChainOrder(t *testing.T) {
	cs := NewConnectionCryptoState()

	// Synthetic but correctly-structured messages (start with SMB2 protocol ID)
	negReq := buildSyntheticSMB2Message(types.SMB2Negotiate, 0, []byte("negotiate-request-body"))
	negResp := buildSyntheticSMB2Message(types.SMB2Negotiate, types.FlagResponse, []byte("negotiate-response-body"))
	ssReq1 := buildSyntheticSMB2Message(types.SMB2SessionSetup, 0, []byte("session-setup-req-1"))
	ssResp1 := buildSyntheticSMB2Message(types.SMB2SessionSetup, types.FlagResponse, []byte("session-setup-resp-1-more-processing"))
	ssReq2 := buildSyntheticSMB2Message(types.SMB2SessionSetup, 0, []byte("session-setup-req-2-final"))

	// Step 1: Connection hash with NEGOTIATE request
	cs.UpdatePreauthHash(negReq)
	h1 := cs.GetPreauthHash()

	// Step 2: Connection hash with NEGOTIATE response
	cs.UpdatePreauthHash(negResp)
	h2 := cs.GetPreauthHash()

	// Step 3: Init per-session hash with SESSION_SETUP request bytes
	sessionID := uint64(0x17592186044441)
	cs.InitSessionPreauthHash(sessionID, ssReq1)
	h3 := cs.GetSessionPreauthHash(sessionID)

	// Step 4: Per-session hash with SESSION_SETUP response (MORE_PROCESSING)
	cs.UpdateSessionPreauthHash(sessionID, ssResp1)
	h4 := cs.GetSessionPreauthHash(sessionID)

	// Step 5: Per-session hash with final SESSION_SETUP request
	cs.UpdateSessionPreauthHash(sessionID, ssReq2)
	h5 := cs.GetSessionPreauthHash(sessionID)

	// Verify all intermediate hashes are non-zero
	zeroHash := [64]byte{}
	for _, tc := range []struct {
		name string
		hash [64]byte
	}{
		{"after NEGOTIATE req", h1},
		{"after NEGOTIATE resp", h2},
		{"after session init + stash", h3},
		{"after SESSION_SETUP resp", h4},
		{"after final SESSION_SETUP req", h5},
	} {
		if tc.hash == zeroHash {
			t.Errorf("%s: hash should not be all zeros", tc.name)
		}
	}

	// Verify chain ordering: each hash should differ from the previous
	if h1 == h2 {
		t.Error("Connection hash should differ after NEGOTIATE resp")
	}
	if h2 == h3 {
		t.Error("Per-session hash should differ from connection hash (includes SESSION_SETUP req)")
	}
	if h3 == h4 {
		t.Error("Per-session hash should change after SESSION_SETUP resp")
	}
	if h4 == h5 {
		t.Error("Per-session hash should change after final SESSION_SETUP req")
	}

	// Verify final per-session hash differs from connection hash (h2)
	if h5 == h2 {
		t.Error("Final per-session hash should differ from connection hash")
	}

	// Verify manual SHA-512 computation matches
	// H(0) = 64 zero bytes
	// H(1) = SHA-512(H(0) || negReq)
	manualH1 := chainHash(zeroHash, negReq)
	if h1 != manualH1 {
		t.Error("H(1) mismatch: UpdatePreauthHash(negReq) != manual SHA-512")
	}

	// H(2) = SHA-512(H(1) || negResp)
	manualH2 := chainHash(manualH1, negResp)
	if h2 != manualH2 {
		t.Error("H(2) mismatch: UpdatePreauthHash(negResp) != manual SHA-512")
	}

	// Per-session H(3) = SHA-512(H(2) || ssReq1)
	manualH3 := chainHash(manualH2, ssReq1)
	if h3 != manualH3 {
		t.Errorf("H(3) mismatch: session hash after stash+init != manual SHA-512\ngot:  %x\nwant: %x", h3, manualH3)
	}

	// Per-session H(4) = SHA-512(H(3) || ssResp1)
	manualH4 := chainHash(manualH3, ssResp1)
	if h4 != manualH4 {
		t.Error("H(4) mismatch: session hash after SESSION_SETUP resp != manual SHA-512")
	}

	// Per-session H(5) = SHA-512(H(4) || ssReq2)
	manualH5 := chainHash(manualH4, ssReq2)
	if h5 != manualH5 {
		t.Error("H(5) mismatch: session hash after final SESSION_SETUP req != manual SHA-512")
	}
}

// TestPreauthHash_InitWithRequestBytes verifies that
// InitSessionPreauthHash(sessionID, ssReq) is equivalent to
// InitSessionPreauthHash(sessionID, nil) + UpdateSessionPreauthHash(sessionID, ssReq).
//
// (Pre-#362 fix this used a stash mechanism, removed because of a concurrent
// SESSION_SETUP race; the bytes now flow through SMBHandlerContext.RawRequest.)
func TestPreauthHash_InitWithRequestBytes(t *testing.T) {
	negReq := buildSyntheticSMB2Message(types.SMB2Negotiate, 0, []byte("neg-req"))
	negResp := buildSyntheticSMB2Message(types.SMB2Negotiate, types.FlagResponse, []byte("neg-resp"))
	ssReq := buildSyntheticSMB2Message(types.SMB2SessionSetup, 0, []byte("session-setup-req"))

	// Path 1: Init with rawMessage in one call.
	cs1 := NewConnectionCryptoState()
	cs1.UpdatePreauthHash(negReq)
	cs1.UpdatePreauthHash(negResp)
	cs1.InitSessionPreauthHash(42, ssReq)

	// Path 2: Init then explicit Update.
	cs2 := NewConnectionCryptoState()
	cs2.UpdatePreauthHash(negReq)
	cs2.UpdatePreauthHash(negResp)
	cs2.InitSessionPreauthHash(42, nil)
	cs2.UpdateSessionPreauthHash(42, ssReq)

	hash1 := cs1.GetSessionPreauthHash(42)
	hash2 := cs2.GetSessionPreauthHash(42)

	if hash1 != hash2 {
		t.Errorf("Init(req) should equal Init(nil)+Update(req)\nInit(req): %x\nInit+Update: %x", hash1, hash2)
	}

	// Both should equal manual computation.
	zeroHash := [64]byte{}
	expectedConnHash := chainHash(chainHash(zeroHash, negReq), negResp)
	expectedSessionHash := chainHash(expectedConnHash, ssReq)

	if hash1 != expectedSessionHash {
		t.Errorf("Init(req) doesn't match manual SHA-512 chain\ngot:  %x\nwant: %x", hash1, expectedSessionHash)
	}
}

// TestPreauthHash_FinalResponseNotIncluded verifies that the hash used for key
// derivation (GetSessionPreauthHash) does NOT include the final SESSION_SETUP
// response. Per MS-SMB2 3.3.5.5, the final response (STATUS_SUCCESS) is NOT
// included in the preauth hash used for signing key derivation.
func TestPreauthHash_FinalResponseNotIncluded(t *testing.T) {
	cs := NewConnectionCryptoState()

	negReq := buildSyntheticSMB2Message(types.SMB2Negotiate, 0, []byte("neg-req"))
	negResp := buildSyntheticSMB2Message(types.SMB2Negotiate, types.FlagResponse, []byte("neg-resp"))
	ssReq1 := buildSyntheticSMB2Message(types.SMB2SessionSetup, 0, []byte("ss-req-1"))
	ssResp1 := buildSyntheticSMB2Message(types.SMB2SessionSetup, types.FlagResponse, []byte("ss-resp-1"))
	ssReq2 := buildSyntheticSMB2Message(types.SMB2SessionSetup, 0, []byte("ss-req-2-final"))
	ssRespFinal := buildSyntheticSMB2Message(types.SMB2SessionSetup, types.FlagResponse, []byte("ss-resp-final-success"))

	sessionID := uint64(99)

	// Full chain through to key derivation point
	cs.UpdatePreauthHash(negReq)
	cs.UpdatePreauthHash(negResp)
	cs.InitSessionPreauthHash(sessionID, ssReq1)
	cs.UpdateSessionPreauthHash(sessionID, ssResp1)
	cs.UpdateSessionPreauthHash(sessionID, ssReq2)

	// Get hash at derivation point -- BEFORE final response
	derivationHash := cs.GetSessionPreauthHash(sessionID)

	// Now delete the session preauth hash (simulating what configureSessionSigningWithKey does)
	cs.DeleteSessionPreauthHash(sessionID)

	// Try to update with the final response -- this should be a no-op since the
	// session was deleted from the preauth table
	cs.UpdateSessionPreauthHash(sessionID, ssRespFinal)

	// GetSessionPreauthHash now returns the connection hash (fallback) since the
	// session entry was deleted
	postDeleteHash := cs.GetSessionPreauthHash(sessionID)

	// The derivation hash should NOT equal what you'd get if the final response
	// were included
	zeroHash := [64]byte{}
	connHash := chainHash(chainHash(zeroHash, negReq), negResp)
	sessionHash := chainHash(chainHash(chainHash(connHash, ssReq1), ssResp1), ssReq2)
	hashWithFinalResp := chainHash(sessionHash, ssRespFinal)

	if derivationHash == hashWithFinalResp {
		t.Error("Derivation hash should NOT include the final SESSION_SETUP response")
	}

	// The derivation hash should equal the session hash without the final response
	if derivationHash != sessionHash {
		t.Errorf("Derivation hash should equal session hash without final response\ngot:  %x\nwant: %x", derivationHash, sessionHash)
	}

	// After deletion, GetSessionPreauthHash falls back to connection hash
	if postDeleteHash != connHash {
		t.Errorf("After deletion, GetSessionPreauthHash should return connection hash\ngot:  %x\nwant: %x", postDeleteHash, connHash)
	}
}

// TestRawMessageStartsWithSMB2ProtocolID verifies that rawMessage bytes
// constructed via the header Encode+body pattern always start with
// 0xFE534D42 (SMB2 protocol ID), never with 0x00 (NetBIOS framing).
//
// This validates the rawMessage reconstruction used in connection.go (lines 191-193):
//
//	rawMessage := make([]byte, header.HeaderSize+len(body))
//	copy(rawMessage, hdr.Encode())
//	copy(rawMessage[header.HeaderSize:], body)
func TestRawMessageStartsWithSMB2ProtocolID(t *testing.T) {
	// Build a header using the same method as connection.go
	hdr := &header.SMB2Header{
		Command:   types.SMB2Negotiate,
		MessageID: 1,
		Credits:   1,
		Flags:     0,
	}

	body := []byte("test body content for negotiate request")

	// Reconstruct rawMessage the same way connection.go does
	rawMessage := make([]byte, header.HeaderSize+len(body))
	copy(rawMessage, hdr.Encode())
	copy(rawMessage[header.HeaderSize:], body)

	// Assert first 4 bytes are SMB2 protocol ID, not NetBIOS framing (0x00)
	if binary.LittleEndian.Uint32(rawMessage[0:4]) != types.SMB2ProtocolID {
		t.Errorf("rawMessage should start with SMB2 protocol ID (0x%08x)\ngot:  %x", types.SMB2ProtocolID, rawMessage[0:4])
	}

	// Verify Parse+Encode roundtrip produces identical bytes
	parsed, err := header.Parse(rawMessage[:header.HeaderSize])
	if err != nil {
		t.Fatalf("Failed to parse rawMessage header: %v", err)
	}
	reencoded := parsed.Encode()
	if !bytes.Equal(rawMessage[:header.HeaderSize], reencoded) {
		t.Errorf("Parse+Encode roundtrip produced different bytes\noriginal:  %x\nreencoded: %x", rawMessage[:header.HeaderSize], reencoded)
	}
}

// TestRawMessageRoundtripFidelity verifies that Parse(data) followed by Encode()
// produces byte-identical output to the original data, for various header
// configurations including signed messages and compound requests.
func TestRawMessageRoundtripFidelity(t *testing.T) {
	tests := []struct {
		name string
		hdr  *header.SMB2Header
	}{
		{
			name: "basic negotiate",
			hdr: &header.SMB2Header{
				Command:   types.SMB2Negotiate,
				MessageID: 0,
				Credits:   1,
			},
		},
		{
			name: "session setup with session ID",
			hdr: &header.SMB2Header{
				Command:   types.SMB2SessionSetup,
				MessageID: 2,
				Credits:   64,
				SessionID: 0x17592186044441,
			},
		},
		{
			name: "response with status",
			hdr: &header.SMB2Header{
				Command:   types.SMB2Negotiate,
				MessageID: 0,
				Credits:   256,
				Flags:     types.FlagResponse,
				Status:    types.StatusSuccess,
			},
		},
		{
			name: "signed response",
			hdr: &header.SMB2Header{
				Command:   types.SMB2SessionSetup,
				MessageID: 3,
				Credits:   1,
				Flags:     types.FlagResponse | types.FlagSigned,
				SessionID: 42,
				Signature: [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10},
			},
		},
		{
			name: "compound with NextCommand",
			hdr: &header.SMB2Header{
				Command:     types.SMB2Create,
				MessageID:   5,
				Credits:     2,
				NextCommand: 152,
				TreeID:      7,
				SessionID:   100,
			},
		},
		{
			name: "with reserved/processID",
			hdr: &header.SMB2Header{
				Command:      types.SMB2Read,
				MessageID:    10,
				Credits:      1,
				Reserved:     0xFEFE,
				CreditCharge: 3,
				TreeID:       15,
				SessionID:    200,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Encode the header
			original := tc.hdr.Encode()

			// Parse it back
			parsed, err := header.Parse(original)
			if err != nil {
				t.Fatalf("Parse failed: %v", err)
			}

			// Re-encode
			reencoded := parsed.Encode()

			if !bytes.Equal(original, reencoded) {
				t.Errorf("Parse+Encode roundtrip not byte-identical\noriginal:  %x\nreencoded: %x", original, reencoded)
				// Find first differing byte
				for i := 0; i < len(original) && i < len(reencoded); i++ {
					if original[i] != reencoded[i] {
						t.Errorf("First difference at byte %d: original=0x%02x, reencoded=0x%02x", i, original[i], reencoded[i])
						break
					}
				}
			}
		})
	}
}

// TestPreauthHash_ChainHashMatchesSHA512 verifies that chainHash produces
// the same result as a direct SHA-512 computation of H(i-1) || message.
func TestPreauthHash_ChainHashMatchesSHA512(t *testing.T) {
	// Start from zero hash
	var current [64]byte
	message := []byte("test message for hash chain verification")

	result := chainHash(current, message)

	// Manual SHA-512
	h := sha512.New()
	h.Write(current[:])
	h.Write(message)
	expected := h.Sum(nil)

	if !bytes.Equal(result[:], expected) {
		t.Errorf("chainHash doesn't match SHA-512\ngot:  %x\nwant: %x", result[:], expected)
	}

	// Chain two messages
	message2 := []byte("second message in chain")
	result2 := chainHash(result, message2)

	h2 := sha512.New()
	h2.Write(result[:])
	h2.Write(message2)
	expected2 := h2.Sum(nil)

	if !bytes.Equal(result2[:], expected2) {
		t.Errorf("chainHash second step doesn't match SHA-512\ngot:  %x\nwant: %x", result2[:], expected2)
	}
}

// buildSyntheticSMB2Message constructs a synthetic SMB2 message with the given
// command, flags, and body content. The message starts with a valid SMB2 header
// (64 bytes) followed by the body.
func buildSyntheticSMB2Message(command types.Command, flags types.HeaderFlags, body []byte) []byte {
	hdr := &header.SMB2Header{
		Command: command,
		Flags:   flags,
		Credits: 1,
	}
	encoded := hdr.Encode()
	return append(encoded, body...)
}
