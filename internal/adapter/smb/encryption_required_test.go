package smb

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/handlers"
	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// TestCheckEncryptionRequired_GlobalMode verifies the global ("required")
// encryption gate in checkEncryptionRequired: post-session-setup requests on a
// non-anonymous session must be encrypted, while NEGOTIATE/SESSION_SETUP and any
// already-encrypted request are exempt. Per-share enforcement is covered by the
// shouldRejectUnencryptedTreeConnect tests in the handlers package.
func TestCheckEncryptionRequired_GlobalMode(t *testing.T) {
	mgr := session.NewDefaultManager()
	h := handlers.NewHandlerWithSessionManager(mgr)
	connInfo := &ConnInfo{Handler: h, SessionManager: mgr}

	const someSession = uint64(0x1234)

	cases := []struct {
		name        string
		mode        string
		command     types.Command
		sessionID   uint64
		isEncrypted bool
		want        types.Status
	}{
		{"required_unencrypted_read_denied", "required", types.SMB2Read, someSession, false, types.StatusAccessDenied},
		{"required_encrypted_read_ok", "required", types.SMB2Read, someSession, true, 0},
		{"required_negotiate_exempt", "required", types.SMB2Negotiate, someSession, false, 0},
		{"required_session_setup_exempt", "required", types.SMB2SessionSetup, someSession, false, 0},
		{"required_no_session_skips_global", "required", types.SMB2Read, 0, false, 0},
		{"preferred_unencrypted_read_ok", "preferred", types.SMB2Read, someSession, false, 0},
		{"disabled_unencrypted_read_ok", "disabled", types.SMB2Read, someSession, false, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h.EncryptionConfig.Mode = c.mode
			reqHeader := &header.SMB2Header{
				Command:   c.command,
				SessionID: c.sessionID,
			}
			got := checkEncryptionRequired(reqHeader, connInfo, c.isEncrypted)
			if got != c.want {
				t.Errorf("checkEncryptionRequired = 0x%08x, want 0x%08x", got, c.want)
			}
		})
	}
}

// TestCheckEncryptionRequired_ShareEncryptDataFailsClosedForAnonymous pins the
// per-share ordering: the tree.EncryptData check runs BEFORE the anonymous/guest
// bypass, so an anonymous or guest session touching a share that demands
// encryption is rejected outright. Those sessions have no session key to
// encrypt with, so the request can never be made compliant and failing closed
// is the only safe answer.
func TestCheckEncryptionRequired_ShareEncryptDataFailsClosedForAnonymous(t *testing.T) {
	mgr := session.NewDefaultManager()
	h := handlers.NewHandlerWithSessionManager(mgr)
	connInfo := &ConnInfo{Handler: h, SessionManager: mgr}

	// A tree connected to a share with EncryptData=true.
	const treeID = uint32(0x77)
	h.StoreTree(&handlers.TreeConnection{
		TreeID:      treeID,
		ShareName:   "/encrypted-share",
		EncryptData: true,
	})

	// Guest session.
	guest := session.NewSession(0x2001, "127.0.0.1:1", true, "guest", "DOMAIN")
	mgr.StoreSession(guest)

	// Anonymous (null) session: no username, not a guest.
	anon := session.NewSession(0x2002, "127.0.0.1:2", false, "", "DOMAIN")
	mgr.StoreSession(anon)

	// Authenticated session for the control arm.
	authed := session.NewSession(0x2003, "127.0.0.1:3", false, "alice", "DOMAIN")
	mgr.StoreSession(authed)

	cases := []struct {
		name      string
		sessionID uint64
		want      types.Status
	}{
		{"guest_fail_closed", guest.SessionID, types.StatusAccessDenied},
		{"anonymous_fail_closed", anon.SessionID, types.StatusAccessDenied},
		{"authenticated_fail_closed", authed.SessionID, types.StatusAccessDenied},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h.EncryptionConfig.Mode = "preferred"
			reqHeader := &header.SMB2Header{
				Command:   types.SMB2Read,
				SessionID: c.sessionID,
				TreeID:    treeID,
			}
			got := checkEncryptionRequired(reqHeader, connInfo, false)
			if got != c.want {
				t.Errorf("checkEncryptionRequired = 0x%08x, want 0x%08x", got, c.want)
			}
		})
	}

	// Control: the same anonymous session on a tree WITHOUT EncryptData is
	// still allowed unencrypted in preferred mode (the bypass is intact where
	// no share demands encryption).
	h.StoreTree(&handlers.TreeConnection{
		TreeID:      0x78,
		ShareName:   "/plain-share",
		EncryptData: false,
	})
	h.EncryptionConfig.Mode = "preferred"
	reqHeader := &header.SMB2Header{
		Command:   types.SMB2Read,
		SessionID: anon.SessionID,
		TreeID:    0x78,
	}
	if got := checkEncryptionRequired(reqHeader, connInfo, false); got != 0 {
		t.Errorf("anonymous on non-encrypting share = 0x%08x, want 0 (bypass intact)", got)
	}
}
