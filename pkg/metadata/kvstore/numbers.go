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

// ErrIDsExhausted is a server-wide counter of client, open or lock IDs that
// has issued every ID it can.
var ErrIDsExhausted = errors.New("kvstore: no client, open or lock IDs left")

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
// installation, never zero, which names none. The IDs come from server-wide
// counters under IDS‖kind, reserved in ranges as file numbers are, so an open
// does not write the counter. A lock ID needs to differ only from the other
// locks under its open and offset; the server-wide counter gives more than
// that, at no extra cost. An NFSv4 lock state's ID is random, never one of
// these.
//
// ponytail: in a cluster these three keys are the only server-wide keys
// opens contend on, once per range. Widen the ranges, or give each node its
// own, if their reservations show in a profile.
func (s *Store) nextID(ctx context.Context, kind byte) (uint64, error) {
	for {
		n, err := s.numbers.next(ctx, s.kv, idKey(kind), true, math.MaxUint64-numberRange, ErrIDsExhausted)
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
