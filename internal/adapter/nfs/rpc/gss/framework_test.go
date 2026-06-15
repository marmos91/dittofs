package gss

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/gssapi"
	"github.com/jcmturner/gokrb5/v8/types"
	identity "github.com/marmos91/dittofs/pkg/adapter/nfs/identity"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// mockVerifier implements Verifier for testing without a real KDC.
type mockVerifier struct {
	// result to return on VerifyToken calls
	principal         string
	realm             string
	sessionKey        types.EncryptionKey
	apRepToken        []byte
	hasAcceptorSubkey bool
	err               error
}

func newMockVerifier(principal, realm string) *mockVerifier {
	return &mockVerifier{
		principal: principal,
		realm:     realm,
		sessionKey: types.EncryptionKey{
			KeyType:  17, // aes128-cts-hmac-sha1-96
			KeyValue: []byte("test-session-key"),
		},
	}
}

func newFailingVerifier(err error) *mockVerifier {
	return &mockVerifier{err: err}
}

func (v *mockVerifier) VerifyToken(gssToken []byte) (*VerifiedContext, error) {
	if v.err != nil {
		return nil, v.err
	}
	return &VerifiedContext{
		Principal:         v.principal,
		Realm:             v.realm,
		SessionKey:        v.sessionKey,
		APRepToken:        v.apRepToken,
		HasAcceptorSubkey: v.hasAcceptorSubkey,
	}, nil
}

func buildINITCredBody(t *testing.T) []byte {
	t.Helper()
	cred := &RPCGSSCredV1{
		GSSProc: RPCGSSInit,
		SeqNum:  0,
		Service: RPCGSSSvcIntegrity,
		Handle:  nil, // empty during INIT
	}
	body, err := EncodeGSSCred(cred)
	if err != nil {
		t.Fatalf("encode INIT cred: %v", err)
	}
	return body
}

func buildDESTROYCredBody(t *testing.T, handle []byte, seqNum uint32) []byte {
	t.Helper()
	cred := &RPCGSSCredV1{
		GSSProc: RPCGSSDestroy,
		SeqNum:  seqNum,
		Service: RPCGSSSvcNone,
		Handle:  handle,
	}
	body, err := EncodeGSSCred(cred)
	if err != nil {
		t.Fatalf("encode DESTROY cred: %v", err)
	}
	return body
}

func newTestMapper() identity.IdentityMapper {
	return identity.NewStaticMapper(&identity.StaticMapperConfig{
		DefaultUID: 65534,
		DefaultGID: 65534,
		StaticMap: map[string]identity.StaticIdentity{
			"alice@EXAMPLE.COM": {UID: 1000, GID: 1000},
			"bob@EXAMPLE.COM":   {UID: 1001, GID: 1001},
		},
	})
}

// extractContextHandle extracts the handle from the first GSS context in the processor.
func extractContextHandle(t *testing.T, proc *GSSProcessor) []byte {
	t.Helper()
	var handle []byte
	proc.contexts.contexts.Range(func(key, value any) bool {
		ctx := value.(*GSSContext)
		handle = ctx.Handle
		return false
	})
	if len(handle) == 0 {
		t.Fatal("no context handle found in processor")
	}
	return handle
}

// mockVerifierSessionKey is the session key the mock verifier installs into every
// context it establishes. Tests sign header MICs with this key so the
// processor's verifyHeaderMIC check passes. aes128-cts-hmac-sha1-96 (etype 17)
// requires a 16-byte key; "test-session-key" is exactly 16 bytes.
var mockVerifierSessionKey = types.EncryptionKey{
	KeyType:  17,
	KeyValue: []byte("test-session-key"),
}

// signHeaderMIC produces an RPCSEC_GSS call verifier: a GSS-API MIC token
// (RFC 4121) computed by the initiator (client) over the given RPC call-header
// preimage using the context session key with initiator-sign key usage. This
// is what verifyHeaderMIC expects on every DATA request.
func signHeaderMIC(t *testing.T, sessionKey types.EncryptionKey, preimage []byte) []byte {
	t.Helper()
	tok := gssapi.MICToken{
		Flags:     0x00, // sent by initiator (acceptor flag clear)
		SndSeqNum: 0,
		Payload:   preimage,
	}
	if err := tok.SetChecksum(sessionKey, KeyUsageInitiatorSign); err != nil {
		t.Fatalf("sign header MIC: %v", err)
	}
	mic, err := tok.Marshal()
	if err != nil {
		t.Fatalf("marshal header MIC: %v", err)
	}
	return mic
}

// processDATA establishes a valid header MIC over credBody (used as the
// header preimage for test purposes) and runs a DATA request through the
// processor. Using credBody as the preimage is sufficient for unit tests:
// verifyHeaderMIC only requires the MIC to match whatever preimage is supplied.
func processDATA(t *testing.T, proc *GSSProcessor, sessionKey types.EncryptionKey, credBody, requestBody []byte) *GSSProcessResult {
	t.Helper()
	mic := signHeaderMIC(t, sessionKey, credBody)
	return proc.Process(context.Background(), credBody, mic, credBody, requestBody)
}

// encodeOpaqueToken wraps raw bytes as XDR opaque data (length-prefixed with padding).
func encodeOpaqueToken(data []byte) []byte {
	length := uint32(len(data))
	paddedLen := len(data)
	if len(data)%4 != 0 {
		paddedLen += 4 - (len(data) % 4)
	}

	result := make([]byte, 4+paddedLen)
	binary.BigEndian.PutUint32(result[:4], length)
	copy(result[4:], data)
	return result
}

func TestProcessINITReturnsControl(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	credBody := buildINITCredBody(t)
	gssToken := encodeOpaqueToken([]byte("mock-ap-req-token"))

	result := proc.Process(context.Background(), credBody, nil, nil, gssToken)

	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if !result.IsControl {
		t.Fatal("expected IsControl=true for INIT")
	}
	if result.GSSReply == nil {
		t.Fatal("expected non-nil GSSReply for INIT")
	}
}

