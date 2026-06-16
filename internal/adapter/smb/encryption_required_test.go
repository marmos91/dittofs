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
