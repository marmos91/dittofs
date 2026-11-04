package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// groupCommit commits the pending existence of one journal's files: one
// batch at a time, on a goroutine of its own, each batch the files due first,
// at most group_commit.max_files of them, synced then committed together.
//
// ponytail: one batch per journal at a time caps existence throughput at one
// transaction chain per journal. Pipeline batches when stability points are
// measured queueing behind one.
type groupCommit struct {
	e *Engine
	j Journal

	mu       sync.Mutex
	due      map[entity.FileID]*dueFile
	inFlight bool          // a batch is between its capture and its return
	closed   bool          // the engine closed: no batch runs, no file is noted
	halted   bool          // the journal's device is lost: no batch runs, no file is noted
	returned chan struct{} // closed, and replaced, each time a batch returns
	nextAt   time.Time     // when the loop's timer fires; zero, none is set
	losses   losses
	failing  map[entity.FileID]*failure
	marks    marks

	wake   chan struct{} // a file became due sooner than the timer, or now
	stop   chan struct{}
	done   chan struct{}
	ctx    context.Context // the batches' context, cancelled at stop
	cancel context.CancelFunc
}

// marks is what a group commit knows of its journal's sync marks.
//
// decision: nothing opens a journal yet, so a group commit records its marks
// under the zero identity at open generation 0, which a J‖journal‖og never
// raised matches. Withdraw this when Start opens the journal: it sets both,
// the generation from Lifecycle.SyncMarks, before the first group commit.
type marks struct {
	journal   [16]byte          // the journal's identity
	gen       kvstore.OpenGen   // the open generation of the journal's current open
	recorded  []entity.SyncMark // what the last committed marks write recorded
	reads     uint64            // the marks reads made so far
	readAt    uint64            // the read whose marks are recorded
	ask       bool              // a stability point waits for marks read after the last read, or a failed write is retried
	backoff   time.Duration     // a marks write that failed: its last backoff
	notBefore time.Time         // and the next one's earliest time
	stopped   bool              // another open replaced this one: no marks are recorded again
}

// errClosed answers a write after the engine's group commits stopped.
var errClosed = fmt.Errorf("engine: closed: %w", vfserr.ErrUnavailable)

// errLost is note's answer once the journal's device is lost whole.
var errLost = fmt.Errorf("engine: journal device lost: %w", vfserr.ErrUnavailable)

// dueFile is a file with uncommitted operations, waiting for a batch.
type dueFile struct {
	b         *binding
	oldest    time.Time // when its oldest uncommitted operation was staged
	now       bool      // a stability point, recovery or a clone asked for it
	notBefore time.Time // a failed commit's retry, not taken before this
}

// failure is a file whose existence commit failed: retried alone in the next
// batch, then, failing alone, backed off by itself.
type failure struct {
	alone    bool          // it failed beside other files: its next batch holds it alone
	backoff  time.Duration // its last backoff, once it failed alone
	conflict bool          // its last failure was a metadata conflict
	raised   bool          // existence_commit_failing names it
}

// Its stability points answer this while it fails: DelayConflict for a
// conflict, else DelayCommit.
func (f *failure) cause() vfserr.DelayCause {
	if f.conflict {
		return vfserr.DelayConflict
	}
	return vfserr.DelayCommit
}

// ponytail: a file failing alone is retried on a fixed doubling schedule,
// 100 ms to 30 s, not the shared scheduler's offload.retry_min and
// offload.retry_max. Move it onto that scheduler when offload brings one.
const (
	backoffMin = 100 * time.Millisecond
	backoffMax = 30 * time.Second
)

// captured is one file of a batch: its share and the operations the capture
// claimed, the entry's first n, which stay there until the batch returns.
type captured struct {
	b  *binding
	f  entity.FileID
	fe *fileEntry
	n  int
}

