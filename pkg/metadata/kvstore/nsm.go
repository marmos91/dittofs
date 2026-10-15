package kvstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// ErrNSMNotRaised is a raise of the NSM state number to one not above the
// number held: it never goes down, or an NLM host would not see the restart.
var ErrNSMNotRaised = errors.New("kvstore: NSM state number not raised")

// NSMState is the installation's NSM state number: zero before the first
// raise. An NLM host recognises a server restart only by a higher number.
func (s *Store) NSMState(ctx context.Context) (n uint32, err error) {
	err = s.kv.View(ctx, func(r kv.Reader) (err error) {
		n, err = nsmIn(r)
		return err
	})
	return n, err
}

// RaiseNSMState sets the NSM state number to n, which must be above the
// number held: before the grace of a failover that lost NLM locks, and at a
// moved address's arrival. Two raises racing commit in turn, and the second
// is refused unless its n is above the first's.
func (s *Store) RaiseNSMState(ctx context.Context, n uint32) error {
	return s.kv.Update(ctx, func(tx kv.Txn) error {
		cur, err := nsmIn(tx)
		if err != nil {
			return err
		}
		if n <= cur {
			return fmt.Errorf("%w: %d, holding %d", ErrNSMNotRaised, n, cur)
		}
		return tx.Set(nsmKey(), encodeCounter(uint64(n)))
	})
}

func nsmIn(r kv.Reader) (uint32, error) {
	b, ok, err := get(r, nsmKey())
	if err != nil || !ok {
		return 0, err
	}
	n, err := decodeCounter(b)
	return uint32(n), err
}
