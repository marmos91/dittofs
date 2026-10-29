package openstate

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

const names = ChangeAddEntry | ChangeRemove | ChangeRename

func (f *fixture) watch(t *testing.T, who Caller, dir FileKey, recursive bool, m ChangeMask) WatchID {
	t.Helper()
	w, err := f.Watches().Watch(ctx, who, dir, recursive, m)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	return w
}

func (f *fixture) changed(dir FileKey, what ChangeMask, name string, nested bool) {
	f.Watches().Changed(ctx, dir, Change{What: what, Name: []byte(name), Nested: nested})
}

// handWatched runs what the timer runs on each directory whose watches
// handed changes since it last looked.
func (f *fixture) handWatched() {
	f.idx.Lock()
	dirs := slices.Collect(maps.Keys(f.watched))
	f.idx.Unlock()
	f.Table.handWatched(f.sink, dirs)
}

// reported is the names each Notify since the last take carried, one string
// per call, an enumerate as "*".
func (f *fixture) reported() []string {
	var out []string
	for _, n := range f.sink.takeNotes() {
		var s []string
		for _, ch := range n.ch {
			if ch.What == ChangeEnumerate {
				s = append(s, "*")
			} else {
				s = append(s, string(ch.Name))
			}
		}
		out = append(out, strings.Join(s, ","))
	}
	return out
}

// A watch reports the changes its filter names, below its directory only
// when recursive; its first change goes to the Sink at once and the ones
// after it wait for the timer, which lets the next go at once again after a
// pass that found nothing held. Unwatch is its client's alone, and ends what
// it reports.
func TestWatchReportsWhatItFilters(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, nfs("a", 1, entity.NewUser())), f.caller(t, nfs("b", 1, entity.NewUser()))
	dir := in(1)
	flat := f.watch(t, a, dir, false, names)
	deep := f.watch(t, b, dir, true, names|ChangeSize)

	f.changed(dir, ChangeAddEntry, "x", false)
	f.changed(dir, ChangeSize, "x", false)
	name := []byte("sub/y")
	f.Watches().Changed(ctx, dir, Change{What: ChangeRemove, Name: name, Nested: true})
	copy(name, "reuse") // the caller's buffer is its own again once Changed returns
	if got := f.reported(); !slices.Equal(got, []string{"x", "x"}) {
		t.Fatalf("reported at once %q, want each watch's first change", got)
	}
	f.handWatched()
	if got := f.reported(); !slices.Equal(got, []string{"x,sub/y"}) {
		t.Fatalf("reported by the timer %q, want only the recursive watch's held size and nested remove", got)
	}
	f.handWatched()
	f.changed(dir, ChangeRename, "z", false)
	if got := f.reported(); len(got) != 2 {
		t.Errorf("reported %q after a pass that found nothing held, want both watches at once", got)
	}

	if err := f.Watches().Unwatch(ctx, b, flat); !errors.Is(err, ErrNotYours) {
		t.Errorf("Unwatch of another client's watch: %v, want ErrNotYours", err)
	}
	if err := f.Watches().Unwatch(ctx, a, flat); err != nil {
		t.Fatal(err)
	}
	if err := f.Watches().Unwatch(ctx, a, flat); !errors.Is(err, ErrBadState) {
		t.Errorf("Unwatch again: %v, want ErrBadState", err)
	}
	f.handWatched()
	f.handWatched()
	f.reported()
	f.changed(dir, ChangeAddEntry, "w", false)
	if got := f.sink.takeNotes(); len(got) != 1 || got[0].w != deep {
		t.Errorf("after Unwatch %+v, want only the other watch", got)
	}
}

// A watch is never reclaimed: it goes with its shard's lost state, and in
// grace a new one is refused as other new state is.
func TestWatchInGrace(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfsAs("a", entity.RecoverStatus))
	f.mustOpen(t, a, in(1), OpenRequest{Access: rd})
	dir := in(1)
	w := f.watch(t, a, dir, false, names)
	if err := f.Recovery().ServeShard(ctx, f.fs.Share.ID, dir.Shard, instances.Add(1)); err != nil {
		t.Fatal(err)
	}
	f.changed(dir, ChangeAddEntry, "x", false)
	if got := f.reported(); len(got) != 0 {
		t.Errorf("a watch of a shard that lost its state reported %q", got)
	}
	if err := f.Watches().Unwatch(ctx, a, w); !errors.Is(err, ErrStaleState) {
		t.Errorf("Unwatch of a watch its shard lost: %v, want ErrStaleState", err)
	}
	g := f.restart(t)
	g.renew(t, a)
	if _, err := g.Watches().Watch(ctx, a, in(1), false, names); !errors.Is(err, ErrGrace) {
		t.Errorf("Watch in grace: %v, want ErrGrace", err)
	}
}

// A client holds at most 1,024 watches here, and a watch whose client does
// not read holds at most 64 changes or 64 KiB of them: a burst past either
// leaves one ChangeEnumerate.
func TestWatchesAreBounded(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("a", 1, entity.NewUser()))
	dir := in(1)
	for range f.caps[cappedWatches] {
		f.watch(t, a, in(1), false, names)
	}
	if _, err := f.Watches().Watch(ctx, a, dir, false, names); delayedFor(err) != DelayResources {
		t.Fatalf("a watch past the cap: %v, want ErrDelay (DelayResources)", err)
	}
	b := f.caller(t, nfs("b", 1, entity.NewUser()))
	w := f.watch(t, b, dir, false, names)
	held := func() []Change {
		var p []Change
		if err := f.step(dir.ID, func(e *entry) error { p = slices.Clone(e.watches[w.Token].pending); return nil }); err != nil {
			t.Fatal(err)
		}
		return p
	}

	f.changed(dir, ChangeAddEntry, "first", false)
	for range maxChanges {
		f.changed(dir, ChangeAddEntry, "n", false)
	}
	if p := held(); len(p) != maxChanges || p[0].What == ChangeEnumerate {
		t.Fatalf("held %d changes, want the %d after the first, in detail", len(p), maxChanges)
	}
	f.changed(dir, ChangeAddEntry, "n", false)
	if p := held(); len(p) != 1 || p[0].What != ChangeEnumerate {
		t.Fatalf("held %+v past the count, want one ChangeEnumerate", p)
	}
	f.handWatched()

	long := strings.Repeat("l", 4096-16)
	f.handWatched()
	f.reported()
	f.changed(dir, ChangeAddEntry, "first", false)
	for range maxChangeBytes / 4096 {
		f.changed(dir, ChangeAddEntry, long, false)
	}
	if p := held(); len(p) != maxChangeBytes/4096 || p[0].What == ChangeEnumerate {
		t.Fatalf("held %d changes of 64 KiB, want each in detail", len(p))
	}
	f.changed(dir, ChangeAddEntry, long, false)
	if p := held(); len(p) != 1 || p[0].What != ChangeEnumerate {
		t.Fatalf("held %d changes past 64 KiB, want one ChangeEnumerate", len(p))
	}
	for range 100_000 {
		f.changed(dir, ChangeAddEntry, long, false)
	}
	if p := held(); len(p) != 1 || p[0].What != ChangeEnumerate {
		t.Fatalf("held %d changes after a burst, want one ChangeEnumerate", len(p))
	}
	if got := f.reported(); !slices.Equal(got, []string{"first"}) {
		t.Errorf("reported during the burst %q, want only its first change", got)
	}
	f.sweep(ctx)
	f.Stop() // waits for the steps the sweep started
	if got := f.reported(); !slices.Equal(got, []string{"*"}) {
		t.Errorf("the timer reported %q, want one ChangeEnumerate", got)
	}
}