// pending is c's claimed operations as the store commits them, read under the
// entry's lock after the batch's sync, so a loss the sync's drain marked is
// committed lost.
//
// decision: a corrupt loss the journal reports after this read and before
// the store commits the batch is committed as written, charged, and the
// batch's return drops the mark with the operation: the window is one store
// call long. Close it with a loss path for committed, unoffloaded content,
// which records such a loss against the File once its write has committed.
func (e *Engine) pending(c captured) kvstore.PendingExistence {
	e.files.lock(c.fe)
	defer c.fe.mu.Unlock()
	p := kvstore.PendingExistence{Share: c.b.share.ID, File: c.f}
	for _, op := range c.fe.ops {
		if !op.claimed || op.dropped {
			continue
		}
		var mtime time.Time
		if op.setsMtime {
			mtime = op.at
		}
		p.Ops = append(p.Ops, storeOp(op, mtime))
	}
	return p
}

// storeOp is op as the store commits it: a write, a write the journal lost,
// or a truncate up.
func storeOp(op pendingOp, mtime time.Time) kvstore.PendingOp {
	o := kvstore.PendingOp{
		Version: entity.JournalVersion(op.version.Bytes()),
		Kind:    kvstore.OpWrite,
		Extent:  entity.Extent{Offset: op.extent.Off, Length: op.extent.Len},
		Mtime:   mtime,
		Ctime:   op.at,
	}
	switch {
	case op.grow:
		o.Kind, o.Extent, o.Size = kvstore.OpGrow, entity.Extent{}, op.extent.Off
	case op.lost:
		o.Kind = kvstore.OpLost
	}
	return o
}

// groupCommitFor is journal j's group commit, started on first use. Once
// the engine's group commits are closed it starts none: it answers a closed
// one, which runs no goroutine and notes no file.
func (e *Engine) groupCommitFor(j Journal) *groupCommit {
	e.gcMu.Lock()
	defer e.gcMu.Unlock()
	if g := e.gcs[j]; g != nil {
		return g
	}
	g := &groupCommit{
		e:        e,
		j:        j,
		due:      map[entity.FileID]*dueFile{},
		returned: make(chan struct{}),
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	g.ctx, g.cancel = context.WithCancel(context.Background())
	if e.gcsClosed {
		g.closed = true
		g.cancel()
		close(g.done)
		return g
	}
	if e.gcs == nil {
		e.gcs = map[Journal]*groupCommit{}
	}
	e.gcs[j] = g
	go g.run()
	return g
}

// closeGroupCommits stops every journal's group commit and joins its
// goroutine: a batch in flight has its context cancelled, and no batch
// starts after it.
func (e *Engine) closeGroupCommits() {
	e.gcMu.Lock()
	gs := e.gcs
	e.gcs = nil
	e.gcsClosed = true
	e.gcMu.Unlock()
	for _, g := range gs {
		g.mu.Lock()
		g.closed = true
		g.mu.Unlock()
		g.cancel()
		close(g.stop)
		<-g.done
		g.mu.Lock()
		close(g.returned) // wakes every waiter, which then finds the group commit closed
		g.returned = make(chan struct{})
		g.mu.Unlock()
	}
}

// note puts f on the due set, its oldest uncommitted operation staged at
// oldest, unless it is there already, waking the loop when f is due before
// its timer fires. It answers errClosed once the group commit is closed and
// errLost once the journal's device is lost. The caller holds f's entry lock,
// so a write is either noted before lostWhole takes the due set or refused
// after.
func (g *groupCommit) note(b *binding, f entity.FileID, oldest time.Time) error {
	age, _ := g.e.groupSettings()
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return errClosed
	}
	if g.halted {
		g.mu.Unlock()
		return errLost
	}
	if g.due[f] == nil {
		g.due[f] = &dueFile{b: b, oldest: oldest}
	}
	sooner := g.nextAt.IsZero() || (age >= 0 && oldest.Add(age).Before(g.nextAt))
	g.mu.Unlock()
	if sooner {
		g.poke()
	}
	return nil
}

func (g *groupCommit) poke() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

