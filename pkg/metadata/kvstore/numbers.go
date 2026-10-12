package kvstore

import (
	"context"
	"errors"
	"sync"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// ErrNumbersExhausted is a share that has issued every numeric file id below
// the bound.
var ErrNumbersExhausted = errors.New("kvstore: share has no numeric file ids left")

const (
	// maxNumber bounds numeric file ids below 2^48, so a snapshot's id can
	// carry an ordinal above them.
	maxNumber = 1 << 48
	// numberRange is how many ids one reservation takes. A range a process
	// had not used up when it stopped is abandoned, never issued again.
	numberRange = 1024
)

// numbers hands out each share's numeric file ids from ranges reserved in
// the store, so creates do not all write the allocator record.
type numbers struct {
	mu     sync.Mutex
	ranges map[entity.ShareID]*[2]uint64 // next, end
}

// nextNumber issues a numeric file id of share that no other file of the
// share has ever had: from the current range, or a fresh one reserved in its
// own transaction before it is used.
func (s *Store) nextNumber(ctx context.Context, share entity.ShareID) (uint64, error) {
	s.numbers.mu.Lock()
	defer s.numbers.mu.Unlock()
	if s.numbers.ranges == nil {
		s.numbers.ranges = map[entity.ShareID]*[2]uint64{}
	}
	r := s.numbers.ranges[share]
	if r == nil || r[0] == r[1] {
		var start uint64
		err := s.kv.Update(ctx, func(tx kv.Txn) error {
			key := numberKey(share)
			b, err := tx.Get(key)
			if err != nil {
				return err
			}
			if start, err = decodeCounter(b); err != nil {
				return err
			}
			if start+numberRange > maxNumber {
				return ErrNumbersExhausted
			}
			return tx.Set(key, encodeCounter(start+numberRange))
		})
		if err != nil {
			return 0, err
		}
		r = &[2]uint64{start, start + numberRange}
		s.numbers.ranges[share] = r
	}
	n := r[0]
	r[0]++
	return n, nil
}
