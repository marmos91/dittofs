package kvstore

import (
	"bytes"
	"context"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// FoldDirectory folds a batch of a directory's time deltas into its record,
// within the key budget, and reports whether deltas are left: the record
// takes the latest times and the highest version among them, and each
// delta's version-index entry gives way to the directory's own. A Get
// reads the same before and after.
//
// decision: the store folds whenever it is asked. That no create, link or
// rename into the directory is in flight during a fold, so the fold never
// aborts the creates it folds, is the directory's primary's to arrange
// before it calls this; a create that does race it conflicts on the
// directory record it guards and retries, which is correct, only slower.
//
// recount reports a directory whose Subdirs came out below zero: a breach,
// stored as the deltas sum to rather than clamped, which the caller raises
// as a health condition and repairs with RecountSubdirs. Reads report the
// link count as 2 meanwhile, and nothing wedges.
func (n Namespace) FoldDirectory(ctx context.Context, share entity.ShareID, dir entity.FileID) (more, recount bool, err error) {
	err = n.s.kv.Update(ctx, func(tx kv.Txn) error {
		d, err := fileIn(tx, share, dir)
		if err != nil {
			return err
		}
		was := d.Version
		folded := 0
		// Each delta is two deletes, and the record with its two
		// version-index keys is three writes.
		if folded, more, err = foldDeltas(tx, &d, max(1, (n.s.k-3)/2)); err != nil || folded == 0 {
			return err
		}
		recount = d.Subdirs < 0
		return putFile(tx, was, d)
	})
	return more, recount, err
}

// RecountSubdirs repairs a directory's Subdirs from its entries: it counts
// those naming a directory, a page per transaction, then sets Subdirs so
// that with the deltas not yet folded it sums to that count.
//
// decision: as with FoldDirectory, the store recounts whenever asked, and
// holding the directory's entry changes while the count runs is the
// directory's primary's to arrange; an entry changed between two pages is
// counted as of its page.
//
// ponytail: the last transaction scans every unfolded delta of the
// directory, unbounded, as SetAttrs' fold does; the primary's regular fold
// keeps them few. Bound it by K when a directory shows many.
func (n Namespace) RecountSubdirs(ctx context.Context, share entity.ShareID, dir entity.FileID) error {
	var count int64
	for t, err := range pages(ctx, n.s.kv, entriesPrefix(share, dir), nil, func(_ kv.Reader, row kv.KeyValue) (entity.FileType, error) {
		e, err := decodeEntry(dir, row.Value)
		return e.Type, err
	}) {
		if err != nil {
			return err
		}
		if t == entity.Directory {
			count++
		}
	}
	return n.s.kv.Update(ctx, func(tx kv.Txn) error {
		d, err := fileIn(tx, share, dir)
		if err != nil {
			return err
		}
		pending := int64(0)
		for row, err := range tx.Scan(deltasPrefix(share, dir), nil) {
			if err != nil {
				return err
			}
			x, err := decodeDelta(row.Value)
			if err != nil {
				return err
			}
			pending += x.Subdirs
		}
		if d.Subdirs == count-pending {
			return nil
		}
		d.Subdirs = count - pending
		return putFile(tx, d.Version, d)
	})
}

// foldDeltas folds up to limit of d's deltas into d in memory, a negative
// limit meaning all, deleting each delta and its version-index entry. The
// caller writes d.
//
// ponytail: deltas are folded in key order, not in the order of the cut
// each was written after, since no snapshot cut is recorded yet and every
// delta carries the same one. Fold one cut's deltas per transaction, oldest
// first, and move the superseded record to history, when cuts land.
func foldDeltas(tx kv.Txn, d *entity.File, limit int) (folded int, more bool, err error) {
	pre := deltasPrefix(d.Share, d.ID)
	var keys [][]byte
	for row, err := range tx.Scan(pre, nil) {
		if err != nil {
			return 0, false, err
		}
		if limit >= 0 && folded == limit {
			more = true
			break
		}
		x, err := decodeDelta(row.Value)
		if err != nil {
			return 0, false, err
		}
		*d = applyDelta(*d, x)
		keys = append(keys, bytes.Clone(row.Key), deltaFloorKey(d.Share, d.ID, x.Version, [32]byte(row.Key[len(pre):])))
		folded++
	}
	for _, k := range keys {
		if err := tx.Delete(k); err != nil {
			return 0, false, err
		}
	}
	return folded, more, nil
}

// applyDelta is d with one delta applied: the latest times and the highest
// version win, and Subdirs takes the change.
func applyDelta(d entity.File, x delta) entity.File {
	d.Subdirs += x.Subdirs
	if x.Modify.After(d.Modify) {
		d.Modify = x.Modify
	}
	if x.Change.After(d.Change) {
		d.Change = x.Change
	}
	d.Version = entity.MaxVersion(d.Version, x.Version)
	return d
}
