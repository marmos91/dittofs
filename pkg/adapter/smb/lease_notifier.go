package smb

import (
	"fmt"
	"sync"

	smb "github.com/marmos91/dittofs/internal/adapter/smb"
	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// transportNotifier implements lease.LeaseBreakNotifier by sending
// LEASE_BREAK_NOTIFICATION or OPLOCK_BREAK_NOTIFICATION messages over
// the wire to SMB clients.
//
// It maintains a session → ConnInfo mapping so that when a lease break
// is dispatched by the LockManager, the notification can be sent on
// the correct TCP connection.
//
// For traditional oplocks (mapped to leases internally), it also maintains
// a synthetic lease key → FileID mapping so that the correct 24-byte
// oplock break notification format is sent instead of the 44-byte lease format.
type transportNotifier struct {
	sessionConns  *sync.Map         // sessionID (uint64) → *smb.ConnInfo
	oplockFileIDs *sync.Map         // leaseKey hex (string) → [16]byte (SMB FileID)
	handler       sessionHandlerAPI // used to look up *session.Session for channel fan-out
}

// sessionHandlerAPI is the narrow slice of *handlers.Handler that the
// notifier needs. Keeping it as an interface keeps this file free of a
// handler import cycle and makes it trivial to substitute in tests.
type sessionHandlerAPI interface {
	GetSession(sessionID uint64) (*session.Session, bool)
}

// RegisterOplockFileID registers a synthetic lease key → FileID mapping so that
// break notifications for traditional oplocks use the 24-byte oplock format.
func (n *transportNotifier) RegisterOplockFileID(leaseKey [16]byte, fileID [16]byte) {
	keyHex := fmt.Sprintf("%x", leaseKey)
	n.oplockFileIDs.Store(keyHex, fileID)
}

// UnregisterOplockFileID removes a synthetic lease key → FileID mapping.
func (n *transportNotifier) UnregisterOplockFileID(leaseKey [16]byte) {
	keyHex := fmt.Sprintf("%x", leaseKey)
	n.oplockFileIDs.Delete(keyHex)
}

// SendLeaseBreak sends an SMB2 LEASE_BREAK_NOTIFICATION [MS-SMB2] 2.2.23.2
// or OPLOCK_BREAK_NOTIFICATION [MS-SMB2] 2.2.23.1 to the client.
//
// If the lease key corresponds to a traditional oplock (registered via
// RegisterOplockFileID), a 24-byte oplock break notification is sent.
// Otherwise, a 44-byte lease break notification is sent.
//
// The notification is an unsolicited server-to-client message with:
//   - MessageID = 0xFFFFFFFFFFFFFFFF (unsolicited)
//   - SessionID = 0 (per MS-SMB2 2.2.23.2)
//   - Command = SMB2_OPLOCK_BREAK
//
// Per MS-SMB2 3.3.4.7: unsolicited notifications are NOT signed.
// If the session requires encryption, the message is encrypted using the
// session's encryption key (the transform header carries the real session ID).
//
// Multi-channel (MS-SMB2 §3.3.4.7): the break is delivered on exactly ONE
// channel — broadcasting would cause the client to see duplicate breaks and
// fail the multichannel.leases/oplocks test suites. The channel is selected
// in order: (1) the session's primary connection (the one the open was
// created on, recorded in sessionConns); (2) any other channel registered
// on the session. If the first pick fails (e.g. closed connection), the
// remaining channels are tried in turn.
func (n *transportNotifier) SendLeaseBreak(sessionID uint64, leaseKey [16]byte, currentState, newState uint32, epoch uint16) error {
	sess, _ := n.handler.GetSession(sessionID)

	// Build SMB2 header for unsolicited notification.
	// Per MS-SMB2 2.2.23.2: SessionId MUST be 0 in the SMB2 header.
	hdr := &header.SMB2Header{
		Command:   types.SMB2OplockBreak,
		Status:    types.StatusSuccess,
		Flags:     types.FlagResponse,
		MessageID: 0xFFFFFFFFFFFFFFFF, // Unsolicited notification
		SessionID: 0,                  // Per MS-SMB2 2.2.23.2
		TreeID:    0,
		Credits:   0,
	}

	var body []byte

	// Check if this is a traditional oplock (has FileID mapping)
	keyHex := fmt.Sprintf("%x", leaseKey)
	if fileIDVal, isOplock := n.oplockFileIDs.Load(keyHex); isOplock {
		fileID := fileIDVal.([16]byte)
		newLevel := leaseStateToOplockLevelNotify(newState)
		body = encodeOplockBreakNotificationBody(newLevel, fileID)

		logger.Debug("Sending OPLOCK_BREAK_NOTIFICATION",
			"sessionID", sessionID,
			"fileID", fmt.Sprintf("%x", fileID),
			"newLevel", newLevel)
	} else {
		// Standard lease break notification
		var flags uint32
		if (currentState & (lock.LeaseStateWrite | lock.LeaseStateHandle)) != 0 {
			flags = 0x01 // SMB2_NOTIFY_BREAK_LEASE_FLAG_ACK_REQUIRED
		}

		body = encodeLeaseBreakNotificationBody(leaseKey, currentState, newState, epoch, flags)

		logger.Debug("Sending LEASE_BREAK_NOTIFICATION",
			"sessionID", sessionID,
			"currentState", lock.LeaseStateToString(currentState),
			"newState", lock.LeaseStateToString(newState),
			"epoch", epoch,
			"ackRequired", flags != 0)
	}

	smbPayload := append(hdr.Encode(), body...)
	encrypt := sess != nil && sess.ShouldEncrypt()

	transports := n.orderedTransports(sessionID, sess)
	if len(transports) == 0 {
		logger.Debug("LeaseBreakNotifier: no transports for session, break will timeout",
			"sessionID", sessionID)
		return nil
	}

	// transports is non-empty here (early-returned above), so lastErr is
	// guaranteed non-nil if we exit the loop without returning success.
	var lastErr error
	for _, t := range transports {
		if err := t.SendFrame(sessionID, smbPayload, encrypt); err != nil {
			logger.Debug("LeaseBreakNotifier: channel delivery failed, trying next",
				"sessionID", sessionID,
				"error", err)
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("send break: %w", lastErr)
}

// orderedTransports returns the candidate channels to deliver a break on,
// in preferred order: primary connection first (sessionConns entry), then
// any remaining bound channels. Consumers deliver to the first candidate
// and fall through to the next only on write failure.
func (n *transportNotifier) orderedTransports(sessionID uint64, sess *session.Session) []session.ChannelTransport {
	var primary session.ChannelTransport
	if ci, ok := n.sessionConns.Load(sessionID); ok {
		if ct, ok := ci.(*smb.ConnInfo); ok && ct != nil {
			primary = ct
		}
	}

	var channels []*session.Channel
	if sess != nil {
		channels = sess.ListChannels()
	}

	out := make([]session.ChannelTransport, 0, 1+len(channels))
	if primary != nil {
		out = append(out, primary)
	}
	for _, ch := range channels {
		if ch.Transport == nil || ch.Transport == primary {
			continue
		}
		out = append(out, ch.Transport)
	}
	return out
}

// encodeLeaseBreakNotificationBody encodes the 44-byte body of an
// SMB2 LEASE_BREAK_NOTIFICATION [MS-SMB2] 2.2.23.2.
func encodeLeaseBreakNotificationBody(leaseKey [16]byte, currentState, newState uint32, epoch uint16, flags uint32) []byte {
	w := smbenc.NewWriter(44)
	w.WriteUint16(44)           // StructureSize
	w.WriteUint16(epoch)        // NewEpoch
	w.WriteUint32(flags)        // Flags (ACK_REQUIRED or 0)
	w.WriteBytes(leaseKey[:])   // LeaseKey (16 bytes)
	w.WriteUint32(currentState) // CurrentLeaseState
	w.WriteUint32(newState)     // NewLeaseState
	w.WriteZeros(12)            // Reserved (12 bytes)
	return w.Bytes()
}

// encodeOplockBreakNotificationBody encodes the 24-byte body of an
// SMB2 OPLOCK_BREAK_NOTIFICATION [MS-SMB2] 2.2.23.1.
func encodeOplockBreakNotificationBody(newLevel uint8, fileID [16]byte) []byte {
	w := smbenc.NewWriter(24)
	w.WriteUint16(24)       // StructureSize
	w.WriteUint8(newLevel)  // OplockLevel (break to level)
	w.WriteUint8(0)         // Reserved
	w.WriteUint32(0)        // Reserved2
	w.WriteBytes(fileID[:]) // FileId (16 bytes)
	return w.Bytes()
}

// leaseStateToOplockLevelNotify maps a lease state to the oplock level
// to use in an OPLOCK_BREAK_NOTIFICATION [MS-SMB2] 2.2.23.1.
//
// Per MS-SMB2 2.2.23.1: "OplockLevel: The server MUST set this to one of
// SMB2_OPLOCK_LEVEL_NONE (0x00) or SMB2_OPLOCK_LEVEL_II (0x01)."
//
// Batch (0x09) and Exclusive (0x08) are NOT valid break-to levels.
// Any state with Read maps to Level II; otherwise None.
func leaseStateToOplockLevelNotify(state uint32) uint8 {
	if state&lock.LeaseStateRead != 0 {
		return 0x01 // Level II
	}
	return 0x00 // None
}

// connRegistryTracker wraps a SessionTracker to also maintain the
// session → ConnInfo mapping (fallback for primary-only sessions) and to
// register the primary channel on each session so break fan-out (MS-SMB2
// §3.3.4.7) can enumerate every connection attached to a session.
type connRegistryTracker struct {
	inner         smb.SessionTracker
	connInfo      *smb.ConnInfo
	sessionConns  *sync.Map
	sessionLookup sessionHandlerAPI
}

// TrackSession records a session on the connection, registers the ConnInfo
// for break-notification fallback, and adds the primary Channel to the
// session's channel registry. The primary channel's Signer is left nil —
// dispatch falls back to the session-level signer via SignMessageOnChannel.
func (t *connRegistryTracker) TrackSession(sessionID uint64) {
	t.inner.TrackSession(sessionID)
	t.sessionConns.Store(sessionID, t.connInfo)
	if t.sessionLookup == nil {
		return
	}
	sess, ok := t.sessionLookup.GetSession(sessionID)
	if !ok {
		return
	}
	sess.AddChannel(&session.Channel{
		ConnID:     t.connInfo.ConnID,
		RemoteAddr: t.connInfo.Conn.RemoteAddr().String(),
		Transport:  t.connInfo,
	})
}

// UntrackSession removes a session from the connection, deregisters its
// ConnInfo from the break fallback, and unbinds the channel from the
// session registry.
func (t *connRegistryTracker) UntrackSession(sessionID uint64) {
	t.inner.UntrackSession(sessionID)
	t.sessionConns.Delete(sessionID)
	if t.sessionLookup != nil {
		if sess, ok := t.sessionLookup.GetSession(sessionID); ok {
			sess.RemoveChannel(t.connInfo.ConnID)
		}
	}
}
