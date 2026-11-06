package engine

import (
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The peak is the highest count of any one second of the last 60: a second
// 59 seconds ago still counts, one 60 seconds ago no longer does, and a
// bucket reused for a new second starts from zero.
func TestPeakOver60Seconds(t *testing.T) {
	var r rateBuckets

	for range 100 {
		r.add(0)
	}

	for sec := uint64(1); sec < 59; sec++ {
		r.add(sec)
	}

	if got := r.add(59); got != 100 {
		t.Errorf("peak at second 59 = %d, want 100 from second 0", got)
	}

	if got := r.add(60); got != 1 {
		t.Errorf("peak at second 60 = %d, want 1: second 0 has left the window", got)
	}
}

// The versions left after v are the ones from v+1 to limit-1, and the raise
// is due only when they are fewer than the trigger.
func TestHeadroomBoundary(t *testing.T) {
	const block = 16 // half a block, 8, is the trigger when no version was drawn

	for _, c := range []struct {
		v    uint64
		left uint64
		due  bool
	}{
		{991, 8, false},
		{992, 7, true},
		{999, 0, true},
		{1000, 0, true},
	} {
		left := headroom(1000, c.v)

		if left != c.left {
			t.Errorf("headroom(1000, %d) = %d, want %d", c.v, left, c.left)
		}

		if got := due(left, 0, 32*time.Second, block); got != c.due {
			t.Errorf("due with %d left = %v, want %v", left, got, c.due)
		}
	}
}

// limitJournal is a journal that reports its next version and records the
// limits it is given; every other call is the nil Journal's.
type limitJournal struct {
	Journal
	next  uint64
	marks []journal.Version
}

func (j *limitJournal) Stats() journal.Stats {
	return journal.Stats{
		NextVersion: journal.Version{
			Counter: j.next,
		},
	}
}

func (j *limitJournal) SetVersionLimit(mark journal.Version) error {
	j.marks = append(j.marks, mark)

	return nil
}

// limitStore accepts every version limit write; every other call is the nil
// Lifecycle's.
type limitStore struct {
	Lifecycle
}

func (l *limitStore) RaiseVersionLimits(
	_ context.Context,
	_ map[entity.ShareID]entity.JournalVersion,
) error {
	return nil
}

// stillClock is a clock that never moves.
type stillClock struct{}

func (stillClock) Now() time.Time {
	return time.Time{}
}

func (stillClock) Elapsed() time.Duration {
	return 0
}

func (stillClock) After(time.Duration) <-chan struct{} {
	return make(chan struct{})
}

// The trigger with no version drawn is half a block, 8 here: with the limit
// last passed at 1000, a journal whose next version is 992 has 8 versions
// left and is not due, 993 leaves 7 and is due, and 991 leaves 9. stillDue,
// which the extra pass of a raise calls as is, answers so, and Bind's limit
// write raises every share exactly when it is due.
func TestStillDueBoundary(t *testing.T) {
	share := entity.ShareID{1}

	for _, c := range []struct {
		next uint64
		due  bool
	}{
		{991, false},
		{992, false},
		{993, true},
	} {
		j := &limitJournal{
			next: c.next,
		}
		store := &limitStore{}
		e := &Engine{
			d: Deps{
				Lifecycle: store,
				Clock:     stillClock{},
			},
			block: 16,
		}
		vl := &versionLimit{
			j: j,
		}
		vl.last.Store(1000)

		if got := e.stillDue(vl); got != c.due {
			t.Errorf("stillDue with next version %d = %v, want %v", c.next, got, c.due)
		}

		s := Share{
			ID: share,
		}

		if err := e.attachLimit(context.Background(), s, vl); err != nil {
			t.Fatalf("attachLimit with next version %d: %v", c.next, err)
		}

		if raised := len(j.marks) == 1; raised != c.due {
			t.Errorf("Bind with next version %d raised: %v, want %v", c.next, raised, c.due)
		}
	}
}
