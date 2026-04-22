package session

import (
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/signing"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// ChannelTransport is the minimal interface a channel exposes to senders
// that need to push bytes out on its TCP connection — specifically the
// lease/oplock break notifier (MS-SMB2 §3.3.4.7). Implementations are
// responsible for framing (NetBIOS), write-mutex coordination, and optional
// SMB3 Transform-Header encryption.
type ChannelTransport interface {
	// SendFrame serializes plaintext as a NetBIOS-framed SMB2 message on the
	// underlying connection. When encrypt is true and the implementation has
	// encryption keys for sessionID, plaintext is wrapped in a Transform
	// Header before the write.
	SendFrame(sessionID uint64, plaintext []byte, encrypt bool) error
}

// Channel represents a TCP connection attached to an SMB session. The primary
// connection (established by the initial SESSION_SETUP) and any secondary
// connections bound via SMB2_SESSION_FLAG_BINDING (MS-SMB2 §3.3.5.5.2) are
// each registered as a Channel.
//
// All channels on a session share the session key and session identity, but
// each bound channel derives its own signing key from the session key via the
// dialect-specific KDF (MS-SMB2 §3.1.4.2). The primary channel uses the
// session-level signer (Signer is nil) — the dispatch path falls back via
// Session.SignMessageOnChannel.
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

	// Transport is the write-side adapter for this channel's TCP connection.
	// Used by the break notifier (MS-SMB2 §3.3.4.7) to deliver unsolicited
	// lease / oplock break notifications on every channel of a session. Nil
	// for channels in tests that never emit breaks.
	Transport ChannelTransport
}

// MaxChannelsPerSession is the spec-mandated upper bound on the number of
// concurrent channels bound to a single SMB session. MS-SMB2 §3.3.5.5.2
// requires the server to reject further binds once the per-session channel
// table is full; Windows and Samba enforce 32 (`smbXsrv_client.max_channels`),
// and smb2.multichannel.generic.num_channels asserts the 33rd bind is
// rejected with STATUS_INSUFFICIENT_RESOURCES.
const MaxChannelsPerSession = 32

// AddChannel registers a channel on the session, keyed by ConnID. Returns
// true on success, false when the per-session channel cap would be exceeded.
//
// A replacement of an existing ConnID (which callers must not rely on per the
// §3.3.5.5.2 language "server MUST return STATUS_REQUEST_NOT_ACCEPTED if the
// connection is already bound to the session") does not count against the cap:
// it's the same slot.
//
// The count-and-insert are held under channelsMu to keep the cap race-free
// under concurrent binds — see smb2.multichannel.bugs.bug_15346.
func (s *Session) AddChannel(c *Channel) bool {
	if c == nil {
		return false
	}
	if c.BoundAt.IsZero() {
		c.BoundAt = time.Now()
	}
	s.channelsMu.Lock()
	defer s.channelsMu.Unlock()
	if _, replacing := s.channels[c.ConnID]; !replacing {
		if len(s.channels) >= MaxChannelsPerSession {
			return false
		}
	}
	s.channels[c.ConnID] = c
	return true
}

// GetChannel returns the channel for the given ConnID, or nil if none is
// registered. Safe for concurrent use.
func (s *Session) GetChannel(connID uint64) *Channel {
	s.channelsMu.RLock()
	defer s.channelsMu.RUnlock()
	return s.channels[connID]
}

// RemoveChannel unregisters a channel. Called when the TCP connection closes.
func (s *Session) RemoveChannel(connID uint64) {
	s.channelsMu.Lock()
	defer s.channelsMu.Unlock()
	delete(s.channels, connID)
}

// ListChannels returns a snapshot of all channels currently bound to the
// session. Used for break-notification fan-out in the lease/oplock layer.
// Order is not guaranteed.
func (s *Session) ListChannels() []*Channel {
	s.channelsMu.RLock()
	defer s.channelsMu.RUnlock()
	out := make([]*Channel, 0, len(s.channels))
	for _, c := range s.channels {
		out = append(out, c)
	}
	return out
}

// ChannelCount returns the current number of channels bound to the session.
func (s *Session) ChannelCount() int {
	s.channelsMu.RLock()
	defer s.channelsMu.RUnlock()
	return len(s.channels)
}
