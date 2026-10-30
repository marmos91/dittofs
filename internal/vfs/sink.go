package vfs

import (
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/openstate"
)

// The filesystem is open state's Sink: the table raises each of these after
// the step that raised it has released every table lock, on a goroutine that
// may still hold the file's admission lock. So the filesystem only queues and
// returns: a method that took that lock, itself or through a table call that
// serves waiters, would wait on its own caller.
//
// ponytail: every method drops what it is handed. Until the Clients area
// keeps each client's Callbacks, there is nobody to deliver a recall, a
// notification, a revocation or a lock wait's end to; until release is built,
// nothing runs a close's release or a shard's pending releases. A recall
// dropped is revoked at its deadline, a waiter granted keeps its lock unknown
// to its client, and an orphan's content stays held. Fill each with the piece
// that builds what it delivers to.

func (fs *Filesystem) ClientEnded(c entity.ClientID, shards []openstate.ShardID, why openstate.RevokeReason) {
}

func (fs *Filesystem) Closed(f entity.FileID, r openstate.CloseResult) {}

func (fs *Filesystem) LockWaitOver(w openstate.LockWaiter, granted bool) {}

func (fs *Filesystem) RecallGrant(g openstate.CachingGrant) {}

func (fs *Filesystem) RecallAny(c entity.ClientID, keep int) {}

func (fs *Filesystem) Revoked(r openstate.Revocation) {}

func (fs *Filesystem) Notify(c entity.ClientID, w openstate.WatchID, ch []openstate.Change) {}

func (fs *Filesystem) GraceEnded(share entity.ShareID, sh openstate.ShardID) {}
