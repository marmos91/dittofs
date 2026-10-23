package openstate

import (
	"crypto/rand"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

func newTable(t *testing.T) *Table {
	t.Helper()
	return newFixture(t).Table
}

// entryOf is file f's entry, or nil if it has none.
func (t *Table) entryOf(f entity.FileID) *entry {
	fs := &t.files[f[0]]
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.entries[f]
}

func mintIn(t *testing.T, tab *Table, f entity.FileID, c entity.ClientID, s ShardID) StateRef {
	t.Helper()
	var ref StateRef
	if err := tab.step(f, func(e *entry) (err error) {
		ref, err = tab.mint(e, c, s, StateOpen)
		return err
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
	tab := newTable(t)
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

// A reference resolves only for its own client and, when the request names a
// file, only for its own file.
func TestResolveChecksClientAndFile(t *testing.T) {
	tab := newTable(t)
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
	tab := newTable(t)
	f := entity.NewFileID()
	a, b := mintIn(t, tab, f, 1, 0), mintIn(t, tab, f, 1, 0)
	releaseIn(t, tab, f, a)
	if tab.entryOf(f) == nil {
		t.Fatal("entry freed while it still holds state")
	}
	releaseIn(t, tab, f, b)
	if tab.entryOf(f) != nil {
		t.Fatal("entry kept after its last state was released")
	}

	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			for range 100 {
				var ref StateRef
				_ = tab.step(f, func(e *entry) (err error) { ref, err = tab.mint(e, 1, 0, StateOpen); return err })
				_ = tab.step(f, func(e *entry) error { tab.release(e, ref); return nil })
			}
		})
	}
	wg.Wait()
	if tab.entryOf(f) != nil || len(tab.refs) != 0 {
		t.Fatalf("after concurrent steps: entry %v, %d references; want none", tab.entryOf(f), len(tab.refs))
	}
}

// Every random bit of a token is read from Deps.Rand, and a failed read
// mints nothing: no reference is held and the file keeps no entry.
func TestMintReadsDepsRand(t *testing.T) {
	tab := newTable(t)
	tab.d.Rand = io.LimitReader(rand.Reader, 8)
	f := entity.NewFileID()
	mintIn(t, tab, f, 1, 0)
	g := entity.NewFileID()
	if err := tab.step(g, func(e *entry) error { _, err := tab.mint(e, 1, 0, StateOpen); return err }); err == nil {
		t.Fatal("a mint past the end of Deps.Rand succeeded")
	}
	if tab.entryOf(g) != nil || len(tab.refs) != 1 {
		t.Fatalf("after a failed mint: entry %v, %d references; want none and the first one", tab.entryOf(g), len(tab.refs))
	}
}

// A reference's sequence is judged against the state's: zero and the current
// one pass, an older one is ErrOldState, a newer one ErrBadState, across the
// wrap from the highest value to 1 too.
func TestStateRefSequence(t *testing.T) {
	tab := newTable(t)
	f := entity.NewFileID()
	ref := mintIn(t, tab, f, 1, 0)
	var now uint32
	if err := tab.step(f, func(e *entry) (err error) { now, err = tab.advance(e, ref); return err }); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		seq  uint32
		want error
	}{{0, nil}, {now, nil}, {now - 1, ErrOldState}, {now + 1, ErrBadState}} {
		r := ref
		r.Seq = tc.seq
		if _, err := tab.resolve(1, f, r); !errors.Is(err, tc.want) {
			t.Errorf("seq %d against %d: %v, want %v", tc.seq, now, err, tc.want)
		}
	}

	k := refKey{ref.Shard, ref.Token}
	h := tab.refs[k]
	h.seq = ^uint32(0)
	tab.refs[k] = h
	if err := tab.step(f, func(e *entry) (err error) { now, err = tab.advance(e, ref); return err }); err != nil {
		t.Fatal(err)
	}
	if now != 1 {
		t.Fatalf("advancing past the highest sequence gave %d, want 1", now)
	}
	r := ref
	r.Seq = ^uint32(0)
	if _, err := tab.resolve(1, f, r); !errors.Is(err, ErrOldState) {
		t.Errorf("the sequence before the wrap: %v, want ErrOldState", err)
	}
}

// Advancing a released reference is ErrBadState and adds nothing to the index.
func TestAdvanceAddsNoRecord(t *testing.T) {
	tab := newTable(t)
	f := entity.NewFileID()
	ref := mintIn(t, tab, f, 1, 0)
	releaseIn(t, tab, f, ref)
	if err := tab.step(f, func(e *entry) error { _, err := tab.advance(e, ref); return err }); !errors.Is(err, ErrBadState) {
		t.Errorf("advancing a released reference: %v, want ErrBadState", err)
	}
	if len(tab.refs) != 0 {
		t.Fatalf("after advancing a released reference: %d references, want none", len(tab.refs))
	}
}

// A step on one file cannot release or advance another file's state: the
// other file's reference still resolves and both entries keep true counts.
func TestStepTouchesOnlyItsOwnFile(t *testing.T) {
	tab := newTable(t)
	f, g := entity.NewFileID(), entity.NewFileID()
	ref := mintIn(t, tab, f, 1, 0)
	releaseIn(t, tab, g, ref)
	if _, err := tab.resolve(1, f, ref); err != nil {
		t.Fatalf("after another file's step released it: %v", err)
	}
	if err := tab.step(g, func(e *entry) error { _, err := tab.advance(e, ref); return err }); !errors.Is(err, ErrBadState) {
		t.Errorf("advancing it under another file's step: %v, want ErrBadState", err)
	}
	if tab.entryOf(g) != nil {
		t.Fatal("the other file kept an entry")
	}
	releaseIn(t, tab, f, ref)
	if tab.entryOf(f) != nil {
		t.Fatal("the file's entry outlived its last state")
	}
}
