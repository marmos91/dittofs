package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// TestShouldRejectUnencryptedTreeConnect_SMB2DialectRejected pins the dialect
// gate: an SMB 2.x session (Dialect < 3.0) cannot negotiate encryption at all
// (MS-SMB2 3.3.5.3 — encryption requires dialect >= 3.0), so honoring the
// per-share SMB2_SHAREFLAG_ENCRYPT_DATA flag for it would hand the client a
// tree it can never send encrypted commands on. The 2.x session is rejected
// up front even when its crypto state would report encryption support.
func TestShouldRejectUnencryptedTreeConnect_SMB2DialectRejected(t *testing.T) {
	h := NewHandler()
	h.EncryptionConfig = EncryptionConfig{Mode: "required"}
	share := &runtime.Share{Name: "/encrypted", EncryptData: true}

	sess := h.CreateSession("127.0.0.1:12345", false, "testuser", "DOMAIN")
	sess.Dialect = types.Dialect0202

	if !shouldRejectUnencryptedTreeConnect(h.EncryptionConfig.Mode, share, sess) {
		t.Fatal("SMB 2.x session must be rejected for an EncryptData share in required mode")
	}

	// An SMB 3.x session with encryption support is still accepted.
	sess3 := h.CreateSession("127.0.0.1:12346", false, "testuser", "DOMAIN")
	sess3.Dialect = types.Dialect0300
	sessCrypto := sess3.GetCryptoState()
	sessCrypto.EncryptData = true
	sessCrypto.Encryptor = &mockEncryptor{}
	if shouldRejectUnencryptedTreeConnect(h.EncryptionConfig.Mode, share, sess3) {
		t.Fatal("SMB 3.0 session with encryption support must not be rejected")
	}
}

// TestPendingLockRegistry_UnregisterAllForTreeInvokesCallbackSynchronously
// pins the teardown ordering: the parked-LOCK callbacks returned by
// UnregisterAllForTree are invoked synchronously by TREE_DISCONNECT (like the
// CLOSE and LOGOFF siblings), so by the time UnregisterAllForTree returns the
// callbacks have run — no unowned goroutine outlives the teardown.
func TestPendingLockRegistry_UnregisterAllForTreeInvokesCallbackSynchronously(t *testing.T) {
	h := NewHandler()
	h.PendingLockRegistry = NewPendingLockRegistry()

	called := 0
	done := make(chan struct{})
	parked := &PendingLock{
		ConnID:    1,
		SessionID: 100,
		TreeID:    7,
		MessageID: 42,
		AsyncId:   1,
		Callback: func(sessionID, messageID, asyncID uint64, status types.Status, body []byte) error {
			called++
			if status != types.StatusRangeNotLocked {
				t.Errorf("status = 0x%x, want STATUS_RANGE_NOT_LOCKED", status)
			}
			close(done)
			return nil
		},
	}
	if err := h.PendingLockRegistry.Register(parked); err != nil {
		t.Fatalf("Register: %v", err)
	}

	removed := h.PendingLockRegistry.UnregisterAllForTree(7)
	if len(removed) != 1 {
		t.Fatalf("expected 1 parked lock removed, got %d", len(removed))
	}
	for _, p := range removed {
		if p.Callback != nil {
			// Synchronous invocation (as TREE_DISCONNECT now does): by the time
			// this loop returns, the callback must have run. A fire-and-forget
			// goroutine would leave done unclosed here on a serial test run —
			// the select makes the async case fail instead of hanging.
			if err := p.Callback(p.SessionID, p.MessageID, p.AsyncId, types.StatusRangeNotLocked, nil); err != nil {
				t.Errorf("callback: %v", err)
			}
		}
	}
	select {
	case <-done:
		if called != 1 {
			t.Fatalf("callback ran %d times, want 1", called)
		}
	default:
		t.Fatal("callback not invoked synchronously")
	}
}

// TestShouldRejectUnencryptedTreeConnect_SMB2WithCrypto pins the
// red-without-fix case: even a 2.x session whose crypto state reports
// encryption support (unreachable in real traffic — 2.x cannot negotiate
// encryption) must be rejected, because the old `!ShouldEncrypt()` check
// alone would accept it and hand the client a tree it can never use.
func TestShouldRejectUnencryptedTreeConnect_SMB2WithCrypto(t *testing.T) {
	h := NewHandler()
	h.EncryptionConfig = EncryptionConfig{Mode: "required"}
	share := &runtime.Share{Name: "/encrypted", EncryptData: true}

	sess := h.CreateSession("127.0.0.1:12347", false, "testuser", "DOMAIN")
	sess.Dialect = types.Dialect0202
	cs := sess.GetCryptoState()
	cs.EncryptData = true
	cs.Encryptor = &mockEncryptor{}

	if !shouldRejectUnencryptedTreeConnect(h.EncryptionConfig.Mode, share, sess) {
		t.Fatal("2.x session must be rejected by the dialect gate even with crypto support")
	}
}
