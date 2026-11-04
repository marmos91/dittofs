package segment_test

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"math"
	"runtime"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// rollEnv is one environment whose stream rolls: a seam, a ledger, a maker,
// a started pool, a cache and the stream that takes its segments from the
// pool, the shape every roll check starts from.
type rollEnv struct {
	seam   *journaltest.Seam
	set    *segment.Set
	maker  *segment.Maker
	pool   *segment.Pool
	cache  *segment.Descriptors
	budget *core.DescriptorBudget
	s      *segment.Stream
}

// stream returns the env as the stream checks' own streamEnv, so their
// helpers run over the roll's cache and stream too.
func (e rollEnv) stream(t *testing.T) streamEnv {
	t.Helper()

	return streamEnv{seam: e.seam, set: e.set, maker: e.maker, cache: e.cache, budget: e.budget, s: e.s}
}

// rollTargetsMax is the maximum that leaves every class's make standing:
// the sum of the target counts, so the worker fills each class once and the
// take a test hands out, or that a roll makes, dips its class for good — the
// refill is refused by the ledger before any storage is touched, which makes
// a refusal's seam counts stand on the stream alone. A check that needs the
// refill to keep serving rolls passes the open streamMax instead.
func rollTargetsMax(cap int64, targets segment.Targets) int64 {
	return int64(targets.Content+targets.Headroom+targets.Reserve) * cap
}

// rollBuild makes one rolling environment over a fresh seam and ledger: the
// pool over the seam's maker and the given targets, started so its worker
// keeps them ready, and a stream that takes its segments from it. The maker's
// maximum is rollMax, open, so the refill keeps serving a stream that rolls
// again and again; a check that needs a class it drains to stay drained
// builds over rollPin instead.
//
// setup, when given, runs before the pool starts, so a fault it arms is up
// for the first make. The teardown runs in the reverse order: the stream
// releases its refs before the cache's Shutdown, and the pool's Stop joins
// the worker last.
func rollBuild(t *testing.T, cap int64, targets segment.Targets, setup func(e rollEnv)) rollEnv {
	t.Helper()

	return rollBuildMax(t, cap, targets, rollMax, setup)
}

// rollPin builds over the maximum that leaves every class's make standing:
// the sum of the target counts, so the worker fills each class once and a
// take — the one a test hands out or a roll makes — dips its class for good.
// The refill the take kicks is refused by the ledger before any storage is
// touched, so a refusal's seam counts stand on the stream alone, never on a
// race with the worker.
func rollPin(t *testing.T, cap int64, targets segment.Targets, setup func(e rollEnv)) rollEnv {
	t.Helper()

	return rollBuildMax(t, cap, targets, rollTargetsMax(cap, targets), setup)
}

// rollBuildMax builds one rolling environment over a fresh seam and ledger,
// with the maker's maximum named by the caller.
func rollBuildMax(t *testing.T, cap int64, targets segment.Targets, max int64, setup func(e rollEnv)) rollEnv {
	t.Helper()

	seam, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	set := segment.NewSet(cap, nil)
	maker := segment.NewMaker(seam.FS(), set, max, 0, nil)
	budget := core.NewDescriptorBudget(64)
	cache := segment.NewDescriptors(seam.FS(), format.SegmentName, budget)
	pool := segment.NewPool(maker, targets, nil)

	e := rollEnv{seam: seam, set: set, maker: maker, cache: cache, pool: pool, budget: budget}
	if setup != nil {
		setup(e)
	}
	t.Cleanup(func() {
		if err := cache.Shutdown(); err != nil {
			t.Errorf("the cache's Shutdown: %v", err)
		}
	})
	// The pool's Stop is registered after the cache's cleanup so it runs
	// before it, and the stream's Close after that, before both: the stream
	// holds refs the Shutdown would otherwise wait on, and the pool's
	// worker holds the maker over the seam.
	t.Cleanup(func() { pool.Stop() })
	if e.s != nil {
		t.Cleanup(func() { _ = e.s.Close() })
	}
	pool.Start()
	return e
}

// rollStream finishes the build with its stream, whose appends roll.
func (e rollEnv) withStream(t *testing.T, no uint32) rollEnv {
	t.Helper()

	e.s = segment.NewStream(no, streamIdentity, e.set, e.pool, e.cache)
	t.Cleanup(func() { _ = e.s.Close() })
	return e
}

// rollHeader reads a segment's 48-byte header back through the env's cache
// and decodes it, failing the test on any refused step.
func rollHeader(t *testing.T, e rollEnv, id core.SegmentID) format.SegmentHeader {
	t.Helper()

	raw := make([]byte, format.SegmentHeaderSize)
	if _, err := e.s.ReadAt(id, raw, 0); err != nil {
		t.Fatalf("ReadAt of segment %d's header: %v", id, err)
	}
	hdr, err := format.DecodeSegmentHeader(raw, streamIdentity)
	if err != nil {
		t.Fatalf("segment %d's header does not decode: %v", id, err)
	}
	return hdr
}

// rollOrderedRecorder is a recording fault over the whole seam: it logs
// every call that reaches it, named by op and file name, in their order.
type rollOrderedRecorder struct {
	mu  sync.Mutex
	log []rollCall
}

// rollCall is one seam call the recorder saw.
type rollCall struct {
	op   string
	name string
}

func (f *rollOrderedRecorder) At(op string, key []byte) (time.Duration, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, rollCall{op: op, name: string(key)})
	return 0, 0, nil
}

// install arms the recorder on the env's seam.
func (f *rollOrderedRecorder) install(e rollEnv) {
	e.seam.SetFault(f)
}

// calls returns the recorded calls in their order.
func (f *rollOrderedRecorder) calls() []rollCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rollCall(nil), f.log...)
}

// segmentName returns the segment name of id, for the log's matching.
func segmentName(id core.SegmentID) string { return format.SegmentName(id) }

// rollAllCounters snapshots the seam's total count of every op, so a check
// asserts a refused roll made no call of any op at all.
func rollAllCountersOf(e rollEnv) map[string]int {
	c := map[string]int{}
	for _, op := range []string{
		journaltest.OpCreate, journaltest.OpOpen, journaltest.OpAllocate,
		journaltest.OpWrite, journaltest.OpSync, journaltest.OpTruncate,
		journaltest.OpRemove, journaltest.OpList, journaltest.OpSyncDir,
	} {
		c[op] = e.seam.Calls(op)
	}
	return c
}

// rollMax is the maximum the roll checks' makers reserve against: open, as
// the stream checks' is, so a roll or a refusal is never confused with the
// journal maximum's own refusal.
const rollMax = math.MaxInt64

// TestRollAStraddlingWriteLandsInTwoSegmentsNamingItsPredecessor checks the
// roll (R2, R8, R24): a write larger than the remaining room places its first
// piece in the full segment and the rest, appended next, in a new segment
// whose header names the first as predecessor and carries the stream number;
// each piece carries its own file offset, its length and the write's shared
// version. In the ordered seam log the full segment's two seal syncs precede
// the successor's header write, so the seal is durable before the successor
// is taken. It catches a successor taken before the seal is durable, and a
// header naming none.
func TestRollAStraddlingWriteLandsInTwoSegmentsNamingItsPredecessor(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// A cap the first record nearly fills: one record of 1024 payload bytes,
	// then a straddling write that rolls after its first piece.
	e := rollBuild(t, 4096, segment.Targets{Content: 8}, nil).withStream(t, 1)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 2 })
	se := e.stream(t)

	payload1 := make([]byte, 1024)
	h1, b1 := streamWriteHeaderPair(streamFileID(1), 0, payload1)
	p1 := streamAppendWrite(t, se, h1, b1)
	if p1.N != int64(len(payload1)) {
		t.Fatalf("the first record placed %d bytes, want the whole %d", p1.N, len(payload1))
	}

	// The recorder is installed after the first record: from here on the
	// ordered log names the roll's own calls alone.
	f := &rollOrderedRecorder{}
	f.install(e)

	payload2 := streamTestBody(3072, 0xa5)
	h2, b2 := streamWriteHeader(streamFileID(2), 4096, payload2, 7, core.Version{Counter: 9})
	p2, err := e.s.AppendRoll(h2, b2, segment.Roll{})
	if err != nil {
		t.Fatalf("the straddling AppendRoll: %v", err)
	}
	if p2.N == 0 || p2.N >= int64(len(payload2)) {
		t.Fatalf("the straddling AppendRoll placed %d bytes, want a piece in [1, %d)", p2.N, len(payload2))
	}
	seg1 := p1.Loc.Segment
	h2p, b2p := streamReadRecord(t, se, p2.Loc)
	if h2p.Extent.Off != 4096 {
		t.Fatalf("the piece's file offset is %d, want the write's 4096", h2p.Extent.Off)
	}
	if h2p.Extent.Len != p2.N {
		t.Fatalf("the piece's length is %d, want the %d it placed", h2p.Extent.Len, p2.N)
	}
	if !bytes.Equal(b2p.Payload, payload2[:p2.N]) {
		t.Fatal("the piece's payload is not the bytes it claims to stage")
	}

	// The rest rolls into a second segment.
	rest := payload2[p2.N:]
	h3, b3 := streamWriteHeader(streamFileID(2), 4096+p2.N, rest, 7, core.Version{Counter: 9})
	p3, err := e.s.AppendRoll(h3, b3, segment.Roll{})
	if err != nil {
		t.Fatalf("the AppendRoll of the rest: %v", err)
	}
	if p3.N != int64(len(rest)) {
		t.Fatalf("the rest placed %d bytes, want the whole %d", p3.N, len(rest))
	}
	seg2 := p3.Loc.Segment
	if seg2 == seg1 {
		t.Fatalf("the rest landed in segment %d, want a new one", seg2)
	}

	// The rest carries its own file offset and the write's shared version.
	h3p, b3p := streamReadRecord(t, se, p3.Loc)
	if want := 4096 + p2.N; h3p.Extent.Off != want {
		t.Fatalf("the rest's file offset is %d, want %d", h3p.Extent.Off, want)
	}
	if h3p.Extent.Len != p3.N {
		t.Fatalf("the rest's length is %d, want the %d it placed", h3p.Extent.Len, p3.N)
	}
	if h3p.Version != (core.Version{Counter: 9}) || h2p.Version != (core.Version{Counter: 9}) {
		t.Fatal("a piece does not carry the write's version")
	}
	if !bytes.Equal(b3p.Payload, rest) {
		t.Fatal("the rest's payload is not the bytes it claims to stage")
	}

	// The second segment's header names the first as predecessor and carries
	// the stream number, under the journal's identity.
	hdr := rollHeader(t, e, seg2)
	if hdr.Predecessor != seg1 {
		t.Fatalf("the second segment's header names predecessor %d, want the first segment %d", hdr.Predecessor, seg1)
	}
	if hdr.Stream != 1 {
		t.Fatalf("the second segment's header names stream %d, want 1", hdr.Stream)
	}
	if hdr.Identity != streamIdentity {
		t.Fatal("the second segment's header names another identity")
	}

	// The seal of the first segment is durable before the successor's header
	// write: in the ordered log, the second of the first segment's seal
	// syncs comes before the successor segment's first write.
	log := f.calls()
	sealEnd := -1
	syncs := 0
	seg1Name := segmentName(seg1)
	seg2Name := segmentName(seg2)
	for i, c := range log {
		if c.op == journaltest.OpSync && c.name == seg1Name {
			syncs++
			if syncs == 2 {
				sealEnd = i
			}
		}
	}
	if sealEnd < 0 {
		t.Fatalf("the ordered log holds %v, want the full segment's two seal syncs", log)
	}
	writeAt := -1
	for i, c := range log {
		if c.op == journaltest.OpWrite && c.name == seg2Name {
			writeAt = i
			break
		}
	}
	if writeAt < 0 {
		t.Fatalf("the ordered log holds %v, want the successor's header write", log)
	}
	if sealEnd >= writeAt {
		t.Fatalf("the seal's second sync ends at log index %d, after the successor's header write at %d: the successor was taken before the seal was durable", sealEnd, writeAt)
	}

	// The first segment is sealed and the second active in the ledger.
	if state, err := e.set.State(seg1); err != nil || state != segment.Sealed {
		t.Fatalf("the first segment's state after the roll is %v, %v, want Sealed", state, err)
	}
	if state, err := e.set.State(seg2); err != nil || state != segment.Active {
		t.Fatalf("the second segment's state after the roll is %v, %v, want Active", state, err)
	}
}

