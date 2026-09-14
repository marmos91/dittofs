package handlers

import (
	"context"
	"sync/atomic"

	"github.com/marmos91/dittofs/internal/adapter/smb/pending"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// newTestPendingCreate builds a parked CREATE for tests in this package that
// exercise the handler paths around the registry rather than the registry
// itself. The registry's own tests carry their own copy, since the two packages
// cannot share unexported test helpers.
func newTestPendingCreate(connID, sessionID, messageID, asyncId uint64, calls *atomic.Int32) *pending.PendingCreate {
	_, cancel := context.WithCancel(context.Background())
	return &pending.PendingCreate{
		ConnID:    connID,
		SessionID: sessionID,
		MessageID: messageID,
		AsyncId:   asyncId,
		Cancel:    cancel,
		Callback: func(_, _, _ uint64, _ types.Status, _ []byte) error {
			calls.Add(1)
			return nil
		},
	}
}

// newTestPendingLock is the blocking-LOCK twin of newTestPendingCreate.
func newTestPendingLock(connID, sessionID, messageID, asyncId uint64, treeID uint32, cancelCount *atomic.Int32) *pending.PendingLock {
	_, cancel := context.WithCancel(context.Background())
	return &pending.PendingLock{
		ConnID:    connID,
		SessionID: sessionID,
		TreeID:    treeID,
		MessageID: messageID,
		AsyncId:   asyncId,
		OwnerID:   "owner",
		Cancel: func() {
			if cancelCount != nil {
				cancelCount.Add(1)
			}
			cancel()
		},
		Callback: func(uint64, uint64, uint64, types.Status, []byte) error { return nil },
	}
}

// newTestPipeRead is the named-pipe READ twin.
func newTestPipeRead(fileID byte, connID, sessionID, messageID, asyncId uint64, done chan<- types.Status) *pending.PendingPipeRead {
	var fid [16]byte
	fid[0] = fileID
	return &pending.PendingPipeRead{
		FileID:    fid,
		ConnID:    connID,
		SessionID: sessionID,
		MessageID: messageID,
		AsyncId:   asyncId,
		Callback: func(_, _, _ uint64, st types.Status, _ []byte) error {
			if done != nil {
				done <- st
			}
			return nil
		},
	}
}
