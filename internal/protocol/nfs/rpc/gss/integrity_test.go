package gss

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/gssapi"
	krbTypes "github.com/jcmturner/gokrb5/v8/types"
)

// testSessionKey creates a valid AES128-CTS-HMAC-SHA1-96 session key for testing.
func testSessionKey() krbTypes.EncryptionKey {
	key := krbTypes.EncryptionKey{
		KeyType:  17, // aes128-cts-hmac-sha1-96
		KeyValue: make([]byte, 16),
	}
	for i := range key.KeyValue {
		key.KeyValue[i] = byte(i + 1)
	}
	return key
}

// buildInitiatorIntegData builds an rpc_gss_integ_data from the initiator (client) side.
// This is what the server's UnwrapIntegrity must parse.
func buildInitiatorIntegData(t *testing.T, key krbTypes.EncryptionKey, seqNum uint32, args []byte) []byte {
	t.Helper()

	// Build databody_integ: seq_num + args
	databody := make([]byte, 4+len(args))
	binary.BigEndian.PutUint32(databody[0:4], seqNum)
	copy(databody[4:], args)

	// Compute MIC as initiator (flag = 0, key usage = initiator sign = 23)
	micToken := gssapi.MICToken{
		Flags:     0x00, // Initiator
		SndSeqNum: uint64(seqNum),
		Payload:   databody,
	}
	if err := micToken.SetChecksum(key, KeyUsageInitiatorSign); err != nil {
		t.Fatalf("compute initiator MIC: %v", err)
	}
	micBytes, err := micToken.Marshal()
	if err != nil {
		t.Fatalf("marshal initiator MIC: %v", err)
	}

	// Encode rpc_gss_integ_data
	var buf bytes.Buffer
	_ = writeOpaque(&buf, databody)
	_ = writeOpaque(&buf, micBytes)
	return buf.Bytes()
}

// ============================================================================
// UnwrapIntegrity Tests (server unwraps client request)
// ============================================================================

func TestUnwrapIntegrityValidRequest(t *testing.T) {
	key := testSessionKey()
	seqNum := uint32(42)
	originalArgs := []byte("test-procedure-arguments")

	requestBody := buildInitiatorIntegData(t, key, seqNum, originalArgs)

	args, bodySeqNum, err := UnwrapIntegrity(key, seqNum, requestBody)
	if err != nil {
		t.Fatalf("UnwrapIntegrity failed: %v", err)
	}

	if bodySeqNum != seqNum {
		t.Fatalf("expected seq_num %d, got %d", seqNum, bodySeqNum)
	}

	if !bytes.Equal(args, originalArgs) {
		t.Fatalf("expected args %q, got %q", originalArgs, args)
	}
}

func TestUnwrapIntegrityEmptyArgs(t *testing.T) {
	key := testSessionKey()
	seqNum := uint32(1)
	originalArgs := []byte{} // empty procedure args

	requestBody := buildInitiatorIntegData(t, key, seqNum, originalArgs)

	args, bodySeqNum, err := UnwrapIntegrity(key, seqNum, requestBody)
	if err != nil {
		t.Fatalf("UnwrapIntegrity failed: %v", err)
	}

	if bodySeqNum != seqNum {
		t.Fatalf("expected seq_num %d, got %d", seqNum, bodySeqNum)
	}

	if len(args) != 0 {
		t.Fatalf("expected empty args, got %d bytes", len(args))
	}
}

func TestUnwrapIntegrityLargePayload(t *testing.T) {
	key := testSessionKey()
	seqNum := uint32(100)
	originalArgs := make([]byte, 65536)
	for i := range originalArgs {
		originalArgs[i] = byte(i % 256)
	}

	requestBody := buildInitiatorIntegData(t, key, seqNum, originalArgs)

	args, _, err := UnwrapIntegrity(key, seqNum, requestBody)
	if err != nil {
		t.Fatalf("UnwrapIntegrity failed: %v", err)
	}

	if !bytes.Equal(args, originalArgs) {
		t.Fatal("payload mismatch for large data")
	}
}