// TestRollARecordThatExactlyFillsTheRoomIsWrittenInPlace checks the exact
// fill under the roll: a record whose padded size plus the seal's reserve
// equals the remaining room exactly is placed whole — N equals the payload's
// length, no empty piece and no early roll — and the next append rolls. It
// catches an early roll and an empty piece.
func TestRollARecordThatExactlyFillsTheRoomIsWrittenInPlace(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The cap is chosen so one record fills what remains exactly: after the
	// header and one record of payload n, next + size + sealRoom(2) == cap.
	headerPayload := 1024
	reserve := int64(format.SealMarkerSize) + 2*int64(format.CatalogEntrySize) + int64(format.TrailerSize)
	recordSize := int64(format.RecordHeaderSize) + 8 + int64(headerPayload)
	recordSize = (recordSize + 7) &^ 7
	capNeeded := int64(format.SegmentHeaderSize) + recordSize + reserve

	e := rollBuild(t, capNeeded, segment.Targets{Content: 4}, nil).withStream(t, 1)
	awaitCounters(t, "the two content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 2 })
	se := e.stream(t)

	payload := make([]byte, headerPayload)
	h, body := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 1})
	p := streamAppendWrite(t, e.stream(t), h, body)
	if p.N != int64(headerPayload) {
		t.Fatalf("the exactly-filling record placed %d bytes, want the whole %d: it rolled early or left an empty piece", p.N, headerPayload)
	}
	if p.Loc.Len != recordSize {
		t.Fatalf("the record's size is %d, want the whole %d", p.Loc.Len, recordSize)
	}
	if p.Loc.Off != format.SegmentHeaderSize {
		t.Fatalf("the exactly-filling record landed at %d, want the first slot %d", p.Loc.Off, format.SegmentHeaderSize)
	}
	if _, b := streamReadRecord(t, se, p.Loc); !bytes.Equal(b.Payload, payload) {
		t.Fatal("the exactly-filling record does not read back byte for byte")
	}

	// Nothing more fits, so the next append rolls into a new segment naming
	// the exact-fill one as predecessor.
	h2, body2 := streamWriteHeader(streamFileID(2), 0, make([]byte, 1), 7, core.Version{Counter: 2})
	p2, err := e.s.AppendRoll(h2, body2, segment.Roll{})
	if err != nil {
		t.Fatalf("the append after the exact fill: %v", err)
	}
	if p2.Loc.Segment == p.Loc.Segment {
		t.Fatalf("the append after the exact fill landed in segment %d, want a rolled-to new one", p2.Loc.Segment)
	}
	if hdr := rollHeader(t, e, p2.Loc.Segment); hdr.Predecessor != p.Loc.Segment {
		t.Fatalf("the new segment names predecessor %d, want the exact-fill segment %d", hdr.Predecessor, p.Loc.Segment)
	}
}

