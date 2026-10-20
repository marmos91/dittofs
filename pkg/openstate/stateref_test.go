package openstate

import (
	"errors"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

func mintIn(t *testing.T, tab *Table, f entity.FileID, c entity.ClientID, s ShardID) StateRef {
	t.Helper()
	var ref StateRef
	if err := tab.step(f, func(e *entry) error {
		ref = tab.mint(e, f, c, s, StateOpen)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return ref
}

func releaseIn(t *testing.T, tab *Table, f entity.FileID, ref StateRef) {
	t.Helper()
	if err := tab.step(f, func(e *entry) error { tab.release(e, ref); return nil }); err != nil {
		t.Fatal(err)
	}
}

// No flip of one of a reference's 64 random bits names another record: the
// token is random, not a counter, so holding one reveals no other.
func TestStateRefTokenIsUnguessable(t *testing.T) {
	tab := New()
	f := entity.NewFileID()
	refs := make([]StateRef, 1<<16)
	for i := range refs {
		refs[i] = mintIn(t, tab, f, 1, 7)
	}
	for _, ref := range refs {
		for bit := range 64 {
			flipped := ref
			flipped.Token ^= 1 << bit
			if _, err := tab.resolve(1, f, flipped); !errors.Is(err, ErrBadState) {
				t.Fatalf("token %#x with bit %d flipped: %v, want ErrBadState", ref.Token, bit, err)
			}
		}
	}
}

// A miss is stale while its shard is in grace or when it was minted under an
// earlier grace instance, so the client reclaims; any other miss is bad.
func TestMissIsStaleOrBad(t *testing.T) {
	f := entity.NewFileID()
	for _, tc := range []struct {
		name string
		now  shardGrace // the shard's state when the reference is presented
		want error
	}{
		{"released, same instance", shardGrace{instance: 3}, ErrBadState},
		{"in grace", shardGrace{instance: 3, inGrace: true}, ErrStaleState},
		{"earlier instance", shardGrace{instance: 4}, ErrStaleState},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tab := New()
			tab.shards[9] = shardGrace{instance: 3}
			ref := mintIn(t, tab, f, 1, 9)
			releaseIn(t, tab, f, ref)
			tab.shards[9] = tc.now
			if _, err := tab.resolve(1, f, ref); !errors.Is(err, tc.want) {
				t.Errorf("resolve: %v, want %v", err, tc.want)
			}
		})
	}
}

// A reference resolves only for its own client and, when the request names a
// file, only for its own file.
func TestResolveChecksClientAndFile(t *testing.T) {
	tab := New()
	f, other := entity.NewFileID(), entity.NewFileID()
	ref := mintIn(t, tab, f, 1, 0)
	if _, err := tab.resolve(1, f, ref); err != nil {
		t.Fatalf("own client and file: %v", err)
	}
	if _, err := tab.resolve(1, entity.FileID{}, ref); err != nil {
		t.Fatalf("no file named: %v", err)
	}
	if _, err := tab.resolve(2, f, ref); !errors.Is(err, ErrNotYours) {
		t.Errorf("another client: %v, want ErrNotYours", err)
	}
	if _, err := tab.resolve(1, other, ref); !errors.Is(err, ErrBadState) {
		t.Errorf("another file: %v, want ErrBadState", err)
	}
	wrongType := ref
	wrongType.Type = StateLock
	if _, err := tab.resolve(1, f, wrongType); !errors.Is(err, ErrBadState) {
		t.Errorf("another type: %v, want ErrBadState", err)
	}
}

// A file's entry exists while it holds state and is freed with its last
// piece, also when steps on the file run concurrently.
func TestEntryFreedWithLastState(t *testing.T) {
	tab := New()
	f := entity.NewFileID()
	a, b := mintIn(t, tab, f, 1, 0), mintIn(t, tab, f, 1, 0)
	releaseIn(t, tab, f, a)
	if tab.files[f] == nil {
		t.Fatal("entry freed while it still holds state")
	}
	releaseIn(t, tab, f, b)
	if tab.files[f] != nil {
		t.Fatal("entry kept after its last state was released")
	}

	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			for range 100 {
				var ref StateRef
				_ = tab.step(f, func(e *entry) error { ref = tab.mint(e, f, 1, 0, StateOpen); return nil })
				_ = tab.step(f, func(e *entry) error { tab.release(e, ref); return nil })
			}
		})
	}
	wg.Wait()
	if len(tab.files) != 0 || len(tab.refs) != 0 {
		t.Fatalf("after concurrent steps: %d entries, %d references; want none", len(tab.files), len(tab.refs))
	}
}
