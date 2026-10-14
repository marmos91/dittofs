package kvstore

import (
	"bytes"
	"context"
	"errors"
	"slices"

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
// open of it or of a stream, no clone onto it not done, the open and removal
// records range-guarded so either committing first aborts it — then deletes
// the File, its ACL, xattrs, stream links, delete-pending record and pending
// release, writes a pending release for each stream, and records the removal
// of [0, ∞), which masks the refs until Resume drops them and carries the
// File's Version, which names its version-index entry. The fences, that
// entry and the rest of the file's prefix go when that removal is pruned. A
// file already gone was released before, and a replay changes nothing.
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
	// An unfinished clone onto the file holds it, as an open does. Its
	// removal records are found by scan, so their range is guarded: a clone
	// whose phase 1 commits after this snapshot aborts the release.
	if err := tx.GuardRange(removalsPrefix(share, file)); err != nil {
		return err
	}
	if cloning, err := cloneRunning(tx, share, file); err != nil {
		return err
	} else if cloning {
		return ErrHeld
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
	for _, k := range [][]byte{fileKey(share, file), aclKey(share, file), deletePendingKey(share, file)} {
		if err := tx.Delete(k); err != nil {
			return err
		}
	}
	if err := dropRelease(tx, share, f.Shard, file); err != nil {
		return err
	}
	for _, id := range streams {
		s, err := fileIn(tx, share, id)
		if err != nil {
			return err
		}
		if err := putRelease(tx, share, s.Shard, id); err != nil {
			return err
		}
	}
	if err := chargeFile(tx, share, &f, nil); err != nil {
		return err
	}
	// The file's version-index entry stays at its Version, which the removal
	// record carries as FileVersion until the prune deletes both.
	m := entity.Removal{Version: op.Version, Extent: entity.ExtentOf(0, entity.EndOfFile), Kind: entity.RemoveRelease,
		OvSpan: f.MaxOverwrite, FileVersion: f.Version}
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

// pruneReleased deletes the keys a released file left under its prefix, its
// history apart — a removed directory's leftover time deltas, each with its
// version-index entry, and anything else there — in batches within the key
// budget, since a directory can keep any number of unfolded deltas. Each
// batch reads the offload fence, tracked, as PruneRemovals does. The last
// deletes the release's removal, the fences and the File's own index entry,
// at the removal's FileVersion, so no key of the file outlives it. A re-run
// resumes where the last stopped: what a batch deleted is gone.
//
// ponytail: each batch walks the keys it keeps — the fences, the removal and
// the history — again from the prefix's start. History is empty until
// snapshot cuts land; scan around its range then if its walk shows.
func (s *Store) pruneReleased(ctx context.Context, share entity.ShareID, file entity.FileID, m entity.Removal) error {
	limit := max(2, s.k-4) // a delta and its index entry are two deletes
	for {
		var more bool
		err := s.kv.Update(ctx, func(tx kv.Txn) error {
			if _, _, err := get(tx, offloadFenceKey(share, file)); err != nil {
				return err
			}
			if _, ok, err := get(tx, removalKey(share, file, m.Version)); err != nil || !ok {
				more = false
				return err // pruned already
			}
			ks, err := releasedKeys(tx, share, file, m.Version, limit)
			if err != nil {
				return err
			}
			if more = len(ks) > 0; !more {
				ks = [][]byte{removalKey(share, file, m.Version), existenceFenceKey(share, file), offloadFenceKey(share, file),
					floorKey(share, file, m.FileVersion)}
			}
			for _, k := range ks {
				if err := tx.Delete(k); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil || !more {
			return err
		}
	}
}

// releasedKeys is up to limit keys a released file's prune deletes before
// its last batch: every key under its prefix but its history, its fences and
// the release's removal at v, with a delta's index entry beside the delta.
func releasedKeys(r kv.Reader, share entity.ShareID, file entity.FileID, v entity.JournalVersion, limit int) ([][]byte, error) {
	his, deltas, rms := historyPrefix(share, file), deltasPrefix(share, file), removalsPrefix(share, file)
	keep := [][]byte{existenceFenceKey(share, file), offloadFenceKey(share, file), removalKey(share, file, v)}
	var ks [][]byte
	for row, err := range r.Scan(fileKey(share, file), nil) {
		if err != nil {
			return nil, err
		}
		if bytes.HasPrefix(row.Key, his) || slices.ContainsFunc(keep, func(k []byte) bool { return bytes.Equal(k, row.Key) }) {
			continue
		}
		if bytes.HasPrefix(row.Key, rms) {
			if v, _, err := keys.ReadVersion(row.Key[len(rms):]); err != nil {
				return nil, err
			} else if m, err := decodeRemoval(v, row.Value); err != nil {
				return nil, err
			} else if m.Kind == entity.RemoveClone && !m.Done {
				continue // a clone not done is never pruned
			}
		}
		if len(ks)+2 > limit {
			break
		}
		ks = append(ks, bytes.Clone(row.Key))
		if bytes.HasPrefix(row.Key, deltas) {
			d, err := decodeDelta(row.Value)
			if err != nil {
				return nil, err
			}
			ks = append(ks, deltaFloorKey(share, file, d.Version, [32]byte(row.Key[len(deltas):])))
		}
	}
	return ks, nil
}
