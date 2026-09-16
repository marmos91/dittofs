package handlers

import (
	"context"
	"encoding/binary"
	"strings"
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

// TestAppInstanceId_FailoverMalformedNamesStayDistinct pins that the failover
// share/path match keeps malformed SMB names distinct. SMB filenames are
// arbitrary 16-bit code-unit sequences and may carry unpaired surrogates, which
// the decoder preserves as invalid WTF-8. Simple case folding decodes every
// invalid byte to U+FFFD, so the two lone surrogates below fold equal even
// though they name different files — a failover CREATE on one would displace
// the durable handle of the other. The comparison falls back to byte-exact for
// names that are not well-formed UTF-8, so they must stay distinct.
func TestAppInstanceId_FailoverMalformedNamesStayDistinct(t *testing.T) {
	// WTF-8 encodings of the lone surrogates U+D800 and U+DC00: distinct byte
	// strings, neither valid UTF-8, equal under simple case folding.
	const claimed = "vm/\xed\xa0\x80.vhdx"
	const other = "vm/\xed\xb0\x80.vhdx"
	if claimed == other {
		t.Fatal("setup: the two names must differ bytewise")
	}
	if !strings.EqualFold(claimed, other) {
		t.Skip("simple case folding no longer collapses these names; the guard is moot")
	}

	store := newMockDurableStore()
	ctx := context.Background()
	appId := [16]byte{0xC5}

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID: "claimed", AppInstanceId: appId, ShareName: "/share1", Path: claimed,
	})
	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID: "other-malformed", AppInstanceId: appId, ShareName: "/share1", Path: other,
	})

	appIdData := make([]byte, 20)
	binary.LittleEndian.PutUint16(appIdData[0:2], 20)
	copy(appIdData[4:20], appId[:])
	contexts := []CreateContext{{Name: AppInstanceIdTag, Data: appIdData}}

	ProcessAppInstanceId(ctx, store, nil, contexts, "/share1", claimed)

	if h, _ := store.GetDurableHandle(ctx, "claimed"); h != nil {
		t.Error("the persisted handle at the claimed malformed path should be force-closed")
	}
	if h, _ := store.GetDurableHandle(ctx, "other-malformed"); h == nil {
		t.Error("a persisted handle under a different malformed name must survive: case folding must not collapse invalid UTF-8")
	}
}

// TestAppInstanceId_FailoverScopedToClaimedFile_LiveOpens covers the half of the
// scoping rule the persisted test cannot reach.
//
// ProcessAppInstanceId force-closes live opens as well as persisted handles, and
// the two run through different filters. Passing a nil handler — which the
// persisted test does — skips the live loop entirely, so that test still passes
// against a live filter matching on AppInstanceId alone, which is the defect the
// scoping exists to prevent: a CREATE carrying an AppInstanceId would close the
// user's open on any file on any share.
func TestAppInstanceId_FailoverScopedToClaimedFile_LiveOpens(t *testing.T) {
	ctx := context.Background()
	appId := [16]byte{0xC0, 0xC1, 0xC2}

	// A handler with a live Registry: closeFilesWithFilter resolves the metadata
	// service to release locks, so the bare handler the persisted test can get
	// away with is not enough here.
	h, _, _, _, _ := setupSetInfoGateTest(t, gateFileReadData)
	open := func(id byte, share, path string) [16]byte {
		fileID := [16]byte{id}
		f := &OpenFile{
			FileID:         fileID,
			AppInstanceId:  appId,
			ShareName:      share,
			MetadataHandle: []byte{id},
		}
		f.SetName(OpenName{Path: path, FileName: path})
		h.StoreOpenFile(f)
		return fileID
	}
	claimed := open(0x01, "/share1", "vm/disk.vhdx")
	otherFile := open(0x02, "/share1", "vm/other.vhdx")
	otherShare := open(0x03, "/share2", "vm/disk.vhdx")

	appIdData := make([]byte, 20)
	binary.LittleEndian.PutUint16(appIdData[0:2], 20)
	copy(appIdData[4:20], appId[:])
	contexts := []CreateContext{{Name: AppInstanceIdTag, Data: appIdData}}

	ProcessAppInstanceId(ctx, newMockDurableStore(), h, contexts, "/share1", "vm/disk.vhdx")

	if _, ok := h.GetOpenFile(claimed); ok {
		t.Error("the live open at the claimed (share, path) should be force-closed")
	}
	if _, ok := h.GetOpenFile(otherFile); !ok {
		t.Error("a live open on a different path must survive: the AppInstanceId was reused for another file")
	}
	if _, ok := h.GetOpenFile(otherShare); !ok {
		t.Error("a live open on a different share must survive: the AppInstanceId was reused there")
	}
}
