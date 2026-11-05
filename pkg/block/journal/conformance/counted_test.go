//go:build journal_conformance

package conformance

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

// The counted checks of the sync path. None of them times anything: each
// counts what the seam saw, and holds or gates the calls so no schedule can
// decide the answer.

// countedWriters is the number of concurrent writers the syncs-per-write
// check runs, the number of its rounds' syncs the gate must gather, and the
// factor by which the bound sits from a design that syncs each write alone
// and one that groups the whole round.
const countedWriters = 16

// countedRounds is how many write-then-Sync rounds each writer of the
// syncs-per-write check makes.
const countedRounds = 8

// countedWrite is the size of one write the counted checks make through a
// handle, chosen small enough that a round of writers fits beside the
// records of one segment without a roll deciding where the writes land.
const countedWrite = 4096

// syncGate is the fault that holds every sync of a segment at storage until
// the round it belongs to is whole — every active writer has entered that
// round's Sync — and then stays open for the rest of the round, so the
// journal's own batching decides how the round's syncs group, not the order
// the scheduler ran the writers in. It passes every other call.
//
// A writer counts as entered a round once it has called Sync for it, not
// once its write has landed, so a sync may open before every write of the
// round reached the segment; the bound's factor of four absorbs that.
//
// decision: the gate latches per round, not on a live count of syncs in
// flight. A count a returning Sync decrements lets the first writer to
// finish a round leave, write, re-enter and open the gate on its own, before
// the round's stragglers have entered at all, so the scheduler again decides
// which syncs group; latching on the round — writers entered it, and a
// return never lowers that count — is what holds each sync until all 16 are
// waiting. The round barrier in left keeps any writer from starting the next
// round's WriteAt before every writer returned from this one, so a fast
// writer cannot latch the gate it and a straggler share on the next round.
//
// decision: the gate is not closed by cleanup alone. A journal that syncs
// inside WriteAt, or holds its append lock across a fsync, parks one gated
// sync and never lets the other writers reach theirs; closing only at the
// test's end would hang the run, so the check closes the gate itself once
// its bounded wait runs out, and the writers left parked are freed before it
// fails. The ceiling: a journal whose rounds legitimately take longer than
// the bound fails without being wrong. Overturned by a journal that reports
// the waits it is in, when a check can see a parked writer rather than time
// it out.
type syncGate struct {
	mu       sync.Mutex
	cond     *sync.Cond
	entered  int  // writers that entered the current round's Sync
	returned int  // writers that returned from the current round's Sync
	latched  bool // the current round's gate has opened and stays open
	active   int  // writers still making rounds
	passAll  bool // set by close, so every parked sync and barrier is freed
}

