package index

import (
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// walkFile adds f's pieces to out by their record's segment: the held ones to
// Held, the retained ones to Retained. The caller holds at least f's read lock.
func walkFile(f *file, out map[core.SegmentID]SegmentBytes) {
	for _, p := range f.held.pieces(nil) {
		b := out[p.Record.Segment]
		b.Held += p.Extent.Len
		out[p.Record.Segment] = b
	}
	for _, r := range f.retained.list(nil) {
		b := out[r.Record.Segment]
		b.Retained += r.Extent.Len
		out[r.Record.Segment] = b
	}
}

// walkSegments recomputes what every segment backs by visiting every shard and
// summing each entry's held and retained pieces by their record's segment: the
// slowest possible oracle, which the counters must equal without ever walking.
func walkSegments(t *testing.T, fs *files) map[core.SegmentID]SegmentBytes {
	t.Helper()
	out := map[core.SegmentID]SegmentBytes{}
	for i := range fs.shards {
		s := &fs.shards[i]
		s.mu.Lock()
		entries := make([]*file, 0, len(s.m))
		for _, f := range s.m {
			entries = append(entries, f)
		}
		s.mu.Unlock()
		for _, f := range entries {
			f.mu.RLock()
			walkFile(f, out)
			f.mu.RUnlock()
		}
	}
	return out
}

// checkSegments asserts, at the caller's step, that the counters equal the
// walk's recomputation: for every segment in the union of segs and the walk,
// the counter matches — zero where the walk has none — and neither field is
// negative or inconsistent with live. Catches: a hook missing on any path, a
// split or narrow counted wrong, a repoint not moving bytes between segments,
// a re-hold not moving bytes from retained to held, a counter going negative,
// a live figure that forgot a half of its update.
func checkSegments(t *testing.T, step int, fs *files, segs ...core.SegmentID) {
	t.Helper()
	walk := walkSegments(t, fs)
	seen := slices.Clone(segs)
	for seg := range walk {
		seen = append(seen, seg)
	}
	slices.Sort(seen)
	for _, seg := range slices.Compact(seen) {
		got := fs.segmentBytes(seg)
		if got.Held < 0 || got.Retained < 0 {
			t.Fatalf("step %d: segment %d counts %+v: a counter went negative", step, seg, got)
		}
		want := SegmentBytes{}
		if w, ok := walk[seg]; ok {
			want = w
		}
		if got != want {
			t.Fatalf("step %d: segment %d counts %+v, walk says %+v", step, seg, got, want)
		}
		// Between updates live is exactly what the segment backs: another
		// statement of the equality above, which separately catches a live
		// counter that forgot its increment or its decrement and so never
		// returns to zero for reclamation.
		if l := fs.segmentLive(seg); l != want.Held+want.Retained {
			t.Fatalf("step %d: segment %d counts live %d, want %d", step, seg, l, want.Held+want.Retained)
		}
	}
}

// winRec is one retaining write in an open sync window: the file that made it
// and its record.
type winRec struct {
	id core.FileID
	w  WindowRecord
}

// TestSegmentBytesMatchWalk is the "done when" test: the counters equal a
// recomputed walk after every step of a long random sequence, and never go
// negative. The sequence drives every path that does or could hold, cut,
// retain, re-hold, move or drop pieces — write, retaining write, fill, mark and
// release, truncate, punch, delete with settling, a window syncing, a window
// failing, repack of a segment, and retire of a segment that backs nothing — so
// a hook missing on any of them fails the comparison at the step that caused
// it.
func TestSegmentBytesMatchWalk(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 3373))
	var fs files
	fs.openBuckets(ver(0))
	ids := []core.FileID{fid(1), fid(2), fid(3), fid(4)}
	const rounds, steps = 200, 300 // ~60,000 steps, each checked against the walk

	nextSeg, nextVersion, nextOff := core.SegmentID(0), uint64(0), uint32(0)
	issued := map[core.SegmentID]bool{}
	var windows [2][]winRec
	// active draws a record in the active segment, opening a new segment now
	// and then, as an append stream filling segments does.
	active := func(activeSeg *core.SegmentID) RecordAt {
		if *activeSeg == 0 || rng.IntN(16) == 0 {
			nextSeg++
			*activeSeg = nextSeg
			nextOff = 0
			issued[nextSeg] = true
		}
		nextOff += 8
		return rec(*activeSeg, nextOff)
	}
	inRange := func(max, size int64) (int64, int64) {
		off := rng.Int64N(max)
		return off, min(1+rng.Int64N(size), max-off)
	}
	stepAll := 0

	for range rounds {
		var activeSeg core.SegmentID
		for range steps {
			stepAll++
			switch rng.IntN(12) {
			case 0, 1: // write; a retaining one joins one of the windows
				id := ids[rng.IntN(len(ids))]
				off, n := inRange(512, 64)
				retaining := rng.IntN(2) == 1
				var r RecordAt
				_ = fs.update(id, func(f *file) error {
					r = active(&activeSeg)
					nextVersion++
					if retaining {
						w := 0
						if len(windows[1]) > 0 || rng.IntN(2) == 0 {
							w = 1
						}
						windows[w] = append(windows[w], winRec{id, WindowRecord{Record: r, Version: ver(nextVersion)}})
						f.writeRetain(write(off, n, r, nextVersion))
					} else {
						f.write(write(off, n, r, nextVersion))
					}
					return nil
				})
			case 2: // fill of absent bytes
				id := ids[rng.IntN(len(ids))]
				off, n := inRange(512, 64)
				nextVersion++
				_ = fs.fill(id, Placed{Extent: ext(off, n), Record: active(&activeSeg), Version: ver(nextVersion)}, ver(nextVersion))
			case 3: // mark and release of offloaded spans; a refused one changes nothing
				id := ids[rng.IntN(len(ids))]
				off, n := inRange(512, 64)
				_ = fs.update(id, func(f *file) error {
					f.held.markOffloaded(ext(off, n), ver(0), ver(nextVersion))
					_, err := f.held.release(ext(off, n), nil)
					return err
				})
			case 4: // truncate
				id := ids[rng.IntN(len(ids))]
				size := rng.Int64N(512)
				nextVersion++
				_ = fs.update(id, func(f *file) error { f.truncate(size, ver(nextVersion)); return nil })
			case 5: // punch
				id := ids[rng.IntN(len(ids))]
				off, n := inRange(512, 64)
				nextVersion++
				_ = fs.update(id, func(f *file) error { return f.deallocate(ext(off, n), ver(nextVersion)) })
			case 6: // delete, then settle, so the marker leaves with the walk
				id := ids[rng.IntN(len(ids))]
				nextVersion++
				_ = fs.update(id, func(f *file) error { f.delete(ver(nextVersion)); return nil })
				fs.settleRemovals(id, ver(nextVersion))
			case 7: // a window syncs: what its records retained leaves
				wi := rng.IntN(2)
				for _, id := range ids {
					window := windows[wi]
					_ = fs.update(id, func(f *file) error {
						for _, wr := range window {
							if wr.id == id {
								f.retained.drop(whole, bySuperseder(wr.w.Version), nil)
							}
						}
						return nil
					})
				}
				windows[wi] = windows[wi][:0]
			case 8: // a window fails, as a unit, on the files that wrote it
				wi := rng.IntN(2)
				for _, id := range ids {
					window := slices.Clone(windows[wi])
					rng.Shuffle(len(window), func(i, j int) { window[i], window[j] = window[j], window[i] })
					_ = fs.update(id, func(f *file) error {
						recs := make([]WindowRecord, 0, len(window))
						for _, wr := range window {
							if wr.id == id {
								recs = append(recs, wr.w)
							}
						}
						f.failWindow(recs, nil)
						return nil
					})
				}
				windows[wi] = windows[wi][:0]
			case 9: // repack: a segment's held and retained pieces move to a fresh one
				var pool []core.SegmentID
				for seg := range issued {
					pool = append(pool, seg)
				}
				if len(pool) == 0 {
					break
				}
				src := pool[rng.IntN(len(pool))]
				nextSeg++
				to := nextSeg
				nextOff = 0
				issued[to] = true
				for _, id := range ids {
					_ = fs.update(id, func(f *file) error {
						// Collect the records of src this file holds from either
						// side, then move each whole, as repack does.
						var froms []RecordAt
						f.held.visit(whole, func(p Piece) bool {
							if p.Record.Segment == src && !slices.Contains(froms, p.Record) {
								froms = append(froms, p.Record)
							}
							return true
						})
						for _, r := range f.retained.in(src, nil) {
							if !slices.Contains(froms, r.Record) {
								froms = append(froms, r.Record)
							}
						}
						for _, from := range froms {
							f.held.repoint(whole, from, rec(to, 0), 0)
							f.retained.repoint(whole, from, rec(to, 0), 0)
						}
						return nil
					})
					_ = fs.update(id, func(f *file) error {
						_ = f.recorded(to, ext(0, 0), Numbers{}, 1, id)
						return nil
					})
				}
			case 10: // retire of a segment the walk shows backs nothing
				// Never the active segment: an append into a retired id is
				// what the retire contract calls a caller bug, and this test
				// is its caller.
				walk := walkSegments(t, &fs)
				var empty []core.SegmentID
				for seg := range issued {
					if seg == activeSeg {
						continue
					}
					if b, ok := walk[seg]; !ok || b == (SegmentBytes{}) {
						if fs.segmentBytes(seg) == (SegmentBytes{}) {
							empty = append(empty, seg)
						}
					}
				}
				if len(empty) == 0 {
					break
				}
				seg := empty[rng.IntN(len(empty))]
				for _, id := range ids {
					_ = fs.update(id, func(f *file) error { f.segmentGone(seg); return nil })
				}
				// The segment may never have entered the table — a fill can be
				// masked or refused after its record was drawn — in which case
				// retire reports it absent; either way it must report no leftover.
				if b, _ := fs.retireSegment(seg); b != (SegmentBytes{}) {
					t.Fatalf("step %d: retire of segment %d returned leftover %+v", stepAll, seg, b)
				}
				// No draw lands in the retired id again: a later round's first
				// write opens a fresh active segment, and repack only draws
				// segments still issued.
				delete(issued, seg)
			case 11: // release without a mark: refused and changes nothing
				id := ids[rng.IntN(len(ids))]
				off, n := inRange(512, 64)
				_ = fs.update(id, func(f *file) error {
					_, err := f.held.release(ext(off, n), nil)
					return err
				})
			}
			segs := make([]core.SegmentID, 0, len(issued))
			for seg := range issued {
				segs = append(segs, seg)
			}
			checkSegments(t, stepAll, &fs, segs...)
		}
	}
	checkSegments(t, -1, &fs)
	if len(walkSegments(t, &fs)) == 0 {
		t.Fatal("the sequence left every segment backing nothing: it never exercised a counter")
	}
}

