package badger

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Key Namespace
// ============================================================================
//
// Block Record Store:
//   br:{blockID}          value = JSON-encoded block.BlockRecord
//
// Local Chunk Index:
//   li:{hex(hash)}        value = JSON-encoded block.LocalChunkLocation
//
// Hex-encoding the 32-byte hash keeps keys printable and unambiguous without
// the separator complications that raw binary would require against the
// br: prefix.
// ============================================================================

const (
	prefixBlockRecord   = "br:"
	prefixLocalChunkIdx = "li:"
)

// keyBlockRecord builds the br:{blockID} key.
func keyBlockRecord(blockID string) []byte {
	return append([]byte(prefixBlockRecord), blockID...)
}

// keyLocalChunk builds the li:{hex(hash)} key.
func keyLocalChunk(hash block.ContentHash) []byte {
	h := hex.EncodeToString(hash[:])
	return append([]byte(prefixLocalChunkIdx), h...)
}

// ============================================================================
// JSON value helpers
// ============================================================================

func encodeBlockRecord(rec block.BlockRecord) ([]byte, error) {
	return json.Marshal(rec)
}

func decodeBlockRecord(b []byte) (block.BlockRecord, error) {
	var rec block.BlockRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return block.BlockRecord{}, fmt.Errorf("badger: decode BlockRecord: %w", err)
	}
	return rec, nil
}

func encodeLocalChunkLocation(loc block.LocalChunkLocation) ([]byte, error) {
	return json.Marshal(loc)
}

func decodeLocalChunkLocation(b []byte) (block.LocalChunkLocation, error) {
	var loc block.LocalChunkLocation
	if err := json.Unmarshal(b, &loc); err != nil {
		return block.LocalChunkLocation{}, fmt.Errorf("badger: decode LocalChunkLocation: %w", err)
	}
	return loc, nil
}

// ============================================================================
// BlockRecordStore — transaction level
// ============================================================================

func (tx *badgerTransaction) PutBlockRecord(_ context.Context, rec block.BlockRecord) error {
	data, err := encodeBlockRecord(rec)
	if err != nil {
		return fmt.Errorf("badger PutBlockRecord encode: %w", err)
	}
	return tx.txn.Set(keyBlockRecord(rec.BlockID), data)
}

func (tx *badgerTransaction) GetBlockRecord(_ context.Context, blockID string) (block.BlockRecord, bool, error) {
	item, err := tx.txn.Get(keyBlockRecord(blockID))
	if errors.Is(err, badgerdb.ErrKeyNotFound) {
		return block.BlockRecord{}, false, nil
	}
	if err != nil {
		return block.BlockRecord{}, false, fmt.Errorf("badger GetBlockRecord: %w", err)
	}
	var rec block.BlockRecord
	if verr := item.Value(func(val []byte) error {
		var decErr error
		rec, decErr = decodeBlockRecord(val)
		return decErr
	}); verr != nil {
		return block.BlockRecord{}, false, verr
	}
	return rec, true, nil
}

func (tx *badgerTransaction) DeleteBlockRecord(_ context.Context, blockID string) error {
	err := tx.txn.Delete(keyBlockRecord(blockID))
	if errors.Is(err, badgerdb.ErrKeyNotFound) {
		return nil // idempotent
	}
	return err
}

// WalkBlockRecords iterates br:* within the transaction. Collects all records
// first, then calls fn outside iteration so the callback never runs with the
// iterator open (mirrors EnumerateSynced's safe pattern).
func (tx *badgerTransaction) WalkBlockRecords(_ context.Context, fn func(block.BlockRecord) error) error {
	prefix := []byte(prefixBlockRecord)
	opts := badgerdb.DefaultIteratorOptions
	opts.Prefix = prefix

	// Collect inside an inner closure so the iterator is closed by defer
	// (panic-safe) yet still closed before fn runs — fn must never execute with
	// the iterator open.
	collect := func() ([]block.BlockRecord, error) {
		it := tx.txn.NewIterator(opts)
		defer it.Close()
		var recs []block.BlockRecord
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			var rec block.BlockRecord
			if verr := item.Value(func(val []byte) error {
				var decErr error
				rec, decErr = decodeBlockRecord(val)
				return decErr
			}); verr != nil {
				return nil, verr
			}
			recs = append(recs, rec)
		}
		return recs, nil
	}
	recs, err := collect()
	if err != nil {
		return err
	}

	for _, rec := range recs {
		if err := fn(rec); err != nil {
			return err
		}
	}
	return nil
}