func newSyncGate(writers int) *syncGate {
	g := &syncGate{active: writers}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// close passes every parked and later sync, so cleanup never hangs on a
// journal that stopped draining its syncs.
func (g *syncGate) close() {
	g.mu.Lock()
	g.passAll = true
	g.cond.Broadcast()
	g.mu.Unlock()
}

// entering counts one writer's Sync of the current round as begun, and
// latches the gate open once every active writer has entered it: a Sync that
// returns does not unlatch it, so the round's stragglers pass the moment
// they arrive, and no sync of the round waits alone.
func (g *syncGate) entering() {
	g.mu.Lock()
	g.entered++
	if g.entered >= g.active {
		g.latched = true
		g.cond.Broadcast()
	}
	g.mu.Unlock()
}

// left counts one writer's Sync as returned, and parks it at the round's
// barrier until every active writer has: no writer starts the next round's
// WriteAt while a sync of this one can still arrive, so the round the gate
// latches on is the round the writers are all in. The writer through last
// resets the counts for the next round and frees the rest.
func (g *syncGate) left() {
	g.mu.Lock()
	g.returned++
	if g.returned >= g.active {
		// The next round starts with the gate closed and no count carried
		// over: entered said every writer called Sync, returned that every
		// writer is past it, and latched served its round.
		g.entered, g.returned, g.latched = 0, 0, false
		g.cond.Broadcast()
		g.mu.Unlock()
		return
	}
	for !g.passAll && g.returned > 0 {
		g.cond.Wait()
	}
	g.mu.Unlock()
}

// finished counts one writer out of the rounds, for one that failed its
// write or sync: the writers that remain are what the gate and the barrier
// wait on, so one writer's error does not park the rest behind it. Called
// before entering — a writer whose write failed never reached its Sync — it
// latches the round's gate on the writers that remain, so their syncs do not
// wait for a writer that is out of the run; called after left it opens the
// barrier the writers that already returned wait at.
func (g *syncGate) finished() {
	g.mu.Lock()
	g.active--
	if g.returned == 0 && g.entered >= g.active {
		// Nobody is past the barrier yet, and every writer that remains has
		// entered: open the gate, and let each pass and roll the counts over
		// as it returns.
		g.latched = true
	}
	if g.returned > 0 && g.returned >= g.active {
		// The last active writer is past the barrier: open it for the rest.
		g.entered, g.returned, g.latched = 0, 0, false
	}
	g.cond.Broadcast()
	g.mu.Unlock()
}

// At holds each sync of a segment file until the round it was called in is
// whole: every active writer has entered it. Once latched, every sync of
// that round passes, whatever a correct journal batches them into. The seam
// calls it with no lock of its own held, so a gated sync stalls only itself.
func (g *syncGate) At(op string, key []byte) (delay time.Duration, tearAfter int, err error) {
	if op != journaltest.OpSync || !strings.HasSuffix(string(key), ".seg") {
		return 0, 0, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for !g.passAll && !g.latched {
		g.cond.Wait()
	}
	return 0, 0, nil
}

// TestCounted_SyncsPerWrite checks that a stream of writers, each calling
// Sync after every write, gets group commit: with the gate holding each
// sync until the whole round is in flight, the run issues at most one sync
// per four writes — four away from a design that syncs each write alone, and
// four away from one that groups the whole round.
//
// decision: the ratio is over handle writes, not over the writes the seam
// saw on the segment files. A journal may coalesce concurrent appends into
// one pwrite or split one record across several; counting pwrites would let
// the latter pass at one sync per two writes and fail the former, so the
// denominator is the count of successful WriteAt calls: one per writer per
// round. The numerator is the difference of the seam's sync counts over the
// segment files, taken before and after the run.
func TestCounted_SyncsPerWrite(t *testing.T) {
	f := journaltest.NewWith(t, func(s *journal.Settings) {
		s.Streams = 1
	})
	h := f.Share(0)

	segmentSyncs := func() (n int) {
		for _, name := range segmentNames(t, f) {
			n += f.Seam.CallsFor(journaltest.OpSync, name)
		}
		return n
	}
	syncsBefore := segmentSyncs()

	gate := newSyncGate(countedWriters)
	t.Cleanup(gate.close)
	f.Seam.SetFault(gate)

	var wg sync.WaitGroup
	errs := make(chan error, countedWriters)
	for w := range countedWriters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := journaltest.File(w)
			for range countedRounds {
				_, err := h.WriteAt(t.Context(), id, 0, make([]byte, countedWrite), 0)
				if err != nil {
					errs <- err
					gate.finished()
					return
				}
				gate.entering()
				err = h.Sync(t.Context(), id)
				gate.left()
				if err != nil {
					errs <- err
					gate.finished()
					return
				}
			}
			gate.finished()
		}()
	}
	wgDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(wgDone)
	}()
	select {
	case <-wgDone:
	case <-time.After(backgroundWait):
		gate.close()
		t.Fatalf("%d writers of the counted run did not return within %v: a writer is parked behind the gate", countedWriters, backgroundWait)
	}
	close(errs)
	f.Seam.SetFault(nil)
	for err := range errs {
		stub(t, "WriteAt/Sync", err)
		if err != nil {
			t.Fatalf("a writer of the counted run: %v", err)
		}
	}

	writes := countedWriters * countedRounds
	syncs := segmentSyncs() - syncsBefore
	if syncs*4 > writes {
		t.Fatalf("%d syncs for %d writes: one per %.1f writes, want at most one per four", syncs, writes, float64(writes)/float64(syncs))
	}
}

// countedStream is the size of one write of the streaming writer of the
// mixed-durability check: one sync interval of the stream is one of these,
// handed by the check to each of the small writer's syncs while they are in
// flight.
const countedStream = 1 << 20