// TestRollRefusedWithAnEmptyPoolMakesNoSeamCall checks the spare refusal
// (R15, R20): a pool whose only ready spare was taken by hand answers a
// content roll at once with the NoSpaceError naming `spare`, changes nothing
// of the stream — the segment stays open, sealed by nobody — and makes no
// seam call of any op. It catches a refusal that seals first, or makes a
// spare.
func TestRollRefusedWithAnEmptyPoolMakesNoSeamCall(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// Targets{Content: 1, Headroom: 1} under the pinned maximum of both
	// caps: the worker fills each class once and every take after that dips
	// its class for good, so the stream's warm append takes the one content
	// spare and the refused roll below finds only the headroom's — which
	// its class never serves.
	e := rollPin(t, 4096, segment.Targets{Content: 1, Headroom: 1}, nil).withStream(t, 1)
	awaitCounters(t, "the content and headroom spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares == 1 && c.HeadroomBytes == 4096 })

	// The warm append takes the content spare — the class's only spare,
	// which the pinned maximum keeps from being refilled — and fills the
	// segment to the cap: its payload is sized so the record ends exactly
	// at the cap minus the seal's reserve for two entries, so the
	// straddling write below can place not even one byte of its own there
	// and its whole record must roll.
	warm := make([]byte, 3736)
	wh, wbody := streamWriteHeaderPair(streamFileID(1), 0, warm)
	p := streamAppendWrite(t, e.stream(t), wh, wbody)
	id := p.Loc.Segment
	if got := e.pool.Counters().Spares; got != 0 {
		t.Fatalf("the warm append left %d content spares, want the class drained", got)
	}

	straddle := streamTestBody(1024, 0xc3)
	h, body := streamWriteHeader(streamFileID(2), 0, straddle, 7, core.Version{Counter: 2})
	before := rollAllCountersOf(e)
	leads, err := e.s.AppendRoll(h, body, segment.Roll{})
	assertSpareRefusal(t, "the refused roll", err, 1)
	if leads != (segment.Placed{}) {
		t.Fatalf("the refused roll placed %+v, want nothing", leads)
	}
	if now := rollAllCountersOf(e); !maps.Equal(now, before) {
		t.Fatalf("the refused roll moved the seam: %v, want %v", now, before)
	}

	// The segment is exactly as the refusal found it: open, unsealed, its
	// one record charged.
	if state, err := e.set.State(id); err != nil || state != segment.Active {
		t.Fatalf("the refused roll left the segment %v, %v, want Active: a refusal seals nothing", state, err)
	}
	if c := e.set.Counters(); c.Sealed != 0 {
		t.Fatalf("the refused roll sealed %d segments, want none", c.Sealed)
	}
	if got := e.seam.CallsFor(journaltest.OpSync, streamSegName(id)); got != 0 {
		t.Fatalf("the refused roll made %d syncs of the segment, want none", got)
	}
}

// TestRollAHeaderOnlyRecordTakesAContentSpareBeforeAHeadroomOne checks the
// header-only roll's draw order (D12): with a content spare and a headroom
// spare both ready — the worker's targets met, so it makes no more — a
// header-only record rolls onto the content spare and the headroom's ready
// bytes are left whole. It catches a header-only draw that takes the headroom
// first and falls back to content, which would drain the headroom on every
// header-only roll while content spares sit ready.
func TestRollAHeaderOnlyRecordTakesAContentSpareBeforeAHeadroomOne(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The pins hold every class at exactly its target, so no refill makes a
	// spare behind the assertions: the worker filled each class once and every
	// take below is final for its class.
	e := rollPin(t, streamCap, segment.Targets{Content: 1, Headroom: 1}, nil).withStream(t, 1)
	awaitCounters(t, "the content and headroom spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares == 1 && c.HeadroomBytes == segCap })

	// A content roll over the same pool would be refused nothing: the
	// content spare is an unreserved take's own. A header-only record takes
	// it first (D12) — by the unreserved take, so holds keep theirs — and
	// only a content miss falls back to the headroom.
	hdr := format.RecordHeader{Kind: format.KindStamp, File: streamFileID(1), Version: core.Version{Counter: 1}, Tag: 7}
	p, err := e.s.AppendRoll(hdr, format.RecordBody{Cut: 4}, segment.Roll{})
	if err != nil {
		t.Fatalf("the stamp's append onto the content spare: %v", err)
	}
	if p.Loc.Segment == 0 {
		t.Fatal("the stamp's append named no segment")
	}

	// The headroom is left whole: the sparing it exists for is drawn by no
	// header-only record while a content spare was ready.
	if got := e.pool.Counters().HeadroomBytes; got != segCap {
		t.Fatalf("a header-only roll over a ready content spare left the headroom at %d bytes, want its whole %d", got, segCap)
	}
	if got := e.pool.Counters().Spares; got != 0 {
		t.Fatalf("the header-only roll left %d content spares, want the one it took gone", got)
	}
	if got := e.pool.Counters().SpareRefusals; got != 0 {
		t.Fatalf("a header-only roll that found its content spare counts %d refusals, want 0", got)
	}
}

// TestRollSpareRefusalsRiseByOnePerRefusedRoll checks the counter (R22):
// `spare_refusals` rises by exactly one per refused roll — three refused
// appends, three refusals — and a header-only append that falls back to
// headroom adds none. It catches an uncounted refusal and a counted
// fallback.
func TestRollSpareRefusalsRiseByOnePerRefusedRoll(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The content class holds no target, the headroom one spare: every
	// content roll is refused alone, and the header-only fallback below
	// finds its spare.
	e := rollPin(t, streamCap, segment.Targets{Content: 0, Headroom: 1}, nil).withStream(t, 1)
	awaitCounters(t, "the headroom spare ready", e.pool,
		func(c segment.PoolCounters) bool { return c.HeadroomBytes == segCap && c.Spares == 0 })

	if got := e.pool.Counters().SpareRefusals; got != 0 {
		t.Fatalf("a fresh pool already counts %d spare refusals, want 0", got)
	}
	for i := range 3 {
		payload := streamTestBody(64, byte(0x10+i))
		h, body := streamWriteHeader(streamFileID(1), int64(i)*64, payload, 7, core.Version{Counter: uint64(i + 1)})
		_, err := e.s.AppendRoll(h, body, segment.Roll{})
		assertSpareRefusal(t, fmt.Sprintf("the refused append %d", i+1), err, 1)
		if got, want := e.pool.Counters().SpareRefusals, int64(i+1); got != want {
			t.Fatalf("after %d refused rolls the pool counts %d spare refusals, want %d", i+1+1, got, want)
		}
	}

	// A header-only record falls back to the headroom spare: the missed
	// content take is not a refusal.
	hdr := format.RecordHeader{Kind: format.KindRelease, File: streamFileID(2), Extent: core.Extent{Off: 0, Len: 8}, Tag: 7}
	if _, err := e.s.AppendRoll(hdr, format.RecordBody{}, segment.Roll{}); err != nil {
		t.Fatalf("the header-only append onto the headroom spare: %v", err)
	}
	if got := e.pool.Counters().SpareRefusals; got != 3 {
		t.Fatalf("the header-only fallback counted a refusal: the pool counts %d, want 3", got)
	}
}

// TestRollHeaderOnlyFallsBackToHeadroomThenRefusesNamingIt checks the
// header-only roll (D12, R17, R23): with the content class empty and a
// headroom spare ready, a content roll is refused `spare` while a
// header-only record proceeds onto the headroom spare; with the headroom gone
// too, the header-only roll is refused naming `headroom` and is not counted.
// It catches a header-only record refused `spare`, and a headroom draw counted
// as a content refusal.
func TestRollHeaderOnlyFallsBackToHeadroomThenRefusesNamingIt(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// One content spare, held out of the stream's reach by a hand take once
	// it is made, and two headroom spares: the content class the rolls see
	// is empty from the stream's first append on.
	e := rollPin(t, streamCap, segment.Targets{Content: 1, Headroom: 2}, nil).withStream(t, 1)
	awaitCounters(t, "the content and headroom spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares == 1 && c.HeadroomBytes == 2*segCap })
	if _, err := e.pool.Take(segment.Content); err != nil {
		t.Fatalf("the content spare, held out of the rolls' reach: %v", err)
	}

	// The stream's first roll — a header-only record — rolls from nothing:
	// the content miss falls back to the headroom and the record lands.
	hdr1 := format.RecordHeader{Kind: format.KindStamp, File: streamFileID(1), Version: core.Version{Counter: 1}, Tag: 7}
	p1, err := e.s.AppendRoll(hdr1, format.RecordBody{Cut: 4}, segment.Roll{})
	if err != nil {
		t.Fatalf("the stamp's append onto the headroom spare: %v", err)
	}
	if p1.Loc.Segment == 0 {
		t.Fatal("the stamp's append named no segment")
	}
	if got := e.pool.Counters().SpareRefusals; got != 0 {
		t.Fatalf("a header-only roll that found its headroom spare counts %d refusals, want 0", got)
	}
	if got := e.pool.Counters().HeadroomBytes; got != segCap {
		t.Fatalf("the header-only roll left the headroom at %d bytes, want the one spare it took gone", got)
	}

	// Fill the segment the stamp landed in to its cap, so every later
	// record rolls: writes of a third of the cap land whole until one
	// splits, and its piece takes the segment to the cap before its rest —
	// appended or not, the filled segment takes no further record.
	big := streamTestBody(int(streamCap/3), 0x5a)
	for i := 0; ; i++ {
		h2, b2 := streamWriteHeader(streamFileID(2), int64(i)*int64(len(big)), big, 7, core.Version{Counter: 2})
		p, err := e.s.AppendRoll(h2, b2, segment.Roll{})
		if err != nil {
			t.Fatalf("the filling append %d: %v", i, err)
		}
		if p.N < int64(len(big)) {
			break // the split piece filled the segment to its cap
		}
	}

	// A content write rolls and is refused: the content class holds nothing
	// and the headroom never lends to a content record.
	payload := streamTestBody(64, 0x3c)
	h, body := streamWriteHeader(streamFileID(3), 0, payload, 7, core.Version{Counter: 3})
	_, err = e.s.AppendRoll(h, body, segment.Roll{})
	assertSpareRefusal(t, "the content roll that finds only headroom", err, 1)
	if got := e.pool.Counters().SpareRefusals; got != 1 {
		t.Fatalf("after the refused content roll the pool counts %d, want 1", got)
	}

	// The refused content roll took nothing, so the second header-only
	// record rolls onto the last headroom spare.
	hdr2 := format.RecordHeader{Kind: format.KindStamp, File: streamFileID(4), Version: core.Version{Counter: 4}, Tag: 7}
	p2, err := e.s.AppendRoll(hdr2, format.RecordBody{Cut: 4}, segment.Roll{})
	if err != nil {
		t.Fatalf("the second stamp's append onto the last headroom spare: %v", err)
	}
	if p2.Loc.Segment == p1.Loc.Segment {
		t.Fatalf("the second stamp landed in segment %d, want a rolled-to new one", p2.Loc.Segment)
	}

	// Fill the second stamp's segment with one big content write, so the
	// next header-only record must roll again — and with the headroom gone
	// too, the roll is refused naming the headroom, not `spare`, and is
	// counted nowhere.
	big2 := streamTestBody(int(streamCap/2), 0x99)
	hb, bb2 := streamWriteHeader(streamFileID(4), 0, big2, 7, core.Version{Counter: 4})
	if _, err := e.s.AppendRoll(hb, bb2, segment.Roll{}); err != nil {
		t.Fatalf("the filling write into the second stamp's segment: %v", err)
	}
	big3 := streamTestBody(int(streamCap/2), 0xaa)
	hb3, bb3 := streamWriteHeader(streamFileID(4), int64(len(big2)), big3, 7, core.Version{Counter: 4})
	if _, err := e.s.AppendRoll(hb3, bb3, segment.Roll{}); err != nil {
		t.Fatalf("the second filling write, which fills the segment to its cap: %v", err)
	}
	hdr3 := format.RecordHeader{Kind: format.KindStamp, File: streamFileID(5), Version: core.Version{Counter: 5}, Tag: 7}
	_, err = e.s.AppendRoll(hdr3, format.RecordBody{Cut: 4}, segment.Roll{})
	var ns *core.NoSpaceError
	if !errors.As(err, &ns) {
		t.Fatalf("the header-only roll with neither class ready = %v, want a *core.NoSpaceError", err)
	}
	if ns.Limit != core.LimitHeadroom {
		t.Fatalf("the header-only refusal named the %s limit, want %s", ns.Limit, core.LimitHeadroom)
	}
	if ns.Missing != 1 || ns.Unit != core.UnitSegments {
		t.Fatalf("the header-only refusal said %d %s missing, want 1 segment", ns.Missing, ns.Unit)
	}
	if !errors.Is(err, core.ErrNoSpace) {
		t.Fatalf("the header-only refusal is not ErrNoSpace: %v", err)
	}
	if got := e.pool.Counters().SpareRefusals; got != 1 {
		t.Fatalf("the header-only refusal raised the counter to %d, want it at 1: a missed content take is not a refusal", got)
	}
}

// TestRollAHeadroomTakeTheLedgerRefusedIsNotAnsweredAsHeadroom checks the
// header-only roll's fallback: only the pool's own `spare` miss names the
// headroom, so a headroom take the ledger refused — the pool popped a spare
// the ledger no longer owns, an accounting bug the operator must see — comes
// back as itself rather than as a headroom-named limit, which the caller would
// read as a drainable capacity and retry. It catches the silent relabelling of
// every headroom-take failure as a headroom refuse. The stopped pool answers
// ErrClosed unchanged, never a limit of any name.
func TestRollAHeadroomTakeTheLedgerRefusedIsNotAnsweredAsHeadroom(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The content class has no target, and the one headroom spare is the
	// pool's by adoption — its make final under the pinned maximum, since the
	// adoption already spent the cap the maximum holds. The spare is pulled
	// out of the ledger behind the pool's back, so the header-only roll's
	// fallback pops it and the ledger refuses it: the take must answer the
	// refusal itself, not relabel it.
	e := rollPin(t, streamCap, segment.Targets{Content: 0, Headroom: 1}, func(e rollEnv) {
		id := core.SegmentID(1)
		if err := e.pool.Adopt(id, segment.Headroom); err != nil {
			t.Fatalf("adopting the spare into the headroom class, accounted in the ledger: %v", err)
		}
		if _, derr := e.set.DropSpare(id); derr != nil {
			t.Fatalf("pulling the adopted spare out of the ledger behind the pool's back: %v", derr)
		}
	}).withStream(t, 1)
	awaitCounters(t, "the headroom spare the pool alone still counts", e.pool,
		func(c segment.PoolCounters) bool { return c.HeadroomBytes == segCap && c.Spares == 0 })

	hdr := format.RecordHeader{Kind: format.KindStamp, File: streamFileID(1), Version: core.Version{Counter: 1}, Tag: 7}
	_, err := e.s.AppendRoll(hdr, format.RecordBody{Cut: 4}, segment.Roll{})
	var ns *core.NoSpaceError
	if errors.As(err, &ns) && ns.Limit == core.LimitHeadroom {
		t.Fatalf("the header-only roll over a headroom spare the ledger refused = %v, want the refusal itself: a ledger that lost a spare is a bug, never a capacity limit", err)
	}
	if !errors.Is(err, segment.ErrNotInSet) {
		t.Fatalf("the header-only roll = %v, want the ledger's own refusal of the take", err)
	}
	if got := e.pool.Counters().SpareRefusals; got != 0 {
		t.Fatalf("the refused header-only roll counted %d spare refusals, want 0", got)
	}

	// The stopped pool answers ErrClosed unchanged: nothing of it becomes a
	// limit, for no caller drains a closed pool.
	e2 := rollPin(t, streamCap, segment.Targets{Content: 0, Headroom: 1}, nil).withStream(t, 1)
	awaitCounters(t, "the headroom spare ready", e2.pool,
		func(c segment.PoolCounters) bool { return c.HeadroomBytes == segCap && c.Spares == 0 })
	e2.pool.Stop()
	hdr2 := format.RecordHeader{Kind: format.KindStamp, File: streamFileID(1), Version: core.Version{Counter: 2}, Tag: 7}
	_, err = e2.s.AppendRoll(hdr2, format.RecordBody{Cut: 4}, segment.Roll{})
	if !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the header-only roll over a stopped pool = %v, want core.ErrClosed unchanged", err)
	}
	if errors.As(err, &ns) {
		t.Fatalf("the stopped pool was answered as a limit too: %v", err)
	}
}

// TestRollARepackCopyTakesAReserveSpareNeverAContentOne checks the repack
// roll (R23): with only reserve spares ready a Roll{Repack: true} rolls onto
// one and is not refused `spare` while one is ready, while a plain write over
// the same pool is refused; with only content spares ready, a repack roll
// does not take one. It catches a repack copy drawing content spares.
func TestRollARepackCopyTakesAReserveSpareNeverAContentOne(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// Only reserve spares ready, their target met so the worker makes no more.
	e := rollPin(t, streamCap, segment.Targets{Reserve: 2}, nil).withStream(t, 1)
	awaitCounters(t, "the two reserve spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.RepackReserveBytes == 2*segCap && c.Spares == 0 })

	// The plain write is refused `spare`: the content class holds nothing.
	payload := streamTestBody(64, 0x3c)
	h, body := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 1})
	_, err := e.s.AppendRoll(h, body, segment.Roll{})
	assertSpareRefusal(t, "the plain write over only reserve spares", err, 1)

	// The repack copy's roll succeeds onto a reserve spare.
	h2, b2 := streamWriteHeader(streamFileID(2), 0, payload, 7, core.Version{Counter: 2})
	p2, err := e.s.AppendRoll(h2, b2, segment.Roll{Repack: true})
	if err != nil {
		t.Fatalf("the repack roll over a ready reserve spare: %v", err)
	}
	if p2.Loc.Segment == 0 {
		t.Fatal("the repack roll named no segment")
	}
	if got := e.pool.Counters().RepackReserveBytes; got != segCap {
		t.Fatalf("the repack roll left the reserve at %d bytes, want the one spare it took gone", got)
	}
	if got := e.pool.Counters().SpareRefusals; got != 1 {
		t.Fatalf("a repack roll that found its spare counted a refusal: the pool counts %d, want 1", got)
	}

	// The next repack append lands in the segment the first made: no roll
	// while room remains.
	h3, b3 := streamWriteHeader(streamFileID(3), 64, payload, 7, core.Version{Counter: 3})
	p3, err := e.s.AppendRoll(h3, b3, segment.Roll{Repack: true})
	if err != nil {
		t.Fatalf("the second repack append: %v", err)
	}
	if p3.Loc.Segment != p2.Loc.Segment {
		t.Fatalf("the second repack append rolled to segment %d, want it in the first %d", p3.Loc.Segment, p2.Loc.Segment)
	}

	// Only content spares ready: the repack roll is refused and takes none.
	e2 := rollPin(t, streamCap, segment.Targets{Content: 2}, nil).withStream(t, 1)
	awaitCounters(t, "the two content spares ready", e2.pool,
		func(c segment.PoolCounters) bool { return c.Spares == 2 })

	payload2 := streamTestBody(64, 0x55)
	hr, br := streamWriteHeader(streamFileID(1), 0, payload2, 7, core.Version{Counter: 1})
	_, err = e2.s.AppendRoll(hr, br, segment.Roll{Repack: true})
	assertSpareRefusal(t, "the repack roll that finds only content", err, 1)
	if got := e2.pool.Counters().Spares; got != 2 {
		t.Fatalf("the refused repack roll left %d content spares, want 2: it took one", got)
	}
}

// TestRollAReservedRollTakesTheHoldsSpareAnUnreservedIsRefused checks R80:
// with one content spare left, held by a Hold, an unreserved roll is refused
// `spare` while the reserved write's Roll{Hold: h} roll succeeds onto the
// held spare. It catches a reserved roll answering like an unreserved one.
func TestRollAReservedRollTakesTheHoldsSpareAnUnreservedIsRefused(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// One content spare, held: the worker is at target once it is made, and
	// the reservation holds it.
	e := rollPin(t, streamCap, segment.Targets{Content: 1}, nil).withStream(t, 1)
	awaitCounters(t, "the content spare ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares == 1 })

	hold, err := e.pool.Hold(1)
	if err != nil {
		t.Fatalf("the Hold: %v", err)
	}
	defer hold.Close()
	if got := e.pool.Counters().Spares; got != 1 {
		t.Fatalf("the pool holds %d ready spares against the open hold, want 1", got)
	}

	// The unreserved roll is refused: the one spare belongs to the hold.
	payload := streamTestBody(64, 0x3c)
	hh, bb := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 1})
	_, err = e.s.AppendRoll(hh, bb, segment.Roll{})
	assertSpareRefusal(t, "the unreserved roll under an open hold", err, 1)

	// The reserved write's roll takes the hold's spare.
	hr, br := streamWriteHeader(streamFileID(2), 0, payload, 7, core.Version{Counter: 2})
	p, err := e.s.AppendRoll(hr, br, segment.Roll{Hold: hold})
	if err != nil {
		t.Fatalf("the reserved roll onto the hold's spare: %v", err)
	}
	if p.Loc.Segment == 0 {
		t.Fatal("the reserved roll named no segment")
	}
	if got := e.pool.Counters().Held; got != 0 {
		t.Fatalf("after the reserved roll the pool owes the hold %d spares, want none", got)
	}
	if got := e.pool.Counters().SpareRefusals; got != 1 {
		t.Fatalf("the reserved roll counted a refusal: the pool counts %d, want 1", got)
	}
}

