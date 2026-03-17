package smb

import (
	"fmt"
	"sync"

	smb "github.com/marmos91/dittofs/internal/adapter/smb"
	"github.com/marmos91/dittofs/internal/adapter/smb/header"
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
	sessionConns  *sync.Map // sessionID (uint64) → *smb.ConnInfo
	oplockFileIDs *sync.Map // leaseKey hex (string) → [16]byte (SMB FileID)
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
//   - SessionID = target session (for encryption key lookup)
//   - Command = SMB2_OPLOCK_BREAK
//
// Per MS-SMB2 3.3.4.7: if the session requires encryption, the message
// is encrypted using the session's encryption key via the standard
// SendMessage path.
func (n *transportNotifier) SendLeaseBreak(sessionID uint64, leaseKey [16]byte, currentState, newState uint32, epoch uint16) error {
	ci, ok := n.sessionConns.Load(sessionID)
	if !ok {
		logger.Debug("LeaseBreakNotifier: no connection for session, break will timeout",
			"sessionID", sessionID)
		return nil
	}
	connInfo := ci.(*smb.ConnInfo)

	// Build SMB2 header for unsolicited notification
	hdr := &header.SMB2Header{
		Command:   types.SMB2OplockBreak,
		Status:    types.StatusSuccess,
		Flags:     types.FlagResponse,
		MessageID: 0xFFFFFFFFFFFFFFFF, // Unsolicited notification
		SessionID: sessionID,          // For encryption/signing lookup
		TreeID:    0,
		Credits:   0,
	}

	// Check if this is a traditional oplock (has FileID mapping)
	keyHex := fmt.Sprintf("%x", leaseKey)
	if fileIDVal, isOplock := n.oplockFileIDs.Load(keyHex); isOplock {
		fileID := fileIDVal.([16]byte)
		newLevel := leaseStateToOplockLevelNotify(newState)
		body := encodeOplockBreakNotificationBody(newLevel, fileID)

		logger.Debug("Sending OPLOCK_BREAK_NOTIFICATION",
			"sessionID", sessionID,
			"fileID", fmt.Sprintf("%x", fileID),
			"newLevel", newLevel)

		return smb.SendMessage(hdr, body, connInfo)
	}

	// Standard lease break notification
	var flags uint32
	if (currentState & (lock.LeaseStateWrite | lock.LeaseStateHandle)) != 0 {
		flags = 0x01 // SMB2_NOTIFY_BREAK_LEASE_FLAG_ACK_REQUIRED
	}

	body := encodeLeaseBreakNotificationBody(leaseKey, currentState, newState, epoch, flags)

	logger.Debug("Sending LEASE_BREAK_NOTIFICATION",
		"sessionID", sessionID,
		"currentState", lock.LeaseStateToString(currentState),
		"newState", lock.LeaseStateToString(newState),
		"epoch", epoch,
		"ackRequired", flags != 0)

	return smb.SendMessage(hdr, body, connInfo)
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
// to use in an OPLOCK_BREAK_NOTIFICATION.
func leaseStateToOplockLevelNotify(state uint32) uint8 {
	switch {
	case state&(lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle) ==
		(lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle):
		return 0x09 // Batch
	case state&(lock.LeaseStateRead|lock.LeaseStateWrite) ==
		(lock.LeaseStateRead | lock.LeaseStateWrite):
		return 0x08 // Exclusive
	case state&lock.LeaseStateRead != 0:
		return 0x01 // Level II
	default:
		return 0x00 // None
	}
}

// connRegistryTracker wraps a SessionTracker to also maintain a
// session → ConnInfo mapping for lease break notification routing.
type connRegistryTracker struct {
	inner        smb.SessionTracker
	connInfo     *smb.ConnInfo
	sessionConns *sync.Map
}

// TrackSession records a session on the connection and registers the
// ConnInfo for lease break notification routing.
func (t *connRegistryTracker) TrackSession(sessionID uint64) {
	t.inner.TrackSession(sessionID)
	t.sessionConns.Store(sessionID, t.connInfo)
}

// UntrackSession removes a session from the connection and deregisters
// the ConnInfo from the lease break routing map.
func (t *connRegistryTracker) UntrackSession(sessionID uint64) {
	t.inner.UntrackSession(sessionID)
	t.sessionConns.Delete(sessionID)
}
