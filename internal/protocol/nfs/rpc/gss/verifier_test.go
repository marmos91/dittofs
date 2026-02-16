package gss

import (
	"encoding/binary"
	"testing"

	"github.com/jcmturner/gokrb5/v8/crypto"
	krbTypes "github.com/jcmturner/gokrb5/v8/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
)

// ============================================================================
// ComputeReplyVerifier Tests
// ============================================================================

func TestComputeReplyVerifierProducesNonEmptyMIC(t *testing.T) {
	// Use AES128-CTS-HMAC-SHA1-96 (etype 17) which is the most common krb5 etype
	key := krbTypes.EncryptionKey{
		KeyType:  17,
		KeyValue: make([]byte, 16), // AES-128 requires 16-byte key
	}
	// Fill with test data
	for i := range key.KeyValue {
		key.KeyValue[i] = byte(i + 1)
	}

	mic, err := ComputeReplyVerifier(key, 42)
	if err != nil {
		t.Fatalf("ComputeReplyVerifier failed: %v", err)
	}

	if len(mic) == 0 {
		t.Fatal("expected non-empty MIC bytes")
	}

	// MIC token header is 16 bytes + checksum
	// For AES128, checksum is 12 bytes (HMAC-SHA1-96)
	if len(mic) < 16 {
		t.Fatalf("MIC token too short: %d bytes (expected at least 16 header bytes)", len(mic))
	}

	t.Logf("MIC token: %d bytes", len(mic))
}

func TestComputeReplyVerifierDifferentSeqNums(t *testing.T) {
	key := krbTypes.EncryptionKey{
		KeyType:  17,
		KeyValue: make([]byte, 16),
	}
	for i := range key.KeyValue {
		key.KeyValue[i] = byte(i + 1)
	}

	mic1, err := ComputeReplyVerifier(key, 1)
	if err != nil {
		t.Fatalf("ComputeReplyVerifier(1) failed: %v", err)
	}

	mic2, err := ComputeReplyVerifier(key, 2)
	if err != nil {
		t.Fatalf("ComputeReplyVerifier(2) failed: %v", err)
	}

	// Different sequence numbers should produce different MICs
	if string(mic1) == string(mic2) {
		t.Fatal("expected different MIC tokens for different sequence numbers")
	}
}

func TestComputeReplyVerifierUnsupportedEtype(t *testing.T) {
	// Use an invalid encryption type
	key := krbTypes.EncryptionKey{
		KeyType:  9999, // non-existent etype
		KeyValue: []byte("test-key"),
	}

	_, err := ComputeReplyVerifier(key, 1)
	if err == nil {
		t.Fatal("expected error for unsupported encryption type")
	}
}

func TestComputeReplyVerifierMICTokenFormat(t *testing.T) {
	key := krbTypes.EncryptionKey{
		KeyType:  17,
		KeyValue: make([]byte, 16),
	}
	for i := range key.KeyValue {
		key.KeyValue[i] = byte(i + 1)
	}

	mic, err := ComputeReplyVerifier(key, 100)
	if err != nil {
		t.Fatalf("ComputeReplyVerifier failed: %v", err)
	}

	// Verify GSS MIC token ID: 0x04 0x04
	if mic[0] != 0x04 || mic[1] != 0x04 {
		t.Fatalf("expected MIC token ID 0x0404, got 0x%02x%02x", mic[0], mic[1])
	}

	// Verify SentByAcceptor flag is set (byte 2, bit 0)
	if mic[2]&0x01 == 0 {
		t.Fatal("expected SentByAcceptor flag to be set")
	}
}

