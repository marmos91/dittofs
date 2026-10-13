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
func (n Namespace) FoldDirectory(ctx context.Context, share entity.ShareID, dir entity.FileID) (more bool, err error) {
	err = n.s.kv.Update(ctx, func(tx kv.Txn) error {
		d, err := fileIn(tx, share, dir)
		if err != nil {
			return err
		}
		was := d.Version
		folded := 0
		if folded, more, err = foldDeltas(tx, &d, max(1, (n.s.k-2)/2)); err != nil || folded == 0 {
			return err
		}
		return putFile(tx, was, d)
	})
	return more, err
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
		if limit >= 0 && len(keys) == limit {
			more = true
			break
		}
		x, err := decodeDelta(row.Value)
		if err != nil {
			return 0, false, err
		}
		*d = applyDelta(*d, x)
		keys = append(keys, bytes.Clone(row.Key), deltaFloorKey(d.Share, d.ID, x.Version, [32]byte(row.Key[len(pre):])))
	}
	for _, k := range keys {
		if err := tx.Delete(k); err != nil {
			return 0, false, err
		}
	}
	return len(keys) / 2, more, nil
}

// applyDelta is d with one time delta applied: the latest times and the
// highest version win.
func applyDelta(d entity.File, x delta) entity.File {
	if x.Modify.After(d.Modify) {
		d.Modify = x.Modify
	}
	if x.Change.After(d.Change) {
		d.Change = x.Change
	}
	d.Version = entity.MaxVersion(d.Version, x.Version)
	return d
}
