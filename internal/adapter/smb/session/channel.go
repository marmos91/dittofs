package session

import (
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/signing"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// Channel represents a TCP connection bound to an SMB session via
// SESSION_SETUP with SMB2_SESSION_FLAG_BINDING (MS-SMB2 §3.3.5.5.2).
//
// In Phase 1 only bound (secondary) connections register a Channel. The
// original connection that established the session continues to use the
// session-level Signer via the dispatch fallback. This keeps the change
// surface-area narrow; a future refactor may register every connection as a
// Channel and remove the fallback.
//
// All channels on a session share the session key and session identity, but
// each bound channel derives its own signing key from the session key via the
// dialect-specific KDF (MS-SMB2 §3.1.4.2). The per-channel signing key is used
// to verify signatures on requests arriving on that connection.
//
// Samba reference: smbXsrv_channel_global0 in source3/smbd/smbXsrv_session.c —
// each entry holds a signing_algo, encryption_cipher, and signing_key that
// overrides the session-level defaults for that connection.
type Channel struct {
	// ConnID is a stable per-connection identifier assigned when the TCP
	// connection is accepted. Used to look up the channel when verifying a
	// request's signature.
	ConnID uint64

	// RemoteAddr is the client address for this channel's TCP connection.
	RemoteAddr string

	// Dialect is the SMB2 dialect negotiated on this channel. Per §3.3.5.5.2
	// all channels on a session must share the same dialect.
	Dialect types.Dialect

	// SigningAlgo is the negotiated signing algorithm ID for this channel.
	SigningAlgo uint16

	// SigningKey is the 16-byte per-channel signing key derived from the
	// session key. See DeriveChannelSigningKey.
	SigningKey []byte

	// Signer is the signing/verification implementation built from SigningKey.
	Signer signing.Signer

	// PreauthHash is the per-channel preauth integrity hash (SHA-512, 64 bytes)
	// carried forward from the NEGOTIATE response on this connection plus the
	// binding SESSION_SETUP messages. Only meaningful for SMB 3.1.1; unused for
	// 3.0 / 3.0.2.
	PreauthHash [64]byte

	// BoundAt is the time the channel was registered on the session.
	BoundAt time.Time
}

// AddChannel registers a channel on the session, keyed by ConnID. A subsequent
// AddChannel with the same ConnID replaces the prior entry — callers must not
// rely on deduplication since MS-SMB2 explicitly rejects binding an already-
// bound connection at the handler layer (§3.3.5.5.2).
func (s *Session) AddChannel(c *Channel) {
	if c == nil {
		return
	}
	if c.BoundAt.IsZero() {
		c.BoundAt = time.Now()
	}
	s.channels.Store(c.ConnID, c)
}

// GetChannel returns the channel for the given ConnID, or nil if none is
// registered. Safe for concurrent use.
func (s *Session) GetChannel(connID uint64) *Channel {
	v, ok := s.channels.Load(connID)
	if !ok {
		return nil
	}
	return v.(*Channel)
}

// RemoveChannel unregisters a channel. Called when the TCP connection closes.
func (s *Session) RemoveChannel(connID uint64) {
	s.channels.Delete(connID)
}

// ListChannels returns a snapshot of all channels currently bound to the
// session. Used for break-notification fan-out in the lease/oplock layer.
// Order is not guaranteed.
func (s *Session) ListChannels() []*Channel {
	var out []*Channel
	s.channels.Range(func(_, v any) bool {
		out = append(out, v.(*Channel))
		return true
	})
	return out
}
