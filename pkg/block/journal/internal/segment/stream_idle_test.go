package segment

import (
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// idleClock is a clock the idle checks drive by hand: Elapsed moves only when
// a test advances it, and every After channel is closed by the Advance that
// reaches its deadline, never by real time. It is the shape of the shared
// TestClock, written here because the journal's import rule keeps the
// sim package out of the tree's internal tests. Pending reports the waits a
// test can release with an Advance: a check that needs to know the worker's
// wait is armed before it advances waits on it.
type idleClock struct {
	mu      sync.Mutex
	start   time.Time
	mono    time.Duration
	pending []idleTimer
}

// idleTimer is one After channel not yet closed.
type idleTimer struct {
	due time.Duration
	ch  chan struct{}
}

// newIdleClock returns a clock whose Now starts at the Unix origin and whose
// Elapsed starts at zero.
func newIdleClock() *idleClock {
	return &idleClock{start: time.Unix(0, 0)}
}

// Now returns the wall time: the origin plus everything Advance has moved it.
func (c *idleClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.start.Add(c.mono)
}

// Elapsed returns how far Advance has moved the clock since it was made.
func (c *idleClock) Elapsed() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}

// After returns a channel that is closed once the clock has moved d past its
// current Elapsed. A d of zero or less returns a channel already closed.
func (c *idleClock) After(d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	if d <= 0 {
		close(ch)
		return ch
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, idleTimer{due: c.mono + d, ch: ch})
	return ch
}