// TestRollENOSPCAfterAFailedMakeIsNotAnsweredAsSpare checks R21's answer:
// once making a spare has failed with ENOSPC, a roll that finds no spare
// returns the ENOSPC — never a spare refusal — and `spare_refusals` is
// unchanged. It catches a full device answered as `spare`.
func TestRollENOSPCAfterAFailedMakeIsNotAnsweredAsSpare(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The worker's makes fail ENOSPC, so the pool holds no spare and the
	// state it owes the roll is set.
	e := rollBuild(t, streamCap, segment.Targets{Content: 1}, func(e rollEnv) {
		if err := e.seam.Schedule(journaltest.Failing(journaltest.OpAllocate, "*", 0, syscall.ENOSPC)); err != nil {
			t.Fatalf("Schedule: %v", err)
		}
	}).withStream(t, 1)
	awaitDeviceFull(t, true, e.pool)

	payload := streamTestBody(64, 0x3c)
	h, body := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 1})
	_, err := e.s.AppendRoll(h, body, segment.Roll{})
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("the roll over a full device = %v, want the ENOSPC the make left", err)
	}
	if errors.Is(err, core.ErrNoSpace) {
		t.Fatalf("the roll over a full device answered ErrNoSpace too: %v; a full device is the operator's, never a limit's", err)
	}
	if got := e.pool.Counters().SpareRefusals; got != 0 {
		t.Fatalf("the ENOSPC answer counted a spare refusal: the pool counts %d, want 0", got)
	}
}

// TestRollASegmentEndedByAFailedSealSyncNamesNoPredecessor checks R33: a
// roll whose seal fails on its first or second sync — the record sync or the
// marker sync, injected on the segment the roll seals — hands the failure to
// the append that triggered it, stays on the ended segment, and unwinds the
// spare it took; the next append rolls into a new segment whose predecessor
// is 0, and the failed segment is ended in the ledger, accounted at its cap.
// It catches a successor naming a segment the failures ended.
func TestRollASegmentEndedByAFailedSealSyncNamesNoPredecessor(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, tc := range []struct {
		name  string
		count int
	}{
		{"the sync of the records", 1},
		{"the sync of the marker", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := rollPin(t, 4096, segment.Targets{Content: 4}, nil).withStream(t, 1)
			awaitCounters(t, "the content spares ready", e.pool,
				func(c segment.PoolCounters) bool { return c.Spares >= 2 })

			// One record nearly fills; the straddling write below rolls and
			// its roll seals the first segment, whose sync fault takes this
			// seal alone.
			warm := make([]byte, 1024)
			wh, wb := streamWriteHeaderPair(streamFileID(1), 0, warm)
			p1 := streamAppendWrite(t, e.stream(t), wh, wb)
			if err := e.seam.Schedule(journaltest.Failing(journaltest.OpSync, streamSegName(p1.Loc.Segment), tc.count, syscall.EIO)); err != nil {
				t.Fatalf("Schedule: %v", err)
			}

			straddle := streamTestBody(3072, 0xa5)
			h, body := streamWriteHeader(streamFileID(2), 4096, straddle, 7, core.Version{Counter: 2})
			p2, err := e.s.AppendRoll(h, body, segment.Roll{})
			if err != nil {
				t.Fatalf("the straddling AppendRoll: %v", err)
			}
			if p2.N == 0 || p2.N >= int64(len(straddle)) {
				t.Fatalf("the straddling AppendRoll placed %d bytes, want a piece", p2.N)
			}

			// The append that triggered the roll carries the seal's failure:
			// the roll stays on the ended segment rather than moving the
			// stream past a window no fsync reached, and the spare it took
			// is unwound.
			rest := straddle[p2.N:]
			h2, b2 := streamWriteHeader(streamFileID(2), 4096+p2.N, rest, 7, core.Version{Counter: 2})
			if _, err := e.s.AppendRoll(h2, b2, segment.Roll{}); err == nil {
				t.Fatal("the AppendRoll of the rest answered nil, want the seal's ended failure carried to its caller")
			} else if !errors.Is(err, segment.ErrSegmentEnded) || !errors.Is(err, syscall.EIO) {
				t.Fatalf("the AppendRoll of the rest = %v, want ErrSegmentEnded carrying the fault's EIO", err)
			}
			if state, err := e.set.State(p1.Loc.Segment); err != nil || state != segment.Ended {
				t.Fatalf("the failed segment's state is %v, %v, want Ended", state, err)
			}

			// The next append rolls, and its successor names none: the
			// segment a failed seal sync ended leaves no successor of its
			// own (R33).
			if err := e.seam.Schedule(); err != nil {
				t.Fatalf("disarming the sync fault: %v", err)
			}
			h3, b3 := streamWriteHeader(streamFileID(3), 0, streamTestBody(64, 0x3c), 7, core.Version{Counter: 3})
			p3, err := e.s.AppendRoll(h3, b3, segment.Roll{})
			if err != nil {
				t.Fatalf("the append after the failed seal: %v", err)
			}
			if p3.Loc.Segment == p1.Loc.Segment {
				t.Fatalf("the append after the failed seal stayed in segment %d, want a rolled-to successor", p3.Loc.Segment)
			}
			if hdr := rollHeader(t, e, p3.Loc.Segment); hdr.Predecessor != 0 {
				t.Fatalf("the new segment names predecessor %d, want none: a failed seal sync ends the segment its roll sealed", hdr.Predecessor)
			}
			// Three segments are accounted whole: the ended one, the spare
			// the failed roll took and unwound, and the successor the next
			// append took — each at its cap, none leaked as a spare.
			c := e.set.Counters()
			if got := c.UsedBytes - c.Spares*4096; got != 3*4096 {
				t.Fatalf("the segments account for %d, want three at their caps", got)
			}
			if c.Segments != 3 {
				t.Fatalf("the ledger holds %d segments, want the ended, the unwound and the successor", c.Segments)
			}
		})
	}
}