// DecrLiveChunkCount atomically decrements LiveChunkCount within the
// transaction, flooring at 0. Returns the remaining count. Returns an error
// if blockID does not exist.
func (tx *badgerTransaction) DecrLiveChunkCount(_ context.Context, blockID string, delta uint32) (uint32, error) {
	item, err := tx.txn.Get(keyBlockRecord(blockID))
	if errors.Is(err, badgerdb.ErrKeyNotFound) {
		return 0, fmt.Errorf("badger DecrLiveChunkCount: block %q not found", blockID)
	}
	if err != nil {
		return 0, fmt.Errorf("badger DecrLiveChunkCount get: %w", err)
	}

	var rec block.BlockRecord
	if verr := item.Value(func(val []byte) error {
		var decErr error
		rec, decErr = decodeBlockRecord(val)
		return decErr
	}); verr != nil {
		return 0, verr
	}

	if delta >= rec.LiveChunkCount {
		rec.LiveChunkCount = 0
	} else {
		rec.LiveChunkCount -= delta
	}

	data, err := encodeBlockRecord(rec)
	if err != nil {
		return 0, fmt.Errorf("badger DecrLiveChunkCount encode: %w", err)
	}
	if err := tx.txn.Set(keyBlockRecord(blockID), data); err != nil {
		return 0, fmt.Errorf("badger DecrLiveChunkCount set: %w", err)
	}
	return rec.LiveChunkCount, nil
}

// ============================================================================
// LocalChunkIndex — transaction level
// ============================================================================

func (tx *badgerTransaction) PutLocalLocation(_ context.Context, hash block.ContentHash, loc block.LocalChunkLocation) error {
	data, err := encodeLocalChunkLocation(loc)
	if err != nil {
		return fmt.Errorf("badger PutLocalLocation encode: %w", err)
	}
	return tx.txn.Set(keyLocalChunk(hash), data)
}

func (tx *badgerTransaction) GetLocalLocation(_ context.Context, hash block.ContentHash) (block.LocalChunkLocation, bool, error) {
	item, err := tx.txn.Get(keyLocalChunk(hash))
	if errors.Is(err, badgerdb.ErrKeyNotFound) {
		return block.LocalChunkLocation{}, false, nil
	}
	if err != nil {
		return block.LocalChunkLocation{}, false, fmt.Errorf("badger GetLocalLocation: %w", err)
	}
	var loc block.LocalChunkLocation
	if verr := item.Value(func(val []byte) error {
		var decErr error
		loc, decErr = decodeLocalChunkLocation(val)
		return decErr
	}); verr != nil {
		return block.LocalChunkLocation{}, false, verr
	}
	return loc, true, nil
}

func (tx *badgerTransaction) DeleteLocalLocation(_ context.Context, hash block.ContentHash) error {
	err := tx.txn.Delete(keyLocalChunk(hash))
	if errors.Is(err, badgerdb.ErrKeyNotFound) {
		return nil // idempotent
	}
	return err
}

// ============================================================================
// BlockRecordStore — store level (delegate to Badger txns)
// ============================================================================

func (s *BadgerMetadataStore) PutBlockRecord(ctx context.Context, rec block.BlockRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := encodeBlockRecord(rec)
	if err != nil {
		return fmt.Errorf("badger PutBlockRecord encode: %w", err)
	}
	if err := s.db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set(keyBlockRecord(rec.BlockID), data)
	}); err != nil {
		return fmt.Errorf("badger PutBlockRecord: %w", err)
	}
	return nil
}

func (s *BadgerMetadataStore) GetBlockRecord(ctx context.Context, blockID string) (block.BlockRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.BlockRecord{}, false, err
	}
	var (
		rec   block.BlockRecord
		found bool
	)
	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(keyBlockRecord(blockID))
		if errors.Is(err, badgerdb.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return item.Value(func(val []byte) error {
			var decErr error
			rec, decErr = decodeBlockRecord(val)
			return decErr
		})
	})
	if err != nil {
		return block.BlockRecord{}, false, fmt.Errorf("badger GetBlockRecord: %w", err)
	}
	return rec, found, nil
}