func TestProcessINITStoresContextBeforeReply(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	credBody := buildINITCredBody(t)
	gssToken := encodeOpaqueToken([]byte("mock-ap-req-token"))

	result := proc.Process(context.Background(), credBody, nil, nil, gssToken)
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}

	// The context should already be in the store
	// (stored BEFORE the reply was built per the critical ordering requirement)
	if proc.ContextCount() != 1 {
		t.Fatalf("expected 1 context in store, got %d", proc.ContextCount())
	}
}

func TestProcessINITCreatesContextWithCorrectFields(t *testing.T) {
	verifier := newMockVerifier("bob", "EXAMPLE.COM")
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	credBody := buildINITCredBody(t)
	gssToken := encodeOpaqueToken([]byte("mock-ap-req-token"))

	result := proc.Process(context.Background(), credBody, nil, nil, gssToken)
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}

	// Find the stored context (there should be exactly one)
	var foundCtx *GSSContext
	proc.contexts.contexts.Range(func(key, value any) bool {
		foundCtx = value.(*GSSContext)
		return false // stop after first
	})

	if foundCtx == nil {
		t.Fatal("no context found in store")
	}

	if foundCtx.Principal != "bob" {
		t.Fatalf("expected principal bob, got %s", foundCtx.Principal)
	}
	if foundCtx.Realm != "EXAMPLE.COM" {
		t.Fatalf("expected realm EXAMPLE.COM, got %s", foundCtx.Realm)
	}
	if foundCtx.Service != RPCGSSSvcIntegrity {
		t.Fatalf("expected service %d, got %d", RPCGSSSvcIntegrity, foundCtx.Service)
	}
	if foundCtx.SeqWindow == nil {
		t.Fatal("expected non-nil SeqWindow")
	}
	if len(foundCtx.Handle) != 16 {
		t.Fatalf("expected 16-byte handle, got %d bytes", len(foundCtx.Handle))
	}
	if foundCtx.SessionKey.KeyType != 17 {
		t.Fatalf("expected session key type 17, got %d", foundCtx.SessionKey.KeyType)
	}
}

func TestProcessINITVerificationFailure(t *testing.T) {
	verifier := newFailingVerifier(fmt.Errorf("ticket expired"))
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	credBody := buildINITCredBody(t)
	gssToken := encodeOpaqueToken([]byte("bad-token"))

	result := proc.Process(context.Background(), credBody, nil, nil, gssToken)

	// Should have an error
	if result.Err == nil {
		t.Fatal("expected error for failed verification")
	}

	// Should still be a control message with error reply
	if !result.IsControl {
		t.Fatal("expected IsControl=true even on failure")
	}

	// Should have a GSS reply (error response)
	if result.GSSReply == nil {
		t.Fatal("expected non-nil GSSReply with error status")
	}

	// No context should be stored
	if proc.ContextCount() != 0 {
		t.Fatalf("expected 0 contexts after failed INIT, got %d", proc.ContextCount())
	}
}

func TestProcessDESTROYRemovesContext(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	// First, establish a context via INIT
	credBody := buildINITCredBody(t)
	initResult := proc.Process(context.Background(), credBody, nil, nil, encodeOpaqueToken([]byte("mock-ap-req-token")))
	if initResult.Err != nil {
		t.Fatalf("INIT failed: %v", initResult.Err)
	}

	if proc.ContextCount() != 1 {
		t.Fatalf("expected 1 context after INIT, got %d", proc.ContextCount())
	}

	// Find the context handle
	handle := extractContextHandle(t, proc)

	// Send DESTROY with a valid call-header MIC (required after the MIC-verification fix).
	destroyCredBody := buildDESTROYCredBody(t, handle, 1)
	destroyMIC := signHeaderMIC(t, mockVerifierSessionKey, destroyCredBody)
	destroyResult := proc.Process(context.Background(), destroyCredBody, destroyMIC, destroyCredBody, nil)

	if destroyResult.Err != nil {
		t.Fatalf("DESTROY failed: %v", destroyResult.Err)
	}
	if !destroyResult.IsControl {
		t.Fatal("expected IsControl=true for DESTROY")
	}
	if destroyResult.GSSReply == nil {
		t.Fatal("expected non-nil GSSReply for DESTROY")
	}

	// Context should be removed
	if proc.ContextCount() != 0 {
		t.Fatalf("expected 0 contexts after DESTROY, got %d", proc.ContextCount())
	}
}

func TestProcessDESTROYUnknownContext(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	// DESTROY a context that doesn't exist -- should still succeed per RFC
	destroyCredBody := buildDESTROYCredBody(t, []byte("nonexistent-handle"), 1)
	result := proc.Process(context.Background(), destroyCredBody, nil, nil, nil)

	if result.Err != nil {
		t.Fatalf("DESTROY of unknown context should succeed, got error: %v", result.Err)
	}
	if !result.IsControl {
		t.Fatal("expected IsControl=true for DESTROY")
	}
}