// TestRetireSegment: retire of an emptied segment reports zeros and true; a
// second retire reports false and the segment reads zero; retiring one segment
// leaves another's counters intact; and a retire of a segment still backing
// bytes returns those bytes, the caller-visible leftover. Catches retire not
// removing, a double retire reporting presence, and retire hitting the wrong
// entry.
func TestRetireSegment(t *testing.T) {
	var fs files
	id := fid(1)
	_ = fs.update(id, func(f *file) error {
		f.write(write(0, 8, rec(1, 0), 1))
		f.write(write(8, 8, rec(2, 0), 2))
		return nil
	})
	if b := fs.segmentBytes(1); b != (SegmentBytes{Held: 8}) {
		t.Fatalf("segment 1 counts %+v, want 8 held", b)
	}
	if b := fs.segmentBytes(2); b != (SegmentBytes{Held: 8}) {
		t.Fatalf("segment 2 counts %+v, want 8 held", b)
	}

	// Empty segment 1 — release its piece — then retire it.
	_ = fs.update(id, func(f *file) error {
		f.held.markOffloaded(ext(0, 8), ver(1), ver(1))
		_, err := f.held.release(ext(0, 8), nil)
		return err
	})
	if b, ok := fs.retireSegment(1); !ok || b != (SegmentBytes{}) {
		t.Fatalf("retire of the emptied segment 1 = %+v, %v, want zero, true", b, ok)
	}
	if b := fs.segmentBytes(1); b != (SegmentBytes{}) {
		t.Fatalf("a retired segment reads %+v, want zero", b)
	}
	if b, ok := fs.retireSegment(1); ok || b != (SegmentBytes{}) {
		t.Fatalf("a second retire of segment 1 = %+v, %v, want zero, false", b, ok)
	}
	if b := fs.segmentBytes(2); b != (SegmentBytes{Held: 8}) {
		t.Fatalf("retiring segment 1 disturbed segment 2: %+v, want 8 held", b)
	}

	// A retire of a segment still backing bytes returns the leftover.
	if b, ok := fs.retireSegment(2); !ok || b != (SegmentBytes{Held: 8}) {
		t.Fatalf("retire of the still-occupied segment 2 = %+v, %v, want 8 held, true", b, ok)
	}
	if b := fs.segmentBytes(2); b != (SegmentBytes{}) {
		t.Fatalf("a retired segment reads %+v, want zero", b)
	}
}