// TestRollASegmentWhoseTruncationFailedStaysSealedAndNamed checks R34: after
// an early seal whose truncation failed, the next append rolls into a segment
// naming the sealed one, and the ledger keeps the sealed segment accounted at
// its cap.
func TestRollASegmentWhoseTruncationFailedStaysSealedAndNamed(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := rollBuild(t, streamCap, segment.Targets{Content: 4}, nil).withStream(t, 1)
	awaitCounters(t, "the two content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 2 })
	se := e.stream(t)

	var placed []segment.Placed
	for i := range 3 {
		payload := streamTestBody(64, byte(0x30+i))
		h, body := streamWriteHeader(streamFileID(i+1), int64(i)*64, payload, 7, core.Version{Counter: uint64(i + 1)})
		placed = append(placed, streamAppendWrite(t, se, h, body))
	}
	id := placed[0].Loc.Segment

	// The early seal's truncation fails: the segment is sealed, accounted at
	// its cap, and TailErr names the failure.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpTruncate, streamSegName(id), 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	res, err := e.s.Seal(true)
	if err != nil {
		t.Fatalf("the early Seal: %v", err)
	}
	if res.TailErr == nil {
		t.Fatal("the early seal reported no tail error, want the truncation's EIO")
	}
	if state, err := e.set.State(id); err != nil || state != segment.Sealed {
		t.Fatalf("the truncated-failed segment's state is %v, %v, want Sealed", state, err)
	}
	// The sealed segment is accounted whole — the failed cut took nothing
	// off the footprint — so the segments' own bytes hold its cap: the
	// sealed one's, for no other segment exists yet, the pool's ready spares
	// and the roll's successor aside. (The successor is taken below, so the
	// cap is measured here, alone.)
	c := e.set.Counters()
	if got := c.UsedBytes - c.Spares*streamCap; got != streamCap {
		t.Fatalf("the segments account for %d after the failed truncation, want the sealed segment's cap %d", got, streamCap)
	}

	// The next append rolls into a segment naming the sealed one.
	payload := streamTestBody(64, 0x7d)
	h, body := streamWriteHeader(streamFileID(4), 192, payload, 7, core.Version{Counter: 4})
	p, err := e.s.AppendRoll(h, body, segment.Roll{})
	if err != nil {
		t.Fatalf("the append past the sealed segment: %v", err)
	}
	if p.Loc.Segment == id {
		t.Fatalf("the append landed in the sealed segment %d, want a new one", p.Loc.Segment)
	}
	if hdr := rollHeader(t, e, p.Loc.Segment); hdr.Predecessor != id {
		t.Fatalf("the new segment names predecessor %d, want the sealed segment %d", hdr.Predecessor, id)
	}
	// Sealed at its cap: the ledger never shrank it, and both segments are
	// accounted whole.
	if state, err := e.set.State(id); err != nil || state != segment.Sealed {
		t.Fatalf("the sealed segment's state after the roll is %v, %v, want Sealed", state, err)
	}
	c = e.set.Counters()
	if got := c.UsedBytes - c.Spares*streamCap; got != 2*streamCap {
		t.Fatalf("the segments account for %d after the roll, want both at their caps %d", got, 2*streamCap)
	}
}

// TestRollTwoFilesOnTwoStreamsNeverBlockEachOther checks R6 and D7: two
// FileIDs whose For differs use two streams; a record write held at the seam
// on one stream does not stop an append of another file on its own stream;
// For is stable for one file; and across 64 distinct files every stream is
// chosen at least once. It catches a shared lock across streams, and a
// degenerate hash.
func TestRollTwoFilesOnTwoStreamsNeverBlockEachOther(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := rollBuild(t, streamCap, segment.Targets{Content: 4}, nil)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 2 })
	ss := segment.NewStreams(4, streamIdentity, e.set, e.pool, e.cache)
	t.Cleanup(func() {
		if err := ss.Close(); err != nil {
			t.Errorf("the streams' Close: %v", err)
		}
	})
	// The env carries one stream of the four, so its read helpers reach the
	// cache exactly as a stream does.
	e.s = ss.Stream(0)

	// Two files whose streams differ.
	var fa, fb core.FileID
	for i := 0; ; i++ {
		f := streamFileID(i)
		if fa == (core.FileID{}) {
			fa = f
			continue
		}
		if ss.For(f) != ss.For(fa) {
			fb = f
			break
		}
	}
	sa, sb := ss.For(fa), ss.For(fb)
	if sa == sb {
		t.Fatal("two files whose stream indices differ hold the same stream")
	}

	// One landed record on each stream: the segment each stream holds names
	// it, and the held write below is a record's, on a stream whose segment
	// exists.
	var segA string
	for _, f := range []core.FileID{fa, fb} {
		payload := streamTestBody(64, 0x5a)
		h, body := streamWriteHeader(f, 0, payload, 7, core.Version{Counter: 1})
		p, err := ss.For(f).AppendRoll(h, body, segment.Roll{})
		if err != nil {
			t.Fatalf("the warm append of file %x: %v", f.Bytes(), err)
		}
		if f == fa {
			segA = format.SegmentName(p.Loc.Segment)
		}
	}
	if segA == "" {
		t.Fatal("file a's warm append named no segment")
	}

	// Park file a's next record write at the seam, on its stream's own
	// segment: file b's append, on another stream and another segment, must
	// complete while it waits — a stream blocked in storage never blocks
	// another.
	stop := e.seam.HoldFirst(t, journaltest.OpWrite, segA)
	appendBlocked := make(chan error, 1)
	go func() {
		payload := streamTestBody(64, 0x11)
		h, body := streamWriteHeader(fa, 64, payload, 7, core.Version{Counter: 2})
		_, err := sa.AppendRoll(h, body, segment.Roll{})
		appendBlocked <- err
	}()
	select {
	case <-stop.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("file a's record write never reached the hold")
	}

	appendB := make(chan error, 1)
	go func() {
		payload := streamTestBody(64, 0x3c)
		h, body := streamWriteHeader(fb, 64, payload, 7, core.Version{Counter: 2})
		_, err := sb.AppendRoll(h, body, segment.Roll{})
		appendB <- err
	}()
	select {
	case err := <-appendB:
		if err != nil {
			t.Fatalf("the append of one file while another's write was held at the seam: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("file b's append on its own stream never returned while file a's write was held: the streams share a lock")
	}
	stop.Release()
	if err := <-appendBlocked; err != nil {
		t.Fatalf("file a's held write after the hold released: %v", err)
	}

	// For is stable for one file.
	if ss.For(fa) != sa || ss.For(fb) != sb {
		t.Fatal("For is not stable for one file")
	}

	// Every stream is chosen at least once across 64 distinct files.
	per := make(map[*segment.Stream]int)
	for i := range 64 {
		per[ss.For(streamFileID(i))]++
	}
	if len(per) != ss.Len() {
		t.Fatalf("64 distinct files chose %d streams, want all %d", len(per), ss.Len())
	}

	// Len matches the count built.
	if ss.Len() != 4 {
		t.Fatalf("the streams' Len is %d, want 4", ss.Len())
	}
	// Stream reaches each of them.
	for i := range 4 {
		if ss.Stream(i) == nil {
			t.Fatalf("Stream(%d) reached no stream", i)
		}
	}
}

// TestRollSixteenAppendersOverFourStreamsWithTheRefillUnderRace checks R6
// and R7 under -race: sixteen appenders over four streams, many rolls on a
// small cap with the refill running behind them, spare refusals retried
// after a Kick on a bounded spin; every record reads back byte for byte;
// per stream every segment's header names the previous segment of the same
// stream; each stream holds exactly one Active segment and every other
// segment of it is sealed.
func TestRollSixteenAppendersOverFourStreamsWithTheRefillUnderRace(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const rollCap = int64(4096)
	e := rollBuild(t, rollCap, segment.Targets{Content: 4}, nil)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares == 4 })
	ss := segment.NewStreams(4, streamIdentity, e.set, e.pool, e.cache)
	t.Cleanup(func() {
		if err := ss.Close(); err != nil {
			t.Errorf("the streams' Close: %v", err)
		}
	})
	// The env carries one stream of the four, so its read helpers reach the
	// cache exactly as a stream does.
	e.s = ss.Stream(0)

	const appenders = 16
	const perAppender = 24
	type result struct {
		p       segment.Placed
		payload []byte
		stream  *segment.Stream
	}
	results := make(chan result, appenders*perAppender)
	errs := make(chan error, appenders*perAppender)

	var wg sync.WaitGroup
	for i := range appenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f := streamFileID(i)
			st := ss.For(f)
			for j := range perAppender {
				payload := make([]byte, 64)
				for k := range payload {
					payload[k] = byte(i*16 + j ^ k)
				}
				h, body := streamWriteHeader(f, int64(j)*64, payload, 7, core.Version{Counter: uint64(i*perAppender + j + 1)})
				for {
					p, err := st.AppendRoll(h, body, segment.Roll{})
					if err == nil {
						results <- result{p: p, payload: payload, stream: st}
						break
					}
					var ns *core.NoSpaceError
					if !errors.As(err, &ns) || ns.Limit != core.LimitSpare {
						errs <- err
						break
					}
					// A spare refusal clears on its own: kick the refill
					// and poll its counters until a spare is ready or the
					// bounded attempts run out. The poll yields every pass
					// and re-kicks every few thousand, so the worker
					// behind its coalesced kick slot is never left idle
					// while sixteen consumers wait on its makes.
					e.pool.Kick()
					ready := false
					for i := range poolAttempts {
						if i%4096 == 0 {
							e.pool.Kick()
						}
						runtime.Gosched()
						if c := e.pool.Counters(); c.Spares+c.HeadroomBytes/rollCap+c.RepackReserveBytes/rollCap > 0 {
							ready = true
							break
						}
					}
					if !ready {
						errs <- err
						break
					}
				}
			}
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Errorf("an appender's append: %v", err)
	}

	// Every record reads back, byte for byte.
	if len(results) != appenders*perAppender {
		t.Fatalf("%d records placed, want %d", len(results), appenders*perAppender)
	}
	se := e.stream(t)
	byStream := map[*segment.Stream][]result{}
	for r := range results {
		_, b := streamReadRecord(t, se, r.p.Loc)
		if !bytes.Equal(b.Payload, r.payload) {
			t.Errorf("the record at %d of segment %d does not read back byte for byte", r.p.Loc.Off, r.p.Loc.Segment)
		}
		byStream[r.stream] = append(byStream[r.stream], r)
	}

	// Per stream: the predecessor chain over its segments, exactly one
	// Active segment, every other one sealed (R7).
	if len(byStream) != ss.Len() {
		t.Fatalf("the appenders chose %d streams, want all %d", len(byStream), ss.Len())
	}
	idx := map[*segment.Stream]int{}
	for i := range ss.Len() {
		idx[ss.Stream(i)] = i
	}
	for st, rs := range byStream {
		segs := map[core.SegmentID]bool{}
		for _, r := range rs {
			segs[r.p.Loc.Segment] = true
		}
		var ids []core.SegmentID
		for id := range segs {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for k := 1; k < len(ids); k++ {
			hdr := rollHeader(t, e, ids[k])
			if hdr.Stream != uint32(idx[st]) {
				t.Errorf("segment %d carries stream %d, want %d", ids[k], hdr.Stream, idx[st])
			}
			if hdr.Predecessor != ids[k-1] {
				t.Errorf("stream %d: segment %d names predecessor %d, want %d", idx[st], ids[k], hdr.Predecessor, ids[k-1])
			}
		}
		var act int
		for _, id := range ids {
			state, err := e.set.State(id)
			if err != nil {
				t.Fatalf("the state of segment %d: %v", id, err)
			}
			switch state {
			case segment.Active:
				act++
			case segment.Sealed:
			default:
				t.Errorf("stream %d holds segment %d in state %v, want Active or Sealed", idx[st], id, state)
			}
		}
		if act != 1 {
			t.Errorf("stream %d holds %d active segments, want exactly 1", idx[st], act)
		}
	}
}