func TestProcessDESTROYRequiresMIC(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	proc := NewGSSProcessor(verifier, newTestMapper(), 100, 10*time.Minute)
	defer proc.Stop()

	// Establish a context.
	initCred := &RPCGSSCredV1{GSSProc: RPCGSSInit, SeqNum: 0, Service: RPCGSSSvcNone}
	initCredBody, _ := EncodeGSSCred(initCred)
	initRes := proc.Process(context.Background(), initCredBody, nil, nil,
		encodeOpaqueToken([]byte("mock-token")))
	if initRes.Err != nil {
		t.Fatalf("INIT failed: %v", initRes.Err)
	}
	handle := extractContextHandle(t, proc)

	destroyCred := &RPCGSSCredV1{
		GSSProc: RPCGSSDestroy,
		SeqNum:  1,
		Service: RPCGSSSvcNone,
		Handle:  handle,
	}
	destroyCredBody, _ := EncodeGSSCred(destroyCred)

	t.Run("no MIC rejects DESTROY", func(t *testing.T) {
		// No verifBody supplied — must be rejected before Delete.
		res := proc.Process(context.Background(), destroyCredBody, nil, destroyCredBody, nil)
		if res.Err == nil {
			t.Fatal("expected rejection for DESTROY without MIC")
		}
		if res.AuthStat != AuthStatCredProblem {
			t.Fatalf("expected AuthStatCredProblem, got %d", res.AuthStat)
		}
		// Context must still exist — DESTROY was rejected.
		if proc.ContextCount() != 1 {
			t.Fatalf("expected context to survive rejected DESTROY, got %d", proc.ContextCount())
		}
	})

	t.Run("MIC under wrong key rejects DESTROY", func(t *testing.T) {
		wrongKey := types.EncryptionKey{KeyType: 17, KeyValue: []byte("WRONG-session-ky")}
		mic := signHeaderMIC(t, wrongKey, destroyCredBody)
		res := proc.Process(context.Background(), destroyCredBody, mic, destroyCredBody, nil)
		if res.Err == nil {
			t.Fatal("expected rejection for DESTROY with wrong-key MIC")
		}
		if proc.ContextCount() != 1 {
			t.Fatalf("context must survive rejected DESTROY, got %d", proc.ContextCount())
		}
	})

	t.Run("valid MIC accepts DESTROY", func(t *testing.T) {
		mic := signHeaderMIC(t, mockVerifierSessionKey, destroyCredBody)
		res := proc.Process(context.Background(), destroyCredBody, mic, destroyCredBody, nil)
		if res.Err != nil {
			t.Fatalf("DESTROY with valid MIC failed: %v", res.Err)
		}
		if proc.ContextCount() != 0 {
			t.Fatalf("expected context deleted after valid DESTROY, got %d", proc.ContextCount())
		}
	})
}

func TestProcessDATAWithValidContext(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	// Establish a context via INIT at svc_none so a svc_none DATA call below
	// is not treated as a service downgrade.
	initCred := &RPCGSSCredV1{
		GSSProc: RPCGSSInit,
		SeqNum:  0,
		Service: RPCGSSSvcNone,
		Handle:  nil,
	}
	credBody, err := EncodeGSSCred(initCred)
	if err != nil {
		t.Fatalf("encode INIT cred: %v", err)
	}
	initResult := proc.Process(context.Background(), credBody, nil, nil, encodeOpaqueToken([]byte("mock-ap-req-token")))
	if initResult.Err != nil {
		t.Fatalf("INIT failed: %v", initResult.Err)
	}

	handle := extractContextHandle(t, proc)

	// Send DATA request with svc_none (service level is per-call via credential)
	dataCred := &RPCGSSCredV1{
		GSSProc: RPCGSSData,
		SeqNum:  1,
		Service: RPCGSSSvcNone,
		Handle:  handle,
	}
	dataCredBody, err := EncodeGSSCred(dataCred)
	if err != nil {
		t.Fatalf("encode DATA cred: %v", err)
	}

	procedureArgs := []byte("test-procedure-arguments")
	result := processDATA(t, proc, mockVerifierSessionKey, dataCredBody, procedureArgs)

	if result.Err != nil {
		t.Fatalf("DATA failed: %v", result.Err)
	}
	if result.IsControl {
		t.Fatal("expected IsControl=false for DATA")
	}
	if result.SilentDiscard {
		t.Fatal("expected SilentDiscard=false for valid DATA")
	}
	if string(result.ProcessedData) != string(procedureArgs) {
		t.Fatalf("expected ProcessedData to match procedure args")
	}
	if result.Identity == nil {
		t.Fatal("expected non-nil Identity for DATA")
	}
	if result.Identity.UID == nil || *result.Identity.UID != 1000 {
		t.Fatalf("expected UID 1000, got %v", result.Identity.UID)
	}
	if result.SeqNum != 1 {
		t.Fatalf("expected SeqNum 1, got %d", result.SeqNum)
	}
	if result.Service != RPCGSSSvcNone {
		t.Fatalf("expected Service %d, got %d", RPCGSSSvcNone, result.Service)
	}
}

