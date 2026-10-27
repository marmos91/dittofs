package model

import (
	"context"
	"errors"
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
		n, err := Run(t.Context(), newPlayed(t), steps)
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
	// readAnswer, when set, replaces the answer of every read.
	readAnswer func(id journalv2.FileID, tag journalv2.ShareTag) error
	// reopenFails makes every crash and reopen fail to open the journal.
	reopenFails bool
}

func (s *spy) Share(i int) Handle { return spyHandle{s.played.Share(i), s, ShareTag(i)} }

func (s *spy) Crash(k CrashKind, arg uint64) error {
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

func (h spyHandle) Fill(ctx context.Context, id journalv2.FileID, off int64, p []byte, asOf, v journalv2.Version) error {
	h.s.fills = append(h.s.fills, asOf)
	return h.Handle.Fill(ctx, id, off, p, asOf, v)
}

func (h spyHandle) Offload(ctx context.Context, id journalv2.FileID, through journalv2.Version, limit, widen int64, fn func(context.Context, journalv2.Offer, func([]journalv2.Extent) error) error) error {
	return h.Handle.Offload(ctx, id, through, limit, widen, func(ctx context.Context, o journalv2.Offer, report func([]journalv2.Extent) error) error {
		return fn(ctx, o, func(es []journalv2.Extent) error {
			err := report(es)
			h.s.reported = append(h.s.reported, err)
			return err
		})
	})
}

func runText(t *testing.T, tgt Target, text string) error {
	t.Helper()
	steps, err := ParseCorpus([]byte(text))
	ok(t, err)
	_, err = Run(t.Context(), tgt, steps)
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
