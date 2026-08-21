package badger

import (
	"context"
	"fmt"
	"time"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// shareUsedCacheTTL bounds how long a per-share usage answer is reused. Every
// committed transaction that moves the byte counter already drops the cache,
// so this is a backstop against a bucket surviving a change that bypassed that
// path, not the primary freshness mechanism. It matches the
// filesystem-statistics cache TTL.
const shareUsedCacheTTL = 5 * time.Second

// GetUsedBytesForShare returns the logical bytes held by one share's regular
// files.
//
// The store-wide usedBytes counter cannot answer this: a single store instance
// backs every share that names the same metadata store config, so the counter
// is the sum across all of them. The file records are keyed by UUID with no
// share index, so the per-share split is derived by scanning them.
//
// ponytail: O(n) scan over the file records, shared by all shares (one scan
// fills every bucket) and cached until the next committed size change. The
// store-wide counter is maintained by a transaction delta pipeline that
// carries no share dimension, so a per-share counter means threading the share
// name through every mutation site. Do that only if this scan shows up in a
// profile — it is reached from reporting surfaces, not from the I/O path.
func (s *BadgerMetadataStore) GetUsedBytesForShare(ctx context.Context, shareName string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.shareUsedCache.mu.Lock()
	cached := s.shareUsedCache.byShare
	fresh := time.Since(s.shareUsedCache.timestamp) < shareUsedCacheTTL
	gen := s.shareUsedCache.gen
	s.shareUsedCache.mu.Unlock()

	// A published map is never mutated in place, only replaced, so reading it
	// outside the lock is safe.
	if cached != nil && fresh {
		return cached[shareName], nil
	}

	// Scanned with the lock released: a committing transaction invalidates this
	// cache post-commit, and must never queue behind a scan of every record.
	// Concurrent misses may each scan; that is cheaper than stalling writes.
	byShare, err := s.scanUsedBytesByShare(ctx)
	if err != nil {
		return 0, err
	}

	s.shareUsedCache.mu.Lock()
	// Publish only if nothing invalidated the cache while the scan ran — those
	// results may already describe a superseded state.
	if s.shareUsedCache.gen == gen {
		s.shareUsedCache.byShare = byShare
		s.shareUsedCache.timestamp = time.Now()
	}
	s.shareUsedCache.mu.Unlock()

	return byShare[shareName], nil
}

// scanUsedBytesByShare walks every file record and totals the sizes of regular
// files per share. Corrupted records are skipped rather than failing the scan,
// matching initUsedBytesCounter: a single undecodable blob must not make usage
// unreportable for the whole store.
func (s *BadgerMetadataStore) scanUsedBytesByShare(ctx context.Context) (map[string]int64, error) {
	byShare := make(map[string]int64)

	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = []byte(prefixFile)
		opts.PrefetchValues = true

		it := txn.NewIterator(opts)
		defer it.Close()

		scanned := 0
		for it.Rewind(); it.Valid(); it.Next() {
			if scanned%100 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			scanned++

			if err := it.Item().Value(func(val []byte) error {
				file, err := decodeFile(val)
				if err != nil {
					return nil
				}
				if file.Type == metadata.FileTypeRegular {
					byShare[file.ShareName] += int64(file.Size)
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to total per-share used bytes: %w", err)
	}

	return byShare, nil
}

// invalidateShareUsedCache drops the cached per-share totals so the next caller
// rescans, and bumps the generation so a scan already in flight discards its
// result instead of publishing a superseded one. Called wherever the file
// records change: post-commit in withTransaction, on DeleteShare, on Reset, and
// after a snapshot restore.
func (s *BadgerMetadataStore) invalidateShareUsedCache() {
	s.shareUsedCache.mu.Lock()
	s.shareUsedCache.byShare = nil
	s.shareUsedCache.gen++
	s.shareUsedCache.mu.Unlock()
}
