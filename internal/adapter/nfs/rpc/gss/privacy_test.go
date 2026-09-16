package gss

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/gssapi"
	krbTypes "github.com/jcmturner/gokrb5/v8/types"
)

// buildInitiatorPrivData builds an rpc_gss_priv_data from the initiator (client) side.
// This is what the server's UnwrapPrivacy must parse.
// Per RFC 4121 Section 4.2.4, for sealed (encrypted) wrap tokens:
//
//	Wire format: header (16 bytes) | encrypt(plaintext | filler | header_copy)
func buildInitiatorPrivData(t *testing.T, key krbTypes.EncryptionKey, seqNum uint32, args []byte) []byte {
	t.Helper()

	// Build plaintext: seq_num + args
	plaintext := make([]byte, 4+len(args))
	binary.BigEndian.PutUint32(plaintext[0:4], seqNum)
	copy(plaintext[4:], args)

	// Get encryption type
	encType, err := crypto.GetEtype(key.KeyType)
	if err != nil {
		t.Fatalf("GetEtype: %v", err)
	}

	// Build Wrap token header (16 bytes)
	// Flags: Initiator (0x00) | Sealed (0x02) = 0x02
	flags := byte(wrapFlagSealed) // Initiator + Sealed
	ec := uint16(0)               // No filler for simplicity
	rrc := uint16(0)              // No rotation

	header := make([]byte, wrapTokenHdrLen)
	header[0] = 0x05 // Token ID high byte
	header[1] = 0x04 // Token ID low byte
	header[2] = flags
	header[3] = 0xFF // Filler byte
	binary.BigEndian.PutUint16(header[4:6], ec)
	binary.BigEndian.PutUint16(header[6:8], rrc)
	binary.BigEndian.PutUint64(header[8:16], uint64(seqNum))

	// Build header_copy for encryption: the wire header with RRC zeroed. EC
	// keeps its real value so the receiver can bind the cleartext EC to it.
	headerCopy := make([]byte, wrapTokenHdrLen)
	copy(headerCopy, header)
	binary.BigEndian.PutUint16(headerCopy[6:8], 0) // RRC = 0 in copy

	// Build to-be-encrypted: plaintext | filler | header_copy
	toEncrypt := make([]byte, len(plaintext)+wrapTokenHdrLen)
	copy(toEncrypt, plaintext)
	copy(toEncrypt[len(plaintext):], headerCopy)

	// Encrypt using initiator seal key usage (24)
	_, ciphertext, err := encType.EncryptMessage(key.KeyValue, toEncrypt, KeyUsageInitiatorSeal)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	// Build wire format: header | ciphertext
	wrapTokenBytes := make([]byte, wrapTokenHdrLen+len(ciphertext))
	copy(wrapTokenBytes, header)
	copy(wrapTokenBytes[wrapTokenHdrLen:], ciphertext)

	// Encode rpc_gss_priv_data
	var buf bytes.Buffer
	_ = writeOpaque(&buf, wrapTokenBytes)
	return buf.Bytes()
}

// buildInitiatorPrivDataNonSealed builds a non-sealed (integrity-only) wrap token.
// This uses gokrb5's WrapToken which doesn't encrypt.
func buildInitiatorPrivDataNonSealed(t *testing.T, key krbTypes.EncryptionKey, seqNum uint32, args []byte) []byte {
	t.Helper()

	// Build plaintext: seq_num + args
	plaintext := make([]byte, 4+len(args))
	binary.BigEndian.PutUint32(plaintext[0:4], seqNum)
	copy(plaintext[4:], args)

	// Get encryption type for EC
	encType, err := crypto.GetEtype(key.KeyType)
	if err != nil {
		t.Fatalf("GetEtype: %v", err)
	}

	// Create WrapToken as initiator (non-sealed)
	wrapToken := gssapi.WrapToken{
		Flags:     0x00, // Initiator, NOT sealed
		EC:        uint16(encType.GetHMACBitLength() / 8),
		RRC:       0,
		SndSeqNum: uint64(seqNum),
		Payload:   plaintext,
	}
	if err := wrapToken.SetCheckSum(key, KeyUsageInitiatorSeal); err != nil {
		t.Fatalf("compute WrapToken checksum: %v", err)
	}
	wrapTokenBytes, err := wrapToken.Marshal()
	if err != nil {
		t.Fatalf("marshal WrapToken: %v", err)
	}

	// Encode rpc_gss_priv_data
	var buf bytes.Buffer
	_ = writeOpaque(&buf, wrapTokenBytes)
	return buf.Bytes()
}

