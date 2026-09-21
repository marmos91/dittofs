package badger

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/backup"
)

const (
	// badgerEngineTag identifies the Badger engine in backup envelopes.
	badgerEngineTag = "badger"

	// badgerSchemaVersion is the wire format version for Badger KV streams.
	// Version 1: length-prefixed KV pairs with uint32 LE framing.
	badgerSchemaVersion = uint32(1)

	// restoreBatchSize is the number of KV entries per WriteBatch flush
	// during restore. Keeps memory bounded for large databases.
	restoreBatchSize = 10000

	// maxRestoreAllocSize is the maximum single allocation size (256 MiB)
	// permitted when reading key/value lengths from an untrusted backup
	// stream. Prevents OOM from crafted streams with bogus size fields.
	maxRestoreAllocSize = 256 << 20
)

// Compile-time assertions: BadgerMetadataStore implements Snapshotable and can
// also produce a labelled degraded snapshot.
var (
	_ metadata.Snapshotable          = (*BadgerMetadataStore)(nil)
	_ metadata.DegradableSnapshotter = (*BadgerMetadataStore)(nil)
)

// maxDegradedKeysRecorded bounds the sample of skipped keys carried on a
// degraded snapshot. The count is always exact; only the name list is capped.
//
// ponytail: a flat cap rather than a size budget, because the keys are
// fixed-shape (f: + a uuid) and a store with more than this many undecodable
// inodes has a problem no sample size helps with; widen it only if an operator
// ever needs the full list to act.
const maxDegradedKeysRecorded = 32

// WriteSnapshot serializes all metadata into w using a custom length-prefixed KV
// stream inside a single db.View() MVCC snapshot. It returns the set of
// content-addressed block hashes referenced by file entries (f: prefix).
//
// It aborts on any entry it cannot read. WriteSnapshotDegraded is the variant
// that completes and labels the result instead.
//
// Wire format per KV pair:
//   - key_len   uint32 LE
//   - key       [key_len]byte
//   - value_len uint32 LE
//   - value     [value_len]byte
//
// Stream terminated by sentinel key_len = 0 (4 zero bytes).
func (s *BadgerMetadataStore) WriteSnapshot(ctx context.Context, w io.Writer) (*block.HashSet, error) {
	hs, degraded, err := s.writeSnapshot(ctx, w, false)
	if err != nil {
		return nil, err
	}
	// Unreachable while allowDegraded is false; a nil-check here would read as
	// if the refusal were optional.
	_ = degraded
	return hs, nil
}

// WriteSnapshotDegraded implements metadata.DegradableSnapshotter: it completes
// the snapshot over entries it cannot decode and reports them, so the caller
// can label the result rather than ship a silently short one.
func (s *BadgerMetadataStore) WriteSnapshotDegraded(
	ctx context.Context,
	w io.Writer,
) (*block.HashSet, *metadata.SnapshotDegradation, error) {
	return s.writeSnapshot(ctx, w, true)
}