// TestProcessDATAPropagatesAcceptorSubkey proves that an acceptor subkey
// produced during context establishment (mutual auth) flows from handleInit,
// through the persisted GSSContext, into the handleData result so the DATA
// reply path can set FLAG_ACCEPTOR_SUBKEY. Without the plumbing the result's
// HasAcceptorSubkey is always false and every DATA reply MIC omits the flag,
// which MIT krb5/libtirpc reject with GSS_S_BAD_SIG (RFC 4121 §4.2.2). This
// test FAILS unless the field is threaded end-to-end.
func TestProcessDATAPropagatesAcceptorSubkey(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	// Simulate a mutual-auth INIT that produced an acceptor subkey in the AP-REP.
	verifier.hasAcceptorSubkey = true
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	// INIT at svc_none so the svc_none DATA call below is not a downgrade.
	initCred := &RPCGSSCredV1{GSSProc: RPCGSSInit, SeqNum: 0, Service: RPCGSSSvcNone}
	initCredBody, err := EncodeGSSCred(initCred)
	if err != nil {
		t.Fatalf("encode INIT cred: %v", err)
	}
	initResult := proc.Process(context.Background(), initCredBody, nil, nil, encodeOpaqueToken([]byte("mock-ap-req-token")))
	if initResult.Err != nil {
		t.Fatalf("INIT failed: %v", initResult.Err)
	}
	if !initResult.HasAcceptorSubkey {
		t.Fatal("INIT result should report HasAcceptorSubkey=true")
	}

	// The persisted context must record the subkey flag.
	handle := extractContextHandle(t, proc)
	gssCtx, ok := proc.contexts.Lookup(handle)
	if !ok {
		t.Fatal("context not stored")
	}
	if !gssCtx.HasAcceptorSubkey {
		t.Fatal("persisted GSSContext lost HasAcceptorSubkey")
	}

	// A subsequent DATA request must carry the subkey flag in its result so the
	// reply verifier/wrap path can set FLAG_ACCEPTOR_SUBKEY.
	dataCred := &RPCGSSCredV1{GSSProc: RPCGSSData, SeqNum: 1, Service: RPCGSSSvcNone, Handle: handle}
	dataCredBody, err := EncodeGSSCred(dataCred)
	if err != nil {
		t.Fatalf("encode DATA cred: %v", err)
	}
	result := processDATA(t, proc, mockVerifierSessionKey, dataCredBody, []byte("args"))
	if result.Err != nil {
		t.Fatalf("DATA failed: %v", result.Err)
	}
	if !result.HasAcceptorSubkey {
		t.Fatal("DATA result must report HasAcceptorSubkey=true (subkey flag not propagated to reply path)")
	}

	// And the resulting reply MIC must actually carry the flag.
	mic, err := ComputeReplyVerifier(result.SessionKey, result.SeqNum, result.HasAcceptorSubkey)
	if err != nil {
		t.Fatalf("ComputeReplyVerifier failed: %v", err)
	}
	if mic[2]&gssapi.MICTokenFlagAcceptorSubkey == 0 {
		t.Fatalf("DATA reply MIC missing AcceptorSubkey flag, got flags 0x%02x", mic[2])
	}
}

// establishContext runs an INIT at the given service level and returns the
// resulting context handle. Used by the call-header MIC security tests.
func establishContext(t *testing.T, proc *GSSProcessor, service uint32) []byte {
	t.Helper()
	initCred := &RPCGSSCredV1{
		GSSProc: RPCGSSInit,
		SeqNum:  0,
		Service: service,
		Handle:  nil,
	}
	initCredBody, err := EncodeGSSCred(initCred)
	if err != nil {
		t.Fatalf("encode INIT cred: %v", err)
	}
	res := proc.Process(context.Background(), initCredBody, nil, nil, encodeOpaqueToken([]byte("mock-token")))
	if res.Err != nil {
		t.Fatalf("INIT failed: %v", res.Err)
	}
	return extractContextHandle(t, proc)
}

// TestProcessDATAForgedHeaderRejected is the core H1 regression: an attacker
// who learns a valid context handle but cannot produce a correct call-header
// MIC must be rejected with CREDPROBLEM. This covers svc_none (krb5), where no
// body crypto exists and the header MIC is the ONLY authentication gate.
func TestProcessDATAForgedHeaderRejected(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	proc := NewGSSProcessor(verifier, newTestMapper(), 100, 10*time.Minute)
	defer proc.Stop()

	handle := establishContext(t, proc, RPCGSSSvcNone)

	dataCred := &RPCGSSCredV1{
		GSSProc: RPCGSSData,
		SeqNum:  1,
		Service: RPCGSSSvcNone,
		Handle:  handle,
	}
	dataCredBody, err := EncodeGSSCred(dataCred)
	if err != nil {
		t.Fatalf("encode DATA cred: %v", err)
	}

	t.Run("no verifier", func(t *testing.T) {
		// Stolen handle, no MIC at all: the old code accepted this for svc_none.
		res := proc.Process(context.Background(), dataCredBody, nil, dataCredBody, []byte("forged-args"))
		if res.Err == nil {
			t.Fatal("expected rejection for missing call-header MIC")
		}
		if res.AuthStat != AuthStatCredProblem {
			t.Fatalf("expected AuthStatCredProblem (%d), got %d", AuthStatCredProblem, res.AuthStat)
		}
		if res.ProcessedData != nil || res.Identity != nil {
			t.Fatal("rejected DATA must not yield processed data or identity")
		}
	})

	t.Run("MIC over different header", func(t *testing.T) {
		// Attacker forges the header (e.g. a different seq_num / handle) but can
		// only sign a DIFFERENT preimage — the MIC will not match the real header.
		mic := signHeaderMIC(t, mockVerifierSessionKey, []byte("attacker-chosen-preimage"))
		res := proc.Process(context.Background(), dataCredBody, mic, dataCredBody, []byte("forged-args"))
		if res.Err == nil {
			t.Fatal("expected rejection for MIC over a different header preimage")
		}
		if res.AuthStat != AuthStatCredProblem {
			t.Fatalf("expected AuthStatCredProblem (%d), got %d", AuthStatCredProblem, res.AuthStat)
		}
	})

	t.Run("MIC under wrong key", func(t *testing.T) {
		// Attacker signs the correct preimage but with a key they do not hold.
		wrongKey := types.EncryptionKey{KeyType: 17, KeyValue: []byte("WRONG-session-ky")}
		mic := signHeaderMIC(t, wrongKey, dataCredBody)
		res := proc.Process(context.Background(), dataCredBody, mic, dataCredBody, []byte("forged-args"))
		if res.Err == nil {
			t.Fatal("expected rejection for MIC computed under the wrong key")
		}
		if res.AuthStat != AuthStatCredProblem {
			t.Fatalf("expected AuthStatCredProblem (%d), got %d", AuthStatCredProblem, res.AuthStat)
		}
	})

	t.Run("missing preimage", func(t *testing.T) {
		// A malformed RPC header yields a nil preimage; DATA must be rejected.
		mic := signHeaderMIC(t, mockVerifierSessionKey, dataCredBody)
		res := proc.Process(context.Background(), dataCredBody, mic, nil, []byte("forged-args"))
		if res.Err == nil {
			t.Fatal("expected rejection when header preimage is missing")
		}
		if res.AuthStat != AuthStatCredProblem {
			t.Fatalf("expected AuthStatCredProblem (%d), got %d", AuthStatCredProblem, res.AuthStat)
		}
	})
}

