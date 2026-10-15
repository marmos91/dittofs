package kvstore

import (
	"context"
	"errors"
	"math"
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

// numbers hands out IDs from ranges reserved in the store, one counter per
// key, so issuing one does not write the counter's record each time: each
// share's numeric file ids, and the server-wide client, open and lock IDs.
// Each counter has a lock of its own, held across its reservation, so one
// share reserving a range holds up only that share's creates.
type numbers struct {
	mu       sync.Mutex
	counters map[string]*idRange // by counter key
}

// idRange is one key's current range: next, end.
type idRange struct {
	mu    sync.Mutex
	r     [2]uint64
	ready bool
}

// nextNumber issues a numeric file id of share that no other file of the
// share has ever had: from the current range, or a fresh one reserved in its
// own transaction before it is used.
func (s *Store) nextNumber(ctx context.Context, share entity.ShareID) (uint64, error) {
	return s.numbers.next(ctx, s.kv, numberKey(share), false, maxNumber, ErrNumbersExhausted)
}

// The server-wide ID allocators, each under IDS‖kind.
const (
	idClient byte = 1
	idOpen   byte = 2
	idLock   byte = 3
)

// nextID issues a client, open or lock ID never issued before in this
// installation, never zero, which names none.
//
// decision: the IDs come from server-wide counters under a key of their own,
// IDS‖kind, which the format did not name: an ID that must be unique and
// never reused, but not random, needs a counter, and reserving it in ranges
// keeps an open from writing it. Move the counters if the format gives them
// a home.
func (s *Store) nextID(ctx context.Context, kind byte) (uint64, error) {
	for {
		n, err := s.numbers.next(ctx, s.kv, idKey(kind), true, math.MaxUint64-numberRange, ErrNumbersExhausted)
		if err != nil || n != 0 {
			return n, err
		}
	}
}

// next issues the next ID under key. A counter a share's creation writes is
// never missing; a server-wide one starts at zero when it is (fresh).
func (a *numbers) next(ctx context.Context, db kv.KV, key []byte, fresh bool, bound uint64, exhausted error) (uint64, error) {
	a.mu.Lock()
	if a.counters == nil {
		a.counters = map[string]*idRange{}
	}
	c := a.counters[string(key)]
	if c == nil {
		c = &idRange{}
		a.counters[string(key)] = c
	}
	a.mu.Unlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready || c.r[0] == c.r[1] {
		var start uint64
		err := db.Update(ctx, func(tx kv.Txn) error {
			b, err := tx.Get(key)
			switch {
			case fresh && errors.Is(err, kv.ErrNotFound):
				start = 0
			case err != nil:
				return err
			default:
				if start, err = decodeCounter(b); err != nil {
					return err
				}
			}
			if start+numberRange > bound {
				return exhausted
			}
			return tx.Set(key, encodeCounter(start+numberRange))
		})
		if err != nil {
			return 0, err
		}
		c.r, c.ready = [2]uint64{start, start + numberRange}, true
	}
	n := c.r[0]
	c.r[0]++
	return n, nil
}
