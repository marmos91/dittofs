package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// Write stages p at off of file f of share s and answers once the journal
// holds it: it makes no store call and waits for nothing but the journal. The
// verifier is sampled before the write is staged. The write's operation is in
// f's entry before Write answers, so a read that starts after reflects it. A
// zero-length write stages nothing and calls o.Done once; a refused one never
// calls it. A range below zero or past the largest offset is ErrInvalid.
func (e *Engine) Write(ctx context.Context, s entity.ShareID, f entity.FileID, off int64, p []byte, o WriteOpts) (vfserr.Verifier, error) {
	if off < 0 || int64(len(p)) > math.MaxInt64-off {
		return vfserr.Verifier{}, vfserr.ErrInvalid
	}
	b, err := e.bindingOf(s)
	if err != nil {
		return vfserr.Verifier{}, err
	}
	v := e.sample(b)
	if len(p) == 0 {
		if o.Done != nil {
			o.Done()
		}
		return v, nil
	}
	now := e.d.Clock.Now() // one reading: the entry keeps the time the journal records
	var mtime int64        // nanoseconds since the Unix epoch; 0 leaves the modification time alone
	if o.Suspend&entity.TimeModify == 0 {
		mtime = now.UnixNano()
	}
	ver, err := b.h.WriteAt(ctx, fileID(f), off, p, mtime)
	if err != nil {
		return vfserr.Verifier{}, e.journalErr(err)
	}
	fe := e.files.lockFor(f)
	if len(fe.ops) == 0 {
		// Refused once the engine closed or the journal's device was lost:
		// the write is answered unavailable and its Done is not called, as for
		// any write Write refuses.
		if err := b.gc.note(b, f, now); err != nil {
			e.files.unlock(f, fe)
			return vfserr.Verifier{}, err
		}
	}
	fe.insert(pendingOp{
		version:   ver,
		kind:      opWrite,
		extent:    journal.Extent{Off: off, Len: int64(len(p))},
		at:        now,
		setsMtime: mtime != 0,
		done:      o.Done,
	})
	e.files.unlock(f, fe)
	return v, nil
}

// Draw is the next version of the journal holding file f of share s, for a
// change stored outside the journal: above every version staged or drawn on
// that journal before it, and no version of the engine's own. The caller
// holds the file's accept lock, so Draw takes no lock and creates no entry.
func (e *Engine) Draw(ctx context.Context, s entity.ShareID, f entity.FileID) (entity.JournalVersion, error) {
	b, err := e.bindingOf(s)
	if err != nil {
		return entity.JournalVersion{}, err
	}
	v, err := b.j.Draw()
	if err != nil {
		return entity.JournalVersion{}, e.journalErr(err)
	}
	return entity.JournalVersion(v.Bytes()), nil
}

// insert adds op to the entry's operations in version order. The caller
// holds the entry's lock.
//
// decision: the entry's lock is not held across the journal's WriteAt, so an
// operation is inserted after the journal answers, by its version. Writes of
// one file reach the engine in version order because the filesystem holds the
// file's accept lock across Write, so a capture never sees a later write
// before an earlier one. Withdraw this if a caller can ever stage two writes
// of one file at once.
func (fe *fileEntry) insert(op pendingOp) {
	i, _ := slices.BinarySearchFunc(fe.ops, op.version, func(o pendingOp, v journal.Version) int {
		return o.version.Compare(v)
	})
	fe.ops = slices.Insert(fe.ops, i, op)
}

// fileID is f as the journal names it.
func fileID(f entity.FileID) journal.FileID { return journal.NewFileID(f) }

// journalErr is the content methods' answer for an error the journal
// returned, so no journal type crosses the content boundary.
func (e *Engine) journalErr(err error) error {
	var noSpace *journal.NoSpaceError
	var other *journal.OtherShareError
	switch {
	case errors.As(err, &noSpace):
		// decision: every space refusal is answered as retry-later, never as
		// a full disk, as the refusal table that tells a lasting shortage
		// from a passing one is not built. The version limit is DelayCommit,
		// as it is raised only once a metadata commit records a higher one;
		// every other limit is DelayCapacity. Answer each limit as that table rules
		// once it is.
		if noSpace.Limit == journal.LimitVersion {
			return &vfserr.Delay{Cause: vfserr.DelayCommit}
		}
		return &vfserr.Delay{Cause: vfserr.DelayCapacity}
	case errors.As(err, &other):
		return fmt.Errorf("engine: %v: %w", err, vfserr.ErrCorrupt)
	case errors.Is(err, journal.ErrClosed):
		return fmt.Errorf("engine: journal closed: %w", vfserr.ErrUnavailable)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	e.d.Logger.Error("journal call failed", "err", err)
	return fmt.Errorf("engine: %v: %w", err, vfserr.ErrUnavailable)
}