// ============================================================================
// UnwrapIntegrity Rejection Tests
// ============================================================================

func TestUnwrapIntegrityRejectsTamperedData(t *testing.T) {
	key := testSessionKey()
	seqNum := uint32(42)
	originalArgs := []byte("test-procedure-arguments")

	requestBody := buildInitiatorIntegData(t, key, seqNum, originalArgs)

	// Tamper with the databody_integ inside the XDR opaque
	if len(requestBody) > 10 {
		requestBody[8] ^= 0xFF // Flip a byte inside the databody
	}

	_, _, err := UnwrapIntegrity(key, seqNum, requestBody)
	if err == nil {
		t.Fatal("expected error for tampered data")
	}
}

func TestUnwrapIntegrityRejectsWrongSeqNum(t *testing.T) {
	key := testSessionKey()
	seqNum := uint32(42)
	originalArgs := []byte("test-procedure-arguments")

	requestBody := buildInitiatorIntegData(t, key, seqNum, originalArgs)

	// Try to unwrap with a different credential seq_num (43)
	_, _, err := UnwrapIntegrity(key, 43, requestBody)
	if err == nil {
		t.Fatal("expected error for seq_num mismatch")
	}
}

func TestUnwrapIntegrityRejectsWrongKey(t *testing.T) {
	key1 := testSessionKey()
	key2 := krbTypes.EncryptionKey{
		KeyType:  17,
		KeyValue: make([]byte, 16),
	}
	for i := range key2.KeyValue {
		key2.KeyValue[i] = byte(i + 100) // Different key
	}

	seqNum := uint32(42)
	originalArgs := []byte("test-data")

	// Build with key1
	requestBody := buildInitiatorIntegData(t, key1, seqNum, originalArgs)

	// Try to unwrap with key2
	_, _, err := UnwrapIntegrity(key2, seqNum, requestBody)
	if err == nil {
		t.Fatal("expected error for wrong key")
	}
}

func TestUnwrapIntegrityRejectsTruncatedData(t *testing.T) {
	_, _, err := UnwrapIntegrity(testSessionKey(), 1, []byte{0x00, 0x00})
	if err == nil {
		t.Fatal("expected error for truncated data")
	}
}

// ============================================================================
// WrapIntegrity Format Tests (server wraps reply for client)
// ============================================================================

func TestWrapIntegrityProducesValidFormat(t *testing.T) {
	key := testSessionKey()
	seqNum := uint32(7)
	args := []byte("hello")

	wrapped, err := WrapIntegrity(key, seqNum, args)
	if err != nil {
		t.Fatalf("WrapIntegrity failed: %v", err)
	}

	reader := bytes.NewReader(wrapped)

	// First field: databody_integ as XDR opaque
	var databodyLen uint32
	if err := binary.Read(reader, binary.BigEndian, &databodyLen); err != nil {
		t.Fatalf("read databody length: %v", err)
	}

	// databody_integ should be 4 (seq_num) + len(args)
	expectedDatabodyLen := uint32(4 + len(args))
	if databodyLen != expectedDatabodyLen {
		t.Fatalf("expected databody length %d, got %d", expectedDatabodyLen, databodyLen)
	}

	// Read databody_integ
	databody := make([]byte, databodyLen)
	if _, err := reader.Read(databody); err != nil {
		t.Fatalf("read databody: %v", err)
	}

	// Skip padding
	padding := (4 - (databodyLen % 4)) % 4
	for range int(padding) {
		_, _ = reader.ReadByte()
	}

	// Verify seq_num in databody
	bodySeqNum := binary.BigEndian.Uint32(databody[0:4])
	if bodySeqNum != seqNum {
		t.Fatalf("expected seq_num %d in databody, got %d", seqNum, bodySeqNum)
	}

	// Verify args in databody
	if !bytes.Equal(databody[4:], args) {
		t.Fatalf("expected args %q in databody, got %q", args, databody[4:])
	}

	// Second field: checksum (MIC token) as XDR opaque
	var checksumLen uint32
	if err := binary.Read(reader, binary.BigEndian, &checksumLen); err != nil {
		t.Fatalf("read checksum length: %v", err)
	}

	if checksumLen == 0 {
		t.Fatal("expected non-zero checksum length")
	}

	checksumBytes := make([]byte, checksumLen)
	if _, err := reader.Read(checksumBytes); err != nil {
		t.Fatalf("read checksum: %v", err)
	}

	// Verify it's a valid MIC token (starts with 0x04 0x04)
	if len(checksumBytes) < 16 {
		t.Fatalf("checksum too short for MIC token: %d bytes", len(checksumBytes))
	}
	if checksumBytes[0] != 0x04 || checksumBytes[1] != 0x04 {
		t.Fatalf("expected MIC token ID 0x0404, got 0x%02x%02x", checksumBytes[0], checksumBytes[1])
	}

	// Verify acceptor flag is set (this is a server reply)
	if checksumBytes[2]&0x01 == 0 {
		t.Fatal("expected SentByAcceptor flag in MIC token")
	}
}

