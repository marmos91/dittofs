package badger

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/dgraph-io/badger/v4"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// RollupStore Implementation for BadgerDB Store (Phase 10 LSL-05)
// ============================================================================
//
// Persists the per-file rollup_offset for the hybrid append-log tier. The
// atomic-monotone contract (INV-03) is enforced inside a single Badger
// transaction: the read, compare, and write all happen under the same
// db.Update call, so concurrent rollup workers cannot race the stored value
// backwards.
//
// Key Namespace:
//   - ro:{payloadID}  uint64 rollup_offset (little-endian, 8 bytes)
//
// We keep the value a fixed 8-byte LE uint64 rather than JSON to keep
// Badger-internal copies cheap and the format self-describing — any future
// reader can decode without a schema lookup. If future phases grow the
// per-file rollup row (e.g., chunker params pin), migrate under a new
// prefix to keep the v1 format intact.
// ============================================================================

const rollupOffsetPrefix = "ro:"

// Compile-time assertion: the Badger engine implements RollupStore.
var _ metadata.RollupStore = (*BadgerMetadataStore)(nil)

// keyRollupOffset generates the key for a payloadID's rollup_offset.
func keyRollupOffset(payloadID string) []byte {
	return []byte(rollupOffsetPrefix + payloadID)
}

// SetRollupOffset atomically advances payloadID -> newOffset iff
// newOffset >= the currently-stored value. Returns the PREVIOUS stored value
// on success.
//
// On monotone violation (newOffset < stored), returns (storedOffset,
// metadata.ErrRollupOffsetRegression); the stored value is UNCHANGED.
//
// INV-03 is enforced by wrapping the read+compare+write in a single
// db.Update transaction. Badger's default MVCC conflict detection ensures
// that if two concurrent transactions read the same key, only one commits
// — the other retries automatically inside db.Update, at which point it
// sees the updated value and makes a correct monotonicity decision.
func (s *BadgerMetadataStore) SetRollupOffset(ctx context.Context, payloadID string, newOffset uint64) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	var prev uint64
	var regressed bool
	err := s.db.Update(func(txn *badger.Txn) error {
		// Reset on retry — db.Update may retry the closure on conflict.
		prev = 0
		regressed = false

		item, err := txn.Get(keyRollupOffset(payloadID))
		switch {
		case errors.Is(err, badger.ErrKeyNotFound):
			// First write for this payloadID — prev stays 0, no regression possible.
		case err != nil:
			return err
		default:
			if err := item.Value(func(v []byte) error {
				if len(v) != 8 {
					return fmt.Errorf("badger rollup: malformed offset value (len=%d, want 8) for %q", len(v), payloadID)
				}
				prev = binary.LittleEndian.Uint64(v)
				return nil
			}); err != nil {
				return err
			}
		}

		if newOffset < prev {
			// Regression rejected. Commit the txn with no write so the
			// stored value remains untouched.
			regressed = true
			return nil
		}

		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], newOffset)
		return txn.Set(keyRollupOffset(payloadID), buf[:])
	})
	if err != nil {
		return 0, fmt.Errorf("badger rollup set: %w", err)
	}
	if regressed {
		return prev, metadata.ErrRollupOffsetRegression
	}
	return prev, nil
}

// GetRollupOffset returns the persisted rollup_offset for payloadID, or
// (0, nil) if unset. Matches the contract in metadata.RollupStore — a fresh
// file is treated as rolled-up-to-0.
func (s *BadgerMetadataStore) GetRollupOffset(ctx context.Context, payloadID string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	var out uint64
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(keyRollupOffset(payloadID))
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return item.Value(func(v []byte) error {
			if len(v) != 8 {
				return fmt.Errorf("badger rollup: malformed offset value (len=%d, want 8) for %q", len(v), payloadID)
			}
			out = binary.LittleEndian.Uint64(v)
			return nil
		})
	})
	if err != nil {
		return 0, fmt.Errorf("badger rollup get: %w", err)
	}
	return out, nil
}