// TestRollConcurrentAppendersOnOneFullSegmentRollOnce checks the roll's
// single-flight: eight appenders racing onto one full segment take exactly
// one new segment between them, and all eight records land in it. It catches
// a roll per waiter.
func TestRollConcurrentAppendersOnOneFullSegmentRollOnce(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := rollPin(t, 4096, segment.Targets{Content: 8}, nil).withStream(t, 1)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 2 })

	// One record fills the segment to the cap: no piece of any write
	// below fits what remains — not even one payload byte — so every
	// appender rolls, and none can land a straddling piece in the full
	// segment. The warm payload is sized so its record ends exactly at
	// the cap minus the seal's reserve for two entries.
	warm := make([]byte, 3736)
	wh, wb := streamWriteHeaderPair(streamFileID(1), 0, warm)
	w := streamAppendWrite(t, e.stream(t), wh, wb)
	seg1 := w.Loc.Segment
	before := e.set.Counters()

	// The racing writes are records of a 256-byte payload: too large for
	// the room the warm record left, small enough that all eight fit the
	// new segment whole — so the one roll the gate allows carries them all.
	const appenders = 8
	payload := streamTestBody(256, 0xa5)
	var wg sync.WaitGroup
	placed := make(chan segment.Location, appenders)
	errs := make(chan error, appenders)
	for i := range appenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h, body := streamWriteHeader(streamFileID(i+1), 0, payload, 7, core.Version{Counter: uint64(i + 1)})
			p, err := e.s.AppendRoll(h, body, segment.Roll{})
			if err != nil {
				errs <- err
				return
			}
			placed <- p.Loc
		}(i)
	}
	wg.Wait()
	close(placed)
	close(errs)
	for err := range errs {
		t.Errorf("a concurrent appender failed: %v", err)
	}

	// Exactly one new segment was taken by the eight racing appenders.
	after := e.set.Counters()
	if got := after.Segments - before.Segments; got != 1 {
		t.Fatalf("the racing appenders created %d new segments, want exactly 1", got)
	}
	// All eight records landed in the one new segment.
	var seg2 core.SegmentID
	n := 0
	for loc := range placed {
		if loc.Segment == seg1 {
			t.Fatalf("a record landed back in the full segment %d", seg1)
		}
		if seg2 == 0 {
			seg2 = loc.Segment
		}
		if loc.Segment != seg2 {
			t.Fatalf("the racing appenders landed in segments %d and %d, want the one new one", seg2, loc.Segment)
		}
		n++
	}
	if n != appenders {
		t.Fatalf("%d records rolled off the full segment, want all %d", n, appenders)
	}
	if state, err := e.set.State(seg1); err != nil || state != segment.Sealed {
		t.Fatalf("the full segment's state after the rolls is %v, %v, want Sealed", state, err)
	}
	if state, err := e.set.State(seg2); err != nil || state != segment.Active {
		t.Fatalf("the new segment's state after the rolls is %v, %v, want Active", state, err)
	}
}

// TestRollTheStreamClosedWhileARollRanReturnsClosedAndEndsItsSpare checks
// the close under a roll: with the seal's first sync parked at the seam, the
// stream's Close does not return while the roll runs — the seal is one of
// the in-flight operations Close waits for — and the rolling append returns
// ErrStreamClosed, its taken spare ended in the ledger at its cap, not leaked
// as an active segment or a spare.
func TestRollTheStreamClosedWhileARollRanReturnsClosedAndEndsItsSpare(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := rollPin(t, 4096, segment.Targets{Content: 4}, nil).withStream(t, 1)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 2 })

	// One record nearly fills the segment, and the straddling write below
	// first places its piece; the next append rolls, and its roll seals the
	// full segment — the seal whose first sync is parked at the hold.
	warm := make([]byte, 1024)
	wh, wb := streamWriteHeaderPair(streamFileID(1), 0, warm)
	warmP := streamAppendWrite(t, e.stream(t), wh, wb)
	seg1 := warmP.Loc.Segment

	piece := streamTestBody(3072, 0x5a)
	h1, b1 := streamWriteHeader(streamFileID(2), 0, piece, 7, core.Version{Counter: 2})
	p1, err := e.s.AppendRoll(h1, b1, segment.Roll{})
	if err != nil {
		t.Fatalf("the filling AppendRoll: %v", err)
	}
	if p1.N >= int64(len(piece)) {
		t.Fatalf("the filling AppendRoll placed the whole %d, want a piece that fills the segment", len(piece))
	}

	// The rest rolls: its roll seals the full segment, and the hold parks
	// the seal's first sync — the sync that makes the full segment's
	// records durable, the first thing a seal does.
	stop := e.seam.HoldFirst(t, journaltest.OpSync, streamSegName(seg1))
	appendErr := make(chan error, 1)
	go func() {
		rest := piece[p1.N:]
		h2, b2 := streamWriteHeader(streamFileID(2), p1.N, rest, 7, core.Version{Counter: 2})
		_, err := e.s.AppendRoll(h2, b2, segment.Roll{})
		appendErr <- err
	}()
	// The seal's first sync reaches the hold: by then the roll has taken
	// its spare, for the roll takes before it seals.
	<-stop.Reached()

	// Close in another goroutine: it sets the stream's closed flag and then
	// waits the in-flight roll out. The poll below pins the close ahead of
	// the release, so the roll's next check is guaranteed to see the flag.
	// That check is the post-seal one inside roll — this test's hold parks
	// the roll inside its own Seal, so the install step is never reached;
	// the install-time close check has a case of its own.
	closed := make(chan error, 1)
	go func() { closed <- e.s.Close() }()
	// Seal answers ErrStreamClosed the moment the close has set the stream's
	// flag — it refuses before it could ever touch the sealing segment — so
	// polling it pins the close ahead of the release: the roll's install,
	// still to run, is then guaranteed to see the flag and unwind its
	// taken spare.
	for range 1 << 20 {
		if _, err := e.s.Seal(false); errors.Is(err, segment.ErrStreamClosed) {
			break
		}
		runtime.Gosched()
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v while a roll was in flight, want it to wait the roll's seal out", err)
	case err := <-appendErr:
		t.Fatalf("the rolling append returned %v while its seal's sync was parked and the stream was not yet closed", err)
	default:
	}

	// Release the seal's sync: both then return, the append with the
	// stream's closed refusal, the close nil.
	stop.Release()
	if err := <-closed; err != nil {
		t.Fatalf("Close after the roll's seal: %v", err)
	}
	if err := <-appendErr; !errors.Is(err, segment.ErrStreamClosed) {
		t.Fatalf("the rolling append after a close = %v, want ErrStreamClosed", err)
	}

	// The taken spare is ended in the ledger at its cap: no spare is left
	// outside the ledger, and no segment is left Active but the ones the
	// stream holds. A sync parked at the seam held the seal, so the segment
	// the roll sealed may be either Sealed or Ended here; what the check
	// pins is that every segment the directory holds is accounted whole.
	c := e.set.Counters()
	if got := c.UsedBytes - c.Spares*4096; got != 2*4096 {
		t.Fatalf("the segments account for %d after the closed roll, want both at their caps", got)
	}
	// The two segments — the sealed one and the spare the roll took and
	// ended — are accounted whole, and every spare of the pinned four is
	// either taken or still ready in the class: two made it into segments,
	// two stay ready.
	if c.Spares != 2 {
		t.Fatalf("the ledger holds %d ready spares after the closed roll, want the two of the pinned four the takes never reached", c.Spares)
	}
}

// TestRollTheConstructorGuardsAndTheBadRollOptions checks the guards:
// NewStream panics on a nil pool and on a pool over another set, NewStreams
// panics on a count below one, and AppendRoll refuses a Roll whose Hold and
// Repack are both set and a Hold on a header-only kind, reserving nothing —
// the next good append lands at the segment's first slot.
func TestRollTheConstructorGuardsAndTheBadRollOptions(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := rollBuild(t, streamCap, segment.Targets{Content: 4}, nil).withStream(t, 1)
	se := e.stream(t)

	// A nil pool is a caller bug.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("NewStream with a nil pool did not panic")
			}
		}()
		segment.NewStream(1, streamIdentity, e.set, nil, e.cache)
	}()

	// A pool over another set: the stream's rolls would draw spares a
	// ledger does not own.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("NewStream over another ledger's pool did not panic")
			}
		}()
		segment.NewStream(1, streamIdentity, segment.NewSet(streamCap, nil), e.pool, e.cache)
	}()

	func() {
		defer func() {
			if recover() == nil {
				t.Error("NewStreams with a count below one did not panic")
			}
		}()
		segment.NewStreams(0, streamIdentity, e.set, e.pool, e.cache)
	}()

	// The bad roll options are refused before anything is reserved.
	awaitCounters(t, "the content spare ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares == 4 })
	hold, err := e.pool.Hold(1)
	if err != nil {
		t.Fatalf("the Hold: %v", err)
	}
	defer hold.Close()

	payload := streamTestBody(64, 0x3c)
	bad := []struct {
		name string
		h    format.RecordHeader
		body format.RecordBody
		r    segment.Roll
	}{
		{
			name: "Hold and Repack",
			h:    streamWriteHeaderOf(streamFileID(1), 0, payload),
			body: format.RecordBody{Payload: payload, MTime: 1720000000000000000},
			r:    segment.Roll{Hold: hold, Repack: true},
		},
		{
			name: "a Hold on a header-only kind",
			h:    format.RecordHeader{Kind: format.KindStamp, File: streamFileID(1), Version: core.Version{Counter: 1}, Tag: 7},
			body: format.RecordBody{Cut: 4},
			r:    segment.Roll{Hold: hold},
		},
	}
	writes := e.seam.Calls(journaltest.OpWrite)
	share := e.set.ShareBytes(7)
	for _, tc := range bad {
		p, err := e.s.AppendRoll(tc.h, tc.body, tc.r)
		if !errors.Is(err, segment.ErrBadRoll) {
			t.Fatalf("%s: AppendRoll = %v, want ErrBadRoll", tc.name, err)
		}
		if p != (segment.Placed{}) {
			t.Fatalf("%s: AppendRoll placed %+v, want nothing", tc.name, p)
		}
		if got := e.seam.Calls(journaltest.OpWrite); got != writes {
			t.Fatalf("%s made a seam write, want the refusal to perform none", tc.name)
		}
		if got := e.set.ShareBytes(7); got != share {
			t.Fatalf("%s moved the share bytes to %d, want %d", tc.name, got, share)
		}
	}

	// The next good append lands at the segment's first slot: nothing was
	// reserved.
	h, body := streamWriteHeader(streamFileID(2), 0, payload, 7, core.Version{Counter: 9})
	p := streamAppendWrite(t, se, h, body)
	if p.Loc.Off != format.SegmentHeaderSize {
		t.Fatalf("the append after the bad rolls landed at %d, want the segment's first slot %d", p.Loc.Off, format.SegmentHeaderSize)
	}
}