// Pending returns how many channels from After are not yet closed.
func (c *idleClock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

// Advance moves the clock by d and closes every After channel whose deadline
// it reaches. A negative d panics.
func (c *idleClock) Advance(d time.Duration) {
	if d < 0 {
		panic("segment test: the idle clock advanced by a negative duration")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mono += d
	for i := 0; i < len(c.pending); {
		if c.pending[i].due <= c.mono {
			close(c.pending[i].ch)
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			continue
		}
		i++
	}
}

// idleOver builds one idle-seal rig: a seam, a ledger, a maker, a started
// pool with two content spares, a cache, and a stream over them whose idle
// stamp the returned clock already watches, so every append from the first
// stamps the time the checks read. The pool's two spares are awaited before
// the build returns, so the first append never races the refill.
func idleOver(t *testing.T, cap int64) (*Stream, *journaltest.Seam, *Set, *idleClock) {
	t.Helper()

	seam, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	set := NewSet(cap, nil)
	maker := NewMaker(seam.FS(), set, 1<<62, 0, nil)
	pool := NewPool(maker, Targets{Content: 2}, nil)
	t.Cleanup(func() { pool.Stop() })
	pool.Start()
	for range 1 << 20 {
		if pool.Counters().Spares == 2 {
			break
		}
		runtime.Gosched()
	}
	if c := pool.Counters(); c.Spares != 2 {
		t.Fatalf("the pool never readied its two content spares: %+v", c)
	}
	cache := NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(8))
	t.Cleanup(func() {
		if err := cache.Shutdown(); err != nil {
			t.Errorf("the cache's Shutdown: %v", err)
		}
	})
	s := NewStream(1, probeIdentity, set, pool, cache)
	t.Cleanup(func() { _ = s.Close() })
	c := newIdleClock()
	s.watchIdle(c)
	return s, seam, set, c
}

// idleWrite returns the header and body of one write record of n payload
// bytes at file offset off, under the probe identity, with a version no
// other idle check's record carries.
func idleWrite(n int, off int64) (format.RecordHeader, format.RecordBody) {
	return format.RecordHeader{
		Kind:    format.KindWrite,
		File:    probeFileID,
		Extent:  core.Extent{Off: off, Len: int64(n)},
		Version: core.Version{Counter: idleSeq + uint64(off) + 1},
		Tag:     7,
	}, format.RecordBody{Payload: make([]byte, n), MTime: 1720000000000000000}
}

// idleSeq keeps the idle checks' versions apart from the other tests' own.
var idleSeq uint64 = 1 << 40

// idleAppend writes one record of n payload bytes at file offset off through
// s, failing the test on a refusal, and returns where it landed.
func idleAppend(t *testing.T, s *Stream, n int, off int64) Placed {
	t.Helper()

	h, body := idleWrite(n, off)
	p, err := s.Append(h, body)
	if err != nil {
		t.Fatalf("Append of %d bytes at %d: %v", n, off, err)
	}
	return p
}

// idleState reads the stream's seal state under its lock.
func idleState(s *Stream) sealState {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	return s.sealState
}

// idleLastAppend reads the stream's last-append stamp under its lock.
func idleLastAppend(s *Stream) time.Duration {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	return s.lastAppend
}

// TestStreamIdleDueRefusesEveryStreamThatIsNoCandidate checks idleDue's
// negative verdicts, each of which must answer (false, -1) — the waits the
// worker ignores rather than arms: a stream that has taken no segment, a
// candidate whose bytes beyond the last catalogued point equal the threshold
// exactly, a sealed stream, an ended stream and a closed stream. It catches
// a candidate test that seals one of these, and a threshold compared with
// >= where the rule's own words are "pass".
func TestStreamIdleDueRefusesEveryStreamThatIsNoCandidate(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s, _, _, c := idleOver(t, 64<<10)

	// No segment yet: never a candidate.
	c.Advance(2 * core.IdleSealAge)
	if due, wait := s.idleDue(c.Elapsed(), 0); due || wait != -1 {
		t.Fatalf("idleDue on a stream with no segment = (%v, %d), want (false, -1)", due, wait)
	}

	// One record lands: the bytes beyond the last catalogued point are its
	// whole padded size, so a threshold equal to it names a stream the
	// rule leaves open, and one 8 below seals it.
	w := idleAppend(t, s, 64, 0)
	c.Advance(2 * core.IdleSealAge)
	if due, wait := s.idleDue(c.Elapsed(), w.Loc.Len); due || wait != -1 {
		t.Fatalf("idleDue with the bytes at exactly the threshold = (%v, %d), want (false, -1)", due, wait)
	}
	if due, _ := s.idleDue(c.Elapsed(), w.Loc.Len-8); !due {
		t.Fatal("idleDue with the bytes past the threshold reports not due: the rig's stream is live")
	}

	// Sealed: never a candidate.
	if _, err := s.Seal(false); err != nil {
		t.Fatalf("the explicit Seal: %v", err)
	}
	if due, wait := s.idleDue(c.Elapsed(), w.Loc.Len-8); due || wait != -1 {
		t.Fatalf("idleDue on a sealed stream = (%v, %d), want (false, -1)", due, wait)
	}

	// Ended, staged as a failure's own state: never a candidate.
	s.appendMu.Lock()
	s.sealState = sealEnded
	s.appendMu.Unlock()
	if due, wait := s.idleDue(c.Elapsed(), w.Loc.Len-8); due || wait != -1 {
		t.Fatalf("idleDue on an ended stream = (%v, %d), want (false, -1)", due, wait)
	}

	// Closed for real, with the seal state back to open: never a candidate.
	// The ref Close releases keeps the cache's Shutdown from waiting on it.
	s.appendMu.Lock()
	s.sealState = sealOpen
	s.appendMu.Unlock()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if due, wait := s.idleDue(c.Elapsed(), w.Loc.Len-8); due || wait != -1 {
		t.Fatalf("idleDue on a closed stream = (%v, %d), want (false, -1)", due, wait)
	}
}

// TestStreamIdleDueAnswersTheExactRemainingWait checks the wait of a
// candidate not yet due: it is the idle age less the time since the stream's
// last append, exactly. It catches an idle time measured from the first
// append or from the worker's start, which would answer a shorter wait and
// seal a stream still taking appends.
func TestStreamIdleDueAnswersTheExactRemainingWait(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s, _, _, c := idleOver(t, 64<<10)
	// Two records, so the stream holds 128 payload bytes past its header:
	// a candidate at any positive threshold below that.
	idleAppend(t, s, 64, 0)
	idleAppend(t, s, 64, 64)
	c.Advance(2 * time.Second)

	due, wait := s.idleDue(c.Elapsed(), 8)
	if due {
		t.Fatal("idleDue reports due 2 s after the last append")
	}
	if want := core.IdleSealAge - 2*time.Second; wait != want {
		t.Fatalf("idleDue's remaining wait = %d, want %d", wait, want)
	}
}

// TestStreamIdleDueComesAtExactlyTheIdleAge checks the time bound's edge: a
// candidate idle for exactly the idle age is due, and one a nanosecond shy
// of it is not, with its wait the nanosecond. It catches a > where the
// rule's "for 30 s" wants >=.
func TestStreamIdleDueComesAtExactlyTheIdleAge(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s, _, _, c := idleOver(t, 64<<10)
	idleAppend(t, s, 64, 0)
	c.Advance(core.IdleSealAge - time.Nanosecond)
	if due, wait := s.idleDue(c.Elapsed(), 8); due || wait != time.Nanosecond {
		t.Fatalf("idleDue a nanosecond before the idle age = (%v, %d), want (false, 1ns)", due, wait)
	}
	c.Advance(time.Nanosecond)
	if due, wait := s.idleDue(c.Elapsed(), 8); !due || wait != 0 {
		t.Fatalf("idleDue at exactly the idle age = (%v, %d), want (true, 0)", due, wait)
	}
}

// TestStreamWatchIdleStampsAWatchingStreamJustActive checks watchIdle's
// stamp: a clock installed after an append reads the stream as just active,
// so the idle time counts from the moment the clock starts watching — not
// from the earlier append. It catches a sealer that seals a segment as stale
// the moment it starts watching it.
func TestStreamWatchIdleStampsAWatchingStreamJustActive(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s, _, _, c := idleOver(t, 64<<10)
	idleAppend(t, s, 64, 0)
	c.Advance(10 * time.Second)
	if due, wait := s.idleDue(c.Elapsed(), 8); due || wait != core.IdleSealAge-10*time.Second {
		t.Fatalf("the pre-watch idleDue = (%v, %d), want (false, %d)", due, wait, core.IdleSealAge-10*time.Second)
	}

	// A second clock, installed at its own later reading of Elapsed: the
	// stream counts as just active from that reading.
	later := newIdleClock()
	later.Advance(11 * time.Second)
	s.watchIdle(later)
	if due, wait := s.idleDue(later.Elapsed(), 8); due || wait != core.IdleSealAge {
		t.Fatalf("the post-watch idleDue = (%v, %d), want (false, the whole idle age)", due, wait)
	}
	if got, want := idleLastAppend(s), later.Elapsed(); got != want {
		t.Fatalf("the last-append stamp after watchIdle = %d, want the watch's own %d", got, want)
	}
}

// TestStreamARefusedAppendDoesNotMoveTheStamp checks that only an append
// that reserves stamps the stream: an append refused after the pre-lock
// checks — one that wrote nothing — leaves the last-append stamp exactly
// where it was, so a streamClosing spate of refusals cannot delay a seal.
func TestStreamARefusedAppendDoesNotMoveTheStamp(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s, _, _, c := idleOver(t, 64<<10)
	idleAppend(t, s, 64, 0)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	c.Advance(40 * time.Second)

	h, body := idleWrite(64, 64)
	if _, err := s.Append(h, body); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("the refused Append's error = %v, want ErrStreamClosed", err)
	}
	if got := idleLastAppend(s); got != 0 {
		t.Fatalf("the last-append stamp after a refused append = %d, want 0", got)
	}
}

