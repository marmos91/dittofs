package model

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// Handle is what a step calls on one share of the journal: the calls of
// *journalv2.Handle the model-based test drives.
type Handle interface {
	WriteAt(ctx context.Context, id journalv2.FileID, off int64, p []byte, mtime int64) (journalv2.Version, error)
	Sync(ctx context.Context, ids ...journalv2.FileID) error
	ReadAt(ctx context.Context, id journalv2.FileID, off int64, p []byte) (n int, missing []journalv2.Extent, held []journalv2.Held, asOf journalv2.Version, err error)
	Fill(ctx context.Context, id journalv2.FileID, off int64, p []byte, asOf journalv2.Version, v journalv2.Version) error
	Offload(ctx context.Context, id journalv2.FileID, through journalv2.Version, limit, widen int64, fn func(ctx context.Context, o journalv2.Offer, report func(offloaded []journalv2.Extent) error) error) error
	Release(ctx context.Context, id journalv2.FileID, extents []journalv2.Extent) error
	Truncate(ctx context.Context, id journalv2.FileID, size int64) (journalv2.Version, error)
	Deallocate(ctx context.Context, id journalv2.FileID, e journalv2.Extent) (journalv2.Version, error)
	Delete(ctx context.Context, id journalv2.FileID) (journalv2.Version, error)
	Settle(id journalv2.FileID, v journalv2.Version)
}

var _ Handle = (*journalv2.Handle)(nil)

// Target is a journal the model-based test runs a sequence against, with two
// shares attached: share i under tag i+1.
type Target interface {
	// Share returns the handle of share i, the one open now: a crash or a
	// reopen attaches the shares again.
	Share(i int) Handle
	// View returns what Compare reads for share i.
	View(i int) View
	// Repack runs one repack pass over the whole journal.
	Repack(ctx context.Context) error
	// Crash loses the journal's storage as kind says, with arg the bytes a
	// torn write keeps or the seed of a reordering, and opens the journal
	// again on what is left.
	Crash(kind CrashKind, arg uint64) error
	// Reopen closes the journal cleanly and opens it again.
	Reopen() error
	// Floor returns the floor the journal open now was opened with.
	Floor() journalv2.Version
}

// ShareTag returns the tag share i of a Target is attached under.
func ShareTag(i int) journalv2.ShareTag { return journalv2.ShareTag(i + 1) }

// FileOf returns the ID of file i of a sequence.
func FileOf(i int) journalv2.FileID {
	var id [16]byte
	copy(id[:], fmt.Sprintf("model-file-%05d", i))
	return journalv2.NewFileID(id)
}

// crashSearchable is how many records of one file a crash step leaves
// uncertain at most.
//
// ponytail: a file with more is synced before the crash, so the search for
// what the crash left, which doubles with each record the bytes cannot
// decide, stays fast; a crash therefore never finds more than eight of one
// file's records in doubt. Raise it toward the model's own limit if a
// failure needs a crash over a longer unsynced run of one file.
const crashSearchable = 8

// remoteExtent is content an offload reported: what the remote tier holds,
// and what a fill brings back.
type remoteExtent struct {
	ext  journalv2.Extent
	ver  journalv2.Version
	data []byte
}

// runner runs one sequence against a target, applying every answer to the
// model and comparing the two after every step.
type runner struct {
	ctx context.Context
	tgt Target
	m   *Model
	s   stubs

	// asOf is, per file, the change versions of its last two reads, the last
	// first: what a fill and a stale fill are made as of.
	asOf [Files][2]journalv2.Version
	// remote is, per file, what offloads of it reported.
	remote [Files][]remoteExtent
}

// errStop ends an offload callback because the model found a divergence
// inside it, which the runner reports instead.
var errStop = errors.New("model: the step failed inside the offload callback")

// Run runs steps against tgt, a journal holding nothing, and compares it with
// a model after every step. It returns the number of steps that ran
// and the first error: one wrapping ErrDiverged when the journal did what the
// RFC does not allow, ErrMisuse when the sequence drove the model where it
// cannot follow, a StubError when a journal call it made is not written yet,
// or the target's own error.
//
// decision: a sequence injects no storage fault but its crashes, so a sync
// never fails and no loss is ever recorded, and the model takes any Sync
// error as a divergence. Failed sync windows and losses are checked by the
// conformance tests that own them, each against its fault.
func Run(ctx context.Context, tgt Target, steps []Step) (int, error) {
	r := &runner{ctx: ctx, tgt: tgt, m: New()}
	for i, s := range steps {
		if why := s.valid(); why != "" {
			return i, fmt.Errorf("model: step %d %+v: %s: %w", i, s, why, ErrMisuse)
		}
		r.s = stubs{}
		err := r.step(s)
		if err == nil {
			err = r.compare()
		}
		if err != nil && r.s.call != "" {
			err = StubError{r.s.call}
		}
		if err != nil {
			return i, fmt.Errorf("step %d, %s: %w", i, fmtStep(s), err)
		}
	}
	return len(steps), nil
}

