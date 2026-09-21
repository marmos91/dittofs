package badger

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
)

// prefixQuotaUsage keys the durable per-identity usage counters:
//
//	qu:<share>\x00<scope><id><stripe> -> (bytes, files), two signed 64-bit ints
//
// A share name is a normalized path and can never contain a NUL, so the NUL
// terminates it unambiguously and the fixed six-byte tail can be split off the
// end without a length prefix.
//
// Only per-identity buckets are stored. The per-share total is derived from the
// user-scope entries when the cache is seeded (every regular file has exactly
// one owner uid, so those buckets already partition the share), which keeps the
// hottest number in the system from having a single key of its own.
const prefixQuotaUsage = "qu:"

// keyQuotaCountersBackfilled marks that the qu: counters account for every file
// row. Its presence is what lets an open read the counters instead of deriving
// them from the file keyspace.
var keyQuotaCountersBackfilled = []byte(prefixConfig + "quota-counters-backfilled")

// quotaStripes is how many keys one logical bucket is split across.
//
// Badger has no atomic increment, so folding a delta is a read-modify-write and
// puts the key in the transaction's conflict set. One key per bucket would
// therefore make every writer in a share conflict with every other: files in a
// share commonly share a single owner uid, so that one bucket is the whole
// share's write path. Striping spreads them, and since the stripe is picked per
// attempt, a conflicted retry also moves off the key it collided on.
//
// ponytail: fixed width, so every bucket costs this many keys to read at open
// however little it is contended, and a store with very many distinct owners
// pays it on each one. Make it adaptive only once an open is measurably waiting
// on this scan rather than on the per-share work around it.
const quotaStripes = 16

// quotaStatWidth is the encoded size of one counter value: two signed 64-bit
// ints, bytes then files.
const quotaStatWidth = 16

// keyQuotaUsage builds the key for one stripe of one identity's bucket.
func keyQuotaUsage(share string, scope metadata.QuotaScope, id uint32, stripe uint8) []byte {
	b := make([]byte, 0, len(prefixQuotaUsage)+len(share)+7)
	b = append(b, prefixQuotaUsage...)
	b = append(b, share...)
	b = append(b, 0)
	b = append(b, byte(scope))
	b = binary.BigEndian.AppendUint32(b, id)
	return append(b, stripe)
}

// parseQuotaUsageKey recovers the bucket a stripe key belongs to. The stripe
// index itself is dropped: it exists only to spread writes, and every stripe of
// a bucket folds into the same total.
func parseQuotaUsageKey(key []byte) (basestore.QuotaKey, bool) {
	if !bytes.HasPrefix(key, []byte(prefixQuotaUsage)) {
		return basestore.QuotaKey{}, false
	}
	rest := key[len(prefixQuotaUsage):]
	sep := bytes.IndexByte(rest, 0)
	if sep < 0 || len(rest)-sep-1 != 6 {
		return basestore.QuotaKey{}, false
	}
	return basestore.QuotaKey{
		Share: string(rest[:sep]),
		Scope: metadata.QuotaScope(rest[sep+1]),
		ID:    binary.BigEndian.Uint32(rest[sep+2 : sep+6]),
	}, true
}

// encodeUsageStat renders a counter value. Both fields are written signed: a
// single stripe legitimately holds a negative partial (see decodeUsageStat).
func encodeUsageStat(u metadata.UsageStat) []byte {
	b := make([]byte, quotaStatWidth)
	binary.BigEndian.PutUint64(b[0:8], uint64(u.Bytes))
	binary.BigEndian.PutUint64(b[8:16], uint64(u.Files))
	return b
}

// decodeUsageStat reads a counter value.
//
// decision: a stripe is never clamped at zero, unlike the in-memory buckets it
// feeds. The stripe that a file's create landed on is not the stripe its delete
// lands on, so one stripe holding -1 files while its siblings hold the matching
// +1 is the normal steady state, not corruption. Clamping here would round each
// of those partials up to zero and leave the bucket permanently over-counted —
// the failure direction that denies a user writes they are entitled to. The
// clamp belongs on the summed total, and lives in foldQuotaCounters.
func decodeUsageStat(b []byte) (metadata.UsageStat, error) {
	if len(b) != quotaStatWidth {
		return metadata.UsageStat{}, fmt.Errorf("quota counter value is %d bytes, want %d", len(b), quotaStatWidth)
	}
	return metadata.UsageStat{
		Bytes: int64(binary.BigEndian.Uint64(b[0:8])),
		Files: int64(binary.BigEndian.Uint64(b[8:16])),
	}, nil
}