// TestProcessDATAGenuineMICAccepted confirms a correctly-signed header MIC
// passes for svc_none (the previously-unauthenticated path).
func TestProcessDATAGenuineMICAccepted(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	proc := NewGSSProcessor(verifier, newTestMapper(), 100, 10*time.Minute)
	defer proc.Stop()

	handle := establishContext(t, proc, RPCGSSSvcNone)
	dataCred := &RPCGSSCredV1{
		GSSProc: RPCGSSData,
		SeqNum:  1,
		Service: RPCGSSSvcNone,
		Handle:  handle,
	}
	dataCredBody, err := EncodeGSSCred(dataCred)
	if err != nil {
		t.Fatalf("encode DATA cred: %v", err)
	}

	res := processDATA(t, proc, mockVerifierSessionKey, dataCredBody, []byte("genuine-args"))
	if res.Err != nil {
		t.Fatalf("genuine MIC should pass: %v", res.Err)
	}
	if string(res.ProcessedData) != "genuine-args" {
		t.Fatalf("unexpected processed data: %q", res.ProcessedData)
	}
	if res.Identity == nil || res.Identity.UID == nil || *res.Identity.UID != 1000 {
		t.Fatalf("expected resolved identity uid=1000, got %+v", res.Identity)
	}
}

// TestProcessDATAServiceDowngradeRejected covers C-MED: a context established
// at a stronger service (privacy/integrity) must not accept a weaker per-call
// service. A valid header MIC is supplied so the rejection is attributable to
// the downgrade, not the MIC.
func TestProcessDATAServiceDowngradeRejected(t *testing.T) {
	cases := []struct {
		name      string
		ctxSvc    uint32
		callSvc   uint32
		wantError bool
	}{
		{"privacy->none", RPCGSSSvcPrivacy, RPCGSSSvcNone, true},
		{"privacy->integrity", RPCGSSSvcPrivacy, RPCGSSSvcIntegrity, true},
		{"integrity->none", RPCGSSSvcIntegrity, RPCGSSSvcNone, true},
		{"none->none (no downgrade)", RPCGSSSvcNone, RPCGSSSvcNone, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verifier := newMockVerifier("alice", "EXAMPLE.COM")
			proc := NewGSSProcessor(verifier, newTestMapper(), 100, 10*time.Minute)
			defer proc.Stop()

			handle := establishContext(t, proc, tc.ctxSvc)
			dataCred := &RPCGSSCredV1{
				GSSProc: RPCGSSData,
				SeqNum:  1,
				Service: tc.callSvc,
				Handle:  handle,
			}
			dataCredBody, err := EncodeGSSCred(dataCred)
			if err != nil {
				t.Fatalf("encode DATA cred: %v", err)
			}

			mic := signHeaderMIC(t, mockVerifierSessionKey, dataCredBody)
			res := proc.Process(context.Background(), dataCredBody, mic, dataCredBody, []byte("args"))

			if tc.wantError {
				if res.Err == nil {
					t.Fatalf("expected service-downgrade rejection (ctx=%d call=%d)", tc.ctxSvc, tc.callSvc)
				}
				if res.AuthStat != AuthStatCredProblem {
					t.Fatalf("expected AuthStatCredProblem (%d), got %d", AuthStatCredProblem, res.AuthStat)
				}
			} else if res.Err != nil {
				t.Fatalf("unexpected error for equal service level: %v", res.Err)
			}
		})
	}
}

func TestProcessDATAUnknownContext(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	// DATA request with unknown handle
	handle := []byte("nonexistent-handle")
	dataCred := &RPCGSSCredV1{
		GSSProc: RPCGSSData,
		SeqNum:  1,
		Service: RPCGSSSvcNone,
		Handle:  handle,
	}
	credBody, err := EncodeGSSCred(dataCred)
	if err != nil {
		t.Fatalf("encode cred: %v", err)
	}

	// Context lookup fails before the MIC check, so no verifier is needed.
	result := proc.Process(context.Background(), credBody, nil, nil, []byte("args"))

	if result.Err == nil {
		t.Fatal("expected error for unknown context")
	}
}

func TestProcessDATASilentDiscardForDuplicate(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	// Create context with svc_none
	initCred := &RPCGSSCredV1{
		GSSProc: RPCGSSInit,
		SeqNum:  0,
		Service: RPCGSSSvcNone,
		Handle:  nil,
	}
	initCredBody, _ := EncodeGSSCred(initCred)
	initResult := proc.Process(context.Background(), initCredBody, nil, nil, encodeOpaqueToken([]byte("mock-token")))
	if initResult.Err != nil {
		t.Fatalf("INIT failed: %v", initResult.Err)
	}

	handle := extractContextHandle(t, proc)

	// First DATA with seq_num=1 should succeed
	dataCred := &RPCGSSCredV1{
		GSSProc: RPCGSSData,
		SeqNum:  1,
		Service: RPCGSSSvcNone,
		Handle:  handle,
	}
	dataCredBody, _ := EncodeGSSCred(dataCred)
	result1 := processDATA(t, proc, mockVerifierSessionKey, dataCredBody, []byte("args"))
	if result1.Err != nil {
		t.Fatalf("first DATA failed: %v", result1.Err)
	}

	// Second DATA with same seq_num=1 should be silently discarded (duplicate).
	// The header MIC still verifies; the duplicate is caught by the seq window.
	result2 := processDATA(t, proc, mockVerifierSessionKey, dataCredBody, []byte("args"))
	if !result2.SilentDiscard {
		t.Fatal("expected SilentDiscard=true for duplicate sequence number")
	}
}