// ============================================================================
// UnwrapPrivacy Tests (server unwraps client request)
// ============================================================================

func TestUnwrapPrivacyValidRequest(t *testing.T) {
	key := testSessionKey()
	seqNum := uint32(42)
	originalArgs := []byte("test-procedure-arguments")

	requestBody := buildInitiatorPrivData(t, key, seqNum, originalArgs)

	args, bodySeqNum, err := UnwrapPrivacy(key, seqNum, requestBody)
	if err != nil {
		t.Fatalf("UnwrapPrivacy failed: %v", err)
	}

	if bodySeqNum != seqNum {
		t.Fatalf("expected seq_num %d, got %d", seqNum, bodySeqNum)
	}

	if !bytes.Equal(args, originalArgs) {
		t.Fatalf("expected args %q, got %q", originalArgs, args)
	}
}

func TestUnwrapPrivacyEmptyArgs(t *testing.T) {
	key := testSessionKey()
	seqNum := uint32(1)
	originalArgs := []byte{}

	requestBody := buildInitiatorPrivData(t, key, seqNum, originalArgs)

	args, bodySeqNum, err := UnwrapPrivacy(key, seqNum, requestBody)
	if err != nil {
		t.Fatalf("UnwrapPrivacy failed: %v", err)
	}

	if bodySeqNum != seqNum {
		t.Fatalf("expected seq_num %d, got %d", seqNum, bodySeqNum)
	}

	if len(args) != 0 {
		t.Fatalf("expected empty args, got %d bytes", len(args))
	}
}

func TestUnwrapPrivacyLargePayload(t *testing.T) {
	key := testSessionKey()
	seqNum := uint32(100)
	originalArgs := make([]byte, 65536)
	for i := range originalArgs {
		originalArgs[i] = byte(i % 256)
	}

	requestBody := buildInitiatorPrivData(t, key, seqNum, originalArgs)

	args, _, err := UnwrapPrivacy(key, seqNum, requestBody)
	if err != nil {
		t.Fatalf("UnwrapPrivacy failed: %v", err)
	}

	if !bytes.Equal(args, originalArgs) {
		t.Fatal("payload mismatch for large data")
	}
}

// ============================================================================
// UnwrapPrivacy Rejection Tests
// ============================================================================

func TestUnwrapPrivacyRejectsCorruptedData(t *testing.T) {
	key := testSessionKey()
	seqNum := uint32(42)
	originalArgs := []byte("test-procedure-arguments")

	requestBody := buildInitiatorPrivData(t, key, seqNum, originalArgs)

	// Corrupt the wrap token data inside the XDR opaque
	if len(requestBody) > 24 {
		requestBody[20] ^= 0xFF
	}

	_, _, err := UnwrapPrivacy(key, seqNum, requestBody)
	if err == nil {
		t.Fatal("expected error for corrupted data")
	}
}

func TestUnwrapPrivacyRejectsWrongSeqNum(t *testing.T) {
	key := testSessionKey()
	seqNum := uint32(42)
	originalArgs := []byte("test-data")

	requestBody := buildInitiatorPrivData(t, key, seqNum, originalArgs)

	// Try to unwrap with a different credential seq_num (43)
	_, _, err := UnwrapPrivacy(key, 43, requestBody)
	if err == nil {
		t.Fatal("expected error for seq_num mismatch")
	}
}

func TestUnwrapPrivacyRejectsWrongKey(t *testing.T) {
	key1 := testSessionKey()
	key2 := krbTypes.EncryptionKey{
		KeyType:  17,
		KeyValue: make([]byte, 16),
	}
	for i := range key2.KeyValue {
		key2.KeyValue[i] = byte(i + 100)
	}

	seqNum := uint32(42)
	originalArgs := []byte("test-data")

	requestBody := buildInitiatorPrivData(t, key1, seqNum, originalArgs)

	// Try to unwrap with key2
	_, _, err := UnwrapPrivacy(key2, seqNum, requestBody)
	if err == nil {
		t.Fatal("expected error for wrong key")
	}
}

