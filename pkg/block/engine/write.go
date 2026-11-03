package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/vfs/vfserr"
)

// Write stages p at off of file f of share s and answers once the journal
// holds it: it makes no store call and waits for nothing but the journal. The
// verifier is sampled before the write is staged. The write's operation is in
// f's entry before Write answers, so a read that starts after reflects it. A
// zero-length write stages nothing and calls o.Done once; a refused one never
// calls it.
func (e *Engine) Write(ctx context.Context, s entity.ShareID, f entity.FileID, off int64, p []byte, o WriteOpts) (vfserr.Verifier, error) {
	if off < 0 {
		return vfserr.Verifier{}, vfserr.ErrInvalid
	}
	b, err := e.bindingOf(s)
	if err != nil {
		return vfserr.Verifier{}, err
	}
	v := e.sample()
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

// zeroVerifier is the verifier every write answers until the verifier's
// inputs are derived.
//
// ponytail: a write's verifier, a stable one's included, means nothing yet:
// every write answers the same zero verifier, so a client comparing
// verifiers sees no restart or loss. Derive it from the journal's identity,
// its loss generation and the version floor once commit is built.
func zeroVerifier() vfserr.Verifier { return vfserr.Verifier{} }

// journalErr is the content methods' answer for an error the journal
// returned, so no journal type crosses the content boundary.
func (e *Engine) journalErr(err error) error {
	var noSpace *journal.NoSpaceError
	var other *journal.OtherShareError
	switch {
	case errors.As(err, &noSpace):
		// decision: every capacity refusal, the version limit's included, is
		// answered as retry-later, never as a full disk, as the refusal table
		// that tells a lasting shortage from a passing one is not built.
		// Answer each limit as that table rules once it is.
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
