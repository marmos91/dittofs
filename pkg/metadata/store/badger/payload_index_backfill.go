package badger

import (
	"context"
	"fmt"
	"time"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/marmos91/dittofs/internal/logger"
)

// keyPayloadIndexBackfilled marks that every file row carrying a PayloadID has a
// pl: index entry. Its presence lets a subsequent open skip the scan below.
var keyPayloadIndexBackfilled = []byte(prefixConfig + "payload-index-backfilled")

// backfillPayloadIndexProgressInterval bounds how often the scan reports. The
// scan is linear and usually quick, but a large store predates any log line
// between open and the first listener, which is the window this fills.
const backfillPayloadIndexProgressInterval = 5 * time.Second

// backfillPayloadIndex writes a pl:<payloadID> entry for every file row missing
// one, then records that it has done so.
//
// The index turns GetFileByPayloadID into a point lookup. Rows written before it
// existed have no entry, and the lookup falls back to scanning and decoding the
// whole file keyspace. That fallback is per-call, so a caller that resolves N
// payloads in a loop — the journal size reconcile on the startup path does
// exactly this — costs N full scans, and a store of legacy rows turns an O(N)
// step into an O(N²) one before any listener binds.
//
// Writing the entries on the next write of each row, which is what the index
// originally relied on, never happens for files that are only ever read. So the
// entries are written once, here, rather than waited for.
//
// The scan is linear and runs once per store: the marker makes later opens skip
// it. Entries go through a WriteBatch because a single transaction covering
// every row would exceed Badger's transaction size limit on a large store.
func backfillPayloadIndex(ctx context.Context, db *badgerdb.DB) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	done, err := payloadIndexBackfillDone(db)
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	started := time.Now()
	lastLog := started
	var scanned, written int

	batch := db.NewWriteBatch()
	defer batch.Cancel()

	err = db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.PrefetchSize = 100
		it := txn.NewIterator(opts)
		defer it.Close()

		prefix := []byte(prefixFile)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			scanned++
			if time.Since(lastLog) >= backfillPayloadIndexProgressInterval {
				lastLog = time.Now()
				logger.Info("indexing files by payload", "scanned", scanned, "written", written,
					"elapsed", time.Since(started).Round(time.Second))
			}

			verr := it.Item().Value(func(val []byte) error {
				file, decErr := decodeFile(val)
				if decErr != nil {
					// Consistent with the read paths, which skip a row they cannot
					// decode rather than failing the whole store open. An
					// unreadable row has no payload to index either way.
					return nil
				}
				if file == nil || file.PayloadID == "" {
					return nil
				}
				key := keyPayloadID(file.PayloadID)
				if _, gErr := txn.Get(key); gErr == nil {
					return nil // already indexed
				} else if gErr != badgerdb.ErrKeyNotFound {
					return gErr
				}
				id, mErr := file.ID.MarshalBinary()
				if mErr != nil {
					return mErr
				}
				if sErr := batch.Set(key, id); sErr != nil {
					return sErr
				}
				written++
				return nil
			})
			if verr != nil {
				return verr
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("scan files to index by payload: %w", err)
	}

	if err := batch.Flush(); err != nil {
		return fmt.Errorf("write payload index entries: %w", err)
	}

	// Only after the entries are durable, so an interrupted run repeats the scan
	// rather than recording work it did not finish.
	if err := db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set(keyPayloadIndexBackfilled, []byte{1})
	}); err != nil {
		return fmt.Errorf("record payload index backfill: %w", err)
	}

	if written > 0 {
		logger.Info("indexed files by payload", "scanned", scanned, "written", written,
			"duration", time.Since(started).Round(time.Millisecond))
	}
	return nil
}

func payloadIndexBackfillDone(db *badgerdb.DB) (bool, error) {
	var done bool
	err := db.View(func(txn *badgerdb.Txn) error {
		switch _, err := txn.Get(keyPayloadIndexBackfilled); {
		case err == nil:
			done = true
			return nil
		case err == badgerdb.ErrKeyNotFound:
			return nil
		default:
			return err
		}
	})
	return done, err
}
