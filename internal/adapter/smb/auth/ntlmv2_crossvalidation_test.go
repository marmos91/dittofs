package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"
)

// TestNTLMv2CrossValidation simulates the EXACT go-smb2 client NTLMv2 flow
// against our server's ValidateNTLMv2Response to find the mismatch.
func TestNTLMv2CrossValidation(t *testing.T) {
	password := "TestPass123!"
	username := "testuser"
	clientDomain := "" // go-smb2 typically sends empty domain

	// Step 1: Server builds challenge (as our code does)
	challengeMsg, serverChallenge := BuildChallenge()

	// Step 2: Extract TargetName and TargetInfo from challenge (as go-smb2 does)
	targetNameLen := binary.LittleEndian.Uint16(challengeMsg[12:14])
	targetNameOff := binary.LittleEndian.Uint32(challengeMsg[16:20])
	targetName := challengeMsg[targetNameOff : targetNameOff+uint32(targetNameLen)]

	targetInfoLen := binary.LittleEndian.Uint16(challengeMsg[40:42])
	targetInfoOff := binary.LittleEndian.Uint32(challengeMsg[44:48])
	targetInfo := challengeMsg[targetInfoOff : targetInfoOff+uint32(targetInfoLen)]

	t.Logf("TargetName (hex): %x", targetName)
	t.Logf("TargetName (decoded): %q", decodeUTF16LEBytes(targetName))
	t.Logf("TargetInfo length: %d", len(targetInfo))
	t.Logf("ServerChallenge: %x", serverChallenge)

	// Step 3: Simulate go-smb2 client domain selection
	// In go-smb2: domain = utf16le.EncodeStringToBytes(c.Domain)
	// If domain == nil (empty string), domain = targetName
	domain := encodeUTF16LETest(clientDomain)
	if domain == nil {
		domain = targetName
		t.Logf("Client using TargetName as domain: %x", domain)
	}

	// Step 4: Compute NTLMv2 hash as go-smb2 does
	// ntowfv2(USER, password, domain) where USER and domain are already UTF-16LE
	USER := encodeUTF16LETest(strings.ToUpper(username))
	passwordUTF16 := encodeUTF16LETest(password)

	// MD4(UTF16LE(password)) - NT hash
	ntHash := ComputeNTHash(password)

	// go-smb2's ntowfv2Hash: HMAC-MD5(ntHash, USER + domain)
	// where USER and domain are raw UTF-16LE bytes
	clientHMAC := hmac.New(md5.New, ntHash[:])
	clientHMAC.Write(USER)
	clientHMAC.Write(domain)
	clientNTLMv2Hash := clientHMAC.Sum(nil)

	t.Logf("Client USER bytes: %x", USER)
	t.Logf("Client domain bytes: %x", domain)
	t.Logf("Client NTLMv2 hash: %x", clientNTLMv2Hash)
	t.Logf("Client password UTF16LE: %x (len=%d)", passwordUTF16, len(passwordUTF16))

	// Step 5: Now compute what our server does for each domain variant
	// Our server: ComputeNTLMv2Hash(ntHash, username, domain)
	//   = HMAC-MD5(ntHash, UTF16LE(UPPER(username) + domain))
	// Note: our server concatenates THEN encodes to UTF-16LE

	hostname := decodeUTF16LEBytes(targetName)
	domainsToTry := uniqueStringsTest([]string{
		"",                        // authMsg.Domain when client sends empty
		"",                        // empty string
		strings.ToUpper(hostname), // Server hostname
		"WORKGROUP",
	})

	for _, d := range domainsToTry {
		serverNTLMv2Hash := ComputeNTLMv2Hash(ntHash, username, d)
		match := bytes.Equal(clientNTLMv2Hash, serverNTLMv2Hash[:])
		t.Logf("Server domain=%q -> NTLMv2 hash=%x match=%v", d, serverNTLMv2Hash, match)

		if match {
			t.Logf("MATCH FOUND with domain=%q", d)
		}
	}

	// Step 6: Explicitly compare the byte-level computation
	// The KEY difference: go-smb2 does HMAC-MD5(ntHash, UTF16LE(UPPER(user)) + UTF16LE(domain))
	// where domain is already UTF-16LE (from server TargetName)
	// Our server does HMAC-MD5(ntHash, UTF16LE(UPPER(user) + domain))
	// which is the SAME as UTF16LE(UPPER(user)) + UTF16LE(domain) for ASCII strings.

	// But what if go-smb2 uses utf16le.EncodeStringToBytes which might return nil for ""?
	t.Run("EmptyDomainEncoding", func(t *testing.T) {
		// go-smb2's utf16le.EncodeStringToBytes("") returns nil, not empty []byte
		// So when client Domain is "", go-smb2 sets domain = targetName
		// This means the client ALWAYS uses the TargetName as domain when Domain is empty

		// Reproduce exact client computation:
		clientH := hmac.New(md5.New, ntHash[:])
		clientH.Write(USER)
		clientH.Write(targetName) // UTF-16LE of UPPER(hostname) from server
		clientResult := clientH.Sum(nil)

		// Server computation with hostname as domain:
		serverResult := ComputeNTLMv2Hash(ntHash, username, hostname)

		t.Logf("Client NTLMv2 (with targetName): %x", clientResult)
		t.Logf("Server NTLMv2 (with hostname=%q): %x", hostname, serverResult)
		t.Logf("Match: %v", bytes.Equal(clientResult, serverResult[:]))

		if !bytes.Equal(clientResult, serverResult[:]) {
			// Debug: show byte-by-byte what each computes
			serverCombined := encodeUTF16LETest(strings.ToUpper(username) + hostname)
			clientCombined := append(USER, targetName...)
			t.Logf("Server combined bytes: %x", serverCombined)
			t.Logf("Client combined bytes: %x", clientCombined)
			t.Logf("Combined bytes equal: %v", bytes.Equal(serverCombined, clientCombined))
		}
	})

	// Step 7: Full round-trip test
	t.Run("FullRoundTrip", func(t *testing.T) {
		// Build client blob with modified TargetInfo (as go-smb2 does)
		clientBlob := buildGoSMB2ClientBlob(targetInfo)

		// Build NTLMv2 response as go-smb2 does
		h := hmac.New(md5.New, clientNTLMv2Hash)
		h.Write(serverChallenge[:])
		h.Write(clientBlob)
		ntProofStr := h.Sum(nil)

		ntResponse := make([]byte, 16+len(clientBlob))
		copy(ntResponse[:16], ntProofStr)
		copy(ntResponse[16:], clientBlob)

		t.Logf("NTProofStr: %x", ntProofStr)
		t.Logf("NT Response length: %d", len(ntResponse))

		// Now validate with our server code using all domain variants
		domainsToTry := []string{
			"",
			strings.ToUpper(hostname),
			"WORKGROUP",
			hostname, // lowercase
		}

		for _, d := range domainsToTry {
			_, err := ValidateNTLMv2Response(ntHash, username, d, serverChallenge, ntResponse)
			if err == nil {
				t.Logf("Validation PASSED with domain=%q", d)
				return
			}
			t.Logf("Validation failed with domain=%q: %v", d, err)
		}

		t.Fatal("Validation failed with ALL domain variants")
	})
}

