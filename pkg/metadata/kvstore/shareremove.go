package kvstore

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

var (
	// ErrRehoming is a share whose namespace a re-home is moving.
	ErrRehoming = errors.New("kvstore: share is being re-homed")
	// ErrMoving is a share one of whose namespaces a move has paused.
	ErrMoving = errors.New("kvstore: a namespace of the share is paused by a move")
)

// The steps of a share's removal, recorded in its state record so a crash
// resumes the removal where it stopped.
const (
	stepMarked uint8 = 1 // being removed; name and path free
)

// DeleteShare starts a share's removal and returns once the share is being
// removed: its name and path are free for a new share, ShareList moves, and
// every call into it is stale. Removal is one-way; a second call is stale.
//
// The mark refuses a share a cut is freezing, one with a re-home running, one
// any of whose namespaces a move has paused,
// one with snapshots when the call neither deletes nor detaches them, and,
// when it deletes them, one any use record still holds. Snapshot and use
// records are found under a range guard, and the re-home record by a tracked
// read, so a cut, a re-home or a use record that starts concurrently
// conflicts with the mark in either commit order: each of those reads the
// state the mark writes.
//
// ponytail: only the mark runs. The background steps — snapshots, live state
// and journal, files, server-wide rows, the share's own records, the
// namespace — need the engine, open state, snapshots and GC, which this
// store does not reach yet. Run them from stepMarked when those land. A share
// left at stepMarked serves nothing and holds its records.

func (c ControlPlane) DeleteShare(ctx context.Context, share entity.ShareID, snapshots entity.SnapshotDisposition) error {
	if snapshots.String() == "" {
		return merrs.NewInvalidArgumentError("unknown snapshot disposition")
	}
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		stKey := shareKey(share, keys.SSt)
		b, ok, err := get(tx, stKey)
		if err != nil {
			return err
		}
		if !ok {
			return merrs.NewNotFoundError(share.String(), "share")
		}
		st, err := decodeState(b)
		if err != nil {
			return err
		}
		// ponytail: a running cut does not refuse the removal with ErrDelay,
		// because no cut is recorded yet. Read the share's cut record here
		// when snapshots land.
		if st.State == entity.ShareRemoving || st.State == entity.ShareRetired {
			return entity.ErrStale
		}
		if err := noPausedNamespace(tx, share); err != nil {
			return err
		}
		if _, ok, err := get(tx, shareKey(share, keys.SRh)); err != nil {
			return err
		} else if ok {
			return ErrRehoming
		}
		if snapshots == entity.SnapshotsRefuse {
			if any, err := guardedAny(tx, shareKey(share, keys.SSnap)); err != nil || any {
				if err == nil {
					err = merrs.NewConflictError("delete share", "the share has snapshots; delete or detach them")
				}
				return err
			}
		}
		if snapshots == entity.SnapshotsDelete {
			if any, err := guardedAny(tx, shareKey(share, keys.SUse)); err != nil || any {
				if err == nil {
					err = merrs.NewConflictError("delete share", "a backup, clone, restore, copy or move holds a snapshot")
				}
				return err
			}
		}
		ib, ok, err := get(tx, shareKey(share, keys.SInfo))
		if err != nil {
			return err
		}
		if !ok {
			return merrs.NewNotFoundError(share.String(), "share")
		}
		info, err := decodeShareInfo(share, ib)
		if err != nil {
			return err
		}
		if err := tx.Set(stKey, encodeState(shareState{State: entity.ShareRemoving, Step: stepMarked, Snapshots: snapshots})); err != nil {
			return err
		}
		if err := tx.Delete(nameKey(keys.NXShare, info.Name)); err != nil {
			return err
		}
		if err := tx.Delete(pathKey(info.Path)); err != nil {
			return err
		}
		return raise(tx, shareListKey)
	})
}

// guardedAny reports whether any record lies under prefix, guarding the
// range so a record added after this snapshot aborts the caller.
func guardedAny(tx kv.Txn, prefix []byte) (bool, error) {
	if err := tx.GuardRange(prefix); err != nil {
		return false, err
	}
	for _, err := range tx.Scan(prefix, nil) {
		return err == nil, err
	}
	return false, nil
}

// noPausedNamespace refuses a share any of whose namespaces a move has
// paused, reading the share's namespaces and each pause with tracking.
func noPausedNamespace(tx kv.Txn, share entity.ShareID) error {
	b, ok, err := get(tx, namespacesKey(share))
	if err != nil || !ok {
		return err
	}
	nss, err := decodeNamespaces(b)
	if err != nil {
		return err
	}
	for _, g := range nss.Namespaces {
		if _, paused, err := get(tx, gcPauseKey(g.Namespace)); err != nil {
			return err
		} else if paused {
			return ErrMoving
		}
	}
	return nil
}
