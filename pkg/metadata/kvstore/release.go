package kvstore

import (
	"bytes"
	"errors"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// ErrHeld is a release of a file something still holds: an entry, a durable
// open of it or of one of its streams, or no pending release recorded. The
// release changes nothing.
var ErrHeld = errors.New("kvstore: file still held")

// releasePhase1 is a release's first transaction. It re-checks the file's
// holders inside itself — no entry, its pending release recorded, no durable
// open of it or of a stream, the open records range-guarded so an open
// committing first aborts it — then deletes the File, its ACL, xattrs,
// stream links, delete-pending record and pending release, writes a pending
// release for each stream, and records the removal of [0, ∞), which masks
// the refs until Resume drops them. The fences and the rest of the file's
// prefix go when that removal is pruned. A file already gone was released
// before, and a replay changes nothing.
func releasePhase1(tx kv.Txn, share entity.ShareID, file entity.FileID, epoch uint64, op RemoveOp) error {
	if err := rewriteFences(tx, share, file, epoch); err != nil {
		return err
	}
	f, live, err := fileOrGone(tx, share, file)
	if err != nil || !live {
		return err
	}
	if !f.Applied.Less(op.Version) {
		return nil
	}
	if f.Nlink != 0 || f.Parent == f.ID && f.IsDir() {
		return ErrHeld
	}
	if _, ok, err := get(tx, releaseKey(share, file)); err != nil {
		return err
	} else if !ok {
		return ErrHeld
	}
	var streams []entity.FileID
	pre := streamsPrefix(share, file)
	for row, err := range tx.Scan(pre, nil) {
		if err != nil {
			return err
		}
		id, _, err := keys.ReadID(row.Key[len(pre):], keys.IDLen)
		if err != nil {
			return err
		}
		streams = append(streams, entity.FileID(id))
	}
	for _, id := range append([]entity.FileID{file}, streams...) {
		if err := noDurableOpen(tx, share, id); err != nil {
			return err
		}
	}

	for _, p := range [][]byte{xattrsPrefix(share, file), streamsPrefix(share, file)} {
		if err := deleteUnder(tx, p); err != nil {
			return err
		}
	}
	for _, k := range [][]byte{fileKey(share, file), aclKey(share, file), deletePendingKey(share, file), releaseKey(share, file)} {
		if err := tx.Delete(k); err != nil {
			return err
		}
	}
	for _, id := range streams {
		if err := tx.Set(releaseKey(share, id), encodeRelease()); err != nil {
			return err
		}
	}
	m := entity.Removal{Version: op.Version, Extent: entity.ExtentOf(0, entity.EndOfFile), Kind: entity.RemoveRelease}
	return tx.Set(removalKey(share, file, op.Version), encodeRemoval(m))
}

// noDurableOpen refuses a file with a durable open, guarding the range of
// its open records: the scan alone is untracked, and an open committing
// after the scan must abort the release.
func noDurableOpen(tx kv.Txn, share entity.ShareID, file entity.FileID) error {
	pre := opensPrefix(share, file)
	if err := tx.GuardRange(pre); err != nil {
		return err
	}
	for _, err := range tx.Scan(pre, nil) {
		if err != nil {
			return err
		}
		return ErrHeld
	}
	return nil
}

// deleteUnder deletes every key under prefix. The callers' prefixes are
// bounded by share settings: a file's xattrs and streams.
func deleteUnder(tx kv.Txn, prefix []byte) error {
	var ks [][]byte
	for row, err := range tx.Scan(prefix, nil) {
		if err != nil {
			return err
		}
		ks = append(ks, bytes.Clone(row.Key))
	}
	for _, k := range ks {
		if err := tx.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// dropReleased deletes every key of a released file left under its prefix
// but its history: the fences, the removal records, a removed directory's
// leftover time deltas. Its pruned release removal is the last thing that
// needed them, so no key of the file outlives it.
//
// ponytail: the file's version-floor entries are not deleted, because the
// version index is not written yet. Delete them here when it is.
func dropReleased(tx kv.Txn, share entity.ShareID, file entity.FileID) error {
	his := historyPrefix(share, file)
	var ks [][]byte
	for row, err := range tx.Scan(fileKey(share, file), nil) {
		if err != nil {
			return err
		}
		if !bytes.HasPrefix(row.Key, his) {
			ks = append(ks, bytes.Clone(row.Key))
		}
	}
	for _, k := range ks {
		if err := tx.Delete(k); err != nil {
			return err
		}
	}
	return nil
}
