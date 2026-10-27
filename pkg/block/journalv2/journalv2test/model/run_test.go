package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// A journal that answers every call as the RFC allows, and keeps any state a
// crash allows, runs every generated sequence to the end: the runner drives
// the model only where it can follow.
func TestRunFollowsAnHonestJournal(t *testing.T) {
	for seed := range uint64(5) {
		steps := Generate(seed, 200)
		n, err := runSteps(t.Context(), newPlayed(t), steps)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if n != len(steps) {
			t.Fatalf("seed %d: ran %d of %d steps", seed, n, len(steps))
		}
	}
}

// spy is a played journal that records what the runner asked of it and can
// answer reads and reopens wrongly.
type spy struct {
	*played
	reads    []journalv2.Version // the change version each read answered
	fills    []journalv2.Version // the asOf each fill was made with
	reported []error             // what each report answered
	events   []string            // writes, reports, syncs, settles and crashes, in order
	written  []error             // what each write answered
	offering bool                // an offload's callback is running
	// readAnswer, when set, replaces the answer of every read.
	readAnswer func(id journalv2.FileID, tag journalv2.ShareTag) error
	// reopenFails makes every crash and reopen fail to open the journal.
	reopenFails bool
}

func (s *spy) Share(i int) Handle { return spyHandle{s.played.Share(i), s, tagOf(i)} }

func (s *spy) Crash(k CrashKind, arg uint64) error {
	s.events = append(s.events, "crash")
	if s.reopenFails {
		return ReopenError{errors.New("segment 7 does not verify")}
	}
	return s.played.Crash(k, arg)
}

type spyHandle struct {
	Handle
	s   *spy
	tag journalv2.ShareTag
}

func (h spyHandle) ReadAt(ctx context.Context, id journalv2.FileID, off int64, p []byte) (int, []journalv2.Extent, []journalv2.Held, journalv2.Version, error) {
	if h.s.readAnswer != nil {
		return len(p), nil, nil, journalv2.Version{}, h.s.readAnswer(id, h.tag)
	}
	n, missing, held, asOf, err := h.Handle.ReadAt(ctx, id, off, p)
	h.s.reads = append(h.s.reads, asOf)
	return n, missing, held, asOf, err
}

func (h spyHandle) WriteAt(ctx context.Context, id journalv2.FileID, off int64, p []byte, mtime int64) (journalv2.Version, error) {
	e := "write"
	if h.s.offering {
		e = "write during offer"
	}
	h.s.events = append(h.s.events, e)
	v, err := h.Handle.WriteAt(ctx, id, off, p, mtime)
	h.s.written = append(h.s.written, err)
	return v, err
}

func (h spyHandle) Sync(ctx context.Context, ids ...journalv2.FileID) error {
	h.s.events = append(h.s.events, "sync")
	return h.Handle.Sync(ctx, ids...)
}

func (h spyHandle) Settle(id journalv2.FileID, v journalv2.Version) {
	h.s.events = append(h.s.events, fmt.Sprintf("settle %d", v.Counter))
	h.Handle.Settle(id, v)
}

func (h spyHandle) Fill(ctx context.Context, id journalv2.FileID, off int64, p []byte, asOf, v journalv2.Version) error {
	h.s.fills = append(h.s.fills, asOf)
	return h.Handle.Fill(ctx, id, off, p, asOf, v)
}

func (h spyHandle) Offload(ctx context.Context, id journalv2.FileID, through journalv2.Version, limit, widen int64, fn func(context.Context, journalv2.Offer, func([]journalv2.Extent) error) error) error {
	return h.Handle.Offload(ctx, id, through, limit, widen, func(ctx context.Context, o journalv2.Offer, report func([]journalv2.Extent) error) error {
		h.s.offering = true
		defer func() { h.s.offering = false }()
		return fn(ctx, o, func(es []journalv2.Extent) error {
			err := report(es)
			h.s.reported = append(h.s.reported, err)
			h.s.events = append(h.s.events, "report")
			return err
		})
	})
}

func runText(t *testing.T, tgt Target, text string) error {
	t.Helper()
	steps, err := parseCorpus([]byte(text))
	ok(t, err)
	_, err = runSteps(t.Context(), tgt, steps)
	return err
}

// A fill is made as of the last read of the file, and a stale fill as of the
// read before it.
func TestFillsAreMadeAsOfTheReadsBeforeThem(t *testing.T) {
	s := &spy{played: newPlayed(t)}
	ok(t, runText(t, s, `
write len=8
offload
release
read len=8
write off=2 len=2 b=9
read len=8
fill
stalefill
`))
	if len(s.reads) != 2 || len(s.fills) != 2 {
		t.Fatalf("%d reads and %d fills, want 2 and 2", len(s.reads), len(s.fills))
	}
	if s.reads[0] == s.reads[1] {
		t.Fatalf("both reads answered %v; the write between them changed nothing", s.reads[0])
	}
	if s.fills[0] != s.reads[1] || s.fills[1] != s.reads[0] {
		t.Fatalf("fills made as of %v, want %v then %v", s.fills, s.reads[1], s.reads[0])
	}
}