// share returns the handle of share i, reporting its stubs to the step.
func (r *runner) share(i int) Handle { return stubHandle{r.tgt.Share(i), &r.s} }

// fmtStep prints a step as its corpus line.
func fmtStep(s Step) string {
	return strings.TrimSuffix(string(FormatCorpus([]Step{s})), "\n")
}

func (r *runner) compare() error {
	for i := range Shares {
		if err := r.m.Compare(r.ctx, ShareTag(i), stubView{r.tgt.View(i), &r.s}); err != nil {
			return err
		}
	}
	return nil
}

// payload returns the bytes a write step writes.
func payload(s Step) []byte {
	p := make([]byte, s.Len)
	for i := range p {
		p[i] = s.Byte + byte(i)
	}
	return p
}

func (r *runner) step(s Step) error {
	ctx, m := r.ctx, r.m
	h, tag, id := r.share(s.Share), ShareTag(s.Share), FileOf(s.File)

	switch s.Op {
	case OpWrite:
		p := payload(s)
		v, err := h.WriteAt(ctx, id, s.Off, p, int64(s.Byte))
		return m.WriteAt(tag, id, s.Off, p, int64(s.Byte), v, err)

	case OpSync:
		return m.Sync(tag, []journalv2.FileID{id}, h.Sync(ctx, id))

	case OpRead:
		_, _, _, asOf, err := h.ReadAt(ctx, id, s.Off, make([]byte, max(s.Len, 1)))
		if err := r.readRefusal(tag, id, err); err != nil {
			return err
		}
		if err == nil {
			r.asOf[s.File] = [2]journalv2.Version{asOf, r.asOf[s.File][0]}
		}
		// What the read returned is checked by the comparison after the step.
		return nil

	case OpFill, OpStaleFill:
		rs := r.remote[s.File]
		if len(rs) == 0 {
			return nil
		}
		x := rs[int(s.Pick)%len(rs)]
		if droppableAbove(m.files[id], x.ext, x.ver) != nil {
			// A caller never fills content older than what it released or
			// settled the removal of, so the model does not decide it.
			return nil
		}
		asOf := r.asOf[s.File][0]
		if s.Op == OpStaleFill {
			asOf = r.asOf[s.File][1]
		}
		err := h.Fill(ctx, id, x.ext.Off, x.data, asOf, x.ver)
		return m.Fill(tag, id, x.ext.Off, x.data, asOf, x.ver, err)

	case OpOffload:
		return r.offload(s, h)

	case OpRelease:
		e := journalv2.Extent{Off: s.Off, Len: max(s.Len, 1)}
		if rs := r.remote[s.File]; len(rs) > 0 {
			e = rs[int(s.Pick)%len(rs)].ext
		}
		return m.Release(tag, id, []journalv2.Extent{e}, h.Release(ctx, id, []journalv2.Extent{e}))

	case OpTruncate:
		v, err := h.Truncate(ctx, id, s.Off)
		return m.Truncate(tag, id, s.Off, v, err)

	case OpDeallocate:
		e := journalv2.Extent{Off: s.Off, Len: s.Len}
		v, err := h.Deallocate(ctx, id, e)
		return m.Deallocate(tag, id, e, v, err)

	case OpDelete:
		v, err := h.Delete(ctx, id)
		return m.Delete(tag, id, v, err)

	case OpSettle:
		v := m.maxSeen
		v.Counter = uint64(float64(v.Counter) * float64(s.Pick) / 255)
		h.Settle(id, v)
		return m.Settle(tag, id, v)

	case OpRepack:
		err := r.tgt.Repack(ctx)
		r.s.seen("Repack", err)
		if err != nil {
			return m.mismatch("repack", journalv2.FileID{}, "returned %v", err)
		}
		return nil

	case OpCrash, OpReopen:
		return r.restart(s)
	}
	return fmt.Errorf("model: no operation %v: %w", s.Op, ErrMisuse)
}

// readRefusal checks a read's error: a read through the share holding the
// file, or of a file no share holds, is never refused, and one through
// another share is refused with ErrOtherShare while that share holds the
// file, as every call naming it is, and may be once it holds nothing.
func (r *runner) readRefusal(tag journalv2.ShareTag, id journalv2.FileID, err error) error {
	f := r.m.files[id]
	other := f != nil && f.tag != tag
	switch {
	case cancelled(err):
		return nil
	case other && errors.Is(err, journalv2.ErrOtherShare):
		return nil
	case err != nil:
		return r.m.mismatch("read", id, "returned %v", err)
	case other && f.holdsAnything():
		return r.m.mismatch("read", id, "read through share %d a file share %d holds", tag, f.tag)
	}
	return nil
}

