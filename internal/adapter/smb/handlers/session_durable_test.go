package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// TestSessionSetup_UnknownNonzeroSessionIDRejected locks in the session-table
// rule for SESSION_SETUP: a request carrying a nonzero SessionId that matches
// no session in the session table must fail rather than authenticate under the
// client-supplied ID. A server that accepted such a request would attach an
// authenticated session to an ID an attacker chose, and a later client
// legitimately minted that same ID would collide with it.
func TestSessionSetup_UnknownNonzeroSessionIDRejected(t *testing.T) {
	h := NewHandler()
	ctx := newTestContext(99999) // no session with this ID exists

	ntlm := validNTLMNegotiateMessage()
	body := buildSessionSetupRequestBody(ntlm)

	result, err := h.SessionSetup(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusUserSessionDeleted {
		t.Fatalf("status = 0x%x, want StatusUserSessionDeleted (0x%x)",
			result.Status, types.StatusUserSessionDeleted)
	}
	// The client-supplied ID must not have been adopted: no pending auth may
	// exist under it, and no session may have been minted for it.
	if _, ok := h.GetPendingAuth(99999, ctx.ConnID); ok {
		t.Error("pending auth must not be stored under the rejected client-supplied SessionID")
	}
	if _, ok := h.GetSession(99999); ok {
		t.Error("no session may exist under the rejected client-supplied SessionID")
	}
}

// TestSessionSetup_ReauthNonzeroSessionIDStillAccepted pins the sibling case:
// a nonzero SessionId that DOES match an existing session remains a valid
// re-authentication attempt (MoreProcessingRequired, pending auth stored).
func TestSessionSetup_ReauthNonzeroSessionIDStillAccepted(t *testing.T) {
	h := NewHandler()
	sess := h.CreateSession("127.0.0.1:12345", false, "testuser", "DOMAIN")
	ctx := newTestContext(sess.SessionID)

	ntlm := validNTLMNegotiateMessage()
	body := buildSessionSetupRequestBody(ntlm)

	result, err := h.SessionSetup(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusMoreProcessingRequired {
		t.Fatalf("status = 0x%x, want StatusMoreProcessingRequired", result.Status)
	}
	if _, ok := h.GetPendingAuth(sess.SessionID, ctx.ConnID); !ok {
		t.Error("pending auth should be stored for the re-authentication attempt")
	}
}

// TestAppInstanceId_FailoverScopedToClaimedFile pins the scoping of the
// AppInstanceId failover force-close: only opens of the same file on the same
// share are displaced. An AppInstanceId reused for a different file (same or
// another share) must not close an unrelated open.
func TestAppInstanceId_FailoverScopedToClaimedFile(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	appId := [16]byte{0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8, 0xA9, 0xAA, 0xAB, 0xAC, 0xAD, 0xAE, 0xAF}

	// Two persisted handles under the same AppInstanceId: one at the claimed
	// (share, path), one at a different path on the same share.
	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:            "claimed",
		AppInstanceId: appId,
		ShareName:     "/share1",
		Path:          "vm/disk.vhdx",
	})
	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:            "other-file",
		AppInstanceId: appId,
		ShareName:     "/share1",
		Path:          "vm/other.vhdx",
	})
	// And one at the claimed path on a different share.
	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:            "other-share",
		AppInstanceId: appId,
		ShareName:     "/share2",
		Path:          "vm/disk.vhdx",
	})

	appIdData := make([]byte, 20)
	binary.LittleEndian.PutUint16(appIdData[0:2], 20)
	copy(appIdData[4:20], appId[:])
	contexts := []CreateContext{{Name: AppInstanceIdTag, Data: appIdData}}

	ProcessAppInstanceId(ctx, store, nil, contexts, "/share1", "vm/disk.vhdx")

	if h, _ := store.GetDurableHandle(ctx, "claimed"); h != nil {
		t.Error("the persisted handle at the claimed (share, path) should be force-closed")
	}
	if h, _ := store.GetDurableHandle(ctx, "other-file"); h == nil {
		t.Error("a persisted handle for a different path must survive: the AppInstanceId was reused for another file")
	}
	if h, _ := store.GetDurableHandle(ctx, "other-share"); h == nil {
		t.Error("a persisted handle on a different share must survive: the AppInstanceId was reused there")
	}
}

// TestAppInstanceId_FailoverCaseInsensitivePath pins that the share/path match
// is case-insensitive: SMB namespaces are case-insensitive, so a failover
// CREATE spelling the file with different case must still displace the
// persisted handle for the same file.
func TestAppInstanceId_FailoverCaseInsensitivePath(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	appId := [16]byte{0xB0}
	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:            "case-variant",
		AppInstanceId: appId,
		ShareName:     "/Share1",
		Path:          "VM/DISK.VHDX",
	})

	appIdData := make([]byte, 20)
	binary.LittleEndian.PutUint16(appIdData[0:2], 20)
	copy(appIdData[4:20], appId[:])
	contexts := []CreateContext{{Name: AppInstanceIdTag, Data: appIdData}}

	ProcessAppInstanceId(ctx, store, nil, contexts, "/share1", "vm/disk.vhdx")

	if h, _ := store.GetDurableHandle(ctx, "case-variant"); h != nil {
		t.Error("the persisted handle under a different-case spelling of the same (share, path) must be displaced")
	}
}