// TestSegmentBytesConcurrentFiles: eight goroutines, each owning its own file,
// write and overwrite into a shared set of segments. The writers all hold the
// same round schedule — every worker's round r writes into the same fresh
// segment — and are released together behind one start channel, so the eight
// adds of each new segment collide by construction and the copy-on-write
// creation race is taken deliberately, not left to scheduling luck. Readers
// keep asking what the segments back throughout. At the end the counters equal
// the walk. Catches a non-atomic counter and an entry lost to two concurrent
// creations, under -race.
func TestSegmentBytesConcurrentFiles(t *testing.T) {
	var fs files
	const workers, rounds = 8, 300
	done := make(chan struct{})
	var readers, writers sync.WaitGroup
	for range workers {
		readers.Go(func() { // reader of the readouts, while the writers churn the table
			for i := 1; ; i++ {
				fs.segmentBytes(core.SegmentID(i%2*4 + 1))
				select {
				case <-done:
					return
				default:
				}
			}
		})
	}
	start := make(chan struct{})
	for w := range workers {
		writers.Go(func() {
			id := fid(byte(w + 1))
			<-start // released with all eight writers together
			for r := range rounds {
				seg := core.SegmentID(r%2*4 + 1) // every writer's round r targets the same segment
				_ = fs.update(id, func(f *file) error {
					_ = f.recorded(seg, ext(0, 0), Numbers{}, 1, id)
					f.write(write(int64((r+w)%8)*8, 8, rec(seg, uint32(r)*4), uint64(r+1)))
					return nil
				})
			}
		})
	}
	close(start) // one release: all writers begin together, and collide every round
	writers.Wait()
	close(done) // the readers stop only once every writer is done
	readers.Wait()
	segs := []core.SegmentID{1, 5}
	checkSegments(t, -1, &fs, segs...)
	if len(walkSegments(t, &fs)) == 0 {
		t.Fatal("the walk found nothing: the writers wrote nothing")
	}
}