// writeSnapshot is the shared body. allowDegraded decides what an undecodable
// f: entry does: abort (false) or be recorded and skipped (true). Nothing else
// differs, so the two entrypoints cannot drift on the rest of the format.
func (s *BadgerMetadataStore) writeSnapshot(
	ctx context.Context,
	w io.Writer,
	allowDegraded bool,
) (*block.HashSet, *metadata.SnapshotDegradation, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", metadata.ErrSnapshotAborted, err)
	}

	hs := block.NewHashSet(0)
	var degraded metadata.SnapshotDegradation

	// Declare envW outside the callback so Finish() can be called after
	// the View returns.
	var envW *backup.Writer

	// MVCC snapshot: all reads inside this View see a consistent state.
	// The envelope writer and schema version are created INSIDE the
	// callback so the first Write to w (which triggers the signalWriter
	// in the ConcurrentWriter test) happens after the MVCC snapshot is
	// established.
	err := s.db.View(func(txn *badgerdb.Txn) error {
		var writeErr error
		envW, writeErr = backup.NewWriter(w, badgerEngineTag)
		if writeErr != nil {
			return fmt.Errorf("%w: envelope: %v", metadata.ErrSnapshotAborted, writeErr)
		}

		// Write schema version (uint32 LE).
		var verBuf [4]byte
		binary.LittleEndian.PutUint32(verBuf[:], badgerSchemaVersion)
		if _, writeErr = envW.Write(verBuf[:]); writeErr != nil {
			return fmt.Errorf("%w: schema version: %v", metadata.ErrSnapshotAborted, writeErr)
		}

		opts := badgerdb.DefaultIteratorOptions
		opts.PrefetchValues = true
		opts.PrefetchSize = 100
		it := txn.NewIterator(opts)
		defer it.Close()

		var buf [4]byte
		filePrefix := []byte(prefixFile)

		for it.Rewind(); it.Valid(); it.Next() {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("%w: %v", metadata.ErrSnapshotAborted, err)
			}

			item := it.Item()
			key := item.KeyCopy(nil)
			val, err := item.ValueCopy(nil)
			if err != nil {
				return fmt.Errorf("%w: value copy: %v", metadata.ErrSnapshotAborted, err)
			}

			// Write key_len + key.
			binary.LittleEndian.PutUint32(buf[:], uint32(len(key)))
			if _, err := envW.Write(buf[:]); err != nil {
				return fmt.Errorf("%w: write key_len: %v", metadata.ErrSnapshotAborted, err)
			}
			if _, err := envW.Write(key); err != nil {
				return fmt.Errorf("%w: write key: %v", metadata.ErrSnapshotAborted, err)
			}

			// Write value_len + value.
			binary.LittleEndian.PutUint32(buf[:], uint32(len(val)))
			if _, err := envW.Write(buf[:]); err != nil {
				return fmt.Errorf("%w: write value_len: %v", metadata.ErrSnapshotAborted, err)
			}
			if _, err := envW.Write(val); err != nil {
				return fmt.Errorf("%w: write value: %v", metadata.ErrSnapshotAborted, err)
			}

			// Hash extraction: decode f: prefix entries for block hashes.
			// decodeFile handles both the binary codec (new writes) and the
			// legacy JSON records (dual-read, #1735).
			if bytes.HasPrefix(key, filePrefix) {
				// The raw record was already dumped above, so a row skipped here
				// still lands in the restored store while its hashes never reach
				// the HashSet the durability verify checks. The restore would
				// then reference chunks the snapshot never claimed.
				//
				// decision: the rule enforced here is not "never snapshot a
				// damaged store", it is "never let a snapshot misrepresent its
				// own completeness". So the default aborts — same as
				// loadManifest just below, for the same reason: a hash claim
				// that is short cannot be told apart from one that is wrong —
				// while WriteSnapshotDegraded records the skipped keys and lets
				// the caller label the result. Enforcing the first rule instead
				// trapped the operator: one bad row made the share
				// un-snapshottable AND un-restorable, because restore takes a
				// safety snapshot first and leaves the share disabled when that
				// fails. Overturn this only if a degraded snapshot can be
				// mistaken for a complete one somewhere downstream — that, not
				// the abort, is what the design rests on.
				file, err := decodeFile(val)
				if err != nil {
					if !allowDegraded {
						return fmt.Errorf("%w: decode f: entry %s: %v",
							metadata.ErrSnapshotAborted, string(key), err)
					}
					degraded.Entries++
					if len(degraded.Keys) < maxDegradedKeysRecorded {
						degraded.Keys = append(degraded.Keys, string(key))
					} else {
						degraded.KeysTruncated = true
					}
					continue
				}
				// The manifest lives in fm:<uuid> (legacy blobs embed it). It
				// feeds the durability HashSet, so a missed load would ship an
				// incomplete snapshot — abort rather than silently under-count,
				// exactly as the decode above does.
				if err := loadManifest(txn, file); err != nil {
					return fmt.Errorf("%w: load manifest for %s: %v",
						metadata.ErrSnapshotAborted, string(key), err)
				}
				// Skip unlinked (nlink=0) files: they are dead and GC may have
				// already reclaimed their blocks, so adding them would make the
				// snapshot manifest reference hashes absent from remote and fail
				// the durability verify (#1433). Use the authoritative link count
				// (l: key), not the embedded File.Nlink which SetLinkCount does
				// not rewrite. The raw f: record is still dumped verbatim above so
				// a restore stays consistent (the file remains nlink=0, invisible).
				if fileLinkCountTxn(txn, file) == 0 {
					continue
				}
				for _, br := range file.Blocks {
					hs.Add(br.Hash)
				}
			}
		}

		// Write sentinel: key_len = 0.
		binary.LittleEndian.PutUint32(buf[:], 0)
		if _, err := envW.Write(buf[:]); err != nil {
			return fmt.Errorf("%w: write sentinel: %v", metadata.ErrSnapshotAborted, err)
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	// Write trailing CRC.
	if err := envW.Finish(); err != nil {
		return nil, nil, fmt.Errorf("%w: finish envelope: %v", metadata.ErrSnapshotAborted, err)
	}

	if degraded.Entries == 0 {
		return hs, nil, nil
	}
	return hs, &degraded, nil
}

// RestoreSnapshot reads a backup stream from r and rebuilds metadata state in the
// Badger store. The store must be empty (no existing share data); otherwise
// ErrRestoreDestinationNotEmpty is returned.
//
// Integrity guarantee: KV entries stream straight into Badger via WriteBatch
// (restore RAM bounded by restoreBatchSize, not by share size), the trailing
// CRC is verified last, and any failure triggers a DropAll. Because the
// destination was empty before restore began, a corrupt stream is wiped back
// to empty and the restore stays retryable.
func (s *BadgerMetadataStore) RestoreSnapshot(ctx context.Context, r io.Reader) error {
	// Check destination is empty by looking for any s: prefix key.
	isEmpty, err := s.isStoreEmpty()
	if err != nil {
		return fmt.Errorf("%w: empty check: %v", metadata.ErrRestoreCorrupt, err)
	}
	if !isEmpty {
		return metadata.ErrRestoreDestinationNotEmpty
	}

	// The restored records replace whatever the derived caches were built from,
	// and the failure path drops everything again — either way an entry that
	// outlives this call answers for state that is gone. Nothing else clears
	// them: the restore streams through a WriteBatch rather than a
	// badgerTransaction, so the per-key invalidation an ordinary mutation
	// performs never runs, and a restore reuses the same share name and file
	// UUIDs, so warm keys keep matching.
	defer s.invalidateDerivedCaches()

	// Read envelope header.
	engineTag, payloadReader, acc, err := backup.ReadHeader(r)
	if err != nil {
		return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
	}

	// Verify engine tag.
	if err := backup.VerifyEngine(engineTag, badgerEngineTag); err != nil {
		return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
	}

	// Read schema version.
	var verBuf [4]byte
	if _, err := io.ReadFull(payloadReader, verBuf[:]); err != nil {
		return fmt.Errorf("%w: read schema version: %v", metadata.ErrRestoreCorrupt, err)
	}
	schemaVer := binary.LittleEndian.Uint32(verBuf[:])
	if schemaVer != badgerSchemaVersion {
		return fmt.Errorf("%w: got version %d, want %d", metadata.ErrSchemaVersionMismatch, schemaVer, badgerSchemaVersion)
	}

	// Stream KV entries straight into Badger via WriteBatch so restore RAM is
	// bounded by restoreBatchSize, not by the (potentially multi-GB) share
	// size. The create-path Backup already streams KV-by-KV; this closes the
	// restore-side ceiling (#831).
	//
	// Atomicity is preserved by the empty-destination precondition checked
	// above plus a DropAll on any failure: the store was empty before we
	// started, so wiping the partially-applied data restores the empty,
	// retryable state the previous buffer-then-flush design guaranteed. A
	// crash mid-restore is handled one layer up by the durable restore marker
	// (startup rollback).
	wb := s.db.NewWriteBatch()
	failRestore := func(format string, args ...any) error {
		wb.Cancel()
		if derr := s.db.DropAll(); derr != nil {
			logger.Error("restore: DropAll after failure left store non-empty", "error", derr)
		}
		return fmt.Errorf(format, args...)
	}

	var buf [4]byte
	applied := 0
	for {
		if err := ctx.Err(); err != nil {
			return failRestore("restore cancelled: %w", err)
		}

		// Read key_len.
		if _, err := io.ReadFull(payloadReader, buf[:]); err != nil {
			return failRestore("%w: read key_len: %v", metadata.ErrRestoreCorrupt, err)
		}
		keyLen := binary.LittleEndian.Uint32(buf[:])

		// Sentinel: key_len = 0 means end of stream.
		if keyLen == 0 {
			break
		}

		// Reject oversized key allocations from untrusted streams.
		if keyLen > maxRestoreAllocSize {
			return failRestore("%w: key size %d exceeds maximum %d", metadata.ErrRestoreCorrupt, keyLen, maxRestoreAllocSize)
		}

		// Read key.
		key := make([]byte, keyLen)
		if _, err := io.ReadFull(payloadReader, key); err != nil {
			return failRestore("%w: read key: %v", metadata.ErrRestoreCorrupt, err)
		}

		// Read value_len.
		if _, err := io.ReadFull(payloadReader, buf[:]); err != nil {
			return failRestore("%w: read value_len: %v", metadata.ErrRestoreCorrupt, err)
		}
		valLen := binary.LittleEndian.Uint32(buf[:])

		// Reject oversized value allocations from untrusted streams.
		if valLen > maxRestoreAllocSize {
			return failRestore("%w: value size %d exceeds maximum %d", metadata.ErrRestoreCorrupt, valLen, maxRestoreAllocSize)
		}

		// Read value.
		val := make([]byte, valLen)
		if _, err := io.ReadFull(payloadReader, val); err != nil {
			return failRestore("%w: read value: %v", metadata.ErrRestoreCorrupt, err)
		}

		if err := wb.SetEntry(badgerdb.NewEntry(key, val)); err != nil {
			return failRestore("%w: set entry: %v", metadata.ErrRestoreCorrupt, err)
		}
		applied++
		if applied%restoreBatchSize == 0 {
			if err := wb.Flush(); err != nil {
				return failRestore("%w: flush batch: %v", metadata.ErrRestoreCorrupt, err)
			}
			wb = s.db.NewWriteBatch()
		}
	}

	// Flush the final partial batch so every payload byte has passed through
	// the tee reader before the CRC check.
	if err := wb.Flush(); err != nil {
		return failRestore("%w: flush final batch: %v", metadata.ErrRestoreCorrupt, err)
	}

	// Verify CRC. The tee reader accumulated all payload bytes; r still has the
	// trailing 4 CRC bytes unread. On mismatch, wipe the applied data so the
	// destination is left empty and the restore is retryable.
	if err := backup.VerifyCRC(r, acc); err != nil {
		if derr := s.db.DropAll(); derr != nil {
			logger.Error("restore: DropAll after CRC failure left store non-empty", "error", derr)
		}
		return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
	}

	// The DB has been fully repopulated, but the in-memory usage cache still
	// holds the pre-restore buckets. Reseed it from the restored rows so
	// GetUsedBytesForShare / GetQuotaUsage / GetFilesystemStatistics report
	// correctly without a restart.
	// Both backfill markers are withdrawn first. They describe the keyspace this
	// store held BEFORE the restore, and a dump predating either one carries
	// neither the counters nor the pl: index to replace them — the load only
	// sets the keys the dump contains, it deletes nothing. Left standing on a
	// freshly created destination, whose own first open recorded both, they
	// would certify empty counters as accounting for the restored rows: every
	// identity reads as zero usage, and no later open re-derives it because that
	// is precisely what the markers promise.
	if err := clearQuotaCountersBackfilled(s.db); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if err := clearPayloadIndexBackfilled(s.db); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if err := s.initUsedBytesAndPayloadIndex(); err != nil {
		return fmt.Errorf("restore: reseed the usage cache: %w", err)
	}

	return nil
}

// isStoreEmpty checks if the store contains any share data by seeking
// the s: prefix. Returns true if no share keys exist.
func (s *BadgerMetadataStore) isStoreEmpty() (bool, error) {
	empty := true
	err := s.db.View(func(txn *badgerdb.Txn) error {
		prefix := []byte(prefixShare)
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		it.Seek(prefix)
		if it.ValidForPrefix(prefix) {
			empty = false
		}
		return nil
	})
	return empty, err
}
