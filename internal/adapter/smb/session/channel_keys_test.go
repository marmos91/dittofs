package session

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// TestDeriveChannelSigningKey_MatchesDeriveAllKeys_SMB30 verifies that the
// channel-signing-key helper produces the same bytes as the whole-session
// DeriveAllKeys path for SMB 3.0 (where there is no preauth hash). This is
// effectively a contract check: a bound channel using the session's preauth
// hash must land on the same signing key DeriveAllKeys would derive.
func TestDeriveChannelSigningKey_MatchesDeriveAllKeys_SMB30(t *testing.T) {
	sessionKey := bytes.Repeat([]byte{0x42}, 16)

	cs := DeriveAllKeys(sessionKey, types.Dialect0300, [64]byte{}, 0, 0)

	channelKey, err := DeriveChannelSigningKey(sessionKey, types.Dialect0300, [64]byte{})
	if err != nil {
		t.Fatalf("DeriveChannelSigningKey: %v", err)
	}
	if !bytes.Equal(channelKey, cs.SigningKey) {
		t.Fatalf("channel key differs from session key for same inputs:\n channel=%x\n session=%x", channelKey, cs.SigningKey)
	}
}

// TestDeriveChannelSigningKey_DistinctForDifferentPreauth verifies that
// SMB 3.1.1 produces a different signing key for different preauth hashes.
// This is the core property that makes per-channel signing meaningful — a
// bound channel's preauth hash diverges from the primary's after NEGOTIATE.
func TestDeriveChannelSigningKey_DistinctForDifferentPreauth(t *testing.T) {
	sessionKey := bytes.Repeat([]byte{0x10}, 16)
	var primaryHash, boundHash [64]byte
	for i := range primaryHash {
		primaryHash[i] = byte(i)
		boundHash[i] = byte(255 - i)
	}

	primaryKey, err := DeriveChannelSigningKey(sessionKey, types.Dialect0311, primaryHash)
	if err != nil {
		t.Fatal(err)
	}
	boundKey, err := DeriveChannelSigningKey(sessionKey, types.Dialect0311, boundHash)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(primaryKey, boundKey) {
		t.Fatalf("signing keys collide for different preauth hashes; primary=bound=%x", primaryKey)
	}
	if len(boundKey) != 16 {
		t.Fatalf("signing key length=%d, want 16", len(boundKey))
	}
}

func TestDeriveChannelSigningKey_RejectsSMB2x(t *testing.T) {
	sessionKey := make([]byte, 16)
	if _, err := DeriveChannelSigningKey(sessionKey, types.Dialect0210, [64]byte{}); err == nil {
		t.Fatal("expected error for SMB 2.1 dialect, got nil")
	}
	if _, err := DeriveChannelSigningKey(sessionKey, types.Dialect0202, [64]byte{}); err == nil {
		t.Fatal("expected error for SMB 2.0.2 dialect, got nil")
	}
}

func TestDeriveChannelSigningKey_RejectsEmptySessionKey(t *testing.T) {
	if _, err := DeriveChannelSigningKey(nil, types.Dialect0311, [64]byte{}); err == nil {
		t.Fatal("expected error for empty session key, got nil")
	}
}
