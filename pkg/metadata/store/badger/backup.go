package badger

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
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

// Compile-time assertion: BadgerMetadataStore implements Backupable.
var _ metadata.Backupable = (*BadgerMetadataStore)(nil)

// Backup serializes all metadata into w using a custom length-prefixed KV
// stream inside a single db.View() MVCC snapshot. It returns the set of
// content-addressed block hashes referenced by file entries (f: prefix).
//
// Wire format per KV pair:
//   - key_len   uint32 LE
//   - key       [key_len]byte
//   - value_len uint32 LE
//   - value     [value_len]byte
//
// Stream terminated by sentinel key_len = 0 (4 zero bytes).
func (s *BadgerMetadataStore) Backup(ctx context.Context, w io.Writer) (*block.HashSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", metadata.ErrBackupAborted, err)
	}

	hs := block.NewHashSet(0)

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
			return fmt.Errorf("%w: envelope: %v", metadata.ErrBackupAborted, writeErr)
		}

		// Write schema version (uint32 LE).
		var verBuf [4]byte
		binary.LittleEndian.PutUint32(verBuf[:], badgerSchemaVersion)
		if _, writeErr = envW.Write(verBuf[:]); writeErr != nil {
			return fmt.Errorf("%w: schema version: %v", metadata.ErrBackupAborted, writeErr)
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
				return fmt.Errorf("%w: %v", metadata.ErrBackupAborted, err)
			}

			item := it.Item()
			key := item.KeyCopy(nil)
			val, err := item.ValueCopy(nil)
			if err != nil {
				return fmt.Errorf("%w: value copy: %v", metadata.ErrBackupAborted, err)
			}

			// Write key_len + key.
			binary.LittleEndian.PutUint32(buf[:], uint32(len(key)))
			if _, err := envW.Write(buf[:]); err != nil {
				return fmt.Errorf("%w: write key_len: %v", metadata.ErrBackupAborted, err)
			}
			if _, err := envW.Write(key); err != nil {
				return fmt.Errorf("%w: write key: %v", metadata.ErrBackupAborted, err)
			}

			// Write value_len + value.
			binary.LittleEndian.PutUint32(buf[:], uint32(len(val)))
			if _, err := envW.Write(buf[:]); err != nil {
				return fmt.Errorf("%w: write value_len: %v", metadata.ErrBackupAborted, err)
			}
			if _, err := envW.Write(val); err != nil {
				return fmt.Errorf("%w: write value: %v", metadata.ErrBackupAborted, err)
			}

			// Hash extraction: decode f: prefix entries for block hashes.
			if bytes.HasPrefix(key, filePrefix) {
				var file metadata.File
				if err := json.Unmarshal(val, &file); err != nil {
					logger.Warn("backup: malformed f: entry, skipping hash extraction",
						"key", string(key), "error", err)
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
			return fmt.Errorf("%w: write sentinel: %v", metadata.ErrBackupAborted, err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Write trailing CRC.
	if err := envW.Finish(); err != nil {
		return nil, fmt.Errorf("%w: finish envelope: %v", metadata.ErrBackupAborted, err)
	}

	return hs, nil
}

// Restore reads a backup stream from r and rebuilds metadata state in the
// Badger store. The store must be empty (no existing share data); otherwise
// ErrRestoreDestinationNotEmpty is returned.
//
// Integrity guarantee: KV entries stream straight into Badger via WriteBatch
// (restore RAM bounded by restoreBatchSize, not by share size), the trailing
// CRC is verified last, and any failure triggers a DropAll. Because the
// destination was empty before restore began, a corrupt stream is wiped back
// to empty and the restore stays retryable.
func (s *BadgerMetadataStore) Restore(ctx context.Context, r io.Reader) error {
	// Check destination is empty by looking for any s: prefix key.
	isEmpty, err := s.isStoreEmpty()
	if err != nil {
		return fmt.Errorf("%w: empty check: %v", metadata.ErrRestoreCorrupt, err)
	}
	if !isEmpty {
		return metadata.ErrRestoreDestinationNotEmpty
	}

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