// encodeUTF16LETest encodes string to UTF-16LE. Returns nil for empty string
// (matching go-smb2 behavior).
func encodeUTF16LETest(s string) []byte {
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

// decodeUTF16LEBytes decodes UTF-16LE bytes to a Go string.
func decodeUTF16LEBytes(b []byte) string {
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}
	runes := make([]rune, len(b)/2)
	for i := 0; i < len(b); i += 2 {
		runes[i/2] = rune(binary.LittleEndian.Uint16(b[i : i+2]))
	}
	return string(runes)
}

func uniqueStringsTest(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	result := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}

// buildGoSMB2ClientBlob builds a client blob that mimics what go-smb2 does:
// it takes the server's TargetInfo, modifies it (adds MsvAvFlags, MsvAvChannelBindings,
// new MsvAvEOL), and wraps it in the NTLMv2ClientChallenge structure.
func buildGoSMB2ClientBlob(serverTargetInfo []byte) []byte {
	// Parse server TargetInfo to get the AvPairs
	// go-smb2 modifies: adds MsvAvFlags=0x02, MsvAvChannelBindings (16 zero bytes),
	// optional MsvAvTargetName, new MsvAvEOL
	// It removes the old MsvAvEOL (last 4 bytes)

	// Simplified: copy server target info, modify as go-smb2 does
	modifiedInfo := make([]byte, 0, len(serverTargetInfo)+32)
	// Copy all except the last 4 bytes (MsvAvEOL)
	modifiedInfo = append(modifiedInfo, serverTargetInfo[:len(serverTargetInfo)-4]...)

	// Add MsvAvFlags = 0x02 (MIC present)
	flags := make([]byte, 8)
	binary.LittleEndian.PutUint16(flags[0:2], 0x0006) // MsvAvFlags
	binary.LittleEndian.PutUint16(flags[2:4], 4)      // length
	binary.LittleEndian.PutUint32(flags[4:8], 0x02)   // value: MIC present
	modifiedInfo = append(modifiedInfo, flags...)

	// Add MsvAvChannelBindings (16 zero bytes)
	cb := make([]byte, 20)
	binary.LittleEndian.PutUint16(cb[0:2], 0x000a) // MsvAvChannelBindings
	binary.LittleEndian.PutUint16(cb[2:4], 16)     // length
	// cb[4:20] are zeros
	modifiedInfo = append(modifiedInfo, cb...)

	// Add MsvAvEOL
	modifiedInfo = append(modifiedInfo, 0x00, 0x00, 0x00, 0x00)

	// Build NTLMv2ClientChallenge structure
	// 0-1: RespType (0x01)
	// 1-2: HiRespType (0x01)
	// 2-4: Reserved
	// 4-8: Reserved
	// 8-16: TimeStamp (use server's timestamp from TargetInfo)
	// 16-24: ChallengeFromClient (random)
	// 24-28: Reserved
	// 28-: AvPairs (modified target info)

	blobSize := 28 + len(modifiedInfo) + 4 // +4 for trailing padding
	blob := make([]byte, blobSize)
	blob[0] = 0x01 // RespType
	blob[1] = 0x01 // HiRespType

	// Extract timestamp from server TargetInfo
	ts := extractAvPairValue(serverTargetInfo, 0x0007) // MsvAvTimestamp
	if ts != nil {
		copy(blob[8:16], ts)
	}

	// Random client challenge
	rand.Read(blob[16:24])

	// Copy modified AvPairs
	copy(blob[28:], modifiedInfo)

	return blob
}

