package kvstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// lifecycleStore is a store over an in-memory KV, wrapped by wrap if it is
// given.
func lifecycleStore(t *testing.T, wrap func(kv.KV) kv.KV) *Store {
	t.Helper()
	db := memKV(t)

	if wrap != nil {
		db = wrap(db)
	}

	ctx := context.Background()

	if err := Init(ctx, db, uuid.New()); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

// namedShare creates a share named name and returns it as the store holds
// it, with its tag and shard.
func namedShare(t *testing.T, s *Store, name string) entity.Share {
	t.Helper()
	ctx := context.Background()
	sh := entity.Share{
		ID:   entity.NewShareID(),
		Name: name,
		Path: "/" + name,
		Root: entity.NewFileID(),
	}

	if err := s.ControlPlane().PutShare(ctx, sh); err != nil {
		t.Fatal(err)
	}

	got, _, err := s.ShareIndex().Share(ctx, sh.ID)
	if err != nil {
		t.Fatal(err)
	}

	return got
}

// encodeRemovalStep writes a SH‖shard‖rm value, standing in for the
// removal's per-shard steps, which no code writes yet.
func encodeRemovalStep(step removalStep) []byte {
	return codec.NewWriter(v1).Uint8(uint8(step)).Encode()
}

// keysUnder lists the keys under prefix.
func keysUnder(t *testing.T, s *Store, prefix []byte) [][]byte {
	t.Helper()
	var out [][]byte

	if err := s.kv.View(context.Background(), func(r kv.Reader) error {
		for row, err := range r.Scan(prefix, nil) {
			if err != nil {
				return err
			}

			out = append(out, bytes.Clone(row.Key))
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	return out
}

// RFC 8 §2.7: the limits are raised in as many transactions as the key
// budget K takes, never one over every share: no Update writes more than K
// keys, and 3 × K shares take at least 6 Updates, so a batch holds at most
// K/2 shares, each of which also reads three keys with tracking.
func TestRaiseVersionLimitsWithinK(t *testing.T) {
	const k = 4
	ctx := context.Background()
	var c *kv.Counting
	s := lifecycleStore(t, func(db kv.KV) kv.KV {
		c = &kv.Counting{KV: db}

		return c
	})

	s.SetKeyBudget(k)
	to := map[entity.ShareID]entity.JournalVersion{}

	for i := range 3 * k {
		to[namedShare(t, s, fmt.Sprintf("s%d", i)).ID] = entity.VersionOf(uint64(100 + i))
	}

	c.TakeUpdates()

	if err := s.Lifecycle().RaiseVersionLimits(ctx, to); err != nil {
		t.Fatal(err)
	}

	n := c.TakeUpdates()

	if n.MostWrites > k {
		t.Errorf("one Update wrote %d keys, past K = %d", n.MostWrites, k)
	}

	if n.Updates < 6 {
		t.Errorf("%d shares raised in %d Updates, want at least 6", len(to), n.Updates)
	}

	got, err := s.Lifecycle().VersionLimits(ctx, slices.Collect(maps.Keys(to)))
	if err != nil {
		t.Fatal(err)
	}

	for id, v := range to {
		if got[id] != v {
			t.Errorf("share %v: limit %d, want %d", id, got[id].Counter(), v.Counter())
		}
	}
}

// RFC 16 §3.2: a journal keeps one J‖journal key, naming its node and
// device. Recording a device again deletes the key of the identity its index
// named, and only that one; recording the same identity again keeps it.
func TestRecordJournalKeepsOneKeyPerJournal(t *testing.T) {
	ctx := context.Background()
	s := lifecycleStore(t, nil)
	node := entity.NodeID{1}
	d1, d2 := entity.JournalDir("/j/1"), entity.JournalDir("/j/2")
	id1, id2, id3 := [16]byte{1}, [16]byte{2}, [16]byte{3}
	check := func(when string) {
		t.Helper()
		got := keysUnder(t, s, []byte{keys.KindJ})
		want := [][]byte{journalKey(id2), journalKey(id3)}

		if !slices.EqualFunc(got, want, bytes.Equal) {
			t.Errorf("%s: J‖ keys %x, want %x", when, got, want)
		}

		for dev, id := range map[entity.JournalDir][16]byte{d1: id2, d2: id3} {
			if got, err := s.Lifecycle().JournalID(ctx, node, dev); err != nil || got != id {
				t.Errorf("%s: JournalID(%s) %x, %v; want %x", when, dev, got, err, id)
			}
		}
	}

	for _, r := range []struct {
		id  [16]byte
		dev entity.JournalDir
	}{{id1, d1}, {id3, d2}, {id2, d1}} {
		if err := s.RecordJournal(ctx, r.id, node, r.dev); err != nil {
			t.Fatal(err)
		}
	}

	check("after re-initialising the first device")

	var rec journalRecord
	want := journalRecord{
		Node:   node,
		Device: d1,
	}

	if err := s.kv.View(ctx, func(r kv.Reader) error {
		b, err := r.Get(journalKey(id2))
		if err != nil {
			return err
		}

		rec, err = decodeJournal(b)

		return err
	}); err != nil || rec != want {
		t.Errorf("J‖journal of the new identity: %+v, %v; want the node and its device", rec, err)
	}

	if err := s.RecordJournal(ctx, id2, node, d1); err != nil {
		t.Fatal(err)
	}

	check("after recording the same identity again")
}

// failNthSet fails the nth Set from when n is set, counted across
// transactions, so a write split over two transactions leaves its first.
type failNthSet struct {
	kv.KV
	n int
}

var errInjectedSet = errors.New("injected set failure")

func (f *failNthSet) Update(ctx context.Context, fn func(kv.Txn) error) error {
	return f.KV.Update(ctx, func(tx kv.Txn) error {
		return fn(&failNthSetTxn{
			Txn: tx,
			f:   f,
		})
	})
}

type failNthSetTxn struct {
	kv.Txn
	f *failNthSet
}

func (t *failNthSetTxn) Set(k, v []byte) error {
	if t.f.n > 0 {
		if t.f.n--; t.f.n == 0 {
			return errInjectedSet
		}
	}

	return t.Txn.Set(k, v)
}

// RFC 16 §3.2: RecordJournal writes J‖journal and JD‖node‖device in one
// transaction, so a failure leaves neither.
func TestRecordJournalIsOneTransaction(t *testing.T) {
	ctx := context.Background()
	var f *failNthSet
	s := lifecycleStore(t, func(db kv.KV) kv.KV {
		f = &failNthSet{KV: db}

		return f
	})

	id, node, dir := [16]byte{1}, entity.NodeID{1}, entity.JournalDir("/j")
	f.n = 2

	if err := s.RecordJournal(ctx, id, node, dir); !errors.Is(err, errInjectedSet) {
		t.Fatalf("RecordJournal failing at its second write: %v", err)
	}

	for _, key := range [][]byte{journalKey(id), journalIndexKey(node, dir)} {
		if got := keysUnder(t, s, key); len(got) != 0 {
			t.Errorf("a failed RecordJournal left %x", got)
		}
	}
}

// RFC 8 §2.7: a share being removed or retired is skipped, so it does not
// fail the raise for the shares batched with it, and is not raised; a share
// that does not exist is still refused.
func TestRaiseVersionLimitsSkipsRemovedShares(t *testing.T) {
	ctx := context.Background()
	s := lifecycleStore(t, nil)
	live := namedShare(t, s, "live")
	removing := namedShare(t, s, "removing")
	retired := namedShare(t, s, "retired")

	for _, id := range []entity.ShareID{removing.ID, retired.ID} {
		if err := s.ControlPlane().DeleteShare(ctx, id, entity.SnapshotsRefuse); err != nil {
			t.Fatal(err)
		}
	}

	rows(t, s, shareKey(retired.ID, keys.SSt), encodeState(shareState{State: entity.ShareRetired}))
	ten := entity.VersionOf(10)

	err := s.Lifecycle().RaiseVersionLimits(ctx, map[entity.ShareID]entity.JournalVersion{
		live.ID:     ten,
		removing.ID: ten,
		retired.ID:  ten,
	})
	if err != nil {
		t.Fatalf("a raise naming shares being removed and retired: %v", err)
	}

	for id, want := range map[entity.ShareID]entity.JournalVersion{
		live.ID:     ten,
		removing.ID: {},
		retired.ID:  {},
	} {
		if got, err := s.VersionLimit(ctx, id); err != nil || got != want {
			t.Errorf("share %v: limit %d, %v; want %d", id, got.Counter(), err, want.Counter())
		}
	}

	unknown := map[entity.ShareID]entity.JournalVersion{entity.NewShareID(): ten}

	if err := s.Lifecycle().RaiseVersionLimits(ctx, unknown); !merrs.IsNotFoundError(err) {
		t.Errorf("a raise naming a share that does not exist: %v, want not found", err)
	}
}

// RFC 16 §3 and §2.3.2: Tags names every share record of the installation,
// with its tag and how far its removal has gone, read from every one of its
// shards' rm rows: Serving unless every row is at offloaded or later,
// Forgotten when every row is at forgotten, step 4 or done, and a shard with
// no row is before its step 3.
func TestTagsDerivesEachShareState(t *testing.T) {
	ctx := context.Background()
	s := lifecycleStore(t, nil)
	none := removalStep(0)
	want := map[entity.ShareID]entity.TagRecord{}
	names := map[entity.ShareID]string{}

	for _, c := range []struct {
		name  string
		state entity.ShareState
		// lower and upper are the rm rows of the share's shard and of a second,
		// higher shard; none: no row.
		lower, upper removalStep
		second       bool // whether the share has the second shard
		want         entity.TagState
	}{
		{"a", entity.ShareEnabled, none, none, false, entity.TagServing},
		{"b", entity.ShareRemoving, none, none, false, entity.TagServing},
		{"c", entity.ShareRemoving, stepThree, none, false, entity.TagServing},
		{"d", entity.ShareRemoving, stepOffloaded, none, false, entity.TagOffloaded},
		{"e", entity.ShareRemoving, stepForgotten, none, false, entity.TagForgotten},
		{"f", entity.ShareRemoving, stepFour, none, false, entity.TagForgotten},
		{"g", entity.ShareRetired, stepDone, none, false, entity.TagForgotten},
		{"h", entity.ShareRemoving, stepOffloaded, stepForgotten, true, entity.TagOffloaded},
		{"i", entity.ShareRemoving, stepForgotten, stepThree, true, entity.TagServing},
		{"j", entity.ShareRemoving, none, stepOffloaded, true, entity.TagServing},
	} {
		sh := namedShare(t, s, c.name)

		if c.state != entity.ShareEnabled {
			if err := s.ControlPlane().DeleteShare(ctx, sh.ID, entity.SnapshotsRefuse); err != nil {
				t.Fatal(err)
			}
		}

		if c.state == entity.ShareRetired {
			rows(t, s, shareKey(sh.ID, keys.SSt), encodeState(shareState{State: entity.ShareRetired}))
		}

		lower := uint64(sh.Shard)
		upper := lower + 1000 // above every shard the test's shares were given

		if c.second {
			rows(t, s, keys.Uint64(shardsPrefix(sh.ID), upper), encodeEmpty())
		}

		for shard, step := range map[uint64]removalStep{
			lower: c.lower,
			upper: c.upper,
		} {
			if step != none {
				rows(t, s, shardRemovalKey(shard), encodeRemovalStep(step))
			}
		}

		want[sh.ID] = entity.TagRecord{
			Tag:   sh.Tag,
			State: c.want,
		}
		names[sh.ID] = c.name
	}

	got, err := s.Lifecycle().Tags(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != len(want) {
		t.Errorf("Tags named %d shares, want %d", len(got), len(want))
	}

	for id, w := range want {
		if got[id] != w {
			t.Errorf("share %s: %+v, want %+v", names[id], got[id], w)
		}
	}
}

// RFC 16 §2.3.2: an rm row whose step does not decode fails Tags, rather than
// reading as no removal, which would leave the share's tag in its journal.
func TestTagsRefusesAMalformedRow(t *testing.T) {
	s := lifecycleStore(t, nil)
	sh := namedShare(t, s, "a")
	rows(t, s, shardRemovalKey(uint64(sh.Shard)), encodeRemovalStep(stepDone+1))

	if got, err := s.Lifecycle().Tags(context.Background()); err == nil {
		t.Errorf("Tags over an rm row at step %d: %v, want an error", stepDone+1, got)
	}
}

// RFC 16 §3.2: recording a journal again under another node or directory
// deletes its older index key, so each journal keeps one, and leaves every
// other journal's.
func TestRecordJournalMovesTheIndex(t *testing.T) {
	type place struct {
		node entity.NodeID
		dev  entity.JournalDir
	}

	ctx := context.Background()
	id, other := [16]byte{1}, [16]byte{9}
	n1, n2 := entity.NodeID{1}, entity.NodeID{2}

	for _, c := range []struct {
		name     string
		from, to place
	}{
		{"another node", place{n1, "/j"}, place{n2, "/j"}},
		{"another directory", place{n1, "/j"}, place{n1, "/j2"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := lifecycleStore(t, nil)
			beside := place{c.from.node, "/j9"} // another journal on the old node

			for _, r := range []struct {
				id [16]byte
				at place
			}{{id, c.from}, {other, beside}, {id, c.to}} {
				if err := s.RecordJournal(ctx, r.id, r.at.node, r.at.dev); err != nil {
					t.Fatal(err)
				}
			}

			for _, w := range []struct {
				at   place
				want [16]byte
			}{{c.from, [16]byte{}}, {c.to, id}, {beside, other}} {
				got, err := s.Lifecycle().JournalID(ctx, w.at.node, w.at.dev)
				if err != nil || got != w.want {
					t.Errorf(
						"JournalID(%x, %s): %x, %v; want %x",
						w.at.node[:1],
						w.at.dev,
						got,
						err,
						w.want,
					)
				}
			}

			if got := keysUnder(t, s, journalKey(id)); len(got) != 1 {
				t.Errorf("the journal has %d J‖ keys, want 1", len(got))
			}
		})
	}
}

// The values the lifecycle view decodes were written by another process, so
// each decoder accepts or refuses any bytes and never panics. An rm row is
// accepted exactly when a known step follows the codec's version byte; what
// follows the step belongs to the later steps and is ignored. A J‖journal or
// JD value that is accepted decodes to the same value once written again.
func FuzzLifecycleValues(f *testing.F) {
	for _, seed := range [][]byte{
		nil, {1}, {1, 0}, {1, 1}, {1, 2}, {1, 3}, {1, 4}, {1, 5}, {1, 6}, {2, 1},
		{1, 4, 0, 0, 0, 0, 0, 0, 0, 7}, // step 4 with a cursor after it
	} {
		f.Add(seed)
	}

	for _, v := range [][]byte{
		encodeJournal(journalRecord{Node: entity.NodeID{1}, Device: "/j"}),
		encodeJournalIndex([16]byte{2}),
	} {
		f.Add(v)
		f.Add(v[:len(v)/2])
		f.Add(append(bytes.Clone(v), 0))
	}

	f.Fuzz(func(t *testing.T, b []byte) {
		step, err := decodeRemovalStep(b)
		known := len(b) >= 2 && b[0] == v1 && b[1] >= byte(stepThree) && b[1] <= byte(stepDone)

		if (err == nil) != known {
			t.Errorf("rm row %x: %v, %v; want accepted %v", b, step, err, known)
		}

		if err == nil && step != removalStep(b[1]) {
			t.Errorf("rm row %x: step %d, want %d", b, step, b[1])
		}

		if rec, err := decodeJournal(b); err == nil {
			if again, err := decodeJournal(encodeJournal(rec)); err != nil || again != rec {
				t.Errorf("J‖journal %x: %+v, written again %+v, %v", b, rec, again, err)
			}
		}

		if id, err := decodeJournalIndex(b); err == nil {
			if again, err := decodeJournalIndex(encodeJournalIndex(id)); err != nil || again != id {
				t.Errorf("JD %x: %x, written again %x, %v", b, id, again, err)
			}
		}
	})
}
