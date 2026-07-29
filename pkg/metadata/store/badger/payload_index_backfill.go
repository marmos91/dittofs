package badger

import (
	"errors"
	"fmt"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// keyPayloadIndexBackfilled marks that every file row carrying a PayloadID has a
// pl: index entry. Its presence lets a subsequent open skip the work.
var keyPayloadIndexBackfilled = []byte(prefixConfig + "payload-index-backfilled")

// payloadIndexBackfillNeeded reports whether this store still has to be indexed
// by payload.
//
// GetFileByPayloadID is a point lookup only when the pl:<payloadID> index has an
// entry. Rows written before that index existed have none, and each miss falls
// back to scanning and decoding the whole file keyspace. That fallback is per
// call, so a caller resolving one payload per file in a loop — the journal size
// reconcile on the startup path does exactly this — turns an O(N) step into an
// O(N²) one before any listener binds.
//
// The entries were expected to appear as rows were rewritten, which never
// happens for files that are only ever read, so they are written once instead.
func payloadIndexBackfillNeeded(db *badgerdb.DB) (bool, error) {
	var done bool
	err := db.View(func(txn *badgerdb.Txn) error {
		_, err := txn.Get(keyPayloadIndexBackfilled)
		switch {
		case err == nil:
			done = true
			return nil
		case errors.Is(err, badgerdb.ErrKeyNotFound):
			return nil
		default:
			return err
		}
	})
	return !done, err
}

// indexFileByPayload stages this file's pl: entry on batch. It is called from
// the open-time file scan, so it must not do a read of its own: the entry is
// written unconditionally rather than after checking for one, since rewriting an
// entry with the value it already holds costs less than the lookup that would
// avoid it.
//
// A batch is used because a single transaction spanning every row would exceed
// Badger's transaction size limit on a large store.
func indexFileByPayload(batch *badgerdb.WriteBatch, file *metadata.File) error {
	if batch == nil || file == nil || file.PayloadID == "" {
		return nil
	}
	id, err := file.ID.MarshalBinary()
	if err != nil {
		return fmt.Errorf("marshal file id for payload index: %w", err)
	}
	return batch.Set(keyPayloadID(file.PayloadID), id)
}

// recordPayloadIndexBackfilled marks the backfill complete. Called only after
// the staged entries are durable, so an interrupted run repeats the work rather
// than recording what it did not finish.
func recordPayloadIndexBackfilled(db *badgerdb.DB) error {
	return db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set(keyPayloadIndexBackfilled, []byte{1})
	})
}

// initUsedBytesAndPayloadIndex runs the open-time file scan, extending it to
// write the pl: index when this store still lacks it.
//
// Shared by store open and snapshot restore because both land on a keyspace
// whose rows may predate the index — a restore from an old dump would otherwise
// leave every payload lookup scanning until the next restart.
func (s *BadgerMetadataStore) initUsedBytesAndPayloadIndex() error {
	need, err := payloadIndexBackfillNeeded(s.db)
	if err != nil {
		return fmt.Errorf("check payload index state: %w", err)
	}

	var batch *badgerdb.WriteBatch
	if need {
		batch = s.db.NewWriteBatch()
		defer batch.Cancel()
	}

	if err := s.initUsedBytesCounter(batch); err != nil {
		return err
	}
	if !need {
		return nil
	}

	if err := batch.Flush(); err != nil {
		return fmt.Errorf("write payload index entries: %w", err)
	}
	if err := recordPayloadIndexBackfilled(s.db); err != nil {
		return fmt.Errorf("record payload index backfill: %w", err)
	}
	return nil
}