// extractAvPairValue extracts the value of a specific AV_PAIR from TargetInfo.
func extractAvPairValue(targetInfo []byte, avID uint16) []byte {
	pos := 0
	for pos+4 <= len(targetInfo) {
		id := binary.LittleEndian.Uint16(targetInfo[pos : pos+2])
		length := binary.LittleEndian.Uint16(targetInfo[pos+2 : pos+4])
		if id == avID && pos+4+int(length) <= len(targetInfo) {
			return targetInfo[pos+4 : pos+4+int(length)]
		}
		if id == 0 { // MsvAvEOL
			break
		}
		pos += 4 + int(length)
	}
	return nil
}

// TestDebugNTLMv2HashComputation directly compares the NTLMv2 hash computation
// to find exactly where the mismatch happens.
func TestDebugNTLMv2HashComputation(t *testing.T) {
	password := "TestPass123!"
	username := "testuser"

	ntHash := ComputeNTHash(password)
	t.Logf("NT Hash: %x", ntHash)

	// Simulate server's TargetName
	hostname := "DESKTOP-TEST" // uppercase, as server sends
	targetName := encodeUTF16LETest(hostname)
	t.Logf("TargetName bytes: %x", targetName)

	// Client computation (go-smb2 style):
	// USER = UTF16LE(UPPER(username))
	// domain = targetName (raw UTF-16LE bytes)
	// NTLMv2Hash = HMAC-MD5(ntHash, USER + domain)
	USER := encodeUTF16LETest(strings.ToUpper(username))
	clientH := hmac.New(md5.New, ntHash[:])
	clientH.Write(USER)
	clientH.Write(targetName)
	clientHash := clientH.Sum(nil)

	// Server computation:
	// ComputeNTLMv2Hash(ntHash, username, hostname)
	// = HMAC-MD5(ntHash, UTF16LE(UPPER(username) + hostname))
	serverHash := ComputeNTLMv2Hash(ntHash, username, hostname)

	t.Logf("Client NTLMv2 Hash: %x", clientHash)
	t.Logf("Server NTLMv2 Hash: %x", serverHash)

	// Show the input bytes to HMAC
	clientInput := append(USER, targetName...)
	serverInput := encodeUTF16LETest(strings.ToUpper(username) + hostname)

	t.Logf("Client HMAC input: %x", clientInput)
	t.Logf("Server HMAC input: %x", serverInput)
	t.Logf("HMAC inputs equal: %v", bytes.Equal(clientInput, serverInput))

	if !bytes.Equal(clientInput, serverInput) {
		t.Logf("MISMATCH in HMAC inputs!")
		t.Logf("Client input len: %d", len(clientInput))
		t.Logf("Server input len: %d", len(serverInput))
		for i := 0; i < len(clientInput) && i < len(serverInput); i++ {
			if clientInput[i] != serverInput[i] {
				t.Logf("First diff at byte %d: client=0x%02x server=0x%02x", i, clientInput[i], serverInput[i])
				break
			}
		}
	}

	if bytes.Equal(clientHash, serverHash[:]) {
		t.Log("Hashes MATCH - issue is elsewhere")
	} else {
		t.Fatal("Hashes DO NOT MATCH - found the mismatch point")
	}

	// Also test with empty domain (another variant our server tries)
	serverHashEmpty := ComputeNTLMv2Hash(ntHash, username, "")
	t.Logf("Server NTLMv2 Hash (empty domain): %x", serverHashEmpty)

	// And "WORKGROUP"
	serverHashWG := ComputeNTLMv2Hash(ntHash, username, "WORKGROUP")
	t.Logf("Server NTLMv2 Hash (WORKGROUP): %x", serverHashWG)

	// Show what the client would use as combined input for each
	fmt.Println()
	t.Log("=== Comparing all domain variants ===")
	for _, d := range []string{"", hostname, "WORKGROUP"} {
		serverInput := encodeUTF16LETest(strings.ToUpper(username) + d)
		if serverInput == nil {
			serverInput = encodeUTF16LETest(strings.ToUpper(username))
		}
		t.Logf("Server domain=%q: input=%x", d, serverInput)
	}
}