// An offload whose pick reports an extent it was not offered has that report
// refused.
func TestAnOffloadCanReportWhatItWasNotOffered(t *testing.T) {
	s := &spy{played: newPlayed(t)}
	ok(t, runText(t, s, "write off=4 len=8\noffload pick=6\n"))
	if len(s.reported) != 1 || !errors.Is(s.reported[0], journalv2.ErrNotOffered) {
		t.Fatalf("reports answered %v, want one ErrNotOffered", s.reported)
	}
}

// A read is never refused through the share holding the file, and is refused
// through another share while that share holds it.
func TestReadsAreChecked(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer func(id journalv2.FileID, tag journalv2.ShareTag) error
	}{
		{"an error through the share holding the file", func(journalv2.FileID, journalv2.ShareTag) error {
			return errors.New("EIO")
		}},
		{"served through another share", func(journalv2.FileID, journalv2.ShareTag) error { return nil }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &spy{played: newPlayed(t), readAnswer: c.answer}
			err := runText(t, s, "write len=4\nread s=1 len=4\nread len=4\n")
			if !errors.Is(err, ErrDiverged) {
				t.Fatalf("want a divergence, got %v", err)
			}
		})
	}
}

// A journal that does not open again after a crash has diverged: recovery
// refused what a crash may leave.
func TestAJournalThatDoesNotReopenHasDiverged(t *testing.T) {
	s := &spy{played: newPlayed(t), reopenFails: true}
	err := runText(t, s, "write len=4\ncrash\n")
	if !errors.Is(err, ErrDiverged) || !strings.Contains(err.Error(), "segment 7 does not verify") {
		t.Fatalf("want a divergence naming the open's error, got %v", err)
	}
}

// An offload whose pick asks for it writes to the file while the callback
// runs, before it reports.
func TestAnOffloadCanWriteDuringTheOffer(t *testing.T) {
	s := &spy{played: newPlayed(t)}
	ok(t, runText(t, s, "write len=32\noffload off=8 len=8 pick=1\n"))
	if want := []string{"write", "write during offer", "report"}; !slices.Equal(s.events, want) {
		t.Fatalf("events %v, want %v", s.events, want)
	}
}

// A file with more unsynced records than the crash search takes is synced
// before the crash, so the crash leaves the search a size it can take.
func TestAFileIsSyncedBeforeACrashTooLargeToSearch(t *testing.T) {
	s := &spy{played: newPlayed(t)}
	text := ""
	for i := range crashSearchable + 1 {
		text += fmt.Sprintf("write off=%d len=1\n", 2*i)
	}
	ok(t, runText(t, s, text+"crash\n"))
	if n := len(s.events); n < 2 || s.events[n-2] != "sync" || s.events[n-1] != "crash" {
		t.Fatalf("events end %v, want a sync then the crash", s.events)
	}
}

// A settle reaches a share of every version issued that its pick chooses:
// none at pick 0, all at pick 255.
func TestASettleReachesTheShareItsPickChooses(t *testing.T) {
	s := &spy{played: newPlayed(t)}
	ok(t, runText(t, s, "write len=1\nwrite len=1\nwrite len=1\nwrite len=1\nsettle pick=0\nsettle pick=128\nsettle pick=255\n"))
	settles := slices.DeleteFunc(slices.Clone(s.events), func(e string) bool { return e == "write" })
	if want := []string{"settle 0", "settle 2", "settle 4"}; !slices.Equal(settles, want) {
		t.Fatalf("settled %v, want %v", settles, want)
	}
}

// A crash that would leave one file's settled markers more than the search
// takes does nothing, and is counted, so a sequence that lost its crashes
// this way shows it.
func TestACrashTooLargeToSearchIsCounted(t *testing.T) {
	text := ""
	for i := range crashSearchable + 1 {
		text += fmt.Sprintf("deallocate off=%d len=1\n", 2*i)
	}
	steps, err := parseCorpus([]byte(text + "settle pick=255\ncrash\n"))
	ok(t, err)
	ran, skipped, err := runCounting(t.Context(), newPlayed(t), steps)
	ok(t, err)
	if ran != len(steps) || skipped != 1 {
		t.Fatalf("ran %d of %d steps, skipped %d crashes, want all and 1", ran, len(steps), skipped)
	}
}

// The played journal keeps a file's tag while its records are on disk, as a
// journal must: a file whose content was released is still refused through
// another share until a repack has run, and is claimed by it after.
func TestThePlayedJournalKeepsATagUntilRepack(t *testing.T) {
	s := &spy{played: newPlayed(t)}
	ok(t, runText(t, s, "write len=4\noffload\nrelease\nwrite s=1 len=4\nrepack\nwrite s=1 len=4\n"))
	if len(s.written) != 3 || s.written[0] != nil || !errors.Is(s.written[1], journalv2.ErrOtherShare) || s.written[2] != nil {
		t.Fatalf("writes answered %v, want nil, ErrOtherShare, nil", s.written)
	}
}