func (g *groupCommit) run() {
	defer close(g.done)
	for {
		select {
		case <-g.stop:
			return
		default:
		}
		batch, last, wait := g.take()
		if h := g.e.hooks.taken.Load(); h != nil && len(batch) > 0 {
			(*h)()
		}
		if len(batch) > 0 {
			g.commit(g.ctx, batch, last)
			continue
		}
		switch mw := g.marksDue(); {
		case mw == 0:
			g.commitMarks(g.ctx)
			continue
		case mw > 0 && (wait < 0 || mw < wait):
			wait = mw
		}
		var timer <-chan struct{}
		if wait >= 0 {
			timer = g.e.d.Clock.After(wait)
		}
		select {
		case <-g.wake:
		case <-timer:
		case <-g.stop:
			return
		}
	}
}

// groupSettings is existence_age and group_commit.max_files, or, before the
// first Apply, no age that makes a file due and batches of one.
func (e *Engine) groupSettings() (age time.Duration, maxFiles int) {
	if s := e.settings.Load(); s != nil {
		return s.ExistenceAge, s.GroupCommitMaxFiles
	}
	return -1, 1
}

// take removes the next batch's files from the due set: those asked for now,
// then those whose oldest operation was staged existence_age ago, oldest
// first, at most group_commit.max_files. last is that no other file is due
// beside them, so the batch ends its group commit. When none is due it
// answers how long until the first is, or -1 when the set holds none that the
// clock makes due.
//
// decision: the engine cannot see the journal's timer sync, so a file is due
// once its oldest uncommitted operation was staged existence_age ago by the
// clock, which is no later than the sync that made it recoverable plus
// existence_age. A removal needs no trigger: its first phase commits the
// file's pending existence itself. Revisit if the journal reports its timer
// syncs.
func (g *groupCommit) take() (batch []taken, last bool, wait time.Duration) {
	age, maxFiles := g.e.groupSettings()
	now := g.e.d.Clock.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	defer func() {
		g.nextAt = time.Time{}
		if len(batch) == 0 && wait >= 0 {
			g.nextAt = now.Add(wait)
		}
	}()
	if g.inFlight || g.closed || g.halted {
		return nil, false, -1
	}
	type cand struct {
		f      entity.FileID
		now    bool
		oldest time.Time
		alone  bool
	}
	var ready []cand
	wait = -1
	for f, d := range g.due {
		at := d.oldest.Add(age)
		if d.now {
			at = now
		}
		at = later(at, d.notBefore)
		if (d.now || age >= 0) && !at.After(now) {
			ready = append(ready, cand{f, d.now, d.oldest, g.failing[f] != nil && g.failing[f].alone})
			continue
		}
		if d.now || age >= 0 {
			if w := at.Sub(now); wait < 0 || w < wait {
				wait = w
			}
		}
	}
	slices.SortFunc(ready, func(a, b cand) int {
		if a.alone != b.alone { // a file to retry alone goes first, in a batch of its own
			if a.alone {
				return -1
			}
			return 1
		}
		if a.now != b.now {
			if a.now {
				return -1
			}
			return 1
		}
		return a.oldest.Compare(b.oldest)
	})
	if len(ready) > 0 && ready[0].alone {
		maxFiles = 1
	}
	for _, c := range ready[:min(len(ready), maxFiles)] {
		if c.alone && len(batch) > 0 {
			break
		}
		batch = append(batch, taken{c.f, g.due[c.f].b})
		delete(g.due, c.f)
	}
	if len(batch) > 0 {
		g.inFlight = true
	}
	return batch, len(batch) == len(ready), wait
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// taken is a file take removed from the due set for a batch, with its share.
type taken struct {
	f entity.FileID
	b *binding
}

// commit runs one batch: it captures each file, syncs the files in the
// journal, commits their existence, then settles each file's entry. A file
// whose operations start with one no batch captures, a removal's, goes back
// on the due set, due again one existence_age later. The last batch of a
// group commit also records the journal's sync marks.
func (g *groupCommit) commit(ctx context.Context, files []taken, last bool) {
	var batch []captured
	for _, t := range files {
		c, ok, held := g.e.capture(t.b, t.f)
		switch {
		case ok:
			batch = append(batch, c)
		case !held:
			// Its writes left another way, dropped or committed by another
			// path, so no failure of it is left to report. A file released
			// while it holds pending writes is cleared by its removal.
			g.clearFailing(t.f)
		case held:
			g.mu.Lock()
			if g.due[t.f] == nil {
				g.due[t.f] = &dueFile{b: t.b, oldest: g.e.d.Clock.Now()}
			}
			g.mu.Unlock()
		}
	}
	if h := g.e.hooks.captured.Load(); h != nil {
		(*h)()
	}
	failed, alone := g.e.syncAndCommit(ctx, g, batch, last)
	var dones []func()
	for _, c := range batch {
		retry := g.outcome(c.f, failed[c.f], len(batch) == 1 || alone[c.f])
		dones = append(dones, g.e.settle(g, c, failed[c.f] == nil, retry)...)
	}
	g.mu.Lock()
	g.inFlight = false
	// Every batch that took a file changed the due set, so its waiters look
	// again. A taken file no batch can capture went back as due one
	// existence_age later, so no waiter is woken in a loop.
	close(g.returned)
	g.returned = make(chan struct{})
	g.mu.Unlock()
	for _, d := range dones {
		d()
	}
}

// outcome records f's commit in a batch of one file or more: a success clears
// its failure; a failure beside other files has it retried alone in the next
// batch, and one alone backs it off by itself and raises
// existence_commit_failing naming it. It answers when f may be taken again.
func (g *groupCommit) outcome(f entity.FileID, err error, alone bool) (retry time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	fl := g.failing[f]
	if err == nil {
		g.clearFailingLocked(f)
		return time.Time{}
	}
	if fl == nil {
		fl = &failure{}
		if g.failing == nil {
			g.failing = map[entity.FileID]*failure{}
		}
		g.failing[f] = fl
	}
	fl.conflict = errors.Is(err, kvstore.ErrConflict)
	if !alone {
		fl.alone = true
		return time.Time{}
	}
	fl.backoff = min(max(2*fl.backoff, backoffMin), backoffMax)
	if !fl.raised {
		fl.raised = true
		g.e.d.Logger.Warn("existence_commit_failing", "file", f, "err", err)
	}
	return g.e.d.Clock.Now().Add(fl.backoff)
}

// clearFailing forgets f's failure, clearing existence_commit_failing.
func (g *groupCommit) clearFailing(f entity.FileID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.clearFailingLocked(f)
}

// clearFailingLocked is clearFailing; the caller holds g.mu.
func (g *groupCommit) clearFailingLocked(f entity.FileID) {
	if fl := g.failing[f]; fl != nil {
		delete(g.failing, f)
		if fl.raised {
			g.e.d.Logger.Warn("existence_commit_failing cleared", "file", f)
		}
	}
}

// failCause is the delay f's stability points answer while it fails: its
// failure's cause, or DelayCommit when it has none.
func (e *Engine) failCause(b *binding, f entity.FileID) vfserr.DelayCause {
	g := b.gc
	g.mu.Lock()
	defer g.mu.Unlock()
	if fl := g.failing[f]; fl != nil {
		return fl.cause()
	}
	return vfserr.DelayCommit
}

// capture claims f's leading writes in place under its entry's lock and marks
// the entry in-batch: the operations stay in the entry, so Overlay and Read
// keep answering them until the batch returns. A file with no write at the
// front of its entry is not captured: its operations are a removal's, which
// commits them itself.
//
// decision: a file whose guard a removal holds is skipped without its
// entry's lock, which a truncate holds across its journal call, and is taken
// in a later batch: no file's commit waits on another's removal, and the
// removal's phase 1 commits the writes below it itself. Correctness never
// rests on the skip, as the store's fences order the two either way.
func (e *Engine) capture(b *binding, f entity.FileID) (c captured, ok, held bool) {
	if fe := e.files.entryOf(f); fe != nil && fe.guarded.Load() {
		return captured{}, false, true
	}
	fe := e.files.lockOf(f)
	if fe == nil {
		return captured{}, false, false
	}
	defer e.files.unlock(f, fe)
	if fe.guarded.Load() {
		return captured{}, false, true
	}
	c = captured{b: b, f: f, fe: fe}
	for i := range fe.ops {
		if fe.ops[i].kind != opWrite && !fe.ops[i].grow {
			break
		}
		fe.ops[i].claimed = true
		c.n++
	}
	if c.n == 0 {
		return captured{}, false, true
	}
	fe.inBatch = true
	fe.batchDone = make(chan struct{})
	return c, true, true
}

// syncAndCommit syncs the batch's files in the journal, one Sync per share,
// then commits their existence in one Existence.Commit, which splits by the
// key budget itself, carrying the journal's sync marks, read after the syncs,
// when withMarks. A sync that fails fails the batch: nothing is committed
// that the journal did not make recoverable.
func (e *Engine) syncAndCommit(ctx context.Context, g *groupCommit, batch []captured, withMarks bool) (failed map[entity.FileID]error, alone map[entity.FileID]bool) {
	failed, alone = map[entity.FileID]error{}, map[entity.FileID]bool{}
	if len(batch) == 0 {
		return failed, alone
	}
	byShare := map[*binding][]journal.FileID{}
	for _, c := range batch {
		byShare[c.b] = append(byShare[c.b], fileID(c.f))
	}
	for b, ids := range byShare {
		if err := e.sync(ctx, b, ids...); err != nil {
			for _, c := range batch {
				failed[c.f] = err
			}
			return failed, alone
		}
	}
	pending := make([]kvstore.PendingExistence, len(batch))
	for i, c := range batch {
		pending[i] = e.pending(c)
	}
	var mw *kvstore.MarksWrite
	var read uint64
	if withMarks {
		mw, read = g.readMarks()
	}
	ctx, end := e.d.Tracer(ctx, "engine.existence_commit", slog.Int("files", len(batch)))
	switch recorded, err := e.commitSplit(ctx, pending, mw, failed, alone); {
	case recorded:
		g.recorded(mw, read)
	case mw != nil:
		g.marksFailed(mw, err)
	}
	end(nil)
	return failed, alone
}

// readMarks is the journal's sync marks as a marks write of its current
// open, and the number of the read, which answers a stability point's ask
// made before it; nil once marks are stopped.
func (g *groupCommit) readMarks() (*kvstore.MarksWrite, uint64) {
	g.mu.Lock()
	if g.marks.stopped {
		g.mu.Unlock()
		return nil, 0
	}
	g.marks.ask = false
	g.marks.reads++
	m := g.marks
	g.mu.Unlock()
	return &kvstore.MarksWrite{Journal: m.journal, Gen: m.gen, Marks: g.j.SyncMarks()}, m.reads
}

// recorded notes mw's marks, of the read-th read, as recorded, then hands
// them to the journal.
func (g *groupCommit) recorded(mw *kvstore.MarksWrite, read uint64) {
	g.mu.Lock()
	g.marks.recorded, g.marks.readAt = mw.Marks, read
	g.marks.backoff, g.marks.notBefore = 0, time.Time{}
	g.mu.Unlock()
	g.j.MarksRecorded(mw.Marks)
}

// marksFailed handles marks mw that did not commit, err why. On
// ErrOpenGenMoved the write is dropped, and when it carried the current
// open's generation another open replaced this one, so marks stop. Any other
// failure retries marks alone once a backoff, doubling as a failing file's
// does, has passed.
func (g *groupCommit) marksFailed(mw *kvstore.MarksWrite, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if errors.Is(err, kvstore.ErrOpenGenMoved) {
		if mw.Gen == g.marks.gen && !g.marks.stopped {
			// decision: a process another open replaced stops recording
			// marks but goes on committing existence for the journal,
			// since nothing else stops two processes on one journal
			// directory and the open generation fences only the marks.
			// Overturned if a double open is seen outside a test: then
			// fence the process off the journal.
			g.marks.stopped = true
			g.e.d.Logger.Warn("sync marks stopped: another open replaced this one", "err", err)
		}
		return
	}
	g.marks.ask = true
	g.marks.backoff = min(max(2*g.marks.backoff, backoffMin), backoffMax)
	g.marks.notBefore = g.e.d.Clock.Now().Add(g.marks.backoff)
}

// marksDue is how long until a write of marks alone is due: -1 when no
// stability point asks for one, else 0 once a failed one's backoff has
// passed.
func (g *groupCommit) marksDue() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.marks.ask || g.marks.stopped || g.closed || g.halted {
		return -1
	}
	return max(0, g.marks.notBefore.Sub(g.e.d.Clock.Now()))
}