func TestProcessDATAMaxSeqDestroysContext(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	// Create context
	initCred := &RPCGSSCredV1{
		GSSProc: RPCGSSInit,
		SeqNum:  0,
		Service: RPCGSSSvcNone,
		Handle:  nil,
	}
	initCredBody, _ := EncodeGSSCred(initCred)
	initResult := proc.Process(context.Background(), initCredBody, nil, nil, encodeOpaqueToken([]byte("mock-token")))
	if initResult.Err != nil {
		t.Fatalf("INIT failed: %v", initResult.Err)
	}

	handle := extractContextHandle(t, proc)

	// DATA with seq_num >= MAXSEQ should destroy context
	dataCred := &RPCGSSCredV1{
		GSSProc: RPCGSSData,
		SeqNum:  MAXSEQ, // 0x80000000
		Service: RPCGSSSvcNone,
		Handle:  handle,
	}
	dataCredBody, _ := EncodeGSSCred(dataCred)
	result := processDATA(t, proc, mockVerifierSessionKey, dataCredBody, []byte("args"))

	if result.Err == nil {
		t.Fatal("expected error for MAXSEQ exceeded")
	}

	// Context should be destroyed
	if proc.ContextCount() != 0 {
		t.Fatalf("expected context to be destroyed, got %d contexts", proc.ContextCount())
	}
}

func TestProcessDATASvcIntegrityRequiresValidIntegData(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
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
	initCredBody, _ := EncodeGSSCred(initCred)
	initResult := proc.Process(context.Background(), initCredBody, nil, nil, encodeOpaqueToken([]byte("mock-token")))
	if initResult.Err != nil {
		t.Fatalf("INIT failed: %v", initResult.Err)
	}

	handle := extractContextHandle(t, proc)

	// DATA with svc_integrity but raw (unwrapped) args should fail because
	// UnwrapIntegrity expects rpc_gss_integ_data format
	dataCred := &RPCGSSCredV1{
		GSSProc: RPCGSSData,
		SeqNum:  1,
		Service: RPCGSSSvcIntegrity,
		Handle:  handle,
	}
	dataCredBody, _ := EncodeGSSCred(dataCred)
	// Header MIC passes; integrity body unwrap fails on the raw (non-integ) args.
	result := processDATA(t, proc, mockVerifierSessionKey, dataCredBody, []byte("raw-args-not-integ-format"))

	if result.Err == nil {
		t.Fatal("expected error for svc_integrity with invalid integ data format")
	}
}

func TestProcessInvalidCredentialVersion(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	// Build a credential with version 2 (invalid)
	badCredBody := make([]byte, 24)
	// version = 2 (bytes 0-3)
	badCredBody[3] = 2
	// gss_proc = 0 (bytes 4-7)
	// seq_num = 1 (bytes 8-11)
	badCredBody[11] = 1
	// service = 1 (bytes 12-15)
	badCredBody[15] = 1
	// handle_len = 0 (bytes 16-19)

	result := proc.Process(context.Background(), badCredBody, nil, nil, nil)

	if result.Err == nil {
		t.Fatal("expected error for invalid credential version")
	}
}

func TestProcessUnknownGSSProc(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	// Build a credential with unknown gss_proc (99)
	cred := &RPCGSSCredV1{
		GSSProc: 99,
		SeqNum:  1,
		Service: RPCGSSSvcNone,
		Handle:  nil,
	}
	credBody, err := EncodeGSSCred(cred)
	if err != nil {
		t.Fatalf("encode cred: %v", err)
	}

	result := proc.Process(context.Background(), credBody, nil, nil, nil)

	if result.Err == nil {
		t.Fatal("expected error for unknown GSS procedure")
	}
}

func TestProcessINITNoVerifier(t *testing.T) {
	// Processor with nil verifier
	proc := NewGSSProcessor(nil, nil, 100, 10*time.Minute)
	defer proc.Stop()

	credBody := buildINITCredBody(t)
	result := proc.Process(context.Background(), credBody, nil, nil, encodeOpaqueToken([]byte("mock-token")))

	if result.Err == nil {
		t.Fatal("expected error when no verifier configured")
	}
}

func TestProcessINITMultipleContexts(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	// Create 5 contexts
	for i := 0; i < 5; i++ {
		credBody := buildINITCredBody(t)
		result := proc.Process(context.Background(), credBody, nil, nil, encodeOpaqueToken([]byte("mock-ap-req-token")))
		if result.Err != nil {
			t.Fatalf("INIT %d failed: %v", i, result.Err)
		}
	}

	if proc.ContextCount() != 5 {
		t.Fatalf("expected 5 contexts, got %d", proc.ContextCount())
	}
}

func TestProcessSetVerifier(t *testing.T) {
	verifier1 := newMockVerifier("alice", "EXAMPLE.COM")
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier1, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	// First INIT with original verifier
	credBody := buildINITCredBody(t)
	result := proc.Process(context.Background(), credBody, nil, nil, encodeOpaqueToken([]byte("mock-token")))
	if result.Err != nil {
		t.Fatalf("INIT with verifier1 failed: %v", result.Err)
	}

	// Hot-swap verifier
	verifier2 := newMockVerifier("bob", "OTHER.COM")
	proc.SetVerifier(verifier2)

	// Second INIT with new verifier
	credBody2 := buildINITCredBody(t)
	result2 := proc.Process(context.Background(), credBody2, nil, nil, encodeOpaqueToken([]byte("mock-token")))
	if result2.Err != nil {
		t.Fatalf("INIT with verifier2 failed: %v", result2.Err)
	}

	// Should have 2 contexts: one alice, one bob
	if proc.ContextCount() != 2 {
		t.Fatalf("expected 2 contexts, got %d", proc.ContextCount())
	}

	foundBob := false
	proc.contexts.contexts.Range(func(key, value any) bool {
		ctx := value.(*GSSContext)
		if ctx.Principal == "bob" && ctx.Realm == "OTHER.COM" {
			foundBob = true
		}
		return true
	})
	if !foundBob {
		t.Fatal("expected to find bob's context from verifier2")
	}
}