// countedFiles are the streamed file sizes the mixed-durability check
// compares.
//
// decision: the check streams 64 MiB and the 256 MiB its row names, so its
// cost is disk, not time. The run's peak is about 400 MiB: the legs' content
// is released by nothing until the run ends, so the 64 MiB file's 32 rounds
// sit beside the 256 MiB file's, 320 one-MiB intervals with each round's
// small record added, 22 content segments of 16 MiB — writeRecordBytes(1 MiB)
// is 1,048,680, so a segment holds 15 of them — beside which the run at its
// worst point holds one spare, the headroom's segment and the repack
// reserve's. The race detector is no part of this: it watches memory, not
// disk. The sizes are the one place to lower if a CI runner cannot afford
// that disk — lowering deviates from the row's named size and needs a note
// in the PR.
var countedFiles = []int64{64 << 20, 256 << 20}

// segmentHeaderBytes is the fixed header at offset 0 of every segment, 48
// bytes, written when a stream takes the segment and made durable by the
// stream's next sync along with the first records after it. A sync right
// after a roll can therefore flush it beside the interval's records, so the
// bound carries one.
const segmentHeaderBytes = 48

// mixedFeeds is how many stream intervals the gate hands one of the small
// writer's syncs that keeps asking: the first is the interval one sync
// legitimately waits for — an append landing in flight — and the rest starve
// nothing: each is flushed by the asking sync itself, so each one past the
// first is waited-for bytes the row's bound has no room for. The cap exists
// so a design that keeps asking gives up once the stream stalls, and the
// window ends. It does not by itself name a waiting design wrong — the
// starving fake passes this suite, because under the gated feeder it never
// holds more than one fed interval unflushed, which the decision marker at
// the check states as its ceiling.
const mixedFeeds = 3

// mixedErrs carries a stream goroutine's first failure out of its rounds, so
// the main loop reads it within the bounded waits: values are read only by
// the winner, first-come-first-served, with no reordering needed because
// only one failure is reported.
type mixedErrs struct {
	value chan error
	read  bool
}

func newMixedErrs() *mixedErrs {
	return &mixedErrs{value: make(chan error, 1)}
}

// send reports err if nothing was reported before, and does not block.
func (e *mixedErrs) send(err error) {
	select {
	case e.value <- err:
	default:
	}
}

// take returns the failure the stream's goroutine reported, if one came in,
// without waiting for one.
func (e *mixedErrs) take() error {
	select {
	case err := <-e.value:
		return err
	default:
	}
	return nil
}

// fixtureMixedEnv drives runMixedRounds against a fixture's journal: the
// stream's appends and file-end syncs through the handle, the small writer's
// record and measured Sync through it, durability from the seam's per-name
// durable totals, and the segment names from the journal's directory. The
// gate as the seam's fault is what makes the journal's own fsyncs park.
type fixtureMixedEnv struct {
	t        *testing.T
	f        *journaltest.Fixture
	h        *journal.Handle
	smallID  journal.FileID
	streamID journal.FileID
	files    []int64 // the streamed files, in order

	// mu guards the stream's position — file, off and smalls — across the
	// goroutines that touch them: Append on the stream goroutine, WriteSmall
	// and the floors on the run's. The protocol owes the env a quiescent
	// stream before it reads; the choreography's plain-field case proves
	// that at the check's interval counts, where its floor assertion fails
	// any protocol that reads mid-append and the race detector reports the
	// overlaps a schedule exposes. This lock is the fixture env's own
	// insurance, so it cannot race in the journal's real shape either.
	mu     sync.Mutex
	file   int   // the file the stream is writing now
	off    int64 // the offset of its next interval
	smalls int   // small records written so far, each countedWrite bytes
}

// Install sets the gate as the seam's fault, so every sync of a segment file
// the journal issues parks where the protocol can hand it turns.
func (e *fixtureMixedEnv) Install(gate *journaltest.SimGate) {
	e.f.Seam.SetFault(gate)
}

// Append lands the stream's next interval — one countedStream write through
// the spare-retrying helper, as every reservation in this suite — and, on the
// interval that finishes a file, the file's closing sync, before reporting
// whether the stream has another interval. Each file end goes through Sync:
// the rules must make it durable, and a refusal for want of a spare was
// spared by the helper before it.
func (e *fixtureMixedEnv) Append() (more bool, err error) {
	e.mu.Lock()
	off, fileLen := e.off, e.files[e.file]
	e.mu.Unlock()
	if _, err := tryWrite(e.t, e.h, e.streamID, off, make([]byte, countedStream)); err != nil {
		return false, err
	}
	off += countedStream
	// The interval that ends a file runs the file's closing sync before
	// reporting whether any interval remains, so the closing sync lands
	// on this goroutine — the stream's — where the gate would park it.
	closing := off >= fileLen
	if closing {
		if err := e.h.Sync(e.t.Context(), e.streamID); err != nil {
			return false, err
		}
	}
	e.mu.Lock()
	e.off = off
	if closing {
		e.file++
		e.off = 0
	}
	hasNext := !closing || e.file < len(e.files)
	e.mu.Unlock()
	return hasNext, nil
}

