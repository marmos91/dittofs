package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// Commit makes [off, off+n) of file f of share s stable, n 0 the whole file,
// and answers the verifier and the file's loss sequence, both sampled after
// its sync and its waits. It syncs the file in the journal; when a pending
// write in the extent at or below the version the sync covers overwrites
// committed content, it also waits for the group commit to commit that
// write's existence; and when the extent covers committed content, it waits
// for recorded sync marks to cover the marks its sync returned. Under a store
// that commits nothing either wait answers ErrDelay at the caller's deadline,
// never success.
func (e *Engine) Commit(ctx context.Context, s entity.ShareID, f entity.FileID, off, n int64) (vfserr.Verifier, uint64, error) {
	if off < 0 || n < 0 {
		return vfserr.Verifier{}, 0, vfserr.ErrInvalid
	}
	b, err := e.bindingOf(s)
	if err != nil {
		return vfserr.Verifier{}, 0, err
	}
	e.stabilityPoints.Add(1)
	// decision: the journal's Sync returns no version, so the version a sync
	// covers is the highest pending in f's entry just before it: every
	// operation at or below it was staged before the sync, and one staged
	// during the sync is the next stability point's. Withdraw this once the
	// journal's Sync returns the version it reached.
	through, runs := e.pendingIn(f, off, n)
	sctx, end := e.d.Tracer(ctx, "engine.sync")
	marks, err := e.syncMarks(sctx, b, fileID(f))
	end(err)
	if err != nil {
		return vfserr.Verifier{}, 0, e.journalErr(err)
	}
	if len(runs) > 0 {
		// decision: appends and hole fills are never waited for: a crash
		// before their existence commits leaves them to recovery, which
		// commits them from the journal before the file is served. Only an
		// overwrite of committed content, which recovery cannot tell from
		// the content under it, waits for its commit.
		//
		// ponytail: a stable write's Commit reads the store once to learn
		// whether it overwrites committed content, one read per Commit.
		// Keep the committed size and holes in the entry if that read shows
		// in a profile.
		over, err := e.d.Existence.Overwrites(ctx, s, f, runs)
		if err != nil {
			return vfserr.Verifier{}, 0, e.storeErr(err)
		}
		if over {
			if err := e.commitThrough(ctx, b, f, through); err != nil {
				return vfserr.Verifier{}, 0, e.waitErr(b, f, err)
			}
		}
	}
	if err := e.marksWait(ctx, b, s, f, off, n, marks); err != nil {
		return vfserr.Verifier{}, 0, err
	}
	return e.sample(b), e.LossSeq(s, f), nil
}

// marksWait returns once recorded sync marks cover marks, those f's sync
// returned, or at once when [off, off+n), n 0 the whole file, covers no
// committed content: the journal's proof of a synced record is in-process,
// and only a recorded mark carries it to disk. A write the lazy commit
// already committed, or whose group commit's marks failed, is covered by
// nothing else. It asks the group commit for marks read after the sync; when
// those still cover none, the record's segment ended meanwhile and its copy
// lies in another, so it syncs f again for the copy's mark.
func (e *Engine) marksWait(ctx context.Context, b *binding, s entity.ShareID, f entity.FileID, off, n int64, marks []entity.SyncMark) error {
	g := b.gc
	checked := false
	for {
		g.mu.Lock()
		read, ok := g.marks.reads, covered(marks, g.marks.recorded)
		g.mu.Unlock()
		if ok {
			return nil
		}
		if !checked {
			x := entity.Extent{Offset: off, Length: n}
			if n == 0 {
				x.Length = math.MaxInt64 - off
			}
			over, err := e.d.Existence.Overwrites(ctx, s, f, []entity.Extent{x})
			if err != nil {
				return e.storeErr(err)
			}
			if !over {
				return nil
			}
			checked = true
		}
		if err := g.awaitMarks(ctx, read); err != nil {
			return e.waitErr(b, f, err)
		}
		g.mu.Lock()
		ok = covered(marks, g.marks.recorded)
		g.mu.Unlock()
		if !ok {
			var err error
			if marks, err = e.syncMarks(ctx, b, fileID(f)); err != nil {
				return e.journalErr(err)
			}
		}
	}
}

// covered reports that each returned mark has a recorded one covering it.
func covered(returned, recorded []entity.SyncMark) bool {
	for _, r := range returned {
		if !slices.ContainsFunc(recorded, func(m entity.SyncMark) bool { return m.Covers(r) }) {
			return false
		}
	}
	return true
}

// pendingIn is the highest version pending in f's entry, and the extents of
// its pending writes in [off, off+n), n 0 the whole file, at or below it.
func (e *Engine) pendingIn(f entity.FileID, off, n int64) (journal.Version, []entity.Extent) {
	fe := e.files.lockOf(f)
	if fe == nil {
		return journal.Version{}, nil
	}
	defer e.files.unlock(f, fe)
	if len(fe.ops) == 0 { // alive for an admitted write or a registration only
		return journal.Version{}, nil
	}
	through := fe.ops[len(fe.ops)-1].version
	var runs []entity.Extent
	for _, op := range fe.ops {
		if op.kind != opWrite || op.dropped {
			continue
		}
		x := entity.Extent{Offset: op.extent.Off, Length: op.extent.Len}
		if n > 0 {
			lo, hi := max(x.Offset, off), min(x.End(), off+n)
			if lo >= hi {
				continue
			}
			x = entity.ExtentOf(lo, hi)
		}
		runs = append(runs, x)
	}
	return through, runs
}

// waitErr is the answer for a wait on the group commit that the caller's
// context ended: its deadline is ErrDelay with the cause f's failing commit
// gives, DelayConflict for a conflict, else DelayCommit; a cancel is the
// context's own error.
func (e *Engine) waitErr(b *binding, f entity.FileID, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &vfserr.Delay{Cause: e.failCause(b, f)}
	}
	return err
}

// instanceID is the process instance, drawn at random when the engine is
// built, so an engine built again in the same process answers other
// verifiers.
func instanceID() [16]byte {
	var id [16]byte
	_, _ = rand.Read(id[:]) // crypto/rand.Read never returns an error
	return id
}

// primary is what a binding knows of the node serving its share's shard: the
// node and its epoch. The shard's incarnation, raised each time this node or a
// journal begins serving the shard, is the binding's own, as its attach wrote
// it.
type primary struct {
	node      entity.NodeID
	nodeEpoch uint64
}

// verifier is the write verifier of a file of share b, sampled now.
//
// decision: the verifier is the first 8 bytes of a SHA-256 over the node, the
// node epoch, the process instance, the shard's incarnation and the loss
// generation of the share's journal, in that order, each at a fixed width, so
// no two inputs cancel and no input is left out: it changes exactly when one of
// them does, never with the clock. Revisit if the verifier must be decoded back
// into its inputs.
//
// ponytail: every sample reads the journal's Stats() for its loss generation,
// one call per write and per Commit. Add a journal call returning the
// generation alone if a profile shows Stats() on the write path.
func (e *Engine) verifier(b *binding) vfserr.Verifier {
	var in [16 + 8 + 16 + 8 + 8]byte
	copy(in[0:16], b.primary.node[:])
	binary.BigEndian.PutUint64(in[16:24], b.primary.nodeEpoch)
	copy(in[24:40], e.instance[:])
	binary.BigEndian.PutUint64(in[40:48], b.incarnation)
	binary.BigEndian.PutUint64(in[48:56], b.j.Stats().LossGeneration)
	sum := sha256.Sum256(in[:])
	var v vfserr.Verifier
	copy(v[:], sum[:])
	return v
}
