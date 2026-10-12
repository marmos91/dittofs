package kvstore

import (
	"bytes"
	"context"
	"errors"
	"iter"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// Helpers every view's transactions share, so an operation spanning areas
// stays one transaction.

// get reads key, reporting absence as ok == false rather than an error.
func get(r kv.Reader, key []byte) ([]byte, bool, error) {
	b, err := r.Get(key)
	if errors.Is(err, kv.ErrNotFound) {
		return nil, false, nil
	}
	return b, err == nil, err
}

// raise adds one to the counter at key, an absent counter being zero.
func raise(tx kv.Txn, key []byte) error {
	b, ok, err := get(tx, key)
	if err != nil {
		return err
	}
	var v uint64
	if ok {
		if v, err = decodeCounter(b); err != nil {
			return err
		}
	}
	return tx.Set(key, encodeCounter(v+1))
}

// nextVersion is the version a changed record takes: above old, and at least
// the transaction's store time. Store time never goes back, so a record
// deleted and created again starts above every version it had before, and a
// cache keyed by version cannot serve the old record's decision for the new.
func nextVersion(tx kv.Txn, old uint64) uint64 {
	return max(old+1, uint64(tx.Now().UnixNano()))
}

// nextFileVersion is nextVersion for a File's Version, a journal version with
// the epoch half zero.
//
// ponytail: the counter half is store time, standing in for the journal's
// version counter, which the engine draws below the share's version limit.
// Take the version from the caller once the engine passes one in.
func nextFileVersion(tx kv.Txn, old entity.JournalVersion) entity.JournalVersion {
	return entity.VersionOf(nextVersion(tx, old.Counter()))
}

const pageSize = 256

// pages lists the records under prefix after the key after, loading each
// with load, one View per page, so no read transaction stays open while the
// caller works through the listing.
func pages[T any](ctx context.Context, db kv.KV, prefix, after []byte, load func(kv.Reader, kv.KeyValue) (T, error)) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for {
			var page []T
			var last []byte
			err := db.View(ctx, func(r kv.Reader) error {
				page, last = page[:0], nil
				for kvp, err := range r.Scan(prefix, after) {
					if err != nil {
						return err
					}
					v, err := load(r, kvp)
					if err != nil {
						return err
					}
					if page = append(page, v); len(page) == pageSize {
						last = bytes.Clone(kvp.Key)
						break
					}
				}
				return nil
			})
			if err != nil {
				var zero T
				yield(zero, err)
				return
			}
			for _, v := range page {
				if !yield(v, nil) {
					return
				}
			}
			if last == nil {
				return
			}
			after = last
		}
	}
}

// counter reads the counter at key, an absent counter being zero.
func counter(ctx context.Context, db kv.KV, key []byte) (uint64, error) {
	var v uint64
	err := db.View(ctx, func(r kv.Reader) error {
		b, ok, err := get(r, key)
		if err != nil || !ok {
			return err
		}
		v, err = decodeCounter(b)
		return err
	})
	return v, err
}