func TestWrapIntegrityVerifiableByClient(t *testing.T) {
	key := testSessionKey()
	seqNum := uint32(42)
	replyBody := []byte("nfs-reply-data")

	wrapped, err := WrapIntegrity(key, seqNum, replyBody)
	if err != nil {
		t.Fatalf("WrapIntegrity failed: %v", err)
	}

	// Parse as a client would: extract databody_integ and checksum
	reader := bytes.NewReader(wrapped)
	databody, err := readXDROpaque(reader)
	if err != nil {
		t.Fatalf("read databody: %v", err)
	}
	checksumBytes, err := readXDROpaque(reader)
	if err != nil {
		t.Fatalf("read checksum: %v", err)
	}

	// Client verifies the MIC using acceptor sign key usage (25)
	var micToken gssapi.MICToken
	if err := micToken.Unmarshal(checksumBytes, true /* from acceptor */); err != nil {
		t.Fatalf("unmarshal MIC from acceptor: %v", err)
	}
	micToken.Payload = databody

	ok, err := micToken.Verify(key, KeyUsageAcceptorSign)
	if err != nil {
		t.Fatalf("verify MIC failed: %v", err)
	}
	if !ok {
		t.Fatal("MIC verification returned false")
	}

	// Extract reply body from databody
	bodySeqNum := binary.BigEndian.Uint32(databody[0:4])
	if bodySeqNum != seqNum {
		t.Fatalf("expected seq_num %d, got %d", seqNum, bodySeqNum)
	}
	extractedReply := databody[4:]
	if !bytes.Equal(extractedReply, replyBody) {
		t.Fatalf("expected reply %q, got %q", replyBody, extractedReply)
	}
}

// ============================================================================
// Integration with framework.go (verifying handleData routes krb5i)
// ============================================================================

func TestHandleDataWithIntegrity(t *testing.T) {
	key := testSessionKey()
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	verifier.sessionKey = key
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	// Create context with svc_integrity
	initCred := &RPCGSSCredV1{
		GSSProc: RPCGSSInit,
		SeqNum:  0,
		Service: RPCGSSSvcIntegrity,
		Handle:  nil,
	}
	initCredBody, err := EncodeGSSCred(initCred)
	if err != nil {
		t.Fatalf("encode INIT cred: %v", err)
	}

	initResult := proc.Process(initCredBody, nil, encodeOpaqueToken([]byte("mock-token")))
	if initResult.Err != nil {
		t.Fatalf("INIT failed: %v", initResult.Err)
	}

	// Get the handle
	handle := extractContextHandle(t, proc)

	// Build integrity-wrapped request body as initiator
	procedureArgs := []byte("test-nfs-procedure-data")
	seqNum := uint32(1)
	requestBody := buildInitiatorIntegData(t, key, seqNum, procedureArgs)

	// Build DATA credential
	dataCred := &RPCGSSCredV1{
		GSSProc: RPCGSSData,
		SeqNum:  seqNum,
		Service: RPCGSSSvcIntegrity,
		Handle:  handle,
	}
	dataCredBody, err := EncodeGSSCred(dataCred)
	if err != nil {
		t.Fatalf("encode DATA cred: %v", err)
	}

	result := proc.Process(dataCredBody, nil, requestBody)

	if result.Err != nil {
		t.Fatalf("DATA with integrity failed: %v", result.Err)
	}
	if result.IsControl {
		t.Fatal("expected IsControl=false for DATA")
	}
	if !bytes.Equal(result.ProcessedData, procedureArgs) {
		t.Fatalf("expected processed data %q, got %q", procedureArgs, result.ProcessedData)
	}
	if result.Service != RPCGSSSvcIntegrity {
		t.Fatalf("expected service %d, got %d", RPCGSSSvcIntegrity, result.Service)
	}
}