func (s *BadgerMetadataStore) DeleteBlockRecord(ctx context.Context, blockID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := s.db.Update(func(txn *badgerdb.Txn) error {
		err := txn.Delete(keyBlockRecord(blockID))
		if errors.Is(err, badgerdb.ErrKeyNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("badger DeleteBlockRecord: %w", err)
	}
	return nil
}

func (s *BadgerMetadataStore) WalkBlockRecords(ctx context.Context, fn func(block.BlockRecord) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	prefix := []byte(prefixBlockRecord)

	var recs []block.BlockRecord
	if err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			var rec block.BlockRecord
			if verr := item.Value(func(val []byte) error {
				var decErr error
				rec, decErr = decodeBlockRecord(val)
				return decErr
			}); verr != nil {
				return verr
			}
			recs = append(recs, rec)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("badger WalkBlockRecords: %w", err)
	}

	for _, rec := range recs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	return nil
}

// DecrLiveChunkCount atomically decrements LiveChunkCount for the named block,
// flooring at 0. Returns the remaining count. Returns an error if blockID does
// not exist. The read-modify-write runs under updateWithConflictRetry so a
// Badger SSI ErrConflict from a concurrent decrement is retried internally
// rather than surfaced to the GC caller (which has no retry of its own).
func (s *BadgerMetadataStore) DecrLiveChunkCount(ctx context.Context, blockID string, delta uint32) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var remaining uint32
	err := s.updateWithConflictRetry(ctx, func(txn *badgerdb.Txn) error {
		item, err := txn.Get(keyBlockRecord(blockID))
		if errors.Is(err, badgerdb.ErrKeyNotFound) {
			return fmt.Errorf("badger DecrLiveChunkCount: block %q not found", blockID)
		}
		if err != nil {
			return err
		}
		var rec block.BlockRecord
		if verr := item.Value(func(val []byte) error {
			var decErr error
			rec, decErr = decodeBlockRecord(val)
			return decErr
		}); verr != nil {
			return verr
		}
		if delta >= rec.LiveChunkCount {
			rec.LiveChunkCount = 0
		} else {
			rec.LiveChunkCount -= delta
		}
		remaining = rec.LiveChunkCount
		data, encErr := encodeBlockRecord(rec)
		if encErr != nil {
			return encErr
		}
		return txn.Set(keyBlockRecord(blockID), data)
	})
	if err != nil {
		return 0, fmt.Errorf("badger DecrLiveChunkCount: %w", err)
	}
	return remaining, nil
}

// ============================================================================
// LocalChunkIndex — store level
// ============================================================================

func (s *BadgerMetadataStore) PutLocalLocation(ctx context.Context, hash block.ContentHash, loc block.LocalChunkLocation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := encodeLocalChunkLocation(loc)
	if err != nil {
		return fmt.Errorf("badger PutLocalLocation encode: %w", err)
	}
	if err := s.db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set(keyLocalChunk(hash), data)
	}); err != nil {
		return fmt.Errorf("badger PutLocalLocation: %w", err)
	}
	return nil
}

func (s *BadgerMetadataStore) GetLocalLocation(ctx context.Context, hash block.ContentHash) (block.LocalChunkLocation, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.LocalChunkLocation{}, false, err
	}
	var (
		loc   block.LocalChunkLocation
		found bool
	)
	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(keyLocalChunk(hash))
		if errors.Is(err, badgerdb.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return item.Value(func(val []byte) error {
			var decErr error
			loc, decErr = decodeLocalChunkLocation(val)
			return decErr
		})
	})
	if err != nil {
		return block.LocalChunkLocation{}, false, fmt.Errorf("badger GetLocalLocation: %w", err)
	}
	return loc, found, nil
}

func (s *BadgerMetadataStore) DeleteLocalLocation(ctx context.Context, hash block.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := s.db.Update(func(txn *badgerdb.Txn) error {
		err := txn.Delete(keyLocalChunk(hash))
		if errors.Is(err, badgerdb.ErrKeyNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("badger DeleteLocalLocation: %w", err)
	}
	return nil
}

// ============================================================================
// CommitBlock — delegate to shared DefaultCommitBlock logic
// ============================================================================

// CommitBlock atomically writes the block record and all local chunk locations
// within a single transaction (via DefaultCommitBlock), then marks each chunk
// synced. Idempotent: a second call for an already-committed block is a no-op.
func (s *BadgerMetadataStore) CommitBlock(ctx context.Context, rec block.BlockRecord, chunks []block.BlockChunkCommit) error {
	return metadata.DefaultCommitBlock(ctx, s, rec, chunks)
}

// Compile-time guards: both the store and its transaction implement the new
// block-record and local-index contracts (store-level is not otherwise checked
// via the Transaction interface).
var (
	_ metadata.BlockRecordStore = (*BadgerMetadataStore)(nil)
	_ metadata.BlockRecordStore = (*badgerTransaction)(nil)
	_ metadata.LocalChunkIndex  = (*BadgerMetadataStore)(nil)
	_ metadata.LocalChunkIndex  = (*badgerTransaction)(nil)
)
