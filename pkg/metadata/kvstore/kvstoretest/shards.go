package kvstoretest

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

func wantShard(t *testing.T, s *kvstore.Store, id entity.ShardID, inc, grace uint64) {
	t.Helper()
	want := entity.Shard{ID: id, Incarnation: inc, GraceInstance: grace}
	if got, err := s.Shard(ctx, id); err != nil || got != want {
		t.Errorf("shard %d: %+v, %v; want %+v", id, got, err, want)
	}
}

// RFC 14 §4.4: every start raises the incarnation of each shard it serves by
// one, durably, and on a single node opens a new grace instance at it. A
// shard named twice in one start is raised once; one outside the 24-bit
// shard space is refused and raises nothing.
func testRaiseIncarnations(t *testing.T, kvo Opener) {
	s, c := newStore(t, kvo)
	wantShard(t, s, 1, 0, 0)
	must(t, s.RaiseIncarnations(ctx, []entity.ShardID{1}))
	wantShard(t, s, 1, 1, 1)
	must(t, s.RaiseIncarnations(ctx, []entity.ShardID{1, 2, 1}))
	wantShard(t, s, 1, 2, 2)
	wantShard(t, s, 2, 1, 1)
	if err := s.RaiseIncarnations(ctx, []entity.ShardID{2, entity.ShardLimit}); !invalid(err) {
		t.Errorf("a shard past the shard space: %v", err)
	}
	must(t, s.RaiseIncarnations(ctx, nil))
	must(t, s.RaiseIncarnations(ctx, []entity.ShardID{entity.ShardLimit - 1}))
	again, err := kvstore.Open(ctx, c)
	must(t, err)
	wantShard(t, again, 1, 2, 2)
	wantShard(t, again, 2, 1, 1)
	wantShard(t, again, 3, 0, 0)
	wantShard(t, again, entity.ShardLimit-1, 1, 1)
}

// RFC 14 §4.4: two raises of one shard that race each take their own
// incarnation: the one committing second reads the first's and raises past it.
func testRaiseIncarnationsRace(t *testing.T, kvo Opener) {
	s, h := heldStore(t, kvo.backends()[0])
	raise := func() error { return s.RaiseIncarnations(ctx, []entity.ShardID{1}) }
	outer, inner := race(h, raise, raise)
	must(t, inner)
	must(t, outer)
	wantShard(t, s, 1, 2, 2)
}

// failSets fails the nth Set from when n is set, counted across
// transactions, so a call that commits in several fails part way too.
type failSets struct {
	kv.KV
	n int
}

var errInjected = errors.New("injected")

func (f *failSets) Update(ctx context.Context, fn func(kv.Txn) error) error {
	return f.KV.Update(ctx, func(tx kv.Txn) error { return fn(&failTxn{Txn: tx, f: f}) })
}

type failTxn struct {
	kv.Txn
	f *failSets
}

func (t *failTxn) Set(k, v []byte) error {
	if t.f.n > 0 {
		if t.f.n--; t.f.n == 0 {
			return errInjected
		}
	}
	return t.Txn.Set(k, v)
}

// RFC 14 §4.4: a start raises its shards in one transaction, so a failure
// part way leaves every shard where it was.
func testRaiseIncarnationsAllOrNone(t *testing.T, kvo Opener) {
	f := &failSets{KV: kvo(t)}
	must(t, kvstore.Init(ctx, f, uuid.New()))
	s, err := kvstore.Open(ctx, f)
	must(t, err)
	must(t, s.RaiseIncarnations(ctx, []entity.ShardID{1}))
	f.n = 3
	if err := s.RaiseIncarnations(ctx, []entity.ShardID{1, 2, 3}); !errors.Is(err, errInjected) {
		t.Fatalf("raise failing at its third shard: %v", err)
	}
	wantShard(t, s, 1, 1, 1)
	wantShard(t, s, 2, 0, 0)
	wantShard(t, s, 3, 0, 0)
}
