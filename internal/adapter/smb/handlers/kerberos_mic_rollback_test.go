package handlers

import (
	"testing"

	gokrb5types "github.com/jcmturner/gokrb5/v8/types"
	"github.com/marmos91/dittofs/internal/adapter/smb/auth"
	kerbauth "github.com/marmos91/dittofs/internal/auth/kerberos"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestBuildKerberosAcceptResponse_MicFailureDeletesFreshSession pins that a
// failed client mechListMIC check deletes the session the fresh-session path
// just created: a failed downgrade check must not leave a live authenticated
// session (with signing keys configured) attached to the connection.
func TestBuildKerberosAcceptResponse_MicFailureDeletesFreshSession(t *testing.T) {
	h := NewHandler()

	const sessionID = uint64(0x1234)
	user := &models.User{Username: "alice", Enabled: true}
	h.CreateSessionWithUser(sessionID, "127.0.0.1:1", user, "DITTOFS")

	// A garbage MIC — verification must fail.
	parsed := &auth.ParsedToken{
		Type:          auth.TokenTypeInit,
		MechListBytes: []byte{0x30, 0x0c, 0x06, 0x0a, 0x2b, 0x06, 0x01, 0x04, 0x01, 0x82, 0x37, 0x02, 0x02, 0x0a},
		MechListMIC:   []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
	}

	sess, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatal("session not found before the call")
	}
	// BuildMutualAuth degrades gracefully on a zero AP-REQ (accept-complete
	// without AP-REP), so a zero AuthResult.APReq is safe here.
	authResult := &kerbauth.AuthResult{
		Principal:  "alice@EXAMPLE.COM",
		Realm:      "EXAMPLE.COM",
		SessionKey: gokrb5types.EncryptionKey{},
	}

	// freshSession=true: the caller created this session in the same request.
	if _, err := h.buildKerberosAcceptResponse(sess, authResult, parsed, true); err == nil {
		t.Log("buildKerberosAcceptResponse returned nil error; still asserting session deletion")
	}

	if _, ok := h.GetSession(sessionID); ok {
		t.Error("fresh session survived a failed mechListMIC check — failed downgrade check left a live authenticated session")
	}
}
