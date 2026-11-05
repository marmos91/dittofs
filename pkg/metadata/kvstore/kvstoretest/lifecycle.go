package kvstoretest

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

func wantLimits(t *testing.T, s *kvstore.Store, want map[entity.ShareID]entity.JournalVersion) {
	t.Helper()
	shares := make([]entity.ShareID, 0, len(want))

	for id := range want {
		shares = append(shares, id)
	}

	got, err := s.Lifecycle().VersionLimits(ctx, shares)
	if err != nil {
		t.Fatalf("VersionLimits: %v", err)
	}

	for id, v := range want {
		if got[id] != v {
			t.Errorf("share %v: limit %d, want %d", id, got[id].Counter(), v.Counter())
		}
	}
}

// RFC 8 §2.7: every write of a share's version limit writes the larger of
// the value it reads and its own, so a raise to a lower value leaves the
// stored one. A share with no limit written reads zero.
func testRaiseVersionLimitsNeverLowers(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	a, b := share("a", "/a"), share("b", "/b")
	must(t, s.ControlPlane().PutShare(ctx, a))
	must(t, s.ControlPlane().PutShare(ctx, b))
	l := s.Lifecycle()

	for _, n := range []uint64{100, 50} {
		err := l.RaiseVersionLimits(
			ctx,
			map[entity.ShareID]entity.JournalVersion{a.ID: entity.VersionOf(n)},
		)
		must(t, err)
	}

	wantLimits(t, s, map[entity.ShareID]entity.JournalVersion{
		a.ID: entity.VersionOf(100),
		b.ID: {},
	})
}

// RFC 8 §2.7: the read a limit's raise compares against is tracked, so a
// lower raise that read the limit before a higher one committed conflicts
// and reads again, never writing over the higher.
func testRaiseVersionLimitsRace(t *testing.T, kvo Opener) {
	s, h := heldStore(t, kvo.backends()[0])
	a := share("a", "/a")
	must(t, s.ControlPlane().PutShare(ctx, a))
	l := s.Lifecycle()
	raise := func(n uint64) func() error {
		return func() error {
			return l.RaiseVersionLimits(
				ctx,
				map[entity.ShareID]entity.JournalVersion{a.ID: entity.VersionOf(n)},
			)
		}
	}

	outer, inner := race(h, raise(10), raise(20))
	must(t, inner)
	must(t, outer)

	wantLimits(t, s, map[entity.ShareID]entity.JournalVersion{a.ID: entity.VersionOf(20)})
}

// RFC 16 §3.2: a journal's identity is recorded under its node and device,
// so each node's device answers its own, a device with no record answers the
// zero identity, and the same directory under another node is another
// device.
func testJournalIDPerNodeAndDevice(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	l := s.Lifecycle()
	n1, n2 := entity.NodeID{1}, entity.NodeID{2}
	da, db := entity.JournalDir("/j/a"), entity.JournalDir("/j/b")
	idA, idB, idC := [16]byte{0xA}, [16]byte{0xB}, [16]byte{0xC}

	if got, err := l.JournalID(ctx, n1, da); err != nil || got != ([16]byte{}) {
		t.Fatalf("JournalID with nothing recorded: %x, %v; want the zero identity", got, err)
	}

	must(t, s.RecordJournal(ctx, idA, n1, da))
	must(t, s.RecordJournal(ctx, idB, n1, db))
	must(t, s.RecordJournal(ctx, idC, n2, da))

	for _, c := range []struct {
		node   entity.NodeID
		device entity.JournalDir
		want   [16]byte
	}{
		{n1, da, idA},
		{n1, db, idB},
		{n2, da, idC},
		{n2, db, [16]byte{}},
	} {
		if got, err := l.JournalID(ctx, c.node, c.device); err != nil || got != c.want {
			t.Errorf("JournalID(%x, %s): %x, %v; want %x", c.node[:1], c.device, got, err, c.want)
		}
	}
}

// RFC 16 §3.2: recording a device again, as its re-initialisation does,
// leaves the device's lookup answering the newer identity.
func testRecordJournalAgain(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	node, dir := entity.NodeID{1}, entity.JournalDir("/j")
	first, second := [16]byte{1}, [16]byte{2}

	must(t, s.RecordJournal(ctx, first, node, dir))
	must(t, s.RecordJournal(ctx, second, node, dir))

	if got, err := s.Lifecycle().JournalID(ctx, node, dir); err != nil || got != second {
		t.Errorf("JournalID after a re-initialisation: %x, %v; want %x", got, err, second)
	}
}