// commitMarks records the journal's sync marks in a call of their own, for a
// stability point waiting on them while no batch is due, or retrying a
// failed write, and wakes the waiters.
func (g *groupCommit) commitMarks(ctx context.Context) {
	if mw, read := g.readMarks(); mw != nil {
		if err := g.e.commitMarksAlone(ctx, mw); err == nil {
			g.recorded(mw, read)
		} else {
			g.marksFailed(mw, err)
		}
	}
	g.mu.Lock()
	close(g.returned)
	g.returned = make(chan struct{})
	g.mu.Unlock()
}

// awaitMarks returns once a committed transaction recorded marks read after
// the read-th read, asking the group commit for them, or at ctx's end.
func (g *groupCommit) awaitMarks(ctx context.Context, read uint64) error {
	for {
		g.mu.Lock()
		done, closed, lost, returned := g.marks.readAt > read, g.closed, g.halted, g.returned
		if !done && !closed && !lost {
			g.marks.ask = true
		}
		g.mu.Unlock()
		switch {
		case done:
			return nil
		case closed:
			return errClosed
		case lost:
			return errLost
		}
		g.poke()
		select {
		case <-returned:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// commitSplit commits pending's existence, recording in failed each file that
// failed: those the store names in its map, the others having committed in
// the same call, and, for an error of the whole call that is not store-wide,
// each file the halves leave failing alone, which alone records as already
// failed alone. Each call
// to the store is bounded by conflictBound, past which a conflict the store
// keeps retrying is answered as one. A conflict or any other failure of one
// file therefore fails no other, and no conflict holds the journal's batches.
//
// A non-nil mw rides on the first call, and through a split on the first
// half, until it commits: recorded answers whether it did, and err why not.
// The marks never split the batch nor fail a file: a marks error has the
// call's existence retried at once without them, and when the file left
// beside them fails alone they go in a call of their own.
//
// ponytail: the split is checked by its table of failure shapes rather than
// fuzzed: it parses nothing, and its inputs are the store's answers. Fuzz it
// if the store's failure shapes grow beyond a file in the map and a whole
// call.
func (e *Engine) commitSplit(ctx context.Context, pending []kvstore.PendingExistence, mw *kvstore.MarksWrite, failed map[entity.FileID]error, alone map[entity.FileID]bool) (recorded bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, conflictBound.Load().(time.Duration))
	m, err := e.d.Existence.Commit(cctx, pending, mw)
	cancel()
	if mw != nil && (errors.Is(err, kvstore.ErrOpenGenMoved) || errors.Is(err, kvstore.ErrSyncMarks)) {
		e.commitSplit(ctx, pending, nil, failed, alone)
		return false, err
	}
	if err == nil {
		for f, ferr := range m {
			if ferr != nil {
				failed[f] = ferr
				alone[f] = len(pending) == 1
			}
		}
		return mw != nil, nil
	}
	if len(pending) == 1 || ctx.Err() != nil || storeWide(err) {
		for _, p := range pending {
			failed[p.File] = err
			alone[p.File] = len(pending) == 1
		}
		if mw == nil || len(pending) > 1 || ctx.Err() != nil || storeWide(err) {
			return false, err
		}
		if err := e.commitMarksAlone(ctx, mw); err != nil {
			return false, err
		}
		return true, nil
	}
	e.splits.Add(1)
	mid := len(pending) / 2
	recorded, err = e.commitSplit(ctx, pending[:mid], mw, failed, alone)
	e.commitSplit(ctx, pending[mid:], nil, failed, alone)
	return recorded, err
}

// commitMarksAlone writes mw in a call with no pending existence, under the
// bound every existence commit runs under.
func (e *Engine) commitMarksAlone(ctx context.Context, mw *kvstore.MarksWrite) error {
	cctx, cancel := context.WithTimeout(ctx, conflictBound.Load().(time.Duration))
	defer cancel()
	_, err := e.d.Existence.Commit(cctx, nil, mw)
	return err
}

// storeWide reports that err fails every file alike, so halving the batch
// would only repeat it: the store closed, or a call that ran out its bound
// with no conflict, as a store that answers nothing does. Any other error of
// the whole call, a conflict or one file's record the store cannot decode, is
// split until the file it belongs to stands alone.
func storeWide(err error) bool {
	return errors.Is(err, kvstore.ErrClosed) ||
		(errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, kvstore.ErrConflict))
}

// conflictBound bounds each existence commit, so a conflict the store keeps
// retrying is answered to the group commit as one.
//
// ponytail: one bound, a second, for every commit, a slow one's included.
// Make it a setting, or derive it from the store's transaction limits, when a
// healthy commit is seen to take near it.
var conflictBound atomic.Value // time.Duration

func init() { conflictBound.Store(time.Second) }

// settle ends c's part in its batch under its entry's lock: a committed file
// loses the operations the batch claimed, whose Dones it returns, is settled
// in the journal through the newest of them, and is put back on the due set
// if it holds others; a failed one has its claim released, keeps them and is
// retried at retry. A claimed write a failed window dropped leaves either way,
// its Done having run when it was dropped. The in-batch mark is cleared and
// its waiters woken; the entry is dropped once it holds nothing.
func (e *Engine) settle(g *groupCommit, c captured, ok bool, retry time.Time) (dones []func()) {
	fe := c.fe
	e.files.lock(fe)
	var through journal.Version
	kept := fe.ops[:0]
	for _, op := range fe.ops {
		switch {
		case !op.claimed:
			kept = append(kept, op)
		case op.dropped: // a failed window took it out of the batch; its Done ran then
		case ok:
			through = op.version // the claimed are in version order: the last is the newest
			if op.done != nil {
				dones = append(dones, op.done)
			}
			e.pendingApplied.Add(1)
		default: // the claim is released for the retry
			op.claimed = false
			kept = append(kept, op)
		}
	}
	clear(fe.ops[len(kept):])
	fe.ops = kept
	if through != (journal.Version{}) {
		e.files.removals.Add(1) // committed operations left the entry: see Allocation
	}
	fe.inBatch = false
	close(fe.batchDone)
	fe.batchDone = nil
	if len(fe.ops) > 0 {
		g.mu.Lock()
		d := g.due[c.f]
		if d == nil {
			d = &dueFile{b: c.b, oldest: fe.ops[0].at}
			g.due[c.f] = d
		}
		if !ok {
			// A failed file is retried as soon as its backoff allows, not
			// once its age comes round again.
			d.now, d.notBefore = true, retry
		}
		g.mu.Unlock()
	}
	e.files.unlock(c.f, fe)
	if ok && through != (journal.Version{}) {
		// The journal may drop what it keeps for recovery of f's existence
		// through the version the store now holds.
		c.b.h.Settle(fileID(c.f), through)
	}
	return dones
}

// commitNow commits f's pending existence now, joining the group commit: it
// marks f due at once and waits until a batch covers every operation f held
// when called, which a batch already in flight that captured fewer does not,
// or until ctx ends. An operation the group commit does not capture, a
// removal's, is waited for until ctx ends.
func (e *Engine) commitNow(ctx context.Context, b *binding, f entity.FileID) error {
	fe := e.files.lockOf(f)
	if fe == nil {
		return nil
	}
	if len(fe.ops) == 0 { // alive for an admitted write or a registration only
		e.files.unlock(f, fe)
		return nil
	}
	target := fe.ops[len(fe.ops)-1].version
	e.files.unlock(f, fe)
	return e.commitThrough(ctx, b, f, target)
}

// commitThrough is commitNow covering f's operations at or below target only.
func (e *Engine) commitThrough(ctx context.Context, b *binding, f entity.FileID, target journal.Version) error {
	g := b.gc
	for {
		g.mu.Lock()
		closed, lost, returned := g.closed, g.halted, g.returned
		g.mu.Unlock()
		if e.covered(f, target) {
			g.mu.Lock()
			if d := g.due[f]; d != nil && d.now {
				d.now = false // the ask is answered; a later write is due by its age
			}
			g.mu.Unlock()
			return nil
		}
		if closed {
			return errClosed
		}
		if lost {
			return errLost
		}
		// A removal holding f's guard commits f's operations below it in its
		// own phase 1, and no batch captures f meanwhile: wait for the guard
		// rather than asking a batch that would only skip f again.
		if ch := e.guardOf(f); ch != nil {
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		g.mu.Lock()
		d := g.due[f]
		if d == nil {
			d = &dueFile{b: b, oldest: e.d.Clock.Now()}
			g.due[f] = d
		}
		d.now = true
		g.mu.Unlock()
		g.poke()
		select {
		case <-returned:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// covered reports that f holds no operation at or below v.
func (e *Engine) covered(f entity.FileID, v journal.Version) bool {
	fe := e.files.lockOf(f)
	if fe == nil {
		return true
	}
	defer e.files.unlock(f, fe)
	return len(fe.ops) == 0 || fe.ops[0].version.Compare(v) > 0
}

// commitAll commits the pending existence of every file of journal j, as
// commitNow does for one: it marks every due file due now and returns once
// the due set is empty and no batch is in flight, or at ctx's end.
func (e *Engine) commitAll(ctx context.Context, j Journal) error {
	g := e.groupCommitFor(j)
	for {
		g.mu.Lock()
		if g.closed {
			g.mu.Unlock()
			return errClosed
		}
		for _, d := range g.due {
			d.now = true
		}
		done := len(g.due) == 0 && !g.inFlight
		returned := g.returned
		g.mu.Unlock()
		if done {
			return nil
		}
		g.poke()
		select {
		case <-returned:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// waitBatch returns once f is in no batch, or at ctx's end: a commit of f
// that starts after it cannot overtake one in flight.
func (e *Engine) waitBatch(ctx context.Context, f entity.FileID) error {
	for {
		fe := e.files.lockOf(f)
		if fe == nil || !fe.inBatch {
			if fe != nil {
				e.files.unlock(f, fe)
			}
			return nil
		}
		ch := fe.batchDone
		e.files.unlock(f, fe)
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// dropShare takes share s's files off the due set of journal j and returns
// once no batch is in flight there, so none of them is committed after.
func (e *Engine) dropShare(ctx context.Context, j Journal, s entity.ShareID) error {
	g := e.groupCommitFor(j)
	for {
		g.mu.Lock()
		for f, d := range g.due {
			if d.b.share.ID == s {
				delete(g.due, f)
				g.clearFailingLocked(f)
			}
		}
		inFlight, returned := g.inFlight && !g.closed, g.returned
		g.mu.Unlock()
		if !inFlight {
			return nil
		}
		select {
		case <-returned:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