// EndsFile reports whether the interval of index i is the last of its file:
// the intervals of the run count straight through both files.
func (e *fixtureMixedEnv) EndsFile(i int) bool {
	n := 0
	for _, size := range e.files {
		n += int(size / countedStream)
		if i == n-1 {
			return true
		}
	}
	return false
}

// Intervals is how many intervals both files take at countedStream each.
func (e *fixtureMixedEnv) Intervals() int {
	total := 0
	for _, size := range e.files {
		total += int(size / countedStream)
	}
	return total
}

// SyncStream runs the journal's own sync of the stream's file, on the run's
// goroutine with the gate disarmed: it passes through storage without
// parking, and returns once the journal has flushed what the file holds.
func (e *fixtureMixedEnv) SyncStream() error {
	return e.h.Sync(e.t.Context(), e.streamID)
}

// WriteSmall lands the small writer's next record at the head of its file,
// through the spare-retrying helper, pending into the measured window.
func (e *fixtureMixedEnv) WriteSmall() error {
	e.mu.Lock()
	at := int64(e.smalls) * countedWrite
	e.mu.Unlock()
	if _, err := tryWrite(e.t, e.h, e.smallID, at, make([]byte, countedWrite)); err != nil {
		return err
	}
	e.mu.Lock()
	e.smalls++
	e.mu.Unlock()
	return nil
}

// SyncSmall runs the small writer's measured Sync.
func (e *fixtureMixedEnv) SyncSmall() error {
	return e.h.Sync(e.t.Context(), e.smallID)
}

// DurableTotal sums the seam's per-name durable totals over the segment
// files: the write bytes successful syncs made durable, whatever name the
// file carried when they landed.
func (e *fixtureMixedEnv) DurableTotal() int64 {
	var total int64
	for _, name := range segmentNames(e.t, e.f) {
		total += e.f.Seam.DurableBytes(name)
	}
	return total
}

// SegmentNames lists the segment files in the journal's directory, sorted, so
// two listings compare equal only when the set has not changed.
func (e *fixtureMixedEnv) SegmentNames() []string {
	return segmentNames(e.t, e.f)
}

// The seal-time footprint of a rolled-over segment, per RFC 28 §2.2 and
// §2.5: the 32-byte seal marker just past the last record, one 72-byte
// footer entry per record the segment holds, and the 32-byte trailer.
const (
	sealMarkerBytes     = int64(32)
	catalogEntryBytes   = int64(72)
	catalogTrailerBytes = int64(32)
)

// catalogEntriesMost is the most records a 16 MiB segment of this run
// holds when a roll seals it. No sixteenth whole one-MiB interval ever
// fits: sixteen of them are 16 × 1,048,680 = 16,778,880 record bytes,
// past the 16,777,216-byte cap before the 48-byte header or any seal
// reserve is taken, so a segment holds fifteen whole intervals and so at
// most seventeen pieces of intervals: the fifteen whole, the RFC 28
// §2.2 split of the sixteenth's write at the cap — whose head lands in
// the sealed segment and whose tail opens the next — and, because a
// segment can open on an earlier split's tail, another split's head
// among its records, both pieces and wholes counted alike.
// The small records land one per armed round, and every armed round that
// measures lands at least two intervals — its own opening one and a fed
// one, whose own sync answered the arrival by feeding before the measure
// was read — so smalls sit under one per two intervals over a segment
// holding fifteen of them: eight. Seventeen pieces of intervals plus
// eight smalls is 25; the constant stays above that by a six-entry
// margin, above every count the window's fed-cap regime can add to a
// segment that packs whole records instead.
//
// The fakes of the counted suite model none of this split: their roll rule
// is a bare record-bytes fit against a small segCap, with the seal's
// reserve subtracted for nothing, so they pack whole numbers of intervals
// per segment and roll a little earlier than a reserve-aware journal. That
// only names what the protocol drives the same either way — the fakes
// exercise the window's choreography, not this bound, which the real
// fixture's own derivation carries alone.
const catalogEntriesMost = 25 + 6