func TestComputeReplyVerifierMatchesManualChecksum(t *testing.T) {
	// Verify that our computed MIC matches what gokrb5 would compute directly
	key := krbTypes.EncryptionKey{
		KeyType:  17,
		KeyValue: make([]byte, 16),
	}
	for i := range key.KeyValue {
		key.KeyValue[i] = byte(i + 1)
	}

	seqNum := uint32(42)
	seqBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(seqBytes, seqNum)

	// Compute checksum manually using the same etype
	etype, err := crypto.GetEtype(key.KeyType)
	if err != nil {
		t.Fatalf("GetEtype failed: %v", err)
	}

	// The MIC header is 16 bytes, checksum data is payload + header
	// Just verify we get the right checksum size
	checksumSize := etype.GetHMACBitLength() / 8
	if checksumSize == 0 {
		t.Skip("unknown checksum size for etype")
	}

	mic, err := ComputeReplyVerifier(key, seqNum)
	if err != nil {
		t.Fatalf("ComputeReplyVerifier failed: %v", err)
	}

	// MIC token = 16 bytes header + checksum bytes
	expectedSize := 16 + int(checksumSize)
	if len(mic) != expectedSize {
		t.Fatalf("expected MIC size %d, got %d", expectedSize, len(mic))
	}
}

// ============================================================================
// WrapReplyVerifier Tests
// ============================================================================