func TestUnwrapPrivacyRejectsTruncatedData(t *testing.T) {
	_, _, err := UnwrapPrivacy(testSessionKey(), 1, []byte{0x00, 0x00})
	if err == nil {
		t.Fatal("expected error for truncated data")
	}
}

// ============================================================================
// WrapPrivacy Format Tests (server wraps reply for client)
// ============================================================================

func TestWrapPrivacyProducesValidFormat(t *testing.T) {
	key := testSessionKey()
	seqNum := uint32(7)
	args := []byte("hello")

	wrapped, err := WrapPrivacy(key, seqNum, args, false)
	if err != nil {
		t.Fatalf("WrapPrivacy failed: %v", err)
	}

	reader := bytes.NewReader(wrapped)

	// Field: databody_priv as XDR opaque (contains WrapToken)
	var wrapTokenLen uint32
	if err := binary.Read(reader, binary.BigEndian, &wrapTokenLen); err != nil {
		t.Fatalf("read wrap token length: %v", err)
	}

	if wrapTokenLen == 0 {
		t.Fatal("expected non-zero wrap token length")
	}

	wrapTokenBytes := make([]byte, wrapTokenLen)
	if _, err := reader.Read(wrapTokenBytes); err != nil {
		t.Fatalf("read wrap token: %v", err)
	}

	// Verify it's a valid Wrap token (starts with 0x05 0x04)
	if len(wrapTokenBytes) < 16 {
		t.Fatalf("wrap token too short: %d bytes", len(wrapTokenBytes))
	}
	if wrapTokenBytes[0] != 0x05 || wrapTokenBytes[1] != 0x04 {
		t.Fatalf("expected Wrap token ID 0x0504, got 0x%02x%02x", wrapTokenBytes[0], wrapTokenBytes[1])
	}

	// Verify SentByAcceptor flag is set (server reply)
	if wrapTokenBytes[2]&0x01 == 0 {
		t.Fatal("expected SentByAcceptor flag to be set")
	}
}

func TestWrapPrivacyVerifiableByClient(t *testing.T) {
	key := testSessionKey()
	seqNum := uint32(42)
	replyBody := []byte("nfs-reply-data")

	wrapped, err := WrapPrivacy(key, seqNum, replyBody, false)
	if err != nil {
		t.Fatalf("WrapPrivacy failed: %v", err)
	}

	// Parse as a client would: extract the WrapToken from XDR opaque
	reader := bytes.NewReader(wrapped)
	wrapTokenBytes, err := readXDROpaque(reader)
	if err != nil {
		t.Fatalf("read databody_priv: %v", err)
	}

	// Verify minimum length and token ID
	if len(wrapTokenBytes) < wrapTokenHdrLen {
		t.Fatalf("wrap token too short: %d bytes", len(wrapTokenBytes))
	}
	if wrapTokenBytes[0] != 0x05 || wrapTokenBytes[1] != 0x04 {
		t.Fatalf("expected Wrap token ID 0x0504, got 0x%02x%02x", wrapTokenBytes[0], wrapTokenBytes[1])
	}

	// Parse header
	flags := wrapTokenBytes[2]
	if flags&wrapFlagSealed == 0 {
		t.Fatal("expected Sealed flag to be set")
	}
	if flags&wrapFlagSentByAcceptor == 0 {
		t.Fatal("expected SentByAcceptor flag to be set")
	}

	// For sealed tokens, client must decrypt
	ec := binary.BigEndian.Uint16(wrapTokenBytes[4:6])
	rrc := binary.BigEndian.Uint16(wrapTokenBytes[6:8])
	tokenSeqNum := binary.BigEndian.Uint64(wrapTokenBytes[8:16])

	if tokenSeqNum != uint64(seqNum) {
		t.Fatalf("expected seq_num %d, got %d", seqNum, tokenSeqNum)
	}

	// Get ciphertext and undo RRC rotation
	ciphertext := wrapTokenBytes[wrapTokenHdrLen:]
	if rrc > 0 && len(ciphertext) > 0 {
		ciphertext = rotateLeft(ciphertext, int(rrc))
	}

	// Decrypt using acceptor seal key usage (22)
	decrypted, err := crypto.DecryptMessage(ciphertext, key, KeyUsageAcceptorSeal)
	if err != nil {
		t.Fatalf("decrypt failed: %v", err)
	}

	// Extract plaintext (before header_copy)
	if len(decrypted) < wrapTokenHdrLen {
		t.Fatalf("decrypted data too short: %d bytes", len(decrypted))
	}
	fillerSize := int(ec)
	plaintextEnd := len(decrypted) - wrapTokenHdrLen - fillerSize
	if plaintextEnd < 0 {
		t.Fatalf("invalid EC value %d", ec)
	}
	plaintext := decrypted[:plaintextEnd]

	// Extract reply body from plaintext
	if len(plaintext) < 4 {
		t.Fatalf("plaintext too short: %d bytes", len(plaintext))
	}
	bodySeqNum := binary.BigEndian.Uint32(plaintext[0:4])
	if bodySeqNum != seqNum {
		t.Fatalf("expected seq_num %d, got %d", seqNum, bodySeqNum)
	}
	extractedReply := plaintext[4:]
	if !bytes.Equal(extractedReply, replyBody) {
		t.Fatalf("expected reply %q, got %q", replyBody, extractedReply)
	}
}

