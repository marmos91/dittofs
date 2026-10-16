package badger

import (
	"fmt"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/marmos91/dittofs/internal/logger"
)

// prefixRetiredObjectID is the key prefix of the retired ObjectID index:
// "obj:<hash>" -> file UUID, a unique index over a hash of each file's chunk
// list. Nothing writes or reads it any more, but a store written by an older
// binary, or restored from an older dump, still holds such keys.
const prefixRetiredObjectID = "obj:"

// retiredObjectIDDeleteBatch bounds how many keys one transaction deletes, so a
// store with many leftover keys never hits badger's transaction size limit.
const retiredObjectIDDeleteBatch = 1000

// dropRetiredObjectIDIndex deletes every leftover key of the retired ObjectID
// index and returns how many it deleted.
//
// It runs on every open and after every restore, before the store serves. Once
// the keys are gone and compacted away this is a single seek that finds
// nothing, so it needs no marker.
//
// decision: the keys are deleted by iterating the prefix in bounded batches,
// not with DropPrefix. DropPrefix blocks every writer, flushes the memtables
// and compacts the whole tree, a cost every open would pay even with
// no key left. Overturn only if a store is found holding so many leftover keys
// that batched deletes make the open noticeably slow.
//
// Each scan resumes just after the last key the previous batch deleted. A
// deleted key stays in the tree as a tombstone until compaction, and the
// iterator steps over tombstones one by one, so restarting every scan at the
// prefix would make the whole drop quadratic in the number of keys.
func dropRetiredObjectIDIndex(db *badgerdb.DB) (int, error) {
	prefix := []byte(prefixRetiredObjectID)
	start := prefix
	total := 0

	for {
		var keys [][]byte

		err := db.View(func(txn *badgerdb.Txn) error {
			opts := badgerdb.DefaultIteratorOptions
			opts.PrefetchValues = false // keys only
			opts.Prefix = prefix

			it := txn.NewIterator(opts)
			defer it.Close()

			for it.Seek(start); it.ValidForPrefix(prefix) && len(keys) < retiredObjectIDDeleteBatch; it.Next() {
				keys = append(keys, it.Item().KeyCopy(nil))
			}

			return nil
		})
		if err != nil {
			return total, fmt.Errorf("scan retired ObjectID index: %w", err)
		}

		if len(keys) == 0 {
			break
		}

		if err = db.Update(func(txn *badgerdb.Txn) error {
			for _, k := range keys {
				if err := txn.Delete(k); err != nil {
					return err
				}
			}

			return nil
		}); err != nil {
			return total, fmt.Errorf("delete retired ObjectID index keys: %w", err)
		}

		total += len(keys)
		// The smallest key after the last one deleted.
		start = append(keys[len(keys)-1], 0x00)
	}

	if total > 0 {
		logger.Info("badger: dropped the retired ObjectID index", "keys", total)
	}

	return total, nil
}
