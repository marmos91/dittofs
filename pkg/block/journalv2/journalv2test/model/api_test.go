package model

import (
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// HandleView is the View of a real journal: a change to the handle's methods
// that breaks it breaks the build here.
var _ View = HandleView{}

// An error says whose it is: the journal did what the RFC does not allow, or
// the model was called in a way it cannot take.
func TestErrorsSayWhetherTheJournalOrTheCallerIsWrong(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 4), 0, ver(1), nil))

	diverged := m.WriteAt(shareA, fid(2), 0, fill('A', 4), 0, ver(1), nil)
	if !errors.Is(diverged, ErrDiverged) || errors.Is(diverged, ErrMisuse) {
		t.Fatalf("a repeated version is the journal's fault: %v", diverged)
	}
	if !strings.Contains(diverged.Error(), "assigned version v1") {
		t.Fatalf("message %q does not name the version as v1", diverged)
	}

	ok(t, m.Crash())
	misuse := m.WriteAt(shareA, fid(2), 0, fill('A', 4), 0, ver(2), nil)
	if !errors.Is(misuse, ErrMisuse) || errors.Is(misuse, ErrDiverged) {
		t.Fatalf("a call between Crash and Reopen is the caller's: %v", misuse)
	}
}

// A failure names the bytes, the version and what was broken, in the
// caller's terms.
func TestAFailureNamesTheBytesTheVersionAndTheRule(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 4), 0, ver(1), nil))
	ok(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))
	err := m.Compare(shareA, modelView{New(), shareA})
	diverged(t, err)
	for _, want := range []string{"[0,4)", "v1", "synced", "lost"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "{0 1}") {
		t.Errorf("message %q prints a raw version", err)
	}
}

// Since is compared from every version the file has, not only from the start:
// a journal that yields changes at from itself is refused.
func TestSinceIsComparedFromEveryVersion(t *testing.T) {
	m, j := New(), New()
	script(t, m, j)
	h := modelView{j, shareA}
	inclusive := planted{modelView: h, since: func(id journalv2.FileID, from journalv2.Version) iter.Seq2[journalv2.Change, error] {
		if from.Counter > 0 {
			from.Counter--
		}
		return h.Since(id, from)
	}}
	diverged(t, m.Compare(shareA, inclusive))
}

// Offload calls its callback while dirty content at or below through is held:
// returning without it is refused. Once called, it returns what the callback
// returned.
func TestOffloadCallsItsCallbackAndReturnsWhatItReturned(t *testing.T) {
	m := New()
	diverged(t, m.EndOffer(shareA, fid(1), ver(1), nil, journalv2.ErrCorrupt))
	ok(t, m.EndOffer(shareA, fid(1), ver(1), nil, nil))

	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 4), 0, ver(1), nil))
	diverged(t, m.EndOffer(shareA, fid(1), ver(1), nil, nil))
	ok(t, m.EndOffer(shareA, fid(1), journalv2.Version{}, nil, nil))

	upload := errors.New("upload failed")
	ok(t, m.Offer(shareA, fid(1), ver(1), 0, 0, offerOf(m, fid(1), 1, 1, dirty(0, 4, 1))))
	diverged(t, m.EndOffer(shareA, fid(1), ver(1), upload, nil))
	ok(t, m.Offer(shareA, fid(1), ver(1), 0, 0, offerOf(m, fid(1), 1, 1, dirty(0, 4, 1))))
	ok(t, m.EndOffer(shareA, fid(1), ver(1), upload, upload))
}

// Unresolved names the files a crash left unknown, which Compare must see
// before anything else is called on them.
func TestUnresolvedNamesTheFilesACrashLeftUnknown(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 4), 0, ver(1), nil))
	ok(t, m.WriteAt(shareB, fid(2), 0, fill('B', 4), 0, ver(2), nil))
	ok(t, m.Sync(shareB, []journalv2.FileID{fid(2)}, nil))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))
	if got := m.Unresolved(); len(got) != 1 || got[0] != fid(1) {
		t.Fatalf("Unresolved = %s, want only file 1", fmtFiles(got))
	}
	ok(t, m.Compare(shareA, modelView{New(), shareA}))
	if got := m.Unresolved(); len(got) != 0 {
		t.Fatalf("Unresolved = %s after Compare", fmtFiles(got))
	}
}

// Since is compared from every version a file has, up to the bound: a journal
// wrong from one version only, among twenty, is refused.
func TestSinceIsComparedFromEachOfManyVersions(t *testing.T) {
	m, j := New(), New()
	for _, x := range []*Model{m, j} {
		for i := range 20 {
			ok(t, x.WriteAt(shareA, fid(1), int64(i), []byte{byte('a' + i)}, 0, ver(uint64(i)+1), nil))
		}
	}
	h := modelView{j, shareA}
	wrongAtFive := planted{modelView: h, since: func(id journalv2.FileID, from journalv2.Version) iter.Seq2[journalv2.Change, error] {
		if from == ver(5) {
			from = ver(4)
		}
		return h.Since(id, from)
	}}
	diverged(t, m.Compare(shareA, wrongAtFive))
}

// A journal whose answer leaves more in doubt than the crash did is wrong, not
// misused: serving a record's bytes in alternation with bytes no record holds
// splits it into more undecided pieces than the search takes.
func TestAnAnswerThatFragmentsTheSearchIsTheJournals(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(2), 0, fill('x', 1), 0, ver(5), nil))
	ok(t, m.Fill(shareA, fid(1), 0, fill('A', 40), ver(5), ver(5), nil))
	ok(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
	ok(t, m.Release(shareA, fid(1), []journalv2.Extent{ext(0, 40)}, nil))
	ok(t, m.Fill(shareA, fid(1), 0, fill('A', 40), ver(5), ver(5), nil))
	j := clone(m)
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))

	h := modelView{j, shareA}
	alternating := planted{modelView: h, readAt: func(id journalv2.FileID, off int64, p []byte) (int, []journalv2.Extent, []journalv2.Held, journalv2.Version, error) {
		n, mi, he, as, err := h.ReadAt(id, off, p)
		for i := range p {
			if (off+int64(i))%2 == 1 {
				p[i] = 'B'
			}
		}
		return n, mi, he, as, err
	}}
	diverged(t, m.Compare(shareA, alternating))
}