// offload offloads the file through every version seen. Inside the callback
// it checks the offer, writes to the file when the step asks for a write
// during the offer, and reports what the step's pick chooses; what was
// reported goes to the remote tier.
func (r *runner) offload(s Step, h Handle) error {
	ctx, m := r.ctx, r.m
	tag, id := ShareTag(s.Share), FileOf(s.File)
	through := m.maxSeen

	var stepErr error
	fail := func(err error) error {
		stepErr = err
		return errStop
	}
	callback := func(ctx context.Context, o journalv2.Offer, report func([]journalv2.Extent) error) error {
		if err := m.Offer(tag, id, through, 0, 0, o); err != nil {
			return fail(err)
		}
		if s.Pick&1 != 0 && s.Len > 0 {
			p := payload(s)
			v, err := h.WriteAt(ctx, id, s.Off, p, int64(s.Byte))
			if err := m.WriteAt(tag, id, s.Off, p, int64(s.Byte), v, err); err != nil {
				return fail(err)
			}
		}

		var reported []journalv2.Extent
		switch (s.Pick >> 1) % 4 {
		case 0: // everything offered
			for _, d := range o.Dirty {
				reported = append(reported, d.Extent)
			}
		case 1: // nothing: the callback never reports
			return nil
		case 2: // every other extent, and the first half of the first
			for i, d := range o.Dirty {
				switch {
				case i == 0 && d.Extent.Len > 1:
					reported = append(reported, journalv2.Extent{Off: d.Extent.Off, Len: d.Extent.Len / 2})
				case i%2 == 0:
					reported = append(reported, d.Extent)
				}
			}
		case 3: // an extent that was not offered
			reported = []journalv2.Extent{notOffered(o)}
		}
		if err := m.Report(tag, id, reported, report(reported)); err != nil {
			return fail(err)
		}
		for _, d := range o.Dirty {
			for _, e := range reported {
				x, ok := intersect(d.Extent, e)
				if !ok {
					continue
				}
				data := make([]byte, x.Len)
				if _, err := o.Offered.ReadAt(data, x.Off); err != nil {
					return fail(m.mismatch("offer", id, "reading offered %v: %v", x, err))
				}
				r.remote[s.File] = append(r.remote[s.File], remoteExtent{ext: x, ver: d.Version, data: data})
			}
		}
		return nil
	}

	err := h.Offload(ctx, id, through, 0, 0, callback)
	if stepErr != nil {
		return stepErr
	}
	return m.EndOffer(tag, id, through, nil, err)
}

// notOffered returns a one-byte extent that no extent of o covers.
func notOffered(o journalv2.Offer) journalv2.Extent {
	at := int64(0)
	for _, d := range o.Dirty {
		if d.Extent.Off > at {
			break
		}
		at = max(at, end(d.Extent))
	}
	return journalv2.Extent{Off: at, Len: 1}
}

// restart crashes the journal or reopens it cleanly. A clean close promises
// nothing about durability, so both leave every unsynced record of the model
// uncertain, and the comparison after the step finds what the journal kept.
// Every file with more uncertain records than the search takes is synced
// first.
//
// decision: when syncing is not enough, because a file's settled markers
// alone are too many, the step does nothing rather than fail, so the sequence
// stays valid for shrinking. Only a crash resolves settled markers, so from
// then on every crash and reopen of the sequence is skipped. Settle steps are
// rare and removals settle in batches, so few sequences reach it; draw fewer
// removals per file if crash steps are found skipped often.
func (r *runner) restart(s Step) error {
	ctx, m := r.ctx, r.m
	for _, id := range sortedIDs(m) {
		f := m.files[id]
		if uncertainAt(f) <= crashSearchable {
			continue
		}
		i := int(f.tag) - 1
		if err := m.Sync(f.tag, []journalv2.FileID{id}, r.share(i).Sync(ctx, id)); err != nil {
			return err
		}
		if uncertainAt(f) > crashSearchable {
			return nil
		}
	}

	if err := m.Crash(); err != nil {
		return err
	}
	var err error
	if s.Op == OpCrash {
		err = r.tgt.Crash(s.crash())
	} else {
		err = r.tgt.Reopen()
	}
	if err != nil {
		return fmt.Errorf("model: %s: %w", s.Op, err)
	}
	return m.Reopen(r.tgt.Floor())
}

// sortedIDs returns the files of m in ID order, so what is done to each never
// depends on the order of a map.
func sortedIDs(m *Model) []journalv2.FileID {
	return slices.SortedFunc(maps.Keys(m.files), func(a, b journalv2.FileID) int {
		x, y := a.Bytes(), b.Bytes()
		return slices.Compare(x[:], y[:])
	})
}

// uncertainAt counts what a crash now would leave uncertain in f.
func uncertainAt(f *file) int {
	n := 0
	for _, r := range f.recs {
		if !r.durable || crashMayDropMarker(r) {
			n++
		}
	}
	return n
}
