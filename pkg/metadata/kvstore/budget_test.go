package kvstore

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

func TestKeyBudget(t *testing.T) {
	perKey, fixed := worstCosts()
	b, F := 0, 0
	for _, c := range perKey {
		b = max(b, c.bytes())
	}
	for _, c := range fixed {
		F += c.bytes()
	}
	base := kv.TxnLimits{Key: 1 << 16, Value: 1 << 20, TxnBytes: 8 << 20, TxnEntries: 0}
	k, err := keyBudget(base)
	if err != nil || k != (base.TxnBytes-F)/b {
		t.Fatalf("bytes-bound K = %d, %v; want %d", k, err, (base.TxnBytes-F)/b)
	}
	// A batch of K worst-case keys fits; one more does not.
	if F+k*b > base.TxnBytes || F+(k+1)*b <= base.TxnBytes {
		t.Errorf("K %d is not the largest batch that fits", k)
	}
	entries := base
	entries.TxnEntries = 50
	if k, _ := keyBudget(entries); k != 50-len(fixed) {
		t.Errorf("entry-bound K = %d, want %d", k, 50-len(fixed))
	}
	small := base
	small.Value = 1 << 10 // a block record with its carried list does not fit
	if _, err := keyBudget(small); !errors.Is(err, ErrLimits) {
		t.Errorf("value limit below a record: %v", err)
	}
	tiny := base
	tiny.TxnBytes = F + b*(minItemKeys-1)
	if _, err := keyBudget(tiny); !errors.Is(err, ErrLimits) {
		t.Errorf("a batch too small for one item: %v", err)
	}
}

func TestOpenComputesKeyBudget(t *testing.T) {
	s := openStore(t)
	if s.k < minItemKeys {
		t.Fatalf("K = %d", s.k)
	}
	t.Logf("K on badger in memory: %d", s.k)
}