// TestSegmentBytesNeverDropsBelowWalkWhileAMutationRuns: the counters are read
// with no lock against a file's own lock, and a mutation that keeps the walk
// still — a put of a piece over the whole span, a mark that splits one in
// place, a repoint of a retained piece to another record of its own segment —
// must not let a concurrent reader see less than the walk while it runs, or
// reclamation reading a zero could name a live segment. The never-below bound
// is asserted on live alone: the two bytes() fields are not a consistent
// pair, and neither is any sum of two segments' counters, so a pair read can
// legitimately miss a byte mid-move. A mutation that genuinely churns the
// walk, a failing window, is checked by walk equality after it ends. Catches
// the subtract-before-add ordering of every accounted hook.
func TestSegmentBytesNeverDropsBelowWalkWhileAMutationRuns(t *testing.T) {
	var fs files
	id := fid(1)
	// Segment 1 backs 512 bytes in 64 pieces: every put over the whole span
	// cuts 64 accounted pieces before it holds its own, so a subtract-first
	// order dips the counters for the whole cut, not for a moment; the reader
	// needs to see only one such dip to name the ordering bug. A mark of the
	// first piece does the same within one field.
	const pieces = 64
	_ = fs.update(id, func(f *file) error {
		for i := range int64(pieces) {
			f.write(write(i*8, 8, rec(1, uint32(i)*8), 1))
		}
		return nil
	})
	// Segment 2 backs 16 bytes, held and retained, and a repoint moves the
	// retained piece to another record of the same segment and back: the
	// edit removes the piece from the counters and puts it back inside one
	// call, so a subtract-first order there dips the segment's live for the
	// whole gap — while an order that re-adds first never does, whatever the
	// two steps' timing. One segment, so the read is one atomic and nothing
	// tears.
	seg2 := core.SegmentID(2)
	_ = fs.update(id, func(f *file) error {
		f.write(write(65536, 8, rec(seg2, 0), 1))
		f.writeRetain(write(65536, 8, rec(seg2, 100), 2)) // retains the piece at rec(2,0)
		return nil
	})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() { // writer: puts a piece over every held one, cut before add
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = fs.update(id, func(f *file) error {
				f.held.put(Piece{Extent: ext(0, pieces*8), Record: rec(1, 4096), Version: ver(1)})
				return nil
			})
		}
	})
	wg.Go(func() { // writer: marks and unmarks the same piece, splitting it twice
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = fs.update(id, func(f *file) error {
				f.held.markOffloaded(ext(0, 8), ver(1), ver(1))
				return nil
			})
		}
	})
	wg.Go(func() { // writer: moves the retained piece to another record of seg 2 and back
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = fs.update(id, func(f *file) error {
				f.retained.repoint(ext(65536, 8), rec(seg2, 0), rec(seg2, 512), 0)
				f.retained.repoint(ext(65536, 8), rec(seg2, 512), rec(seg2, 0), 0)
				return nil
			})
		}
	})
	wg.Go(func() { // reader: live, against the bytes the mutations never displace
		for {
			select {
			case <-stop:
				return
			default:
			}
			// The two bytes() fields are not a consistent pair, so only live
			// — one atomic per segment — can be held to a never-below bound
			// while a mutation runs.
			if l := fs.segmentLive(1); l < pieces*8 {
				t.Errorf("a concurrent update dropped segment 1 to live %d, below the %d bytes it backs", l, pieces*8)
				return
			}
			if l := fs.segmentLive(seg2); l < 16 {
				t.Errorf("a concurrent repoint dropped segment 2 to live %d, below the 16 bytes it backs", l)
				return
			}
		}
	})
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
	// The churn is over; the counters must be exactly the walk again.
	checkSegments(t, -1, &fs, 1, seg2)
}