// persistQuotaDelta folds one transaction's usage delta into the durable
// counters from inside that same transaction, so the counters commit with the
// file rows that moved them or not at all. That is what removes the need to
// re-derive them at open: there is no window in which they can disagree, and
// therefore no crash that leaves them needing repair.
//
// Every key this touches goes in the transaction's conflict set, so the whole
// delta is written to a single stripe rather than one stripe per bucket — a
// transaction that charges a uid and a gid collides on two keys either way, and
// spreading them over two stripes would only widen the set.
func (s *BadgerMetadataStore) persistQuotaDelta(txn *badgerdb.Txn, delta map[basestore.QuotaKey]metadata.UsageStat) error {
	if len(delta) == 0 {
		return nil
	}
	stripe := uint8(s.quotaStripe.Add(1) % quotaStripes)
	for k, d := range delta {
		if d.Bytes == 0 && d.Files == 0 {
			continue
		}
		key := keyQuotaUsage(k.Share, k.Scope, k.ID, stripe)

		cur := metadata.UsageStat{}
		item, err := txn.Get(key)
		switch {
		case err == nil:
			if verr := item.Value(func(val []byte) error {
				decoded, derr := decodeUsageStat(val)
				if derr != nil {
					return derr
				}
				cur = decoded
				return nil
			}); verr != nil {
				return verr
			}
		case errors.Is(err, badgerdb.ErrKeyNotFound):
			// First write to this stripe; cur stays zero.
		default:
			return fmt.Errorf("read quota counter: %w", err)
		}

		cur.Bytes += d.Bytes
		cur.Files += d.Files
		if err := txn.Set(key, encodeUsageStat(cur)); err != nil {
			return fmt.Errorf("write quota counter: %w", err)
		}
	}
	return nil
}

// readQuotaCounters sums every stripe back into per-identity buckets. This is
// the open-time path that replaces the file-row scan: it reads one key per
// bucket per stripe, so it is bounded by how many distinct owners the store has
// rather than by how many files they own.
func (s *BadgerMetadataStore) readQuotaCounters() (map[basestore.QuotaKey]*metadata.UsageStat, error) {
	byIdentity := make(map[basestore.QuotaKey]*metadata.UsageStat)
	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = []byte(prefixQuotaUsage)
		opts.PrefetchValues = true
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			k, ok := parseQuotaUsageKey(item.Key())
			if !ok {
				continue
			}
			if err := item.Value(func(val []byte) error {
				u, derr := decodeUsageStat(val)
				if derr != nil {
					return derr
				}
				cur := byIdentity[k]
				if cur == nil {
					cur = &metadata.UsageStat{}
					byIdentity[k] = cur
				}
				cur.Bytes += u.Bytes
				cur.Files += u.Files
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	foldQuotaCounters(byIdentity)
	return byIdentity, nil
}

// seedUsage replaces the usage cache's buckets with the given totals.
func (s *BadgerMetadataStore) seedUsage(byIdentity map[basestore.QuotaKey]*metadata.UsageStat) {
	s.quotaMu.Lock()
	s.quota.Seed(byIdentity, nil)
	s.quotaMu.Unlock()
}

// foldQuotaCounters applies the cache's own invariant to summed bucket totals:
// clamp at zero, and drop what nets to nothing. It runs only once every stripe
// of a bucket has been added in — see decodeUsageStat for why a partial must
// reach here unclamped.
func foldQuotaCounters(byIdentity map[basestore.QuotaKey]*metadata.UsageStat) {
	for k, u := range byIdentity {
		if u.Bytes < 0 {
			u.Bytes = 0
		}
		if u.Files < 0 {
			u.Files = 0
		}
		if u.Bytes == 0 && u.Files == 0 {
			delete(byIdentity, k)
		}
	}
}

// writeQuotaCounters replaces every durable counter with the given buckets,
// collapsing each one back onto a single stripe. Used by the backfill and by an
// operator-invoked realign — both of which have just derived the authoritative
// totals from the file rows.
//
// The caller holds quotaRealign, so no transaction can be folding a delta into
// the keys being dropped.
func (s *BadgerMetadataStore) writeQuotaCounters(byIdentity map[basestore.QuotaKey]*metadata.UsageStat) error {
	if err := s.db.DropPrefix([]byte(prefixQuotaUsage)); err != nil {
		return fmt.Errorf("drop stale quota counters: %w", err)
	}
	batch := s.db.NewWriteBatch()
	defer batch.Cancel()
	for k, u := range byIdentity {
		if u.Bytes == 0 && u.Files == 0 {
			continue
		}
		if err := batch.Set(keyQuotaUsage(k.Share, k.Scope, k.ID, 0), encodeUsageStat(*u)); err != nil {
			return fmt.Errorf("write quota counter: %w", err)
		}
	}
	if err := batch.Flush(); err != nil {
		return fmt.Errorf("flush quota counters: %w", err)
	}
	return nil
}

// quotaCountersBackfilled reports whether the durable counters already account
// for every file row.
func quotaCountersBackfilled(db *badgerdb.DB) (bool, error) {
	var done bool
	err := db.View(func(txn *badgerdb.Txn) error {
		_, err := txn.Get(keyQuotaCountersBackfilled)
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
	return done, err
}

// recordQuotaCountersBackfilled marks the backfill complete. Written only after
// the counters themselves are durable, so an interrupted run repeats the scan
// rather than recording work it did not finish.
func recordQuotaCountersBackfilled(db *badgerdb.DB) error {
	return db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set(keyQuotaCountersBackfilled, []byte{1})
	})
}