// splitRecordBytes is the extra cost of a fed interval arriving as the
// write a roll split: RFC 28 §2.2 splits a write that does not fit what
// remains at the cap, into records one per segment, so its two pieces
// carry one record header, checksum and padding more than the whole
// record does — writeRecordBytes's per-record overhead twice over a body
// that has not grown. Every component of a segment is a multiple of
// eight — the 48-byte header, records padded to 8, the 32-byte marker
// and trailer, 72-byte entries — so the split point lands eight-aligned
// and the padding costs the same split either way: 96 + 8 = 104.
const splitRecordBytes = int64(104)

// WindowBound is what one measured window may make durable: the checked
// sync's own record, one fed stream interval — both as record bytes,
// headers and padding included — the header of a segment a roll just took,
// which the first sync after the roll flushes beside the records after it,
// and the seal-time footprint of the segment the roll sealed: its seal
// marker, its footer entry per record, and its trailer, which the seal's
// own sync makes durable inside the window. Removing either writer's record
// from the bound would fail a correct journal: a sync of the stream's active
// segment that the gate parked and the stream fed — one fed append exactly —
// is durably flushing that appended record too.
//
// The fed interval is counted whole here, plus the extra record the cap
// costs it when the roll split it: RFC 28 §2.2 splits a write that does not
// fit a segment's remainder at the cap, one record per segment, so a fed
// interval that crossed a roll lands as two pieces whose bodies sum to the
// write's own and whose two record headers cost one more than the whole
// record's single one — splitRecordBytes. Both pieces' footer rows sit in
// catalogEntriesMost (the first piece's is one of the sealed segment's,
// the second's one of the next's, whose entry-count share the next seal's
// own footprint covers), so the catalog term needs no extra for the split.
//
// The arithmetic, from writeRecordBytes — 96 + 8 + n, padded to 8:
//
//	segmentHeaderBytes                        48
//	writeRecordBytes(1 << 20)           1,048,680
//	splitRecordBytes                         104
//	writeRecordBytes(4096)                   4,200
//	sealMarkerBytes                           32
//	catalogEntryBytes x 31                 2,232
//	catalogTrailerBytes                       32
//	                                       -------
//	                                     1,055,328
func (e *fixtureMixedEnv) WindowBound() int64 {
	return segmentHeaderBytes + writeRecordBytes(countedStream) + splitRecordBytes + writeRecordBytes(countedWrite) +
		sealMarkerBytes + catalogEntryBytes*catalogEntriesMost + catalogTrailerBytes
}

// DurableFloor is what everything written so far must already have made
// durable once a Sync returned success: every interval the stream landed
// and every small record of a measured round, as record bytes. Segment
// headers flush beside the records and are deliberately left out, so a
// journal whose rolls wrote more headers than another stays inside the
// floor.
func (e *fixtureMixedEnv) DurableFloor() int64 {
	e.mu.Lock()
	file, off, smalls := e.file, e.off, e.smalls
	e.mu.Unlock()
	landed := 0
	for k := range e.files {
		if k < file {
			landed += int(e.files[k] / countedStream)
		}
	}
	landed += int(off / countedStream)
	return int64(landed)*writeRecordBytes(countedStream) + int64(smalls)*writeRecordBytes(countedWrite)
}

