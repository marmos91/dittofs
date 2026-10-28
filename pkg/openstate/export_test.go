package openstate

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// seeded is a Deps.Rand drawn from a fixed seed, so a case mints the same
// tokens on every run; the table reads it from many steps at once.
type seeded struct {
	mu sync.Mutex
	r  *rand.ChaCha8
}

func newSeeded(name string) *seeded {
	var seed [32]byte
	copy(seed[:], name)
	return &seeded{r: rand.NewChaCha8(seed)}
}

func (s *seeded) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r.Read(p)
}

// indexes is what the table keeps beside its files' entries, each of it
// derived from them.
type indexes struct {
	refs    map[refKey]holder
	byID    map[entity.OpenID]refKey
	via     map[entity.FileID]int
	granted map[entity.ClientID]int
	keys    map[clientKey]keyUse
	states  map[entity.FileID]int
}

// kept is the indexes as the table holds them, and each entry's count of its
// state.
func (t *Table) kept() indexes {
	x := indexes{refs: map[refKey]holder{}, byID: map[entity.OpenID]refKey{}, via: map[entity.FileID]int{},
		granted: map[entity.ClientID]int{}, keys: map[clientKey]keyUse{}, states: map[entity.FileID]int{}}
	t.idx.Lock()
	for k, v := range t.refs {
		x.refs[k] = v
	}
	for k, v := range t.byID {
		x.byID[k] = v
	}
	for k, v := range t.via {
		x.via[k] = v
	}
	for k, v := range t.granted {
		x.granted[k] = v
	}
	for k, v := range t.keys {
		x.keys[k] = v
	}
	t.idx.Unlock()
	t.entries(func(e *entry) { x.states[e.file] = e.states })
	return x
}

// rebuild derives the indexes from the files' entries alone.
func (t *Table) rebuild() indexes {
	x := indexes{refs: map[refKey]holder{}, byID: map[entity.OpenID]refKey{}, via: map[entity.FileID]int{},
		granted: map[entity.ClientID]int{}, keys: map[clientKey]keyUse{}, states: map[entity.FileID]int{}}
	t.entries(func(e *entry) {
		for tok, o := range e.opens {
			k := refKey{o.Ref.Shard, tok}
			x.refs[k] = holder{StateOpen, e.file, o.Client, o.Ref.Seq}
			if o.ID != 0 {
				x.byID[o.ID] = k
			}
			if !o.Via.IsZero() {
				x.via[o.Via]++
			}
			if o.lease != (LeaseKey{}) {
				u := x.keys[clientKey{o.Client, o.lease}]
				u.file, u.opens = e.file, u.opens+1
				x.keys[clientKey{o.Client, o.lease}] = u
			}
		}
		for tok, ls := range e.lstates {
			x.refs[refKey{ls.ref.Shard, tok}] = holder{StateLock, e.file, ls.owner.Client, ls.ref.Seq}
		}
		for tok, g := range e.grants {
			x.refs[refKey{g.id.Shard, tok}] = holder{StateGrant, e.file, g.client, 1}
			if g.counted {
				x.granted[g.client]++
			}
		}
		if n := len(e.opens) + len(e.lstates) + len(e.grants); n > 0 {
			x.states[e.file] = n
		}
	})
	return x
}

// entries runs fn on every file's entry, under its step.
func (t *Table) entries(fn func(e *entry)) {
	for i := range t.files {
		fs := &t.files[i]
		fs.mu.Lock()
		for _, e := range fs.entries {
			e.mu.Lock()
			fn(e)
			e.mu.Unlock()
		}
		fs.mu.Unlock()
	}
}

// check reports the first of the table's invariants that does not hold: the
// indexes are those its entries derive; no lock is held under an open that is
// closed; every durable open matches its record; and no revoked reference
// names state still held.
func (t *Table) check() error {
	k, b := t.kept(), t.rebuild()
	for _, d := range []struct {
		name       string
		kept, want any
	}{{"refs", k.refs, b.refs}, {"byID", k.byID, b.byID}, {"via", k.via, b.via}, {"granted", k.granted, b.granted}, {"keys", k.keys, b.keys}, {"states", k.states, b.states}} {
		if !reflect.DeepEqual(d.kept, d.want) {
			return fmt.Errorf("the index %s kept, %v, is not the one the entries derive, %v", d.name, d.kept, d.want)
		}
	}
	var err error
	t.entries(func(e *entry) {
		for _, ls := range e.lstates {
			if ls.open != nil && e.opens[ls.open.Ref.Token] != ls.open && err == nil {
				err = fmt.Errorf("a lock state of %v is held under a closed open", e.file)
			}
		}
		for _, o := range e.opens {
			if !o.durable || o.unsaved || err != nil {
				continue
			}
			rec, gerr := t.d.Records.Opens().Get(ctx, o.share, o.opened(), o.ID)
			want := t.recordOf(e, o)
			// The reference naming it is minted afresh at each install.
			rec.Nonce, rec.StateSeq = want.Nonce, want.StateSeq
			if !o.persistent() {
				// Only a persistent open's record is written again as the open
				// changes; any other durable record names its holder.
				rec = entity.OpenRecord{ID: rec.ID, Client: rec.Client, File: rec.File, Kind: rec.Kind}
				want = entity.OpenRecord{ID: want.ID, Client: want.Client, File: want.File, Kind: want.Kind}
			}
			if gerr != nil || !reflect.DeepEqual(rec, want) {
				err = fmt.Errorf("durable open %d is stored as %+v (%v), held as %+v", o.ID, rec, gerr, want)
			}
		}
	})
	if err != nil {
		return err
	}
	t.idx.Lock()
	defer t.idx.Unlock()
	for k := range t.revoked {
		if _, held := t.refs[k]; held {
			return fmt.Errorf("revoked reference %+v names state still held", k)
		}
	}
	return nil
}

// unchecked is the tables a case changed past their calls, minting state
// no entry holds or setting a count, whose invariants check does not judge.
var unchecked sync.Map

// checked fails t if tab's invariants do not hold once the case is done,
// unless tab was stopped first, as a restart stops it.
func checked(t *testing.T, tab *Table) {
	t.Cleanup(func() {
		tab.cmu.Lock()
		stopped := tab.stopped
		tab.cmu.Unlock()
		if _, raw := unchecked.Load(tab); stopped || raw || t.Failed() {
			return
		}
		if err := tab.check(); err != nil {
			t.Error(err)
		}
	})
}