// streamWriteHeaderOf returns one write's header alone, for a table's
// one-line use.
func streamWriteHeaderOf(fid core.FileID, off int64, payload []byte) format.RecordHeader {
	h, _ := streamWriteHeader(fid, off, payload, 7, core.Version{Counter: 1})
	return h
}

// TestRollASegmentItsSealEndedIsReportedNotRolledPast checks the roll's word
// for the seal it ran: a roll whose seal fails its records' sync — the
// step that makes the acknowledged records durable — ends the segment, and
// the roll does not move the stream past the failure as though nothing had
// happened. The triggering append carries the seal's error, the stream stays
// on the ended segment, and a Sync before any further append reports the
// ended segment rather than answering nil for records no fsync reached. It
// catches a discarded seal failure, the only signal the durability layer
// ever had that a segment's acknowledged records were lost.
func TestRollASegmentItsSealEndedIsReportedNotRolledPast(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := rollPin(t, 4096, segment.Targets{Content: 4}, nil).withStream(t, 1)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 2 })
	se := e.stream(t)

	// One record lands and is acknowledged; no Sync has run yet.
	warm := make([]byte, 1024)
	wh, wb := streamWriteHeaderPair(streamFileID(1), 0, warm)
	warmPlaced := streamAppendWrite(t, se, wh, wb)
	seg1 := warmPlaced.Loc.Segment

	// The seal the roll will run fails its first sync — the records' — with
	// EIO, so the seal ends the segment its roll was closing.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpSync, streamSegName(seg1), 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	// The straddling write places its piece, and appending the rest rolls:
	// the roll's seal fails and ends the segment, and the append carries
	// the failure rather than landing in a successor as though the
	// acknowledged records were durable.
	straddle := streamTestBody(3072, 0x5a)
	h1, b1 := streamWriteHeader(streamFileID(2), 0, straddle, 7, core.Version{Counter: 2})
	p1, err := e.s.AppendRoll(h1, b1, segment.Roll{})
	if err != nil {
		t.Fatalf("the straddling AppendRoll: %v", err)
	}
	if p1.N >= int64(len(straddle)) {
		t.Fatalf("the straddling AppendRoll placed the whole %d, want a piece", len(straddle))
	}
	rest := straddle[p1.N:]
	h2, b2 := streamWriteHeader(streamFileID(2), p1.N, rest, 7, core.Version{Counter: 2})
	_, err = e.s.AppendRoll(h2, b2, segment.Roll{})
	if err == nil {
		t.Fatal("the append whose roll's seal failed the records' sync answered nil: the failure was discarded")
	}
	if !errors.Is(err, segment.ErrSegmentEnded) {
		t.Fatalf("the append whose roll's seal failed = %v, want ErrSegmentEnded carrying the seal's failure", err)
	}
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("the append whose roll's seal failed = %v, want it carrying the fault's EIO", err)
	}

	// The stream stays on the ended segment, so the next Sync reports it
	// rather than answering nil for the records the failed window lost.
	if err := e.s.Sync(); !errors.Is(err, segment.ErrSegmentEnded) {
		t.Fatalf("the Sync after the ended seal = %v, want ErrSegmentEnded: the stream must not move past the failure silently", err)
	}
	if state, err := e.set.State(seg1); err != nil || state != segment.Ended {
		t.Fatalf("the sealed-failed segment's state is %v, %v, want Ended", state, err)
	}

	// The next append rolls again, into a successor naming none (R33).
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the sync fault: %v", err)
	}
	h3, b3 := streamWriteHeader(streamFileID(3), 0, streamTestBody(64, 0x3c), 7, core.Version{Counter: 3})
	p3, err := e.s.AppendRoll(h3, b3, segment.Roll{})
	if err != nil {
		t.Fatalf("the append after the ended seal: %v", err)
	}
	if p3.Loc.Segment == seg1 {
		t.Fatalf("the append after the ended seal stayed in segment %d, want a rolled-to successor", seg1)
	}
	if hdr := rollHeader(t, e, p3.Loc.Segment); hdr.Predecessor != 0 {
		t.Fatalf("the successor after the ended seal names predecessor %d, want none (R33)", hdr.Predecessor)
	}
}

// TestRollContentFollowsARepackRollIntoTheReserveSpare pins the reserve's
// half of the class rule: the class decides only which spare a roll takes,
// and the segment it hands the stream is then the stream's active segment,
// which every later record fills whatever its own class. A repack copy that
// rolled onto the last reserve spare is therefore followed by content in the
// same segment, and a Sync covers it like any record. It pins the ceiling
// the decision at drawSpare names: content spends repack-reserve space until
// a stream of the copies' own keeps them apart.
func TestRollContentFollowsARepackRollIntoTheReserveSpare(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// One reserve spare, pinned so it is the only one the classes hold.
	e := rollPin(t, streamCap, segment.Targets{Reserve: 1}, nil).withStream(t, 1)
	awaitCounters(t, "the reserve spare ready", e.pool,
		func(c segment.PoolCounters) bool { return c.RepackReserveBytes == segCap && c.Spares == 0 })

	// The repack copy rolls onto the reserve spare.
	payload := streamTestBody(64, 0x3c)
	hr, br := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 1})
	p1, err := e.s.AppendRoll(hr, br, segment.Roll{Repack: true})
	if err != nil {
		t.Fatalf("the repack roll onto the ready reserve spare: %v", err)
	}

	// A plain content write follows into the segment the roll made: the
	// class named the roll's spare, and the stream's records after it take
	// the room they find, content included — the ceiling the decision names.
	hh, bb := streamWriteHeader(streamFileID(2), 0, payload, 7, core.Version{Counter: 2})
	p2, err := e.s.AppendRoll(hh, bb, segment.Roll{})
	if err != nil {
		t.Fatalf("the content write into the repack-made segment: %v", err)
	}
	if p2.Loc.Segment != p1.Loc.Segment {
		t.Fatalf("the content write rolled to segment %d, want the repack-made %d: the segment is the stream's once taken", p2.Loc.Segment, p1.Loc.Segment)
	}

	// What the pool holds afterwards: the repack reserve spent, and the
	// content class empty as it began.
	c := e.pool.Counters()
	if c.RepackReserveBytes != 0 {
		t.Fatalf("the reserve holds %d ready bytes after the repack roll, want the spare it took gone", c.RepackReserveBytes)
	}
	if c.Spares != 0 {
		t.Fatalf("the content class holds %d spares, want none", c.Spares)
	}
	// A third write, whose segment is nearly spent of room if the cap were
	// small, stays in the segment while room remains: it neither rolls nor
	// draws another class's spare.
	before := e.set.Counters()
	h3, b3 := streamWriteHeader(streamFileID(3), 64, payload, 7, core.Version{Counter: 3})
	p3, err := e.s.AppendRoll(h3, b3, segment.Roll{Repack: true})
	if err != nil {
		t.Fatalf("the second repack copy into the segment it shares: %v", err)
	}
	if p3.Loc.Segment != p1.Loc.Segment {
		t.Fatalf("the second repack copy rolled to segment %d, want the one segment the stream holds", p3.Loc.Segment)
	}
	if now := e.set.Counters(); now != before {
		t.Fatalf("the second repack copy moved the ledger: %+v, want %+v", now, before)
	}
}

// TestRollARecordTooLargeForAnEmptySegmentIsRefused checks the one refusal
// the roll cannot answer: a record whose size exceeds the room an empty
// segment holds — an entire segment is the largest room a stream hands out
// — answers ErrSegmentFull, reserves nothing, and takes no spare, so it
// neither rolls nor loops the pool drain. It catches the count guard that
// keeps a too-large record from rolling through spare after spare, each one
// sealed empty and ended.
func TestRollARecordTooLargeForAnEmptySegmentIsRefused(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The smallest room the format spells: the header, the marker, one
	// catalog entry and the trailer, with 16 bytes of record room past
	// them — too little for any record's own overhead.
	tinyCap := int64(format.SegmentHeaderSize) + 16 +
		int64(format.SealMarkerSize) + int64(format.CatalogEntrySize) + int64(format.TrailerSize)

	e := rollPin(t, tinyCap, segment.Targets{Content: 2}, nil).withStream(t, 1)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares == 2 })

	// A write whose record cannot fit the empty room: not even one payload
	// byte places against its own overhead.
	payload := streamTestBody(512, 0x3c)
	h, body := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 1})
	_, err := e.s.AppendRoll(h, body, segment.Roll{})
	if !errors.Is(err, segment.ErrSegmentFull) {
		t.Fatalf("the write too large for an empty segment = %v, want ErrSegmentFull", err)
	}

	// A header-only kind whose record does not fit the empty room is
	// refused the same way: it is not the payload the room lacks but the
	// record's own size.
	hh := format.RecordHeader{Kind: format.KindStamp, File: streamFileID(2), Version: core.Version{Counter: 2}, Tag: 7}
	if _, err := e.s.AppendRoll(hh, format.RecordBody{Cut: 4}, segment.Roll{}); !errors.Is(err, segment.ErrSegmentFull) {
		t.Fatalf("the stamp too large for an empty segment = %v, want ErrSegmentFull", err)
	}

	// Both refusals moved nothing: no spare was taken and no segment
	// sealed, so neither the pool nor the ledger shows a roll.
	c := e.pool.Counters()
	if c.Spares != 2 || c.SpareRefusals != 0 {
		t.Fatalf("after the refusals the pool holds %d spares and %d refusals, want 2 and 0: a too-large record must not drain the pool", c.Spares, c.SpareRefusals)
	}
	l := e.set.Counters()
	if l.Segments != 0 || l.Sealed != 0 || l.Spares != 2 {
		t.Fatalf("after the refusals the ledger holds %+v, want the two spares alone: no roll ran", l)
	}
}