func TestWrapPrivacySealedFlagSet(t *testing.T) {
	key := testSessionKey()

	wrapped, err := WrapPrivacy(key, 1, []byte("test"), false)
	if err != nil {
		t.Fatalf("WrapPrivacy failed: %v", err)
	}

	// Parse the XDR opaque to get the WrapToken
	reader := bytes.NewReader(wrapped)
	var tokenLen uint32
	_ = binary.Read(reader, binary.BigEndian, &tokenLen)
	tokenBytes := make([]byte, tokenLen)
	_, _ = reader.Read(tokenBytes)

	// Verify Sealed flag is set (for krb5p encryption)
	flags := tokenBytes[2]
	if flags&wrapFlagSealed == 0 {
		t.Fatal("expected Sealed flag to be set for krb5p")
	}
	if flags&wrapFlagSentByAcceptor == 0 {
		t.Fatal("expected SentByAcceptor flag to be set")
	}

	// For sealed tokens, EC = filler size (we use 0)
	ec := binary.BigEndian.Uint16(tokenBytes[4:6])
	if ec != 0 {
		t.Fatalf("expected EC=0 (no filler), got %d", ec)
	}
}

// privacyWrapFlags wraps a reply via WrapPrivacy and returns the flags byte of
// the Wrap token header (byte 2 of the XDR opaque databody_priv).
func privacyWrapFlags(t *testing.T, key krbTypes.EncryptionKey, hasAcceptorSubkey bool) byte {
	t.Helper()
	wrapped, err := WrapPrivacy(key, 1, []byte("test"), hasAcceptorSubkey)
	if err != nil {
		t.Fatalf("WrapPrivacy failed: %v", err)
	}
	reader := bytes.NewReader(wrapped)
	tokenBytes, err := readXDROpaque(reader)
	if err != nil {
		t.Fatalf("read databody_priv: %v", err)
	}
	if len(tokenBytes) < wrapTokenHdrLen {
		t.Fatalf("wrap token too short: %d bytes", len(tokenBytes))
	}
	return tokenBytes[2]
}

// TestWrapPrivacySetsAcceptorSubkeyFlag asserts that the krb5p reply Wrap token
// carries FLAG_ACCEPTOR_SUBKEY (0x04) iff the context was established with an
// acceptor subkey (RFC 4121 §4.2.2).
func TestWrapPrivacySetsAcceptorSubkeyFlag(t *testing.T) {
	key := testSessionKey()

	if flags := privacyWrapFlags(t, key, true); flags&wrapFlagAcceptorSubkey == 0 {
		t.Fatalf("expected AcceptorSubkey flag set, got flags 0x%02x", flags)
	}
	if flags := privacyWrapFlags(t, key, false); flags&wrapFlagAcceptorSubkey != 0 {
		t.Fatalf("expected AcceptorSubkey flag clear, got flags 0x%02x", flags)
	}
}

// ============================================================================
// Non-sealed Wrap token tests (backward compatibility)
// ============================================================================