func TestProcessContinueInitRoutesToInit(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	// Build CONTINUE_INIT credential
	cred := &RPCGSSCredV1{
		GSSProc: RPCGSSContinueInit,
		SeqNum:  0,
		Service: RPCGSSSvcNone,
		Handle:  []byte("partial-handle"),
	}
	credBody, err := EncodeGSSCred(cred)
	if err != nil {
		t.Fatalf("encode cred: %v", err)
	}

	result := proc.Process(context.Background(), credBody, nil, nil, encodeOpaqueToken([]byte("continue-token")))

	// Should succeed (routes to handleInit)
	if result.Err != nil {
		t.Fatalf("CONTINUE_INIT failed: %v", result.Err)
	}
	if !result.IsControl {
		t.Fatal("expected IsControl=true for CONTINUE_INIT")
	}
}

func TestExtractAPReqRawToken(t *testing.T) {
	// Raw AP-REQ (not wrapped in GSS application tag)
	rawAPReq := []byte{0x6E, 0x82, 0x01, 0x00} // some AP-REQ bytes
	result, err := extractAPReq(rawAPReq)
	if err != nil {
		t.Fatalf("extractAPReq failed for raw token: %v", err)
	}
	if string(result) != string(rawAPReq) {
		t.Fatal("expected raw AP-REQ to be returned as-is")
	}
}

func TestExtractAPReqWrappedToken(t *testing.T) {
	// Build a fake GSS-API wrapped token
	// Format: 0x60 [length] 0x06 [oid-len] [oid] [token-id] [ap-req]
	// Per RFC 1964 Section 1.1, the token ID for AP-REQ is 0x01 0x00.
	apReqData := []byte{0x6E, 0x03, 0x01, 0x02, 0x03}                   // fake AP-REQ
	oid := []byte{0x2A, 0x86, 0x48, 0x86, 0xF7, 0x12, 0x01, 0x02, 0x02} // krb5 OID
	tokenID := []byte{0x01, 0x00}                                       // AP-REQ token ID

	token := []byte{0x60}
	innerLen := 2 + len(oid) + len(tokenID) + len(apReqData) // OID tag(1) + OID length(1) + OID + token ID + AP-REQ
	token = append(token, byte(innerLen))
	token = append(token, 0x06)           // OID tag
	token = append(token, byte(len(oid))) // OID length
	token = append(token, oid...)
	token = append(token, tokenID...)
	token = append(token, apReqData...)

	result, err := extractAPReq(token)
	if err != nil {
		t.Fatalf("extractAPReq failed for wrapped token: %v", err)
	}
	if string(result) != string(apReqData) {
		t.Fatalf("expected AP-REQ data, got %v", result)
	}
}

func TestExtractAPReqTooShort(t *testing.T) {
	_, err := extractAPReq([]byte{0x60})
	if err == nil {
		t.Fatal("expected error for token too short")
	}
}

func TestGSSProcessResultIdentityNilForControl(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	credBody := buildINITCredBody(t)
	result := proc.Process(context.Background(), credBody, nil, nil, encodeOpaqueToken([]byte("mock-token")))

	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if result.Identity != nil {
		t.Fatal("expected Identity to be nil for control messages")
	}
	if result.ProcessedData != nil {
		t.Fatal("expected ProcessedData to be nil for control messages")
	}
}

func TestIdentityMapperInterface(t *testing.T) {
	mapper := newTestMapper()

	resolved, err := mapper.Resolve(context.Background(), "alice@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if resolved == nil {
		t.Fatal("expected non-nil resolved identity")
	}
	if !resolved.Found {
		t.Fatal("expected Found=true")
	}
	if resolved.UID != 1000 {
		t.Fatalf("expected UID 1000, got %d", resolved.UID)
	}
	if resolved.GID != 1000 {
		t.Fatalf("expected GID 1000, got %d", resolved.GID)
	}

	// Unknown principal should get default UID/GID
	unknown, err := mapper.Resolve(context.Background(), "unknown@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("Resolve for unknown failed: %v", err)
	}
	if unknown.UID != 65534 {
		t.Fatalf("expected default UID 65534, got %d", unknown.UID)
	}
}

func TestGSSProcessResultUsesMetadataIdentity(t *testing.T) {
	uid := uint32(1000)
	gid := uint32(1000)
	result := &GSSProcessResult{
		Identity: &metadata.Identity{
			UID: &uid,
			GID: &gid,
		},
	}
	if result.Identity.UID == nil || *result.Identity.UID != 1000 {
		t.Fatal("Identity type integration failed")
	}
}