// TestRollASealRunningUnderARollIsWaitedOut checks the roll's word for a
// segment another seal was already closing: the append whose record needs
// the room parks while the seal runs, and once the seal settles the roll it
// then makes names the sealed segment as its predecessor and installs a
// successor the stream keeps appending to — the old segment's footer written
// once, by the seal that won, and the successor's state never overwritten by
// it. It catches a successor named under a running seal, and a successor
// marked sealed by the seal's later return.
func TestRollASealRunningUnderARollIsWaitedOut(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := rollPin(t, 4096, segment.Targets{Content: 4}, nil).withStream(t, 1)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 3 })
	se := e.stream(t)

	// One record nearly fills the segment; the next record must roll.
	warm := make([]byte, 3400)
	wh, wb := streamWriteHeaderPair(streamFileID(1), 0, warm)
	warmPlaced := streamAppendWrite(t, se, wh, wb)
	seg1 := warmPlaced.Loc.Segment

	// An explicit seal parks inside its first sync: the segment is taken,
	// the state sealing, its footer not yet durable.
	stop := e.seam.HoldFirst(t, journaltest.OpSync, streamSegName(seg1))
	sealed := make(chan error, 1)
	go func() { _, err := e.s.Seal(false); sealed <- err }()
	<-stop.Reached()

	// The record that cannot fit parks while the seal runs — never
	// reserving beneath it — and rolls once the seal has settled.
	payload := streamTestBody(64, 0x3c)
	h, body := streamWriteHeader(streamFileID(2), 0, payload, 7, core.Version{Counter: 2})
	placed := make(chan segment.Placed, 1)
	appErr := make(chan error, 1)
	go func() {
		p, err := e.s.AppendRoll(h, body, segment.Roll{})
		placed <- p
		appErr <- err
	}()
	select {
	case err := <-sealed:
		t.Fatalf("the parked Seal returned %v while the record waited: it must hold the segment until the roll follows it", err)
	case err := <-appErr:
		t.Fatalf("the rolling append returned %v while the seal ran", err)
	case <-time.After(50 * time.Millisecond):
	}

	// The seal settles; the append then rolls and lands in the successor.
	stop.Release()
	if err := <-sealed; err != nil {
		t.Fatalf("the Seal whose roll followed: %v", err)
	}
	// The place is read first, for the append's return fills both channels
	// and a select between them would flip a coin.
	var p segment.Placed
	select {
	case p = <-placed:
	case <-time.After(10 * time.Second):
		t.Fatal("the rolling append never returned after the seal settled")
	}
	if err := <-appErr; err != nil {
		t.Fatalf("the rolling append after the settled seal: %v", err)
	}
	{
		if p.Loc.Segment == seg1 {
			t.Fatalf("the record landed in the sealed segment %d, want its successor", seg1)
		}
		if hdr := rollHeader(t, e, p.Loc.Segment); hdr.Predecessor != seg1 {
			t.Fatalf("the successor names predecessor %d, want the sealed segment %d: it was named under the seal", hdr.Predecessor, seg1)
		}
		// The successor stays the stream's active segment: a second
		// record lands in it without another roll, and the seal that ran
		// never overwrote its state.
		h2, b2 := streamWriteHeader(streamFileID(3), 64, payload, 7, core.Version{Counter: 3})
		p2, err := e.s.AppendRoll(h2, b2, segment.Roll{})
		if err != nil {
			t.Fatalf("the second record in the successor: %v", err)
		}
		if p2.Loc.Segment != p.Loc.Segment {
			t.Fatalf("the second record rolled to segment %d, want the successor %d: its state was overwritten", p2.Loc.Segment, p.Loc.Segment)
		}
		if state, err := e.set.State(seg1); err != nil || state != segment.Sealed {
			t.Fatalf("the sealed segment's state after the roll is %v, %v, want Sealed", state, err)
		}
		if state, err := e.set.State(p.Loc.Segment); err != nil || state != segment.Active {
			t.Fatalf("the successor's state after the roll is %v, %v, want Active", state, err)
		}
	}
}

// TestRollAStreamClosedAtItsSuccessorsHeaderIsRefusedAndTheSpareEnded checks
// the install step's own close check — nearer than the seal's: a close that
// lands while the roll is writing its successor's header, after the seal has
// settled, must make the append answer ErrStreamClosed, and the taken spare
// is ended in the ledger — never installed over the closed stream and never
// left a spare or an active segment behind, so no ref is released twice.
// It catches a successor installed past a close, whose ref Close would not
// hold and whose double release would drop a descriptor an fsync needed.
func TestRollAStreamClosedAtItsSuccessorsHeaderIsRefusedAndTheSpareEnded(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := rollPin(t, 4096, segment.Targets{Content: 4}, nil).withStream(t, 1)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares == 4 })
	se := e.stream(t)

	// One record fills the segment to its cap, so the next record must
	// roll: its roll takes the next spare in id order and writes the
	// successor's header on it — the write the close must race.
	warm := make([]byte, 3736)
	wh, wb := streamWriteHeaderPair(streamFileID(1), 0, warm)
	warmPlaced := streamAppendWrite(t, se, wh, wb)
	seg1 := warmPlaced.Loc.Segment
	seg2 := seg1 + 1 // the pinned pool's FIFO hands the next id out

	// Park the successor's header write: the roll has taken its spare,
	// sealed the full segment, and reached its last I/O before install.
	stop := e.seam.HoldFirst(t, journaltest.OpWrite, segmentName(seg2))
	payload := streamTestBody(1024, 0x3c)
	h, body := streamWriteHeader(streamFileID(2), 0, payload, 7, core.Version{Counter: 2})
	appendErr := make(chan error, 1)
	go func() {
		_, err := e.s.AppendRoll(h, body, segment.Roll{})
		appendErr <- err
	}()
	<-stop.Reached()

	// Close the stream while the header write parks: the close's wait
	// cannot pass until the append does, and the append's roll then finds
	// the flag set at install.
	closed := make(chan error, 1)
	go func() { closed <- e.s.Close() }()
	for range 1 << 20 {
		if _, err := e.s.Seal(false); errors.Is(err, segment.ErrStreamClosed) {
			break
		}
		runtime.Gosched()
	}

	// Release the header write: the append answers the close and the taken
	// spare is ended — never installed.
	stop.Release()
	if err := <-closed; err != nil {
		t.Fatalf("Close after the roll it waited out: %v", err)
	}
	if err := <-appendErr; !errors.Is(err, segment.ErrStreamClosed) {
		t.Fatalf("the append closed under the roll's install = %v, want ErrStreamClosed", err)
	}
	if state, err := e.set.State(seg2); err != nil || state != segment.Ended {
		t.Fatalf("the taken spare's state after the closed install is %v, %v, want Ended at its cap", state, err)
	}
	if state, err := e.set.State(seg1); err != nil || state != segment.Sealed {
		t.Fatalf("the full segment's state after the closed roll is %v, %v, want Sealed", state, err)
	}
	// Nothing leaked: the segments' own footprint holds the sealed and the
	// unwound spare whole, and every other spare is still the pool's.
	c := e.set.Counters()
	if got := c.UsedBytes - c.Spares*4096; got != 2*4096 {
		t.Fatalf("the segments account for %d after the closed install, want the sealed and the unwound at their caps", got)
	}
	if c.Spares != 2 {
		t.Fatalf("the pool holds %d ready spares after the closed install, want the two the takes never reached", c.Spares)
	}
}

// TestRollAWriteLargerThanASegmentIsSplitAndItsRestRolls checks the split at
// the scale of a whole segment (R2): a write whose payload exceeds even an
// empty segment's room is not refused but split — this call places the
// largest piece an empty segment holds, and the caller's next call for the
// rest rolls onto a new segment naming the first. It catches a too-large
// guard that refuses the record instead of splitting it.
func TestRollAWriteLargerThanASegmentIsSplitAndItsRestRolls(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := rollPin(t, 4096, segment.Targets{Content: 4}, nil).withStream(t, 1)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 3 })
	se := e.stream(t)

	// The first segment's piece: the largest an empty segment of this cap
	// holds is the room past the header and the seal's reserve, less the
	// record's own overhead.
	overhead := int64(format.RecordHeaderSize) + 8
	wantPiece := ((4096 - format.SegmentHeaderSize -
		(format.SealMarkerSize + format.CatalogEntrySize + format.TrailerSize)) &^ 7) - overhead

	payload := streamTestBody(9000, 0xa5)
	h, body := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 9})
	p1, err := e.s.AppendRoll(h, body, segment.Roll{})
	if err != nil {
		t.Fatalf("the write larger than a segment: %v, want its largest piece placed", err)
	}
	if p1.N != wantPiece {
		t.Fatalf("the piece placed %d bytes, want the largest an empty segment holds %d", p1.N, wantPiece)
	}
	if p1.N <= 0 || p1.N >= int64(len(payload)) {
		t.Fatalf("the piece placed %d of %d, want a part", p1.N, len(payload))
	}
	seg1 := p1.Loc.Segment

	// The piece carries the write's own file offset and the bytes it
	// claims, at the taken segment's first slot.
	h1, b1 := streamReadRecord(t, se, p1.Loc)
	if h1.Extent.Off != 0 {
		t.Fatalf("the piece's file offset is %d, want 0", h1.Extent.Off)
	}
	if h1.Extent.Len != p1.N || !bytes.Equal(b1.Payload, payload[:p1.N]) {
		t.Fatalf("the piece's decoded record does not match the bytes it placed")
	}
	if p1.Loc.Off != format.SegmentHeaderSize {
		t.Fatalf("the piece landed at %d, want the first slot", p1.Loc.Off)
	}

	// The rest rolls: it lands in a new segment naming the first, as one
	// record split across two (R2) — and its own piece is what the new
	// segment holds, with the caller continuing likewise.
	rest := payload[p1.N:]
	h2, b2 := streamWriteHeader(streamFileID(1), p1.N, rest, 7, core.Version{Counter: 9})
	p2, err := e.s.AppendRoll(h2, b2, segment.Roll{})
	if err != nil {
		t.Fatalf("the rest of the over-large write: %v", err)
	}
	if p2.N <= 0 || p2.N >= int64(len(rest)) {
		t.Fatalf("the rest placed %d of %d, want a part itself: the split continues across segments", p2.N, len(rest))
	}
	seg2 := p2.Loc.Segment
	if seg2 == seg1 {
		t.Fatalf("the rest landed in segment %d, want a rolled-to new one", seg2)
	}
	if hdr := rollHeader(t, e, seg2); hdr.Predecessor != seg1 {
		t.Fatalf("the second piece's segment names predecessor %d, want the first's %d", hdr.Predecessor, seg1)
	}
	if state, err := e.set.State(seg1); err != nil || state != segment.Sealed {
		t.Fatalf("the first segment's state after the split = %v, %v, want Sealed", state, err)
	}
}