// TestStreamAnAppendThatReservedAfterThePeekKeepsTheSegmentOpen checks
// sealIdle's re-check under the hold: idleDue reports due, an append then
// reserves — beating the seal — and sealIdle answers (false, {}, nil) with
// the segment still open. It catches a seal decided on a stale peek, which
// would write its marker over bytes a later append reserved.
func TestStreamAnAppendThatReservedAfterThePeekKeepsTheSegmentOpen(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s, _, set, c := idleOver(t, 64<<10)
	w := idleAppend(t, s, 64, 0)
	threshold := w.Loc.Len - 8
	c.Advance(2 * core.IdleSealAge)
	if due, _ := s.idleDue(c.Elapsed(), threshold); !due {
		t.Fatal("the stream is not due before the winning append")
	}

	// The append that beats the check: it reserves past the peek, stamping
	// the clock's own now as its arrival.
	idleAppend(t, s, 64, 64)

	sealed, res, err := s.sealIdle(c.Elapsed(), threshold)
	if sealed || err != nil || res != (SealResult{}) {
		t.Fatalf("sealIdle after the winning append = (%v, %+v, %v), want (false, {}, nil)", sealed, res, err)
	}
	if state := idleState(s); state != sealOpen {
		t.Fatalf("the stream's seal state after the refused idle seal = %v, want open", state)
	}
	if state, err := set.State(w.Loc.Segment); err != nil || state != Active {
		t.Fatalf("the segment's ledger state after the refused idle seal = (%d, %v), want Active and nil", state, err)
	}
}