// TestFailWindowNeverDipsASegmentItDoesNotWrite: failing a window lowers only
// the failed record's segment, which reclamation cannot touch until the window
// resolves; the superseded segments' bytes move from retained back to held, so
// their live must never dip below what they back while the failure runs. A
// concurrent reader asserts it throughout.
func TestFailWindowNeverDipsASegmentItDoesNotWrite(t *testing.T) {
	var fs files
	id := fid(1)
	// A in segment 1; a retaining write B in segment 2 over it. Failing B's
	// window returns A to held, and segment 1 backs 8 bytes the whole time.
	_ = fs.update(id, func(f *file) error {
		f.writeRetain(write(0, 8, rec(1, 0), 1))
		f.writeRetain(write(0, 8, rec(2, 0), 2)) // supersedes all of A
		return nil
	})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() { // writer: fails B's window, over and over
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = fs.update(id, func(f *file) error {
				f.failWindow([]WindowRecord{{Record: rec(2, 0), Version: ver(2)}}, nil)
				f.writeRetain(write(0, 8, rec(2, 0), 2)) // re-supersede, as a retry does
				return nil
			})
		}
	})
	wg.Go(func() { // reader: segment 1 must back its 8 bytes throughout
		for {
			select {
			case <-stop:
				return
			default:
			}
			if l := fs.segmentLive(1); l < 8 {
				t.Errorf("failing B's window dipped segment 1 to live %d, below the 8 bytes it backs", l)
				return
			}
		}
	})
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
	checkSegments(t, -1, &fs, 1, 2)
}