// An integrity-only Wrap token carries its payload in the clear, and the Sealed
// flag that says so lives in the unencrypted header — nothing binds it to the
// service the client declared. Accepting one on the krb5p path delivers no
// confidentiality at all while the call is accounted for as privacy-protected,
// so the unwrap must refuse it and the arguments must never reach NFS.
func TestUnwrapPrivacyRejectsNonSealedToken(t *testing.T) {
	key := testSessionKey()
	seqNum := uint32(42)
	originalArgs := []byte("SECRET-FILENAME-AND-WRITE-DATA")

	requestBody := buildInitiatorPrivDataNonSealed(t, key, seqNum, originalArgs)

	// The payload really is on the wire in the clear — that is the whole point.
	if !bytes.Contains(requestBody, originalArgs) {
		t.Fatal("expected the non-sealed token to carry its payload unencrypted")
	}

	args, _, err := UnwrapPrivacy(key, seqNum, requestBody)
	if err == nil {
		t.Fatalf("krb5p accepted an unencrypted token and returned %q as procedure arguments", args)
	}
	if args != nil {
		t.Fatalf("rejected token still yielded %d argument octets", len(args))
	}
}

// ============================================================================
// Integration with framework.go (verifying handleData routes krb5p)
// ============================================================================

func TestHandleDataWithPrivacy(t *testing.T) {
	key := testSessionKey()
	verifier := newMockVerifier("bob", "EXAMPLE.COM")
	verifier.sessionKey = key
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	// Create context with svc_privacy
	initCred := &RPCGSSCredV1{
		GSSProc: RPCGSSInit,
		SeqNum:  0,
		Service: RPCGSSSvcPrivacy,
		Handle:  nil,
	}
	initCredBody, err := EncodeGSSCred(initCred)
	if err != nil {
		t.Fatalf("encode INIT cred: %v", err)
	}

	initResult := proc.Process(context.Background(), initCredBody, nil, nil, encodeOpaqueToken([]byte("mock-token")))
	if initResult.Err != nil {
		t.Fatalf("INIT failed: %v", initResult.Err)
	}

	// Get the handle
	handle := extractContextHandle(t, proc)

	// Build privacy-wrapped request body as initiator
	procedureArgs := []byte("test-nfs-procedure-data")
	seqNum := uint32(1)
	requestBody := buildInitiatorPrivData(t, key, seqNum, procedureArgs)

	// Build DATA credential
	dataCred := &RPCGSSCredV1{
		GSSProc: RPCGSSData,
		SeqNum:  seqNum,
		Service: RPCGSSSvcPrivacy,
		Handle:  handle,
	}
	dataCredBody, err := EncodeGSSCred(dataCred)
	if err != nil {
		t.Fatalf("encode DATA cred: %v", err)
	}

	// Sign the call-header MIC (the credBody stands in as the header preimage).
	mic := signHeaderMIC(t, key, dataCredBody)
	result := proc.Process(context.Background(), dataCredBody, mic, dataCredBody, requestBody)

	if result.Err != nil {
		t.Fatalf("DATA with privacy failed: %v", result.Err)
	}
	if result.IsControl {
		t.Fatal("expected IsControl=false for DATA")
	}
	if !bytes.Equal(result.ProcessedData, procedureArgs) {
		t.Fatalf("expected processed data %q, got %q", procedureArgs, result.ProcessedData)
	}
	if result.Service != RPCGSSSvcPrivacy {
		t.Fatalf("expected service %d, got %d", RPCGSSSvcPrivacy, result.Service)
	}
}

// A sealed Wrap token's 16-byte header travels in the clear; only the copy
// appended inside the ciphertext is integrity-protected. EC is what trims the
// filler off the decrypted plaintext, so raising it on the wire shortens an
// authenticated message without the attacker holding any key. UnwrapPrivacy
// must reject such a token rather than hand a truncated body to NFS.
func TestUnwrapPrivacyRejectsTamperedEC(t *testing.T) {
	key := testSessionKey()
	seqNum := uint32(42)
	originalArgs := []byte("procedure-arguments-that-must-not-be-truncated")

	requestBody := buildInitiatorPrivData(t, key, seqNum, originalArgs)

	// The token is accepted intact.
	args, _, err := UnwrapPrivacy(key, seqNum, requestBody)
	if err != nil {
		t.Fatalf("untampered UnwrapPrivacy failed: %v", err)
	}
	if !bytes.Equal(args, originalArgs) {
		t.Fatalf("untampered args mismatch: got %q, want %q", args, originalArgs)
	}

	// Raise EC in the cleartext header. requestBody is an XDR opaque: a 4-byte
	// length prefix then the Wrap token, whose EC field is octets 4..6.
	const stolenOctets = 8
	tampered := append([]byte(nil), requestBody...)
	binary.BigEndian.PutUint16(tampered[8:10], stolenOctets)

	args, _, err = UnwrapPrivacy(key, seqNum, tampered)
	if err == nil {
		t.Fatalf("tampered EC accepted: returned %d argument octets (original %d) with no integrity failure",
			len(args), len(originalArgs))
	}
}