// TestGSSLifecycle_Full tests the complete RPCSEC_GSS lifecycle:
// INIT -> DATA (success) -> DATA (success) -> DATA (duplicate rejection) -> DESTROY -> DATA (context gone)
//
// This test does NOT require a real KDC. The mock Verifier returns a synthetic
// VerifiedContext with a known session key. This tests the RPCSEC_GSS state
// machine and wire protocol handling, not the Kerberos cryptography.
func TestGSSLifecycle_Full(t *testing.T) {
	verifier := newMockVerifier("alice", "EXAMPLE.COM")
	mapper := newTestMapper()
	proc := NewGSSProcessor(verifier, mapper, 100, 10*time.Minute)
	defer proc.Stop()

	// ---- Step 1: RPCSEC_GSS_INIT ----
	// Build INIT credential with svc_none for simplicity
	initCred := &RPCGSSCredV1{
		GSSProc: RPCGSSInit,
		SeqNum:  0,
		Service: RPCGSSSvcNone,
		Handle:  nil,
	}
	initCredBody, err := EncodeGSSCred(initCred)
	if err != nil {
		t.Fatalf("encode INIT cred: %v", err)
	}

	initResult := proc.Process(context.Background(), initCredBody, nil, nil, encodeOpaqueToken([]byte("mock-ap-req-token")))

	// Verify INIT result
	if initResult.Err != nil {
		t.Fatalf("INIT failed: %v", initResult.Err)
	}
	if !initResult.IsControl {
		t.Fatal("Step 1: expected IsControl=true for INIT")
	}
	if initResult.GSSReply == nil {
		t.Fatal("Step 1: expected non-nil GSSReply for INIT")
	}
	if proc.ContextCount() != 1 {
		t.Fatalf("Step 1: expected 1 context, got %d", proc.ContextCount())
	}

	// Extract the handle from the stored context
	handle := extractContextHandle(t, proc)

	// ---- Step 2: RPCSEC_GSS_DATA with SeqNum=1 ----
	dataCred1 := &RPCGSSCredV1{
		GSSProc: RPCGSSData,
		SeqNum:  1,
		Service: RPCGSSSvcNone,
		Handle:  handle,
	}
	dataCredBody1, _ := EncodeGSSCred(dataCred1)
	procedureArgs1 := []byte("GETATTR-request-args")
	dataResult1 := processDATA(t, proc, mockVerifierSessionKey, dataCredBody1, procedureArgs1)

	if dataResult1.Err != nil {
		t.Fatalf("Step 2: DATA(seq=1) failed: %v", dataResult1.Err)
	}
	if dataResult1.IsControl {
		t.Fatal("Step 2: expected IsControl=false for DATA")
	}
	if dataResult1.SilentDiscard {
		t.Fatal("Step 2: expected SilentDiscard=false for valid DATA")
	}
	if string(dataResult1.ProcessedData) != string(procedureArgs1) {
		t.Fatal("Step 2: ProcessedData does not match procedure args")
	}
	if dataResult1.Identity == nil {
		t.Fatal("Step 2: expected non-nil Identity")
	}
	if dataResult1.Identity.UID == nil || *dataResult1.Identity.UID != 1000 {
		t.Fatalf("Step 2: expected UID 1000, got %v", dataResult1.Identity.UID)
	}
	if dataResult1.SeqNum != 1 {
		t.Fatalf("Step 2: expected SeqNum 1, got %d", dataResult1.SeqNum)
	}
	if dataResult1.Service != RPCGSSSvcNone {
		t.Fatalf("Step 2: expected Service svc_none, got %d", dataResult1.Service)
	}

	// ---- Step 3: RPCSEC_GSS_DATA with SeqNum=2 ----
	dataCred2 := &RPCGSSCredV1{
		GSSProc: RPCGSSData,
		SeqNum:  2,
		Service: RPCGSSSvcNone,
		Handle:  handle,
	}
	dataCredBody2, _ := EncodeGSSCred(dataCred2)
	procedureArgs2 := []byte("READ-request-args")
	dataResult2 := processDATA(t, proc, mockVerifierSessionKey, dataCredBody2, procedureArgs2)

	if dataResult2.Err != nil {
		t.Fatalf("Step 3: DATA(seq=2) failed: %v", dataResult2.Err)
	}
	if dataResult2.SilentDiscard {
		t.Fatal("Step 3: expected SilentDiscard=false for valid DATA")
	}
	if string(dataResult2.ProcessedData) != string(procedureArgs2) {
		t.Fatal("Step 3: ProcessedData does not match procedure args")
	}

	// ---- Step 4: Duplicate DATA with SeqNum=1 (should be silently discarded) ----
	dupResult := processDATA(t, proc, mockVerifierSessionKey, dataCredBody1, procedureArgs1)

	if !dupResult.SilentDiscard {
		t.Fatal("Step 4: expected SilentDiscard=true for duplicate seq_num=1")
	}

	// ---- Step 5: RPCSEC_GSS_DESTROY ----
	destroyCred := &RPCGSSCredV1{
		GSSProc: RPCGSSDestroy,
		SeqNum:  3,
		Service: RPCGSSSvcNone,
		Handle:  handle,
	}
	destroyCredBody, _ := EncodeGSSCred(destroyCred)
	destroyMIC := signHeaderMIC(t, mockVerifierSessionKey, destroyCredBody)
	destroyResult := proc.Process(context.Background(), destroyCredBody, destroyMIC, destroyCredBody, nil)

	if destroyResult.Err != nil {
		t.Fatalf("Step 5: DESTROY failed: %v", destroyResult.Err)
	}
	if !destroyResult.IsControl {
		t.Fatal("Step 5: expected IsControl=true for DESTROY")
	}
	if destroyResult.GSSReply == nil {
		t.Fatal("Step 5: expected non-nil GSSReply for DESTROY")
	}
	if proc.ContextCount() != 0 {
		t.Fatalf("Step 5: expected 0 contexts after DESTROY, got %d", proc.ContextCount())
	}

	// ---- Step 6: DATA with old handle (context gone) ----
	dataCred3 := &RPCGSSCredV1{
		GSSProc: RPCGSSData,
		SeqNum:  4,
		Service: RPCGSSSvcNone,
		Handle:  handle,
	}
	dataCredBody3, _ := EncodeGSSCred(dataCred3)
	// Context was destroyed; lookup fails before the MIC check.
	staleResult := proc.Process(context.Background(), dataCredBody3, nil, nil, []byte("stale-request"))

	if staleResult.Err == nil {
		t.Fatal("Step 6: expected error for stale context handle after DESTROY")
	}
}