// TestFailWindowSettleNeverDipsASegmentItDoesNotWrite: the settle pass at the
// end of failWindow removes a segment's retained pieces in one edit and puts
// them back through add, so with the subtractions applied as the edit ends the
// segment reads as having lost what it backs until the last add returns — B no
// longer held, A stays retained by a record of B's window, so each failWindow
// settles A again. A concurrent reader tracks the low-water mark of the
// segment's live; the pass must never take it below what the segment backs
// throughout.
func TestFailWindowSettleNeverDipsASegmentItDoesNotWrite(t *testing.T) {
	var fs files
	id := fid(1)
	b := WindowRecord{Record: rec(2, 0), Version: ver(2)}
	_ = fs.update(id, func(f *file) error {
		f.write(write(0, 8, rec(1, 0), 1))
		f.writeRetain(write(0, 8, b.Record, 2)) // seg 1: 8 retained by v2
		f.truncate(0, ver(3))                   // B no longer held; A stays retained
		return nil
	})
	stop := make(chan struct{})
	var low atomic.Int64
	low.Store(8)
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = fs.update(id, func(f *file) error { f.failWindow([]WindowRecord{b}, nil); return nil })
		}
	})
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if l := fs.segmentLive(1); l < low.Load() {
				low.Store(l)
			}
		}
	})
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	if l := low.Load(); l < 8 {
		t.Fatalf("settle pass dipped segment 1 to live %d, below the 8 bytes it backs throughout", l)
	}
}

// TestSegmentAccountingAllocatesNothing: the accounting on the steady-state
// hot path — a write into a live segment, and a query — must take no lock and
// allocate nothing. The file keeps a second piece that the overwrites never
// touch, so the tree never empties: an insert into an empty tree allocates its
// root, which is the B-tree's, not the accounting's. Catches the hot path
// taking the copy path or boxing a segment id into an interface.
func TestSegmentAccountingAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under -race")
	}
	var fs files
	id := fid(1)
	_ = fs.update(id, func(f *file) error {
		f.write(write(0, 4, rec(1, 0), 1))
		f.write(write(64, 4, rec(1, 8), 1))
		return nil
	})
	v := uint64(2)
	if got := testing.AllocsPerRun(1000, func() {
		_ = fs.update(id, func(f *file) error {
			f.write(write(0, 4, rec(1, 64), v))
			v++
			return nil
		})
	}); got != 0 {
		t.Fatalf("a steady-state overwrite allocated %v times, want 0", got)
	}
	var b SegmentBytes
	if got := testing.AllocsPerRun(1000, func() {
		b = fs.segmentBytes(1)
	}); got != 0 {
		t.Fatalf("segmentBytes allocated %v times, want 0", got)
	}
	if b.Held != 8 {
		t.Fatalf("after the overwrites segment 1 holds %d, want 8", b.Held)
	}
}