// TestStreamSealIdleSealsADueSegmentEarly checks the seal itself: a due
// candidate, sealed by sealIdle, is sealed early — the tail truncated, the
// result's size below the cap — and the stream stays on it, so the next
// append rolls onto a successor naming it as predecessor. It catches an
// idle seal that keeps the tail, or one that takes the spare itself.
func TestStreamSealIdleSealsADueSegmentEarly(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s, _, _, c := idleOver(t, 64<<10)
	w := idleAppend(t, s, 64, 0)
	c.Advance(2 * core.IdleSealAge)

	sealed, res, err := s.sealIdle(c.Elapsed(), w.Loc.Len-8)
	if !sealed || err != nil {
		t.Fatalf("sealIdle on a due stream = (%v, %v), want (true, nil)", sealed, err)
	}
	if res.TailErr != nil {
		t.Fatalf("the idle seal's truncation failed: %v", res.TailErr)
	}
	if res.Size >= 64<<10 {
		t.Fatalf("the idle seal's size = %d, want below the cap %d", res.Size, int64(64<<10))
	}
	if res.Marker != w.Loc.Len+format.SegmentHeaderSize {
		t.Fatalf("the idle seal's marker = %d, want the last record's end %d",
			res.Marker, w.Loc.Len+format.SegmentHeaderSize)
	}
	if state := idleState(s); state != sealSealed {
		t.Fatalf("the stream's seal state after the idle seal = %v, want sealed", state)
	}

	// The next append rolls onto a successor naming the sealed segment.
	p := idleAppend(t, s, 64, 64)
	if p.Loc.Segment == w.Loc.Segment {
		t.Fatal("the append after an idle seal landed in the sealed segment")
	}
	raw := make([]byte, format.SegmentHeaderSize)
	if _, err := s.ReadAt(p.Loc.Segment, raw, 0); err != nil {
		t.Fatalf("ReadAt of the successor's header: %v", err)
	}
	hdr, err := format.DecodeSegmentHeader(raw, probeIdentity)
	if err != nil {
		t.Fatalf("the successor's header does not decode: %v", err)
	}
	if hdr.Predecessor != w.Loc.Segment {
		t.Fatalf("the successor's predecessor = %d, want the idle-sealed segment %d", hdr.Predecessor, w.Loc.Segment)
	}
}

// TestStreamSealIdleRefusesAStreamSomeoneElseSealed checks the race's other
// side: an explicit Seal that ran first leaves the stream sealed, and
// sealIdle then answers (false, {}, nil) rather than writing a second
// footer over one already durable.
func TestStreamSealIdleRefusesAStreamSomeoneElseSealed(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s, _, _, c := idleOver(t, 64<<10)
	w := idleAppend(t, s, 64, 0)
	if _, err := s.Seal(false); err != nil {
		t.Fatalf("the explicit Seal: %v", err)
	}
	c.Advance(2 * core.IdleSealAge)
	sealed, res, err := s.sealIdle(c.Elapsed(), w.Loc.Len-8)
	if sealed || err != nil || res != (SealResult{}) {
		t.Fatalf("sealIdle after an explicit seal = (%v, %+v, %v), want (false, {}, nil)", sealed, res, err)
	}
}
