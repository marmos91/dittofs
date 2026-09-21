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

// clearPayloadIndexBackfilled withdraws the claim that every file row carrying
// a PayloadID has a pl: index entry, so the next open rebuilds it.
func clearPayloadIndexBackfilled(db *badgerdb.DB) error {
	if err := db.Update(func(txn *badgerdb.Txn) error {
		return txn.Delete(keyPayloadIndexBackfilled)
	}); err != nil {
		return fmt.Errorf("clear payload index backfill marker: %w", err)
	}
	return nil
}

// recordPayloadIndexBackfilled marks the backfill complete. Called only after
// the staged entries are durable, so an interrupted run repeats the work rather
// than recording what it did not finish.
func recordPayloadIndexBackfilled(db *badgerdb.DB) error {
	return db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set(keyPayloadIndexBackfilled, []byte{1})
	})
}

// initUsedBytesAndPayloadIndex seeds the usage cache, and writes the pl: index
// when this store still lacks it.
//
// The usage cache is seeded from the durable counters when they already account
// for every file row, which is the ordinary case and reads one key per bucket
// per stripe. Only a store that has neither been backfilled nor indexed reads
// the file keyspace, and each of those is a once-per-store debt: the markers
// they record are what keep a later open off this path.
//
// Shared by store open and snapshot restore because both land on a keyspace
// whose rows may predate either marker.
//
// The markers are read from the store, so they answer for whatever keyspace is
// there now. That makes them trustworthy at open and NOT at restore, where the
// keyspace has just been replaced under them: the restore withdraws both before
// calling this, because a marker recorded by the destination's own first open
// would otherwise certify the dump's rows on the strength of a scan that never
// saw them.
func (s *BadgerMetadataStore) initUsedBytesAndPayloadIndex() error {
	needIndex, err := payloadIndexBackfillNeeded(s.db)
	if err != nil {
		return fmt.Errorf("check payload index state: %w", err)
	}
	haveCounters, err := quotaCountersBackfilled(s.db)
	if err != nil {
		return fmt.Errorf("check quota counter state: %w", err)
	}

	if haveCounters && !needIndex {
		byIdentity, err := s.readQuotaCounters()
		if err != nil {
			return fmt.Errorf("read quota counters: %w", err)
		}
		s.seedUsage(byIdentity)
		return nil
	}

	var batch *badgerdb.WriteBatch
	if needIndex {
		batch = s.db.NewWriteBatch()
		defer batch.Cancel()
	}

	// Held across the scan as well as the write, not just the write. What this
	// path persists is the scan's own result, so a delta committing after the
	// scan's snapshot would be deleted from the durable counters by the rewrite
	// AND overwritten in the cache by the seed — lost on both sides, with no
	// later open re-deriving it. Unlike a realign there is no capture to fold it
	// back, because the buckets being seeded are the scan's, not the cache's.
	//
	// At open this is uncontended. Snapshot restore reaches it on a store that
	// is already open, which is the case that needs the cover.
	s.quotaRealign.Lock()
	defer s.quotaRealign.Unlock()

	byIdentity, err := s.scanUsage(batch)
	if err != nil {
		return err
	}
	s.seedUsage(byIdentity)

	if needIndex {
		if err := batch.Flush(); err != nil {
			return fmt.Errorf("write payload index entries: %w", err)
		}
		if err := recordPayloadIndexBackfilled(s.db); err != nil {
			return fmt.Errorf("record payload index backfill: %w", err)
		}
	}

	if !haveCounters {
		if err := s.writeQuotaCounters(byIdentity); err != nil {
			return err
		}
		if err := recordQuotaCountersBackfilled(s.db); err != nil {
			return fmt.Errorf("record quota counter backfill: %w", err)
		}
	}
	return nil
}