// RRC is applied to the ciphertext after encryption, so the encrypted copy of
// the header carries zero there while the wire header carries the real count.
// A receiver that compared the two fields verbatim would reject every rotated
// token a real client sends.
func TestUnwrapPrivacyAcceptsRotatedToken(t *testing.T) {
	key := testSessionKey()
	seqNum := uint32(7)
	originalArgs := []byte("rotated-payload")

	requestBody := buildInitiatorPrivData(t, key, seqNum, originalArgs)

	// Rotate the ciphertext right by rrc octets — the inverse of the rotateLeft
	// the receiver applies — and record the count in the cleartext header,
	// exactly as RFC 4121 section 4.2.5 describes.
	const rrc = 5
	tokenLen := binary.BigEndian.Uint32(requestBody[0:4])
	token := append([]byte(nil), requestBody[4:4+tokenLen]...)
	body := token[wrapTokenHdrLen:]
	copy(body, rotateLeft(body, len(body)-rrc))
	binary.BigEndian.PutUint16(token[6:8], rrc)

	var buf bytes.Buffer
	if err := writeOpaque(&buf, token); err != nil {
		t.Fatalf("writeOpaque: %v", err)
	}

	args, _, err := UnwrapPrivacy(key, seqNum, buf.Bytes())
	if err != nil {
		t.Fatalf("rotated token rejected: %v", err)
	}
	if !bytes.Equal(args, originalArgs) {
		t.Fatalf("rotated args mismatch: got %q, want %q", args, originalArgs)
	}
}

// The RPC-level statement of the same rule: a client that negotiated krb5p and
// declares svc_privacy on the call, but sends an integrity-only token, must
// have the call refused. Before the Sealed-flag check this succeeded — the
// server returned the cleartext payload as the procedure arguments and resolved
// an identity for it, so NFS executed a call whose filenames and write data had
// crossed the wire unencrypted on a share configured to require privacy.
func TestProcessDATARefusesUnsealedTokenOnPrivacyService(t *testing.T) {
	key := testSessionKey()
	verifier := newMockVerifier("bob", "EXAMPLE.COM")
	verifier.sessionKey = key
	proc := NewGSSProcessor(verifier, newTestMapper(), 100, 10*time.Minute)
	defer proc.Stop()

	handle := establishContext(t, proc, RPCGSSSvcPrivacy)

	const seqNum = uint32(1)
	secret := []byte("SECRET-FILENAME-AND-WRITE-DATA")
	requestBody := buildInitiatorPrivDataNonSealed(t, key, seqNum, secret)

	dataCred := &RPCGSSCredV1{
		GSSProc: RPCGSSData,
		SeqNum:  seqNum,
		Service: RPCGSSSvcPrivacy,
		Handle:  handle,
	}
	dataCredBody, err := EncodeGSSCred(dataCred)
	if err != nil {
		t.Fatalf("encode DATA cred: %v", err)
	}

	// Everything else about the call is genuine: the header MIC verifies and the
	// declared service matches the one negotiated at INIT. Only the token fails
	// to deliver the confidentiality that service promises.
	res := processDATA(t, proc, key, dataCredBody, requestBody)

	if res.Err == nil {
		t.Fatalf("privacy-service DATA accepted an unencrypted token; server returned %q as procedure arguments",
			res.ProcessedData)
	}
	if res.ProcessedData != nil {
		t.Fatalf("refused call still yielded %d octets of procedure data", len(res.ProcessedData))
	}
	if res.Identity != nil {
		t.Fatal("refused call still resolved an identity")
	}
}