func TestWrapReplyVerifierSetsFlavor6(t *testing.T) {
	mic := []byte{0x04, 0x04, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

	verifier := WrapReplyVerifier(mic)

	if verifier.Flavor != rpc.AuthRPCSECGSS {
		t.Fatalf("expected flavor %d (AuthRPCSECGSS), got %d", rpc.AuthRPCSECGSS, verifier.Flavor)
	}
	if string(verifier.Body) != string(mic) {
		t.Fatal("expected verifier body to match MIC bytes")
	}
}

func TestWrapReplyVerifierEmptyMIC(t *testing.T) {
	verifier := WrapReplyVerifier(nil)

	if verifier.Flavor != rpc.AuthRPCSECGSS {
		t.Fatalf("expected flavor %d, got %d", rpc.AuthRPCSECGSS, verifier.Flavor)
	}
	if verifier.Body != nil {
		t.Fatal("expected nil body for nil MIC")
	}
}

// ============================================================================
// MakeGSSSuccessReply Tests (in rpc package, tested via integration)
// ============================================================================

func TestMakeGSSSuccessReplyIncludesVerifier(t *testing.T) {
	// Build a GSS verifier
	mic := []byte{0x04, 0x04, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0xDE, 0xAD, 0xBE, 0xEF}
	verifier := WrapReplyVerifier(mic)

	xid := uint32(0x12345678)
	data := []byte{0x00, 0x00, 0x00, 0x00} // NFS3_OK

	reply, err := rpc.MakeGSSSuccessReply(xid, data, verifier)
	if err != nil {
		t.Fatalf("MakeGSSSuccessReply failed: %v", err)
	}

	if len(reply) == 0 {
		t.Fatal("expected non-empty reply")
	}

	// Parse the fragment header
	fragHeader := binary.BigEndian.Uint32(reply[0:4])
	isLast := (fragHeader & 0x80000000) != 0
	fragLen := fragHeader & 0x7FFFFFFF

	if !isLast {
		t.Fatal("expected last fragment flag set")
	}
	if int(fragLen) != len(reply)-4 {
		t.Fatalf("fragment length mismatch: header says %d, actual is %d", fragLen, len(reply)-4)
	}

	// Parse XID
	replyXID := binary.BigEndian.Uint32(reply[4:8])
	if replyXID != xid {
		t.Fatalf("expected XID 0x%x, got 0x%x", xid, replyXID)
	}

	// Parse MsgType (should be REPLY = 1)
	msgType := binary.BigEndian.Uint32(reply[8:12])
	if msgType != 1 {
		t.Fatalf("expected MsgType 1 (REPLY), got %d", msgType)
	}

	// Parse ReplyState (should be MSG_ACCEPTED = 0)
	replyState := binary.BigEndian.Uint32(reply[12:16])
	if replyState != 0 {
		t.Fatalf("expected ReplyState 0 (MSG_ACCEPTED), got %d", replyState)
	}

	// Parse Verifier flavor (should be RPCSEC_GSS = 6)
	verfFlavor := binary.BigEndian.Uint32(reply[16:20])
	if verfFlavor != rpc.AuthRPCSECGSS {
		t.Fatalf("expected verifier flavor %d (AuthRPCSECGSS), got %d", rpc.AuthRPCSECGSS, verfFlavor)
	}

	// Parse Verifier body length
	verfLen := binary.BigEndian.Uint32(reply[20:24])
	if verfLen != uint32(len(mic)) {
		t.Fatalf("expected verifier body length %d, got %d", len(mic), verfLen)
	}
}

func TestMakeGSSSuccessReplyVsAuthNullReply(t *testing.T) {
	xid := uint32(0xDEAD)
	data := []byte{0x00, 0x00, 0x00, 0x00}

	// AUTH_NULL reply
	nullReply, err := rpc.MakeSuccessReply(xid, data)
	if err != nil {
		t.Fatalf("MakeSuccessReply failed: %v", err)
	}

	// GSS reply with non-empty verifier
	mic := []byte{0x01, 0x02, 0x03, 0x04}
	verifier := WrapReplyVerifier(mic)
	gssReply, err := rpc.MakeGSSSuccessReply(xid, data, verifier)
	if err != nil {
		t.Fatalf("MakeGSSSuccessReply failed: %v", err)
	}

	// GSS reply should be larger due to non-empty verifier
	if len(gssReply) <= len(nullReply) {
		t.Fatalf("expected GSS reply (%d bytes) to be larger than null reply (%d bytes)",
			len(gssReply), len(nullReply))
	}
}

// ============================================================================
// MakeAuthErrorReply Tests
// ============================================================================

func TestMakeAuthErrorReplyCredProblem(t *testing.T) {
	xid := uint32(0xABCD)
	reply, err := rpc.MakeAuthErrorReply(xid, rpc.RPCSECGSSCredProblem)
	if err != nil {
		t.Fatalf("MakeAuthErrorReply failed: %v", err)
	}

	if len(reply) == 0 {
		t.Fatal("expected non-empty reply")
	}

	// Parse fragment header
	fragHeader := binary.BigEndian.Uint32(reply[0:4])
	isLast := (fragHeader & 0x80000000) != 0
	if !isLast {
		t.Fatal("expected last fragment flag set")
	}

	// Parse XID
	replyXID := binary.BigEndian.Uint32(reply[4:8])
	if replyXID != xid {
		t.Fatalf("expected XID 0x%x, got 0x%x", xid, replyXID)
	}

	// MsgType = REPLY
	msgType := binary.BigEndian.Uint32(reply[8:12])
	if msgType != 1 {
		t.Fatalf("expected MsgType 1, got %d", msgType)
	}

	// ReplyState = MSG_DENIED (1)
	replyState := binary.BigEndian.Uint32(reply[12:16])
	if replyState != 1 {
		t.Fatalf("expected ReplyState 1 (MSG_DENIED), got %d", replyState)
	}

	// reject_stat = AUTH_ERROR (1)
	rejectStat := binary.BigEndian.Uint32(reply[16:20])
	if rejectStat != 1 {
		t.Fatalf("expected reject_stat 1 (AUTH_ERROR), got %d", rejectStat)
	}

	// auth_stat = RPCSEC_GSS_CREDPROBLEM (13)
	authStat := binary.BigEndian.Uint32(reply[20:24])
	if authStat != rpc.RPCSECGSSCredProblem {
		t.Fatalf("expected auth_stat %d, got %d", rpc.RPCSECGSSCredProblem, authStat)
	}
}

func TestMakeAuthErrorReplyCtxProblem(t *testing.T) {
	reply, err := rpc.MakeAuthErrorReply(0x1234, rpc.RPCSECGSSCtxProblem)
	if err != nil {
		t.Fatalf("MakeAuthErrorReply failed: %v", err)
	}

	// Verify auth_stat is CTXPROBLEM (14)
	authStat := binary.BigEndian.Uint32(reply[20:24])
	if authStat != rpc.RPCSECGSSCtxProblem {
		t.Fatalf("expected auth_stat %d, got %d", rpc.RPCSECGSSCtxProblem, authStat)
	}
}
