package enginetest

// The journal's Share and Reserve return its concrete handle and reservation,
// and Go does not accept a concrete result for a method whose interface
// declares an interface result, so the fixture wraps those two calls to pass
// the journal as the engine's Journal. It also stands in for the sync marks
// the journal does not report yet. Every other method is the journal's own.

import (
	"context"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// wrapJournal returns j as the engine's Journal. The handles its Share
// returns are the engine's Handle, each handed to opened, and their Reserve
// returns the engine's Reservation.
func wrapJournal(j *journal.Journal, opened func(journal.ShareTag, engine.Handle)) engine.Journal {
	return wrappedJournal{j, opened}
}

var (
	_ engine.Handle      = handle{}
	_ engine.Reservation = (*journal.Reservation)(nil)
)

type wrappedJournal struct {
	*journal.Journal
	opened func(journal.ShareTag, engine.Handle)
}

func (j wrappedJournal) Share(tag journal.ShareTag, limit int64, floor journal.Version) (engine.Handle, error) {
	h, err := j.Journal.Share(tag, limit, floor)
	if err != nil {
		return nil, err
	}
	j.opened(tag, handle{h})
	return handle{h}, nil
}

// decision: the journal's Sync returns no marks and its Stats carries none
// yet, so over the real journal the engine records none and no stability
// point waits on one. Withdraw these three once the journal reports its
// marks.
func (j wrappedJournal) SyncMarks() []entity.SyncMark    { return nil }
func (j wrappedJournal) MarksRecorded([]entity.SyncMark) {}

type handle struct{ *journal.Handle }

func (h handle) Sync(ctx context.Context, ids ...journal.FileID) ([]entity.SyncMark, error) {
	return nil, h.Handle.Sync(ctx, ids...)
}

func (h handle) Reserve(n int64, writes int) (engine.Reservation, error) {
	r, err := h.Handle.Reserve(n, writes)
	if err != nil {
		return nil, err
	}
	return r, nil
}