// TestCounted_MixedDurability checks that a small writer's Sync, measuring
// beside a streaming writer, makes at most one sync interval of the stream's
// appends durable beyond its own record, whatever the streamed file's size:
// the row's bound of RFC 1 §9.7, never growing with the file size, the cost
// §5.2's ponytail names.
//
// Each round hands the stream one opening interval and waits for it to land,
// then syncs the STREAM's file — a sync the rules must answer by flushing
// the segment, so the opening interval is durable and nothing of the
// stream's is left pending — and only then writes the small writer's own
// record, which stays pending into the measured window: the measured Sync
// that follows has a record of its own to answer for. With the gate armed
// around it, the fsyncs it issues on a segment park and are handed fed
// stream intervals, one per release, before they are freed, so the measure
// — the difference of the seam's per-name durable totals across the window
// — reads the bytes the sync chose to flush: its own record, at most one
// fed interval, and a roll's segment header, seal marker, footer and
// trailer. A sync that rides exactly one fed interval and stops passes;
// a sync that parks again is fed a second interval and breaches the bound,
// so the check catches it on its own. What the check cannot tell apart is
// narrower: a design that waits on the stream's backlog without ever
// parking a second time — the starving shape, below the gate's feeder — as
// the decision marker states.
//
// decision: the check cannot tell a small-writer sync that waits on the
// stream's growing backlog from one that does not, because the gate feeds
// the stream one interval at a time and only once the asking sync covered
// the previous one, so no backlog above one interval ever exists inside a
// measured window for any design, whatever its file size. Letting the
// stream run ungated instead fails correct journals: a flush waits for every
// dirty byte of the segment, so a correct sync covers whatever landed during
// its own wait, and a window in which two intervals landed measures two for
// a design that did nothing wrong. The deterministic counting this suite is
// built on and a backlog that grows mid-window are mutually exclusive on
// this seam. Accepted: the check still proves each measured sync covered
// no more than its own record, one fed interval and a roll's footprint —
// the new segment's header and the sealed segment's seal marker, footer
// and trailer, the terms WindowBound carries — and the choreography test —
// the same runMixedRounds against fakes of the correct and starving
// designs — fails each of those when the model drifts.
// Overturned by a seam that can let the stream append during a small sync
// while keeping the bytes the sync waited for separately observable, so a
// window can hold a grown backlog without crediting it to a sync that
// merely rode along.
//
// decision: the window skips the bound when the segment name set changed
// while it was open. What the seam can see is the set of names, not the
// roll itself: a spare renamed into place, or a seal that took a fresh
// segment, changes it, so the skip fires on those. A roll under RFC 28 §2.2
// takes an existing spare and changes no name, so if one lands inside a
// window the byte count the measure reads is the fed interval the roll's
// seal covered plus the roll's own footprint — the new segment's header
// and the sealed segment's seal marker, footer and trailer, each of which
// WindowBound carries at its stated arithmetic, so such a window still
// stays inside the bound — and its seal sync parks as a fed write's own
// and is released unfed. The
// vacuity guard fails a run whose every window was skipped, so a journal
// that never measures cannot pass on no evidence; it stays at that one
// sentence, because a stricter share would fail a correct journal whose
// every round legitimately rolled.
//
// decision: the stream's writes go through the spare-retrying write helper
// like every other reservation in this suite, and each stream file end goes
// through Sync. A spare the roll needs is made off the write path, so a
// correct journal can refuse the write until it is there, and the refusal is
// not the stream's failure. No measured round opens or feeds a file-end
// interval: the arming is skipped when the opening or any interval the
// window could feed ends a stream file, and the count that rule keys on is
// the stream's landed intervals — each fed one counted at its own
// confirmation, never at the round's pace — so the stream's own file-end
// Sync never meets the armed gate. The choreography's watcher cases, run at
// this check's own interval counts, fail the run if any closing sync ever
// does; without them the rule could drift back to a round-keyed count and
// stay green.
func TestCounted_MixedDurability(t *testing.T) {
	// The journal's maximum is sized from what the run allocates at its
	// worst point, with a margin over it: no record of either leg is
	// released or offloaded, so nothing leaves its segment before the run
	// ends. The 320 one-MiB intervals of the two files, with the small
	// records the rounds added beside them, take 22 content segments of
	// 16 MiB — writeRecordBytes(1 MiB) is 1,048,680, so a segment holds 15
	// intervals and still has 1,046,968 bytes left (16,777,216 − 48 −
	// 15 × 1,048,680), about 249 of the rounds' small records, far more
	// than the 15 the run actually appends there. One spare for the single
	// stream, the headroom's segment and the repack reserve's bring the
	// run's need to 25 segments, 400 MiB; the maximum names 28 so it
	// refuses no segment the run's own allocation asks for — a refusal
	// for want of a spare is a write failure a correct journal may report.
	f := journaltest.NewWithSegmentCap(t, 16<<20, func(s *journal.Settings) {
		s.Streams = 1
		s.MaxBytes = 28 * 16 * (1 << 20)
	})
	h := f.Share(0)

	runMixedRounds(t, &fixtureMixedEnv{
		t:        t,
		f:        f,
		h:        h,
		smallID:  journaltest.File(0),
		streamID: journaltest.File(1),
		files:    countedFiles,
	})
}