// TestHandleDataWithIntegrityAfterAuthOnlyInit tests the scenario where:
// - INIT is done with service=NONE (e.g., for MOUNT protocol)
// - DATA is done with service=INTEGRITY (e.g., for NFS with sec=krb5i)
//
// This is the fix for the krb5i bug where the server used the context's service
// level (from INIT) instead of the credential's service level (per-call).
// Per RFC 2203 Section 5.3.3.4, the service level is set per-call in the credential.
func TestHandleDataWithIntegrityAfterAuthOnlyInit(t *testing.T) {
	key := testSessionKey()
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	verifier.sessionKey = key
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	// Create context with svc_none (auth-only, like MOUNT protocol might do)
	initCred := &RPCGSSCredV1{
		GSSProc: RPCGSSInit,
		SeqNum:  0,
		Service: RPCGSSSvcNone, // INIT with auth-only
		Handle:  nil,
	}
	initCredBody, err := EncodeGSSCred(initCred)
	if err != nil {
		t.Fatalf("encode INIT cred: %v", err)
	}

	initResult := proc.Process(initCredBody, nil, encodeOpaqueToken([]byte("mock-token")))
	if initResult.Err != nil {
		t.Fatalf("INIT failed: %v", initResult.Err)
	}

	// Get the handle and verify context was stored with svc_none
	handle := extractContextHandle(t, proc)
	proc.contexts.contexts.Range(func(k, value any) bool {
		ctx := value.(*GSSContext)
		if ctx.Service != RPCGSSSvcNone {
			t.Errorf("context service = %d, want %d", ctx.Service, RPCGSSSvcNone)
		}
		return false
	})

	// Build integrity-wrapped request body (as if NFS client is using krb5i)
	procedureArgs := []byte("test-nfs-procedure-data")
	seqNum := uint32(1)
	requestBody := buildInitiatorIntegData(t, key, seqNum, procedureArgs)

	// Build DATA credential with svc_integrity (NFS with sec=krb5i)
	dataCred := &RPCGSSCredV1{
		GSSProc: RPCGSSData,
		SeqNum:  seqNum,
		Service: RPCGSSSvcIntegrity, // DATA with integrity (different from INIT!)
		Handle:  handle,
	}
	dataCredBody, err := EncodeGSSCred(dataCred)
	if err != nil {
		t.Fatalf("encode DATA cred: %v", err)
	}

	result := proc.Process(dataCredBody, nil, requestBody)

	// This is the key assertion: should succeed even though INIT was svc_none
	if result.Err != nil {
		t.Fatalf("DATA with integrity after auth-only INIT failed: %v", result.Err)
	}
	if result.IsControl {
		t.Fatal("expected IsControl=false for DATA")
	}
	if !bytes.Equal(result.ProcessedData, procedureArgs) {
		t.Fatalf("expected processed data %q, got %q", procedureArgs, result.ProcessedData)
	}
	// Result.Service should match the credential's service, not the context's
	if result.Service != RPCGSSSvcIntegrity {
		t.Fatalf("expected result service %d (from credential), got %d", RPCGSSSvcIntegrity, result.Service)
	}
}
