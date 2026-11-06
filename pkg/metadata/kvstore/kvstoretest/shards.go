package kvstoretest

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/google/uuid"
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
// shard space is refused and raises nothing. Each shard's incarnation comes
// back as the record then holds it, for the binding to carry with no read
// after the raise.
func testRaiseAtStart(t *testing.T, kvo Opener) {
	s, c := newStore(t, kvo)
	l := s.Lifecycle()
	wantShard(t, s, 1, 0, 0)
	raise := func(shards ...entity.ShardID) {
		t.Helper()
		got, err := l.RaiseAtStart(ctx, shards)
		must(t, err)
		want := map[entity.ShardID]uint64{}

		for _, id := range shards {
			sh, err := s.Shard(ctx, id)
			must(t, err)
			want[id] = sh.Incarnation
		}

		if !maps.Equal(got, want) {
			t.Errorf("RaiseAtStart(%v) returned %v; the records hold %v", shards, got, want)
		}
	}

	raise(1)
	wantShard(t, s, 1, 1, 1)
	raise(1, 2, 1)
	wantShard(t, s, 1, 2, 2)
	wantShard(t, s, 2, 1, 1)

	if _, err := l.RaiseAtStart(ctx, []entity.ShardID{2, entity.ShardLimit}); !invalid(err) {
		t.Errorf("a shard past the shard space: %v", err)
	}

	raise()
	raise(entity.ShardLimit - 1)

	again, err := kvstore.Open(ctx, c)
	must(t, err)
	wantShard(t, again, 1, 2, 2)
	wantShard(t, again, 2, 1, 1)
	wantShard(t, again, 3, 0, 0)
	wantShard(t, again, entity.ShardLimit-1, 1, 1)
}

// RFC 14 §4.4: two raises of one shard that race each take their own
// incarnation: the one committing second reads the first's and raises past it.
func testRaiseAtStartRace(t *testing.T, kvo Opener) {
	s, h := heldStore(t, kvo.backends()[0])
	raise := func() error {
		_, err := s.Lifecycle().RaiseAtStart(ctx, []entity.ShardID{1})

		return err
	}

	outer, inner := race(h, raise, raise)
	must(t, inner)
	must(t, outer)
	wantShard(t, s, 1, 2, 2)
}

// RFC 14 §4.4: an attach of a shard whose open state the node holds raises
// its incarnation and keeps its grace instance, so no client reclaims for
// nothing, and returns the incarnation the record then holds. A shard never
// raised, or past the 24-bit shard space, is refused and left as it was.
func testRaiseAtAttachKeepsGrace(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	l := s.Lifecycle()
	_, err := l.RaiseAtStart(ctx, []entity.ShardID{1})
	must(t, err)

	got, err := l.RaiseAtAttach(ctx, 1)
	must(t, err)

	if got != 2 {
		t.Errorf("RaiseAtAttach returned incarnation %d, want 2", got)
	}

	wantShard(t, s, 1, 2, 1)

	if _, err := l.RaiseAtAttach(ctx, 2); !merrs.IsNotFoundError(err) {
		t.Errorf("an attach raise of a shard never raised: %v, want not found", err)
	}

	if _, err := l.RaiseAtAttach(ctx, entity.ShardLimit); !invalid(err) {
		t.Errorf("an attach raise past the shard space: %v", err)
	}

	wantShard(t, s, 2, 0, 0)
	wantShard(t, s, entity.ShardLimit, 0, 0)
}

// RFC 14 §4.4: two attach raises of one shard that race each take their own
// incarnation, so no verifier repeats: the one committing second reads the
// first's and raises past it.
func testRaiseAtAttachRace(t *testing.T, kvo Opener) {
	s, h := heldStore(t, kvo.backends()[0])
	l := s.Lifecycle()
	_, err := l.RaiseAtStart(ctx, []entity.ShardID{1})
	must(t, err)

	var a, b uint64
	outer, inner := race(h,
		func() (err error) {
			a, err = l.RaiseAtAttach(ctx, 1)

			return err
		},
		func() (err error) {
			b, err = l.RaiseAtAttach(ctx, 1)

			return err
		},
	)
	must(t, inner)
	must(t, outer)

	if a == b {
		t.Errorf("two racing attach raises both returned incarnation %d", a)
	}

	wantShard(t, s, 1, 3, 1)
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

// RFC 14 §4.4: a start raises its shards in transactions of at most K keys,
// each all or none: three shards fit in one, so a failure part way through it
// leaves every shard where it was.
func testRaiseAtStartAllOrNone(t *testing.T, kvo Opener) {
	f := &failSets{KV: kvo(t)}
	must(t, kvstore.Init(ctx, f, uuid.New()))
	s, err := kvstore.Open(ctx, f)
	must(t, err)
	_, err = s.Lifecycle().RaiseAtStart(ctx, []entity.ShardID{1})
	must(t, err)
	f.n = 3

	_, err = s.Lifecycle().RaiseAtStart(
		ctx,
		[]entity.ShardID{1, 2, 3},
	)
	if !errors.Is(err, errInjected) {
		t.Fatalf("raise failing at its third shard: %v", err)
	}

	wantShard(t, s, 1, 1, 1)
	wantShard(t, s, 2, 0, 0)
	wantShard(t, s, 3, 0, 0)
}

// RFC 14 §2.11: shard numbers are installation-wide. Each share is created
// with a shard number no share has had, a removed share's included, and its
// files are in that shard.
func testSharesGetTheirOwnShards(t *testing.T, kvo Opener) {
	s, c := newStore(t, kvo)
	cp, ix := s.ControlPlane(), s.ShareIndex()
	shardOf := func(id entity.ShareID) entity.ShardID {
		t.Helper()
		sh, _, err := ix.Share(ctx, id)
		must(t, err)
		return sh.Shard
	}
	gone := share("gone", "/gone")
	must(t, cp.PutShare(ctx, gone))
	f := setupFS(t, s, c, entity.FoldIdentity)
	if shardOf(gone.ID) == shardOf(f.share.ID) {
		t.Errorf("two shares both in shard %d", shardOf(gone.ID))
	}
	if _, file := f.create(f.admin, f.root, "x", regular(0o644)); entity.ShardID(file.Shard) != shardOf(f.share.ID) {
		t.Errorf("a file of the share in shard %d, want the share's %d", file.Shard, shardOf(f.share.ID))
	}
	must(t, cp.DeleteShare(ctx, gone.ID, entity.SnapshotsRefuse))
	next := share("next", "/next")
	must(t, cp.PutShare(ctx, next))
	if n := shardOf(next.ID); n == shardOf(gone.ID) || n == shardOf(f.share.ID) {
		t.Errorf("a share created after a removal took shard %d, which an earlier share had", n)
	}
}
