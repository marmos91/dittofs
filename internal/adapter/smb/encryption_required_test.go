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

// TestCheckEncryptionRequired_NullGuestNotExempt pins that a null or guest
// session is not exempt from encryption enforcement: a share that demands
// encryption rejects their unencrypted traffic, and a globally-required server
// rejects it for any post-session-setup request. Anonymous SMB3 encryption
// derives its key from a fixed all-zero secret, so such a session is expected
// to encrypt like any other — the request can never be made compliant by
// downgrading it to cleartext. NEGOTIATE and SESSION_SETUP stay exempt because
// no keys exist yet at that point in the exchange.
func TestCheckEncryptionRequired_NullGuestNotExempt(t *testing.T) {
	mgr := session.NewDefaultManager()
	h := handlers.NewHandlerWithSessionManager(mgr)
	connInfo := &ConnInfo{Handler: h, SessionManager: mgr}

	// A tree connected to a share that requires encryption.
	const encTreeID = uint32(0x77)
	h.StoreTree(&handlers.TreeConnection{
		TreeID:      encTreeID,
		ShareName:   "/encrypted-share",
		EncryptData: true,
	})
	// A tree connected to a share that does not require encryption.
	const plainTreeID = uint32(0x78)
	h.StoreTree(&handlers.TreeConnection{
		TreeID:      plainTreeID,
		ShareName:   "/plain-share",
		EncryptData: false,
	})

	guest := session.NewSession(0x2001, "127.0.0.1:1", true, "guest", "DOMAIN")
	mgr.StoreSession(guest)
	null := session.NewSession(0x2002, "127.0.0.1:2", false, "", "DOMAIN")
	mgr.StoreSession(null)

	cases := []struct {
		name        string
		mode        string
		command     types.Command
		sessionID   uint64
		treeID      uint32
		isEncrypted bool
		want        types.Status
	}{
		// Per-share EncryptData rejects null/guest traffic outright, in any mode.
		{"guest_encrypt_share_denied", "preferred", types.SMB2Read, guest.SessionID, encTreeID, false, types.StatusAccessDenied},
		{"null_encrypt_share_denied", "preferred", types.SMB2Read, null.SessionID, encTreeID, false, types.StatusAccessDenied},
		{"guest_encrypt_share_denied_global_required", "required", types.SMB2Read, guest.SessionID, encTreeID, false, types.StatusAccessDenied},
		// Global required mode rejects null/guest post-session-setup traffic.
		{"guest_global_required_denied", "required", types.SMB2Read, guest.SessionID, 0, false, types.StatusAccessDenied},
		{"null_global_required_denied", "required", types.SMB2Read, null.SessionID, 0, false, types.StatusAccessDenied},
		// SESSION_SETUP and NEGOTIATE stay exempt: no keys exist yet.
		{"guest_session_setup_exempt", "required", types.SMB2SessionSetup, guest.SessionID, 0, false, 0},
		{"null_negotiate_exempt", "required", types.SMB2Negotiate, null.SessionID, 0, false, 0},
		// An already-encrypted request is always allowed.
		{"guest_encrypt_share_encrypted_ok", "required", types.SMB2Read, guest.SessionID, encTreeID, true, 0},
		// Where nothing demands encryption, null/guest traffic is still allowed.
		{"null_plain_share_preferred_ok", "preferred", types.SMB2Read, null.SessionID, plainTreeID, false, 0},
		{"guest_plain_share_disabled_ok", "disabled", types.SMB2Read, guest.SessionID, plainTreeID, false, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h.EncryptionConfig.Mode = c.mode
			reqHeader := &header.SMB2Header{
				Command:   c.command,
				SessionID: c.sessionID,
				TreeID:    c.treeID,
			}
			got := checkEncryptionRequired(reqHeader, connInfo, c.isEncrypted)
			if got != c.want {
				t.Errorf("checkEncryptionRequired = 0x%08x, want 0x%08x", got, c.want)
			}
		})
	}
}
