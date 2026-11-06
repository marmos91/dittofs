package segment_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// streamCap is the cap of the sets these tests build, small enough that a
// modest record straddles it and large enough that many records fit.
const streamCap = int64(64 << 10)

// streamMax is the journal maximum the stream tests pass, so a take is never
// refused by it.
const streamMax = math.MaxInt64

// streamIdentity is one journal identity every stream test writes under.
var streamIdentity = core.JournalID{0xd, 0xe, 0xc, 0xa, 0xd, 0}

// streamEnv is one stream over the seam, its ledger, its spare pool and its
// cache: the building set of every stream check.
type streamEnv struct {
	seam   *journaltest.Seam
	set    *segment.Set
	maker  *segment.Maker
	pool   *segment.Pool
	cache  *segment.Descriptors
	budget *core.DescriptorBudget
	s      *segment.Stream
	// cap is the segment cap the env's set was built with, the figure the
	// footprint checks below need to strip the pool's ready spares out of
	// the ledger's footprint.
	cap int64
}

// streamEnvBuilt registers the cache's Shutdown as the last cleanup, so a
// leaked reference fails the test rather than passing silently.
func (e streamEnv) built(t *testing.T) streamEnv {
	t.Helper()
	e.cacheBuilt(t)
	return e
}

// cacheBuilt registers the cache's Shutdown as a cleanup, checking its return.
func (e streamEnv) cacheBuilt(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if err := e.cache.Shutdown(); err != nil {
			t.Errorf("the cache's Shutdown = %v, want nil", err)
		}
	})
}

// streamOver builds a stream over a fresh seam and ledger, handing the half
// built environment to setup — seam, set, maker, cache — so a fault or a
// hold the setup installs is armed for the very first Append.
func streamOver(t *testing.T, cap int64, setup func(e streamEnv)) streamEnv {
	t.Helper()

	seam, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	set := segment.NewSet(cap, nil)
	// decision: the maker's maximum is pinned at two caps — exactly the
	// two content spares the pool keeps — so once the worker has met its
	// target the ledger holds nothing else: a stream's take drains the
	// class and the refill it kicks is refused by the maximum before any
	// storage is touched, which makes every count below stand on the
	// appends alone, never on a race with the worker. Checks that need a
	// stream to roll several times build their envs over rolls of their
	// own targets.
	maker := segment.NewMaker(seam.FS(), set, 2*cap, 0, nil)
	budget := core.NewDescriptorBudget(64)
	cache := segment.NewDescriptors(seam.FS(), format.SegmentName, budget)
	pool := segment.NewPool(maker, segment.Targets{Content: 2}, nil, nil)

	e := streamEnv{seam: seam, set: set, maker: maker, pool: pool, cache: cache, budget: budget, cap: cap}
	setup(e)
	// The pool's Stop is registered before the stream's Close and the
	// cache's Shutdown so it runs after them: the stream holds a ref the
	// Shutdown would wait on, and the worker holds the maker over the seam.
	t.Cleanup(func() { pool.Stop() })
	pool.Start()
	// The worker makes spares on its own goroutine, so a stream that appends
	// the moment it is built races the first make: wait until both content
	// spares are ready, which the pinned maximum holds there.
	awaitCounters(t, "the two content spares ready", pool,
		func(c segment.PoolCounters) bool { return c.Spares == 2 })
	e.s = segment.NewStream(1, streamIdentity, set, pool, cache, nopSink())
	return e
}

// streamSegName returns the segment file name of id, as the cache names it.
func streamSegName(id core.SegmentID) string { return format.SegmentName(id) }

// segmentsBytes returns the accounted size of the set's segments alone, the
// pool's ready spares apart: the figure every check that pins a segment's
// own footprint wants, for the ledger counts a ready spare beside them and
// a take moves nothing. Every spare is accounted at the env's cap.
func segmentsBytes(e streamEnv, c segment.Counters) int64 {
	return c.UsedBytes - c.Spares*e.cap
}

// streamWriteHeader returns the header and body of one write of len(payload)
// bytes at file offset off, of the file fid, carrying tag and version.
func streamWriteHeader(fid core.FileID, off int64, payload []byte, tag core.ShareTag, version core.Version) (format.RecordHeader, format.RecordBody) {
	h := format.RecordHeader{
		Kind:    format.KindWrite,
		File:    fid,
		Extent:  core.Extent{Off: off, Len: int64(len(payload))},
		Version: version,
		Tag:     tag,
	}
	body := format.RecordBody{Payload: payload, MTime: 1720000000000000000}
	return h, body
}

// streamAppendWrite appends one record through the stream, failing the test on any
// error, and returns where it landed.
func streamAppendWrite(t *testing.T, e streamEnv, h format.RecordHeader, body format.RecordBody) segment.Placed {
	t.Helper()

	p, err := e.s.Append(h, body)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return p
}

// streamReadRecord reads the record the location names back through the
// stream's ReadAt, decodes its header and verifies its body, failing the test
// on any refused step. It returns the header it decoded.
func streamReadRecord(t *testing.T, e streamEnv, loc segment.Location) (format.RecordHeader, format.RecordBody) {
	t.Helper()

	raw := make([]byte, format.RecordHeaderSize)
	if _, err := e.s.ReadAt(loc.Segment, raw, loc.Off); err != nil {
		t.Fatalf("ReadAt of the record at %d: %v", loc.Off, err)
	}
	h, err := format.DecodeRecordHeader(raw, streamIdentity, loc.Segment, loc.Off)
	if err != nil {
		t.Fatalf("the record at %d does not decode: %v", loc.Off, err)
	}
	body := make([]byte, h.BodySize())
	if _, err := e.s.ReadAt(loc.Segment, body, loc.Off+format.RecordHeaderSize); err != nil {
		t.Fatalf("ReadAt of the record body at %d: %v", loc.Off, err)
	}
	b, err := format.VerifyRecordBody(body, streamIdentity, h)
	if err != nil {
		t.Fatalf("the record body at %d does not verify: %v", loc.Off, err)
	}
	return h, b
}

// streamFileID returns the FileID of the i-th test file.
func streamFileID(i int) core.FileID {
	var id [16]byte
	id[0] = byte(i)
	id[1] = 0xaa
	return core.NewFileID(id)
}

// TestStreamAFailedWriteEndsItsSegmentAndTheStreamRollsPastIt checks R48 at
// the seam: one record lands and is synced, the next write fails with the
// seam's EIO — no location — and the segment is ended in the ledger, never
// taking a further append, while the next append rolls into a new segment
// whose header names no predecessor. It catches a stream that keeps landing
// records in a segment a failed write ended. (R10's synced-through bound over
// the failed slot is held by the concurrent checks of end_test.go, which run
// the zero-slot choreography this sequential shape cannot.)
func TestStreamAFailedWriteEndsItsSegmentAndTheStreamRollsPastIt(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// One synced record, then a failed one: the fault is armed once the
	// first record has landed and been synced — Failing fires from a rule's
	// first matching call on, so arming it after the first record's write
	// makes the very next write of the segment the failed one.
	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	// The first write lands and is synced, so its record carries a
	// synced-through of zero and the stream holds its first segment.
	h1 := streamMustWrite(t, e, streamFileID(1), 0, make([]byte, 64), 7, 1)
	if h1.SyncedThrough != 0 {
		t.Fatalf("the first record carries SyncedThrough %d, want 0", h1.SyncedThrough)
	}
	firstSegment := h1.Segment
	if err := e.s.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// The second write fails: the fault's EIO, and no location. Armed now,
	// the rule fires on this write and this write alone.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	payload2 := make([]byte, 64)
	h2, b2 := streamWriteHeader(streamFileID(1), 64, payload2, 7, core.Version{Counter: 2})
	if _, err := e.s.Append(h2, b2); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the faulted Append = %v, want the EIO", err)
	}
	// The fault has spent its one round; later appends must land.
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the write fault: %v", err)
	}

	// The failed write's segment is ended in the ledger, never sealed: it
	// takes no further append, not even a seal marker (R48).
	if state, err := e.set.State(firstSegment); err != nil || state != segment.Ended {
		t.Fatalf("the failed segment's state is %v, %v, want Ended", state, err)
	}

	// The next appends land in a new segment whose header names no
	// predecessor, and the records there read back and sync well: the
	// failure belongs to the ended segment alone.
	payload := streamTestBody(64, 0x3c)
	h, body := streamWriteHeader(streamFileID(2), 0, payload, 8, core.Version{Counter: 3})
	p := streamAppendWrite(t, e, h, body)
	if p.Loc.Segment == firstSegment {
		t.Fatalf("the append after the failed write stayed in segment %d, want a rolled-to new one: a failed append ends its segment", firstSegment)
	}
	if hdr := streamSegmentHeader(t, e, p.Loc.Segment); hdr.Predecessor != 0 {
		t.Fatalf("the new segment names predecessor %d, want none: the stream continues past a failed append in a segment naming no predecessor", hdr.Predecessor)
	}
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the Sync on the rolled-to segment = %v, want nil", err)
	}

	// The second append stays in that segment: the stream did not end more
	// of it than the one failure.
	h3, b3 := streamWriteHeader(streamFileID(2), 64, streamTestBody(64, 0x7d), 8, core.Version{Counter: 4})
	p3 := streamAppendWrite(t, e, h3, b3)
	if p3.Loc.Segment != p.Loc.Segment {
		t.Fatalf("the second append after the failure landed in segment %d, want the first's %d: one failure rolls once", p3.Loc.Segment, p.Loc.Segment)
	}
}

// streamMustWrite appends one record and reads its own header back, so the
// caller holds the record's decoded header without the ceremony.
func streamMustWrite(t *testing.T, e streamEnv, fid core.FileID, off int64, payload []byte, tag core.ShareTag, version uint64) format.RecordHeader {
	t.Helper()

	h, body := streamWriteHeader(fid, off, payload, tag, core.Version{Counter: version})
	p := streamAppendWrite(t, e, h, body)
	got, _ := streamReadRecord(t, e, p.Loc)
	return got
}

// streamTestBody returns a payload of n bytes, each a function of its index
// and the seed, so two records never hold the same bytes.
func streamTestBody(n int, seed byte) []byte {
	payload := make([]byte, n)
	for i := range payload {
		payload[i] = byte(i) ^ seed
	}
	return payload
}

// TestStreamSixteenConcurrentAppendsCoverTheSegment checks R5, R6 and R87:
// sixteen goroutines, one per file, each append through the one stream; the
// positions are distinct, 8-aligned, non-overlapping and together cover
// [48, next) with no gaps; every record reads back through ReadAt, decodes
// and verifies byte for byte, while a reader goroutine reads completed
// locations at the same time. A double-reserved position or a torn write
// shows up as an overlap or a record that does not read back.
func TestStreamSixteenConcurrentAppendsCoverTheSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	const appenders = 16
	const perAppender = 4

	var wg sync.WaitGroup
	type result struct {
		p       segment.Placed
		payload []byte
		header  format.RecordHeader
	}
	results := make(chan result, appenders*perAppender)

	// One reader that reads the locations as they land, on a channel of its
	// own: the results channel is what the verification below drains, and a
	// reader competing for it would starve the drain.
	reads := make(chan result, appenders*perAppender)
	readStop := make(chan struct{})
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case r := <-reads:
				raw := make([]byte, format.RecordHeaderSize)
				if _, err := e.s.ReadAt(r.p.Loc.Segment, raw, r.p.Loc.Off); err != nil {
					t.Errorf("a concurrent ReadAt at %d: %v", r.p.Loc.Off, err)
					return
				}
			case <-readStop:
				return
			default:
				runtime.Gosched() // yield rather than spin while nothing arrived
			}
		}
	}()

	for i := range appenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range perAppender {
				payload := make([]byte, 32)
				for k := range payload {
					payload[k] = byte(i*16 + j)
				}
				h, body := streamWriteHeader(streamFileID(i), int64(j)*32, payload, core.ShareTag(7), core.Version{Counter: uint64(i*perAppender + j + 1)})
				p, err := e.s.Append(h, body)
				if err != nil {
					t.Errorf("appender %d's append %d: %v", i, j, err)
					return
				}
				r := result{p: p, payload: payload, header: h}
				results <- r
				select {
				case reads <- r:
				default:
				}
			}
		}(i)
	}
	wg.Wait()
	// Stop the reader, never the channel it drains: a closed buffered channel
	// is always ready to receive, so the select's other arm would loop on a
	// zero result forever — reading segment 0 at offset 0 — until stopped.
	// The stop channel is the reader's only exit; whatever the buffered
	// channel still holds is simply left, for the appenders sent on it only
	// to give the reader something to read.
	close(readStop)
	readerWG.Wait()

	// Every record reads back, decodes and verifies, byte for byte.
	positions := make([]int64, 0, appenders*perAppender)
	placedTotal := 0
	for range appenders * perAppender {
		r := <-results
		placedTotal++
		h, b := streamReadRecord(t, e, r.p.Loc)
		if h.Offset != r.p.Loc.Off {
			t.Fatalf("the record at %d decodes with its own offset %d", r.p.Loc.Off, h.Offset)
		}
		if h.File != r.header.File {
			t.Fatalf("the record at %d names the file of another appender", r.p.Loc.Off)
		}
		if !bytes.Equal(b.Payload, r.payload) {
			t.Fatalf("the record at %d does not read back byte for byte", r.p.Loc.Off)
		}
		if r.p.N != int64(len(r.payload)) {
			t.Fatalf("the record at %d placed %d payload bytes, want the whole %d", r.p.Loc.Off, r.p.N, len(r.payload))
		}
		positions = append(positions, r.p.Loc.Off)
	}
	if placedTotal != appenders*perAppender {
		t.Fatalf("%d appends placed, want %d", placedTotal, appenders*perAppender)
	}

	// The positions are distinct, ascending-sorted covered and gapless from
	// the header's end.
	slices.Sort(positions)
	if positions[0] != format.SegmentHeaderSize {
		t.Fatalf("the first record sits at %d, want the first slot %d", positions[0], format.SegmentHeaderSize)
	}
	for i := 1; i < len(positions); i++ {
		if positions[i]%8 != 0 {
			t.Fatalf("the record at %d is not 8-aligned", positions[i])
		}
		if positions[i] <= positions[i-1] {
			t.Fatalf("the positions %d and %d overlap or repeat", positions[i-1], positions[i])
		}
	}

	// Together with their sizes they cover [48, next) with no gaps: read the
	// bytes between consecutive positions and check each record's length
	// bridges to the next.
	var covered int64 = format.SegmentHeaderSize
	for i, off := range positions {
		if off != covered {
			t.Fatalf("record %d sits at %d, want %d: the appends left a gap", i, off, covered)
		}
		raw := make([]byte, format.RecordHeaderSize)
		if _, err := e.s.ReadAt(1, raw, off); err != nil {
			t.Fatalf("ReadAt at %d: %v", off, err)
		}
		h, err := format.DecodeRecordHeader(raw, streamIdentity, 1, off)
		if err != nil {
			t.Fatalf("the record at %d does not decode: %v", off, err)
		}
		covered += h.Size()
	}
	if want := streamScanNext(t, e); covered != want {
		t.Fatalf("the records cover [48, %d) but the stream's next is %d", covered, want)
	}
}

// TestStreamAStraddlingWriteIsSplitAtTheCap checks the split (R2): a small
// cap, a write whose payload straddles the remaining room — the returned
// piece's decoded header names the write's file offset, the version and only
// the bytes that fit; N is below the payload; and the reserve holds after the
// piece. The following append of the rest rolls into a new segment naming the
// first; a header-only kind that does not fit rolls the same way.
func TestStreamAStraddlingWriteIsSplitAtTheCap(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// A cap the first record nearly fills: one record of 1024 payload
	// bytes, then a straddling write.
	e := streamOver(t, 4096, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	payload1 := make([]byte, 1024)
	h1, b1 := streamWriteHeaderPair(streamFileID(1), 0, payload1)
	p1 := streamAppendWrite(t, e, h1, b1)
	if p1.N != int64(len(payload1)) {
		t.Fatalf("the first record placed %d bytes, want the whole %d", p1.N, len(payload1))
	}

	// How much room remains before the straddle: cap - next.
	room := 4096 - streamScanNext(t, e)

	// The straddling write: three kilobytes, so no whole record fits.
	payload2 := make([]byte, 3072)
	for i := range payload2 {
		payload2[i] = byte(0xa5)
	}
	h2, b2 := streamWriteHeader(streamFileID(2), 4096, payload2, 7, core.Version{Counter: 9})
	p2, err := e.s.Append(h2, b2)
	if err != nil {
		t.Fatalf("the straddling Append: %v", err)
	}
	if p2.N == 0 || p2.N >= int64(len(payload2)) {
		t.Fatalf("the straddling Append placed %d bytes, want a piece in [1, %d)", p2.N, len(payload2))
	}

	// The piece decodes with the write's offset and version, and the
	// payload it names matches.
	h2p, b2p := streamReadRecord(t, e, p2.Loc)
	if h2p.Extent.Off != 4096 {
		t.Fatalf("the piece's file offset is %d, want the write's 4096", h2p.Extent.Off)
	}
	if h2p.Extent.Len != p2.N {
		t.Fatalf("the piece's length is %d, want the %d it placed", h2p.Extent.Len, p2.N)
	}
	if h2p.Version != (core.Version{Counter: 9}) {
		t.Fatalf("the piece carries version %+v, want the write's", h2p.Version)
	}
	if !bytes.Equal(b2p.Payload, payload2[:p2.N]) {
		t.Fatal("the piece's payload is not the bytes it claims to stage")
	}

	// The room the seal needs survives the piece: next + sealRoom(count)
	// stays within 8 bytes of the cap (the padding a record takes).
	after := streamScanNext(t, e)
	_, _, count, _ := streamScanOf(t, e)
	seal := format.SealMarkerSize + count*format.CatalogEntrySize + format.TrailerSize
	if after+seal > 4096+8 {
		t.Fatalf("after the piece the stream sits at %d with a seal room of %d, want them within 8 bytes of the cap %d", after, seal, 4096)
	}
	if after <= 4096-room {
		// nothing was placed
		t.Fatalf("after the piece the stream sits at %d, want it past the %d of remaining room", after, 4096-room)
	}

	// The rest does not fit, so the next append rolls: it lands in a new
	// segment whose header names the first, and the piece the stream returns
	// has no empty bytes before it. The roll's contract replaces the refusal
	// the no-roll skeleton answered.
	rest := payload2[p2.N:]
	h3, b3 := streamWriteHeader(streamFileID(2), 4096+p2.N, rest, 7, core.Version{Counter: 10})
	p3, err := e.s.Append(h3, b3)
	if err != nil {
		t.Fatalf("the append of the rest: %v", err)
	}
	if p3.N != int64(len(rest)) {
		t.Fatalf("the rest placed %d bytes, want the whole %d", p3.N, len(rest))
	}
	if p3.Loc.Segment == p2.Loc.Segment {
		t.Fatalf("the rest landed in segment %d, want a rolled-to new one", p3.Loc.Segment)
	}
	if hdr := streamSegmentHeader(t, e, p3.Loc.Segment); hdr.Predecessor != p2.Loc.Segment {
		t.Fatalf("the new segment names predecessor %d, want the full segment %d", hdr.Predecessor, p2.Loc.Segment)
	}

	// A header-only kind that does not fit rolls like any record: the stamp
	// lands in the successor the rest rolled to, whose room takes it whole.
	h4 := format.RecordHeader{Kind: format.KindStamp, File: streamFileID(3), Tag: 7}
	p4, err := e.s.Append(h4, format.RecordBody{Cut: 4})
	if err != nil {
		t.Fatalf("the stamp that does not fit: %v", err)
	}
	if p4.Loc.Segment != p3.Loc.Segment {
		t.Fatalf("the stamp landed in segment %d, want it in the successor %d the roll took", p4.Loc.Segment, p3.Loc.Segment)
	}
	if p4.Loc.Off != p3.Loc.Off+p3.Loc.Len {
		t.Fatalf("the stamp landed at %d, want the slot past the rest's record %d", p4.Loc.Off, p3.Loc.Off+p3.Loc.Len)
	}
}

// streamWriteHeaderPair returns one write's header and body for payload.
func streamWriteHeaderPair(fid core.FileID, off int64, payload []byte) (format.RecordHeader, format.RecordBody) {
	return streamWriteHeader(fid, off, payload, 7, core.Version{Counter: 1})
}

// TestStreamARecordThatExactlyFillsTheRoomIsOnePiece checks the RFC 1 §9.2
// edge: a write whose record plus the seal's reserve equals the remaining
// room exactly is placed whole — N equals the payload's length, no empty
// piece and no early split.
func TestStreamARecordThatExactlyFillsTheRoomIsOnePiece(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The cap is chosen so one record fills what remains exactly: after the
	// header and one record of payload n, next + size + sealRoom(2) == cap.
	// Work the payload out: RecordHeaderSize + 8 (mtime) + n, padded to 8,
	// plus reserve for two entries.
	headerPayload := 1024
	reserve := int64(format.SealMarkerSize) + 2*int64(format.CatalogEntrySize) + int64(format.TrailerSize)
	recordSize := int64(format.RecordHeaderSize) + 8 + int64(headerPayload)
	recordSize = (recordSize + 7) &^ 7
	capNeeded := int64(format.SegmentHeaderSize) + recordSize + reserve

	e := streamOver(t, capNeeded, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	payload := make([]byte, headerPayload)
	h, body := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 1})
	p := streamAppendWrite(t, e, h, body)
	if p.N != int64(headerPayload) {
		t.Fatalf("the exactly-filling record placed %d bytes, want the whole %d: it split early or left an empty piece", p.N, headerPayload)
	}
	if p.Loc.Len != recordSize {
		t.Fatalf("the record's size is %d, want the whole %d", p.Loc.Len, recordSize)
	}
	if _, b := streamReadRecord(t, e, p.Loc); !bytes.Equal(b.Payload, payload) {
		t.Fatal("the exactly-filling record does not read back byte for byte")
	}

	// Nothing more fits, so the next append rolls: it lands in a new
	// segment naming the exactly-filled one, and the first segment is
	// sealed with its two records' footer intact.
	h2, body2 := streamWriteHeader(streamFileID(2), 0, make([]byte, 1), 7, core.Version{Counter: 2})
	p2, err := e.s.Append(h2, body2)
	if err != nil {
		t.Fatalf("the append after the exact fill: %v", err)
	}
	if p2.Loc.Segment == p.Loc.Segment {
		t.Fatalf("the append after the exact fill landed in segment %d, want a rolled-to new one", p2.Loc.Segment)
	}
	if state, err := e.set.State(p.Loc.Segment); err != nil || state != segment.Sealed {
		t.Fatalf("the exactly-filled segment's state after the roll is %v, %v, want Sealed", state, err)
	}
}

// TestStreamTheSealReserveSurvivesTheAppends checks R11 for several record
// sizes: after appends fill the segment, the space left past next still holds
// the marker, a footer entry per record and the trailer. The loop stops at the
// first roll — the record that did not fit landed in the next segment, which
// is what fills this one — and the scan then reads the filled segment itself.
// It catches a room check that forgets the catalog entry of the record being
// placed.
func TestStreamTheSealReserveSurvivesTheAppends(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, payloadLen := range []int64{16, 128, 1024} {
		t.Run(fmt.Sprintf("payload %d", payloadLen), func(t *testing.T) {
			e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
			defer func() { _ = e.s.Close() }()

			var first core.SegmentID // the segment the records filled, 0 until the first lands
			var count int64
			for i := 0; ; i++ {
				payload := make([]byte, payloadLen)
				h, body := streamWriteHeader(streamFileID(1), int64(i)*payloadLen, payload, 7, core.Version{Counter: uint64(i + 1)})
				p, err := e.s.Append(h, body)
				if err != nil {
					t.Fatalf("append %d: %v", i, err)
				}
				if first == 0 {
					first = p.Loc.Segment
					count++
					continue
				}
				if p.Loc.Segment != first {
					break // the record rolled: the filled segment's room is measured below
				}
				count++
			}
			if count == 0 {
				t.Fatal("no record fit at all")
			}

			// The filled segment: read back its records to where they end
			// and check the room left past the last.
			var next int64
			off := int64(format.SegmentHeaderSize)
			for {
				raw := make([]byte, format.RecordHeaderSize)
				if _, err := e.s.ReadAt(first, raw, off); err != nil {
					t.Fatalf("reading the record header at %d: %v", off, err)
				}
				h, err := format.DecodeRecordHeader(raw, streamIdentity, first, off)
				if err != nil {
					break
				}
				next = off + h.Size()
				off = next
			}
			room := streamCap - next
			want := format.SealMarkerSize + count*format.CatalogEntrySize + format.TrailerSize
			if room < want {
				t.Fatalf("after %d records the segment leaves %d bytes past next, want at least the %d the seal needs (marker, %d entries, trailer)",
					count, room, want, count)
			}
		})
	}
}

// TestStreamEachRangeWrittenOnceAndAccounted checks R87 and R75: after N
// appends the segment's file saw exactly N+1 writes (the header and the
// records), the footprint never moved past the take, and the share's record
// bytes are the sum of the records' sizes.
func TestStreamEachRangeWrittenOnceAndAccounted(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	const records = 8
	var sum int64
	var locations []segment.Location
	for i := range records {
		payload := make([]byte, 128)
		h, body := streamWriteHeader(streamFileID(1), int64(i)*128, payload, 7, core.Version{Counter: uint64(i + 1)})
		p := streamAppendWrite(t, e, h, body)
		sum += p.Loc.Len
		locations = append(locations, p.Loc)
	}

	name := streamSegName(1)
	if got := e.seam.CallsFor(journaltest.OpWrite, name); got != records+1 {
		t.Fatalf("the segment saw %d writes, want the header plus %d records = %d", got, records, records+1)
	}

	// The take accounted the segment at its cap, and no append moved the
	// footprint (the bytes are inside storage the segment already accounts):
	// the segments' bytes, the pool's ready spares apart, are the cap alone.
	if c := e.set.Counters(); segmentsBytes(e, c) != streamCap {
		t.Fatalf("the segments account for %d after the appends, want the cap %d alone", segmentsBytes(e, c), streamCap)
	}
	if got := e.set.ShareBytes(7); got != sum {
		t.Fatalf("the share holds %d record bytes, want the sum of the records' sizes %d", got, sum)
	}

	// Each record still reads back whole.
	for _, loc := range locations {
		if _, err := e.s.ReadAt(1, make([]byte, format.RecordHeaderSize), loc.Off); err != nil {
			t.Fatalf("ReadAt at %d: %v", loc.Off, err)
		}
	}
}

// TestStreamTheLocationIsReturnedOnlyAfterTheBytesAreWritten checks R5's
// publishing half: with one record write parked at a seam hold, the append
// has not returned and the share is not yet charged; release the hold and the
// append returns its location. An append that published early would let a
// reader race the bytes it names.
func TestStreamTheLocationIsReturnedOnlyAfterTheBytesAreWritten(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {})
	e.cacheBuilt(t)
	defer func() { _ = e.s.Close() }() // before the cache's Shutdown: the stream holds a ref

	// Warm the stream first: the take's header and one landed record, so the
	// parked write below is a record's, with a reservation ahead of it and a
	// share charge behind it.
	warm := streamTestBody(64, 0x5a)
	wh, wbody := streamWriteHeader(streamFileID(1), 0, warm, 7, core.Version{Counter: 1})
	warmPlaced := streamAppendWrite(t, e, wh, wbody)
	warmCharge := warmPlaced.Loc.Len
	if got := e.set.ShareBytes(7); got != warmCharge {
		t.Fatalf("the share holds %d bytes after the warm record, want its %d alone", got, warmCharge)
	}

	// Park the next record's write: one append is now between its
	// reservation and its bytes.
	h := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	appended := make(chan segment.Placed, 1)
	appErr := make(chan error, 1)
	go func() {
		payload := streamTestBody(64, 0xc3)
		hh, body := streamWriteHeader(streamFileID(2), 64, payload, 7, core.Version{Counter: 2})
		p, err := e.s.Append(hh, body)
		appended <- p
		appErr <- err
	}()
	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the record write never reached the hold")
	}

	select {
	case p := <-appended:
		t.Fatalf("the append returned %v while its record write was parked, want it held: the location is published only once the bytes are written", p.Loc)
	case err := <-appErr:
		t.Fatalf("the append returned %v while its record write was parked", err)
	default:
	}
	if got := e.set.ShareBytes(7); got != warmCharge {
		t.Fatalf("the share holds %d bytes while the parked record is unwritten, want the warm record's %d alone: the bytes are charged only once written", got, warmCharge)
	}
	// The segment's own footprint never moved past its take's cap.
	if c := e.set.Counters(); segmentsBytes(e, c) != streamCap {
		t.Fatalf("the segments account for %d, want the take's cap %d", segmentsBytes(e, c), streamCap)
	}

	h.Release()
	var parked segment.Placed
	select {
	case p := <-appended:
		if p.Loc.Off == 0 {
			t.Fatal("the released append placed its record at offset 0, want a record slot")
		}
		parked = p
	case <-time.After(10 * time.Second):
		t.Fatal("the append did not return after the hold was released")
	}
	if err := <-appErr; err != nil {
		t.Fatalf("the released append failed: %v", err)
	}
	if got := e.set.ShareBytes(7); got != warmCharge+parked.Loc.Len {
		t.Fatalf("the share holds %d bytes after the append returned, want the warm %d plus the parked record's %d", got, warmCharge, parked.Loc.Len)
	}
}

// TestStreamTheHeaderIsWrittenOnceAndTheTakeSingleFlighted checks R8 and the
// take's gate: sixteen goroutines make their first Append at once; exactly one
// .seg file appears, one create for it, the header is written exactly once —
// the file's writes are one header plus the records — and it decodes with
// the right id, predecessor 0 and stream number. It catches a double take
// and a rewritten header.
func TestStreamTheHeaderIsWrittenOnceAndTheTakeSingleFlighted(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	const appends = 16
	var wg sync.WaitGroup
	errs := make(chan error, appends)
	placed := make(chan segment.Placed, appends)
	for i := range appends {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := make([]byte, 32)
			h, body := streamWriteHeader(streamFileID(i), 0, payload, 7, core.Version{Counter: uint64(i + 1)})
			p, err := e.s.Append(h, body)
			placed <- p
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	close(placed)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent first append: %v", err)
		}
	}
	var count int
	for p := range placed {
		if p.Loc.Segment != 1 {
			t.Fatalf("a concurrent first append landed in segment %d, want 1", p.Loc.Segment)
		}
		count++
	}
	if count != appends {
		t.Fatalf("%d concurrent appends returned a place, want %d", count, appends)
	}

	// Exactly one segment was taken, and its header was written once: the
	// stream's is the only segment the ledger holds, the pool's ready
	// spares beside it — a ready spare carries a `.seg` name too, its zeros
	// never a header, so the directory holds one segment file more than the
	// roll reached.
	if c := e.set.Counters(); c.Segments != 1 {
		t.Fatalf("the concurrent first appends took %d segments into the set, want exactly 1", c.Segments)
	}
	segs := 0
	for _, n := range streamListNames(t, e) {
		if kind, i := format.ClassifyName(n); kind == format.NameSegment {
			raw := make([]byte, format.SegmentHeaderSize)
			if _, err := e.s.ReadAt(i, raw, 0); err != nil {
				continue // a spare the roll never reached
			}
			if _, err := format.DecodeSegmentHeader(raw, streamIdentity); err == nil {
				segs++
			}
		}
	}
	if segs != 1 {
		t.Fatalf("the concurrent first appends left %d segment files with headers, want exactly the one they took", segs)
	}
	if got := e.seam.CallsFor(journaltest.OpCreate, format.SpareName(1)); got != 1 {
		t.Fatalf("the take created the spare %d times, want once", got)
	}
	name := streamSegName(1)
	if got := e.seam.CallsFor(journaltest.OpWrite, name); got != appends+1 {
		t.Fatalf("the segment saw %d writes, want one header plus %d records", got, appends)
	}

	raw := make([]byte, format.SegmentHeaderSize)
	if _, err := e.s.ReadAt(1, raw, 0); err != nil {
		t.Fatalf("ReadAt of the header: %v", err)
	}
	hdr, err := format.DecodeSegmentHeader(raw, streamIdentity)
	if err != nil {
		t.Fatalf("the header does not decode: %v", err)
	}
	if hdr.ID != 1 || hdr.Predecessor != 0 || hdr.Stream != 1 {
		t.Fatalf("the header names id %d, predecessor %d, stream %d, want 1, 0, 1", hdr.ID, hdr.Predecessor, hdr.Stream)
	}
	if hdr.Identity != streamIdentity {
		t.Fatal("the header names another identity")
	}
}

// streamSegmentHeader reads a segment's 48-byte header back through the
// stream's ReadAt and decodes it, for the checks that pin what a taken
// segment named.
func streamSegmentHeader(t *testing.T, e streamEnv, id core.SegmentID) format.SegmentHeader {
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

// streamListNames returns the seam's directory through the seam's List.
func streamListNames(t *testing.T, e streamEnv) []string {
	t.Helper()

	names, err := e.seam.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return names
}

// TestStreamAFailedTakeIsRefusedAndLeftForRetirement checks a take whose
// header write fails: the append returns the fault's error, the segment it
// made is Ended in the ledger — held at its cap for a retirement to unlink —
// and the next append takes a fresh segment with a higher id. It catches a
// failed take that leaves the segment Active or reuses its id.
func TestStreamAFailedTakeIsRefusedAndLeftForRetirement(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The take's header write is faulted: the first OpWrite on the first
	// segment's name fails with EIO.
	e := streamOver(t, streamCap, func(streamEnv) {})
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, streamSegName(1), 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	e.cacheBuilt(t)
	defer func() { _ = e.s.Close() }()

	payload := make([]byte, 64)
	h, body := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 1})
	if _, err := e.s.Append(h, body); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the append whose take's header write failed = %v, want the fault's EIO", err)
	}

	// The segment the failed take made is ended in the ledger: its cap is
	// held for the retirement, and it is no spare.
	// The take that failed wrote the header of the spare it drew, so the
	// spare is ended in the ledger — a segment in its own right, accounted
	// at its cap and held to its retirement — and the pool's content class
	// is one spare down: the take popped it before the error.
	c := e.set.Counters()
	if c.Segments != 1 {
		t.Fatalf("the ledger holds %d segments, want the failed take's one", c.Segments)
	}
	if got := segmentsBytes(e, c); got != streamCap {
		t.Fatalf("the segments account for %d after the failed take, want the failed segment's cap %d held to its retirement", got, streamCap)
	}
	if err := e.set.Check(); err != nil {
		t.Fatalf("the ledger after the failed take: %v", err)
	}
	// The failed segment is Ended in the ledger — not Active, which the
	// counters alone cannot tell: its Seal is refused — an ended segment
	// never takes one, Active does — and its retire runs.
	if err := e.set.Seal(1, true); !errors.Is(err, segment.ErrWrongState) {
		t.Fatalf("sealing the failed take's segment = %v, want ErrWrongState: the segment must be Ended, never Active", err)
	}
	if state, err := e.set.State(1); err != nil || state != segment.Ended {
		t.Fatalf("the failed take's segment's state is %v, %v, want Ended", state, err)
	}
	if freed, err := e.set.Retire(1); freed != streamCap || err != nil {
		t.Fatalf("retiring the failed take's segment freed %d, %v, want its cap %d, nil: only a non-Active segment retires", freed, err, streamCap)
	}

	// The next append retries the take: a fresh segment with a higher id,
	// the spare the roll drew after the faulted one.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, streamSegName(1), 0, syscall.EIO)); err != nil {
		t.Fatalf("disarming the header fault: %v", err)
	}
	h2, body2 := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 2})
	p := streamAppendWrite(t, e, h2, body2)
	if p.Loc.Segment != 2 {
		t.Fatalf("the retry landed in segment %d, want a fresh id above the failed take's 1", p.Loc.Segment)
	}
	// The retire above took the failed segment out, so the segments' own
	// footprint after the retry holds the new segment's cap alone.
	c2 := e.set.Counters()
	if got := segmentsBytes(e, c2); got != streamCap {
		t.Fatalf("the segments account for %d after the retire and the retry, want the new segment's cap %d", got, streamCap)
	}
}

// TestStreamReadAtARefusedSegmentMakesOneAttempt checks the read's refusal
// halves: a segment the cache has closed answers ErrSegmentGone unchanged,
// and a faulted OpRead EIO is returned once, with exactly one OpRead made —
// no retry of its own, which is the consumer's policy to apply.
func TestStreamReadAtARefusedSegmentMakesOneAttempt(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	payload := make([]byte, 64)
	h, body := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 1})
	p := streamAppendWrite(t, e, h, body)

	// An EIO is returned once and the read is made once.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpRead, streamSegName(1), 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	buf := make([]byte, 64)
	if _, err := e.s.ReadAt(1, buf, p.Loc.Off); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the faulted ReadAt = %v, want the EIO unchanged", err)
	}
	if got := e.seam.CallsFor(journaltest.OpRead, streamSegName(1)); got != 1 {
		t.Fatalf("the faulted read made %d OpRead calls, want exactly one attempt", got)
	}

	// A closed (retired) segment answers ErrSegmentGone, errors.Is-friendly.
	// The stream's ref goes first: its own Close releases it before the
	// cache's Close drains, or the drain would wait the stream out.
	if err := e.s.Close(); err != nil {
		t.Fatalf("the stream's Close: %v", err)
	}
	if err := e.cache.Close(1); err != nil {
		t.Fatalf("the cache's Close of the segment: %v", err)
	}
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpRead, streamSegName(1), 0, syscall.EIO)); err != nil {
		t.Fatalf("disarming the read fault: %v", err)
	}
	// A closed stream refuses reads, so a second stream over the same cache
	// reads the retired segment.
	reader := segment.NewStream(2, streamIdentity, e.set, e.pool, e.cache, nopSink())
	if _, err := reader.ReadAt(1, buf, p.Loc.Off); !errors.Is(err, segment.ErrSegmentGone) {
		t.Fatalf("the ReadAt of a closed segment = %v, want ErrSegmentGone", err)
	}
}

// TestStreamCloseRefusesAndShutsDownCleanly checks Close: an append after it
// answers ErrStreamClosed, the cache's Shutdown then returns nil — no ref the
// stream held outlives the close — and a second Close is a no-op.
func TestStreamCloseRefusesAndShutsDownCleanly(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)

	payload := make([]byte, 64)
	h, body := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 1})
	streamAppendWrite(t, e, h, body)

	if err := e.s.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	if err := e.s.Close(); err != nil {
		t.Fatalf("a second Close = %v, want nil", err)
	}

	h2, body2 := streamWriteHeader(streamFileID(2), 0, payload, 7, core.Version{Counter: 2})
	if _, err := e.s.Append(h2, body2); !errors.Is(err, segment.ErrStreamClosed) {
		t.Fatalf("the append after Close = %v, want ErrStreamClosed", err)
	}
	// A sync past the close answers ErrStreamClosed rather than a nil that
	// would read as "everything acknowledged is durable": Close fsyncs
	// nothing of what landed before it.
	if err := e.s.Sync(); !errors.Is(err, segment.ErrStreamClosed) {
		t.Fatalf("Sync after Close = %v, want ErrStreamClosed", err)
	}

	// The cache shuts down with nothing held: the shutdown registered by
	// built runs at cleanup and would fail the test on a leaked ref.
	if err := e.cache.Shutdown(); err != nil {
		t.Fatalf("the cache's Shutdown after the stream's Close = %v, want nil: the stream leaked its ref", err)
	}
}

// TestStreamCloseWaitsForTheInFlightAppend checks the wait Close makes: a
// record write parked at a seam hold keeps the stream open, and Close does
// not return — its caller would see a stream whose in-flight bytes are
// still landing — until the hold releases the append.
func TestStreamCloseWaitsForTheInFlightAppend(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {})
	e.cacheBuilt(t)
	defer func() { _ = e.s.Close() }() // before the cache's Shutdown: the stream holds a ref

	// One landed record warms the stream, then park the next.
	warm := make([]byte, 64)
	wh, wbody := streamWriteHeader(streamFileID(1), 0, warm, 7, core.Version{Counter: 0})
	streamAppendWrite(t, e, wh, wbody)

	h := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	appErr := make(chan error, 1)
	go func() {
		payload := make([]byte, 64)
		hh, body := streamWriteHeader(streamFileID(1), 64, payload, 7, core.Version{Counter: 1})
		_, err := e.s.Append(hh, body)
		appErr <- err
	}()
	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the record write never reached the hold")
	}

	closed := make(chan error, 1)
	go func() { closed <- e.s.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v while an append was still in flight, want it to wait", err)
	case <-time.After(50 * time.Millisecond):
	}

	h.Release()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close after the in-flight append = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after the in-flight append finished")
	}
	if err := <-appErr; err != nil {
		t.Fatalf("the in-flight append failed: %v", err)
	}
}

// streamScan reads the active segment back from storage and returns its
// records' offsets in order plus how many it counted: the one source for
// every check that needs the stream's position or its record count, reading
// what landed rather than what the stream believes.
func streamScanOf(t *testing.T, e streamEnv) (offsets []int64, id core.SegmentID, count int64, next int64) {
	t.Helper()

	names, err := e.seam.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var top core.SegmentID
	for _, n := range names {
		switch kind, i := format.ClassifyName(n); kind {
		case format.NameSegment, format.NameSpare:
			if i > top {
				top = i
			}
		}
	}
	// The stream's segment id: the highest a file name carries may be a
	// spare left by a failed take, so prefer names that hold: the stream
	// wrote, so the cache can read what it names.
	id = streamReadableSegmentTop(t, e, names, top)
	hdr := make([]byte, format.SegmentHeaderSize)
	if _, err := e.s.ReadAt(id, hdr, 0); err != nil {
		t.Fatalf("reading the segment's header: %v", err)
	}
	if _, err := format.DecodeSegmentHeader(hdr, streamIdentity); err != nil {
		t.Fatalf("the segment's header does not decode: %v", err)
	}
	off := int64(format.SegmentHeaderSize)
	for {
		raw := make([]byte, format.RecordHeaderSize)
		if _, err := e.s.ReadAt(id, raw, off); err != nil {
			t.Fatalf("reading a record header at %d: %v", off, err)
		}
		h, err := format.DecodeRecordHeader(raw, streamIdentity, id, off)
		if err != nil {
			break
		}
		offsets = append(offsets, off)
		count++
		off += h.Size()
		next = off
	}
	return offsets, id, count, next
}

// streamReadableSegmentTop picks the stream's segment among the names: the
// highest id whose header decodes under this identity — a spare left by a
// failed take reads as zeros and is skipped.
func streamReadableSegmentTop(t *testing.T, e streamEnv, names []string, top core.SegmentID) core.SegmentID {
	t.Helper()

	for id := top; id > 0; id-- {
		if !slices.Contains(names, format.SegmentName(id)) {
			continue
		}
		hdr := make([]byte, format.SegmentHeaderSize)
		if _, err := e.s.ReadAt(id, hdr, 0); err != nil {
			continue
		}
		if _, err := format.DecodeSegmentHeader(hdr, streamIdentity); err == nil {
			return id
		}
	}
	t.Fatal("no readable segment the stream wrote was found")
	return 0
}

// streamScanNext returns the next free record slot of the stream's segment,
// from the scan: the end of the last record that reads back whole.
func streamScanNext(t *testing.T, e streamEnv) int64 {
	t.Helper()

	_, _, _, next := streamScanOf(t, e)
	return next
}

// TestStreamCloseWaitsOutAnInFlightSync checks the in-flight operation Close
// must not strand: with the stream's fsync parked at a seam hold, Close does
// not return while it runs — releasing the active segment's ref first would
// let the cache close the descriptor under the fsync, or strand a dirty
// entry whose budget slot no reclaim can take — and the cache's Shutdown,
// behind the close, returns nil. It catches a Sync that borrows the stream's
// ref without counting itself in-flight.
func TestStreamCloseWaitsOutAnInFlightSync(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {})
	e.cacheBuilt(t)

	payload := make([]byte, 64)
	wh, wbody := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 1})
	streamAppendWrite(t, e, wh, wbody)

	// Park the stream's fsync: the descriptor is clean, so a close of it now
	// is an eviction the seam would show.
	h := e.seam.HoldFirst(t, journaltest.OpSync, seg)
	syncErr := make(chan error, 1)
	go func() { syncErr <- e.s.Sync() }()
	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the fsync never reached the hold")
	}

	closed := make(chan error, 1)
	go func() { closed <- e.s.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v while a Sync was still in flight, want it to wait the fsync out", err)
	case <-time.After(100 * time.Millisecond):
	}

	// The fsync finishes, and only then does Close return and the cache's
	// Shutdown run clean behind it.
	h.Release()
	select {
	case err := <-syncErr:
		if err != nil {
			t.Fatalf("the parked Sync failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parked Sync did not return after the hold was released")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close after its in-flight Sync = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after its in-flight Sync finished")
	}
	if err := e.cache.Shutdown(); err != nil {
		t.Fatalf("the cache's Shutdown after the stream's Close = %v, want nil: the wait released the ref under the fsync", err)
	}
	if err := e.set.Check(); err != nil {
		t.Fatalf("the ledger after the raced syncs: %v", err)
	}
}

// TestStreamClosesUnderDenseAppendsRacingAppendsDoesNotPanic checks the
// counting rule under the densest shape the reuse needs: one fresh stream per
// round, eight appenders looping from the first moment, and the Close landing
// while they are all still arriving. The rounds keep the window open the way
// a busy writer keeps it, so the run ends answered rather than with the
// WaitGroup's reuse panic or, under -race, its data race report.
func TestStreamClosesUnderDenseAppendsRacingAppendsDoesNotPanic(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const rounds = 100
	payload := make([]byte, 32)
	for round := 0; round < rounds; round++ {
		seam, err := journaltest.NewSeam(t.TempDir())
		if err != nil {
			t.Fatalf("round %d: the seam could not be made: %v", round, err)
		}
		set := segment.NewSet(streamCap, nil)
		maker := segment.NewMaker(seam.FS(), set, 3*streamCap, 0, nil)
		pool := segment.NewPool(maker, segment.Targets{Content: 2}, nil, nil)
		pool.Start()
		budget := core.NewDescriptorBudget(64)
		cache := segment.NewDescriptors(seam.FS(), format.SegmentName, budget)
		s := segment.NewStream(1, streamIdentity, set, pool, cache, nopSink())
		// The round settles itself, so the next round starts from an empty
		// heap — a hundred rounds of caches held to the test's end would
		// read as growth to the profiler.
		defer func() {
			_ = s.Close()
			pool.Stop()
			if err := cache.Shutdown(); err != nil {
				t.Errorf("round %d: the cache's Shutdown = %v, want nil", round, err)
			}
		}()

		stop := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					h, body := streamWriteHeader(streamFileID(i), 0, payload, 7, core.Version{Counter: 1})
					if _, err := s.Append(h, body); err != nil {
						return // ErrStreamClosed or a fault: the round's close
					}
				}
			}(i)
		}
		time.Sleep(500 * time.Microsecond)
		if err := s.Close(); err != nil {
			t.Errorf("round %d's Close under dense appends = %v, want nil", round, err)
		}
		close(stop)
		wg.Wait()
	}
}

// streamAppendHeldFault fails the test the moment a seam call arrives while
// the stream's append lock is held: the leaf-lock contract of the segment
// append domain (R12), the same fault the descriptor cache's own check uses.
type streamAppendHeldFault struct {
	s  *segment.Stream
	ok *bool
}

func (f *streamAppendHeldFault) At(op string, key []byte) (time.Duration, int, error) {
	if segment.StreamAppendLockHeld(f.s) {
		*f.ok = false
	}
	return 0, 0, nil
}

// TestStreamNoSeamCallUnderTheAppendLock checks R12: no storage call — the
// take's create, allocate, sync, close, rename and directory sync, the
// descriptor's open, the header's write, a record's write, a read, and the
// record sync's fsync — runs while the stream's append lock is held, so a
// fault consulted at every seam call must never find the lock taken. A lock
// held across I/O makes every appender of the stream wait on the slowest call
// in flight. It catches a write, a sync or a take moved under appendMu.
func TestStreamNoSeamCallUnderTheAppendLock(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := streamOver(t, streamCap, func(streamEnv) {})
	ok := true
	fault := &streamAppendHeldFault{s: e.s, ok: &ok}
	e.seam.SetFault(fault)
	// The cache's Shutdown is registered here too: with the fault installed
	// it still runs clean, for its own closes also answer the fault.
	t.Cleanup(func() {
		if err := e.cache.Shutdown(); err != nil {
			t.Errorf("the cache's Shutdown under the lock fault = %v, want nil", err)
		}
	})
	defer func() { _ = e.s.Close() }()

	// The first Append drives the take and the header write; the next three
	// records, then a Sync and a ReadAt, drive everything else.
	for i := range 3 {
		payload := streamTestBody(64, 0x3c)
		h, body := streamWriteHeader(streamFileID(i+1), int64(i)*64, payload, 7, core.Version{Counter: uint64(i + 1)})
		if _, err := e.s.Append(h, body); err != nil {
			t.Fatalf("append %d under the lock fault: %v", i+1, err)
		}
	}
	if err := e.s.Sync(); err != nil {
		t.Fatalf("Sync under the lock fault: %v", err)
	}
	buf := make([]byte, format.RecordHeaderSize)
	if _, err := e.s.ReadAt(1, buf, format.SegmentHeaderSize); err != nil {
		t.Fatalf("ReadAt under the lock fault: %v", err)
	}

	if !ok {
		t.Fatal("a seam call was made while the stream's append lock was held: the segment append domain was held across I/O")
	}
}

// TestStreamASplitPieceTakesTheWholeRoom checks the split's maximality (the
// closed form of the record size): for a write and a fill, over several caps
// and straddling payloads, the piece this Append places is the largest one
// that fits before the seal's reserve — what remains past the stream's next
// holds the reserve exactly, with at most the slot's padding left — and a
// payload one byte more would have been refused. It catches a split that
// undershoots by the padding or the body overhead, which the old byte-at-a-
// time search could not do.
func TestStreamASplitPieceTakesTheWholeRoom(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The warm record is one 64-byte write, so the straddling record meets a
	// segment holding 48 bytes of header and 168 of it: a next of 216, one
	// record counted. The payload of each case is the largest its room
	// takes plus nine, so the whole record cannot fit and the piece is the
	// room itself.
	const warmNext = format.SegmentHeaderSize + 160 + 8
	reserve2 := int64(format.SealMarkerSize) + 2*int64(format.CatalogEntrySize) + int64(format.TrailerSize)
	for _, tc := range []struct {
		cap  int64
		kind format.Kind
	}{
		{4096, format.KindWrite},
		{6000, format.KindWrite},
		{8192, format.KindWrite},
		{4096, format.KindFill},
		{6000, format.KindFill},
	} {
		overhead := int64(format.RecordHeaderSize)
		if tc.kind == format.KindWrite {
			overhead += 8
		}
		payload := ((tc.cap - warmNext - reserve2) &^ 7) - overhead + 9
		name := fmt.Sprintf("cap %d payload %d %s", tc.cap, payload, tc.kind)
		t.Run(name, func(t *testing.T) {
			e := streamOver(t, tc.cap, func(streamEnv) {}).built(t)
			defer func() { _ = e.s.Close() }()

			// One landed record warms the segment so the next straddles.
			warm := make([]byte, 64)
			wh, wbody := streamWriteHeader(streamFileID(1), 0, warm, 7, core.Version{Counter: 1})
			p1 := streamAppendWrite(t, e, wh, wbody)
			if p1.N != int64(len(warm)) {
				t.Fatalf("the warm record placed %d bytes, want the whole %d", p1.N, len(warm))
			}

			body := make([]byte, payload)
			for i := range body {
				body[i] = byte(0xa5 ^ i)
			}
			h := format.RecordHeader{
				Kind:    tc.kind,
				File:    streamFileID(2),
				Extent:  core.Extent{Off: 4096, Len: payload},
				Version: core.Version{Counter: 9},
				Tag:     7,
			}
			p, err := e.s.Append(h, format.RecordBody{Payload: body})
			if err != nil {
				t.Fatalf("the straddling Append: %v", err)
			}
			if p.N == 0 || p.N >= payload {
				t.Fatalf("the straddling Append placed %d bytes, want a piece in [1, %d)", p.N, payload)
			}

			// The maximality. After the piece, what remains past next holds
			// exactly the seal's reserve — the piece left the 8-padded room
			// with nothing more a record of its kind could take. The cap
			// and the warm record are aligned on 8, so the reserve takes
			// the rest with nothing left, not 8 bytes of slack: a piece
			// undershot by the padding leaves 8 more than the reserve and
			// one undershot by the body overhead, more still.
			_, _, count, next := streamScanOf(t, e)
			reserve := int64(format.SealMarkerSize) + count*int64(format.CatalogEntrySize) + int64(format.TrailerSize)
			room := tc.cap - next
			if room != reserve {
				t.Fatalf("the piece of %d left %d bytes past next against a reserve of %d, want them equal: the split undershot the room",
					p.N, room, reserve)
			}

			// The bytes past the one the piece places belong to no second
			// piece: appending one more payload byte than the piece placed
			// lands none of it in this segment, which pins the largest-
			// fitting piece from the other side — the record's whole size
			// exceeded the room even before the piece, and one byte more
			// does nothing to change that. Its record has no piece here
			// and rolls, landing whole in the successor; the full segment's
			// own writes after the piece belong to the seal the roll
			// closed it with, not to a record.
			active := e.streamActiveID(t)
			oneMore := make([]byte, p.N+1)
			h2 := format.RecordHeader{
				Kind:    tc.kind,
				File:    streamFileID(3),
				Extent:  core.Extent{Off: 4096, Len: p.N + 1},
				Version: core.Version{Counter: 10},
				Tag:     7,
			}
			p2, err := e.s.Append(h2, format.RecordBody{Payload: oneMore})
			if err != nil {
				t.Fatalf("the append of one more payload byte: %v", err)
			}
			if p2.N != int64(len(oneMore)) {
				t.Fatalf("the append of %d payload bytes placed %d in the successor, want the whole record: the old segment's piece was its largest",
					len(oneMore), p2.N)
			}
			if p2.Loc.Segment != active {
				t.Fatalf("the append of one more byte rolled twice, to segment %d, want one roll to the active %d", p2.Loc.Segment, active)
			}
			if state, err := e.set.State(active); err != nil || state != segment.Active {
				t.Fatalf("the successor's state after the roll is %v, %v, want Active", state, err)
			}
			if state, err := e.set.State(p.Loc.Segment); err != nil || state != segment.Sealed {
				t.Fatalf("the full segment's state after the roll is %v, %v, want Sealed", state, err)
			}
		})
	}
}

// streamActiveID returns the id of the segment the stream is appending to,
// from the directory: the highest id whose header decodes.
func (e streamEnv) streamActiveID(t *testing.T) core.SegmentID {
	t.Helper()

	names, err := e.seam.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var top core.SegmentID
	for _, n := range names {
		if kind, i := format.ClassifyName(n); kind == format.NameSegment && i > top {
			top = i
		}
	}
	return top
}

// TestStreamTheSyncedThroughIsReadBeforeTheFsync checks R10 at the stream
// level: the value a Sync records — and so the value the records after it
// carry — is the completed prefix the Sync found before its fsync started.
// Record B completes while the fsync is parked, so it must not be counted:
// record C, appended after the sync returned, carries a synced-through at or
// below B's own offset. It catches a Sync that records what the tracker holds
// after the fsync returns, which would claim durability for bytes no fsync
// reached.
func TestStreamTheSyncedThroughIsReadBeforeTheFsync(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	// Record A lands and is synced.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	aPlaced := streamAppendWrite(t, e, ah, abody)
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the first Sync: %v", err)
	}

	// The fsync parks: while it waits, record B lands — after the sync
	// started, so no value this sync records may count it.
	h := e.seam.HoldFirst(t, journaltest.OpSync, seg)
	syncErr := make(chan error, 1)
	go func() { syncErr <- e.s.Sync() }()
	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the fsync never reached the hold")
	}

	bPayload := streamTestBody(64, 0x55)
	bh, bbody := streamWriteHeader(streamFileID(2), 64, bPayload, 7, core.Version{Counter: 2})
	bPlaced := streamAppendWrite(t, e, bh, bbody)

	h.Release()
	select {
	case err := <-syncErr:
		if err != nil {
			t.Fatalf("the parked Sync failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parked Sync did not return")
	}

	// Record C, appended after the sync returned, carries the value that
	// sync recorded: at or below B's own offset — never B's end, which the
	// in-flight-at-sync-start record B would have given a Sync that counts
	// what the tracker holds after the fsync.
	cPayload := streamTestBody(64, 0x7d)
	ch, cbody := streamWriteHeader(streamFileID(3), 128, cPayload, 7, core.Version{Counter: 3})
	cPlaced := streamAppendWrite(t, e, ch, cbody)
	got, _ := streamReadRecord(t, e, cPlaced.Loc)
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the closing Sync: %v", err)
	}
	if got.SyncedThrough > bPlaced.Loc.Off {
		t.Fatalf("record C carries SyncedThrough %d, want at most record B's own offset %d: the sync counted a record that landed while its fsync ran", got.SyncedThrough, bPlaced.Loc.Off)
	}
	_ = aPlaced
}

// TestStreamAFailedSyncStopsWhatALaterSyncCovers checks the failed-window half
// of R10: a Sync that fails with the platform's error records nothing, and no
// later successful sync of the same segment may claim the failed window's
// bytes — RFC 1 §5.3: a second sync can succeed without having written what
// the first lost. The next record after the failed sync, and the one after a
// second, successful sync, both carry a synced-through at or below the failed
// window's first record. It catches a Sync that records the prefix after a
// failed one, silently claiming durability for bytes no fsync made durable.
func TestStreamAFailedSyncStopsWhatALaterSyncCovers(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	// Record A lands and its sync succeeds, so the stream has a value worth
	// reading.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	a := streamAppendWrite(t, e, ah, abody)
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the first Sync: %v", err)
	}

	// Record B lands; the window [B..] is the failed window. The fault
	// takes the very next sync of the segment.
	bPayload := streamTestBody(64, 0x55)
	bh, bbody := streamWriteHeader(streamFileID(2), 64, bPayload, 7, core.Version{Counter: 2})
	b := streamAppendWrite(t, e, bh, bbody)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpSync, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if err := e.s.Sync(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the faulted Sync = %v, want the EIO", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the sync fault: %v", err)
	}

	// After the failure, a successful sync covers the failed window too —
	// but it may not claim it: the bytes the failed fsync left are durable
	// only when a later window of the durability layer's says so, and the
	// records after the failure carry the value below the failed window's
	// start.
	cPayload := streamTestBody(64, 0x7d)
	ch, cbody := streamWriteHeader(streamFileID(3), 128, cPayload, 7, core.Version{Counter: 3})
	c := streamAppendWrite(t, e, ch, cbody)
	cHeader, _ := streamReadRecord(t, e, c.Loc)
	if cHeader.SyncedThrough > b.Loc.Off {
		t.Fatalf("the record after the failed sync carries SyncedThrough %d, want at most the failed window's start %d", cHeader.SyncedThrough, b.Loc.Off)
	}

	// A second, successful sync does not lift the value past the failed
	// window either.
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the second Sync: %v", err)
	}
	dPayload := streamTestBody(64, 0x13)
	dh, dbody := streamWriteHeader(streamFileID(4), 192, dPayload, 7, core.Version{Counter: 4})
	d := streamAppendWrite(t, e, dh, dbody)
	dHeader, _ := streamReadRecord(t, e, d.Loc)
	if dHeader.SyncedThrough > b.Loc.Off {
		t.Fatalf("the record after a sync that followed the failure carries SyncedThrough %d, want it still at or below the failed window's start %d", dHeader.SyncedThrough, b.Loc.Off)
	}
	_ = a
}

// streamShortFS wraps a filesystem's files so the first WriteAt of a given
// name returns len(p)-8 with a nil error — a platform that lands almost all
// of a write and still reports success, which the stream must count as a
// failure. Everything else passes through.
type streamShortFS struct {
	storage.FS
	name  string
	armed atomic.Bool
}

type streamShortFile struct {
	storage.File
	fs *streamShortFS
}

func (f *streamShortFile) WriteAt(p []byte, off int64) (int, error) {
	if f.fs.armed.CompareAndSwap(true, false) && f.Name() == f.fs.name {
		return len(p) - 8, nil
	}
	return f.File.WriteAt(p, off)
}

func (fs *streamShortFS) Open(name string) (storage.File, error) {
	f, err := fs.FS.Open(name)
	if err != nil {
		return nil, err
	}
	return &streamShortFile{File: f, fs: fs}, nil
}

// TestStreamAShortWriteCountsAsAFailureAndEndsTheSegment checks that a
// WriteAt that lands all but 8 bytes and reports nil is treated as the
// failure it is: the Append returns io.ErrShortWrite and no location, the
// share is not charged for it, and the segment is ended — the short slot is
// bytes no later synced-through offset may pass — so the next append rolls
// into a new segment. It catches a stream that keeps landing records in a
// segment a short write corrupted.
func TestStreamAShortWriteCountsAsAFailureAndEndsTheSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	streamFS := &streamShortFS{FS: newSeam(t).FS(), name: seg}
	e := streamOverFS(t, streamCap, streamFS, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	// One record lands whole and is synced, so the stream has a value.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	a := streamAppendWrite(t, e, ah, abody)
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the first Sync: %v", err)
	}

	// The next record's write lands short with a nil error.
	streamFS.armed.Store(true)
	bPayload := streamTestBody(64, 0x55)
	bh, bbody := streamWriteHeader(streamFileID(2), 64, bPayload, 7, core.Version{Counter: 2})
	_, err := e.s.Append(bh, bbody)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("the short Append = %v, want io.ErrShortWrite: a write that lands short with a nil error is a failure", err)
	}

	// The share was not charged for the failed record.
	if got := e.set.ShareBytes(7); got != a.Loc.Len {
		t.Fatalf("the share holds %d bytes after the short write, want the landed record's %d alone", got, a.Loc.Len)
	}

	// The short-written segment is ended, and the record after the short
	// one lands in a new segment naming no predecessor whose sync answers
	// nil: the short slot's bytes belong to the ended segment alone.
	if state, err := e.set.State(a.Loc.Segment); err != nil || state != segment.Ended {
		t.Fatalf("the short-written segment's state is %v, %v, want Ended", state, err)
	}
	cPayload := streamTestBody(64, 0x7d)
	ch, cbody := streamWriteHeader(streamFileID(3), 192, cPayload, 7, core.Version{Counter: 3})
	c := streamAppendWrite(t, e, ch, cbody)
	if c.Loc.Segment == a.Loc.Segment {
		t.Fatalf("the append after the short write stayed in segment %d, want a rolled-to new one", a.Loc.Segment)
	}
	if hdr := streamSegmentHeader(t, e, c.Loc.Segment); hdr.Predecessor != 0 {
		t.Fatalf("the new segment names predecessor %d, want none: a short write ends its segment too", hdr.Predecessor)
	}
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the Sync on the segment after the short write: %v", err)
	}
	_ = bh
}

// streamOverFS builds a stream over an explicit filesystem — a wrapper around
// the seam the checks here need — with the same ledger and cache shape
// streamOver builds.
func streamOverFS(t *testing.T, cap int64, fsys storage.FS, setup func(e streamEnv)) streamEnv {
	t.Helper()

	set := segment.NewSet(cap, nil)
	maker := segment.NewMaker(fsys, set, 3*cap, 0, nil)
	budget := core.NewDescriptorBudget(64)
	cache := segment.NewDescriptors(fsys, format.SegmentName, budget)
	pool := segment.NewPool(maker, segment.Targets{Content: 2}, nil, nil)

	e := streamEnv{set: set, maker: maker, pool: pool, cache: cache, budget: budget, cap: cap}
	setup(e)
	t.Cleanup(func() { pool.Stop() })
	pool.Start()
	awaitCounters(t, "the two content spares ready", pool,
		func(c segment.PoolCounters) bool { return c.Spares == 2 })
	e.s = segment.NewStream(1, streamIdentity, set, pool, cache, nopSink())
	return e
}

// TestStreamAppendRefusesRecordsItCannotEncode checks the caller bugs the
// format would panic on mid-append: an unknown kind, a payload on a kind
// that carries none, a length that differs from the bytes given, and a
// negative file offset. Each is refused with ErrBadRecord before the lock,
// leaving the stream exactly as it was: the next good append works, lands at
// its own place and syncs, so no slot was strandable and no position moved.
func TestStreamAppendRefusesRecordsItCannotEncode(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	// Warm the stream so a refusal can be told from the first-take path.
	warm := streamTestBody(64, 0x5a)
	wh, wbody := streamWriteHeader(streamFileID(1), 0, warm, 7, core.Version{Counter: 1})
	warmPlaced := streamAppendWrite(t, e, wh, wbody)
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the warm Sync: %v", err)
	}

	// Every refusal names ErrBadRecord and changes nothing.
	writes := e.seam.CallsFor(journaltest.OpWrite, streamSegName(1))
	share := e.set.ShareBytes(7)

	payload := streamTestBody(64, 0x3c)
	bad := []struct {
		name string
		h    format.RecordHeader
		body format.RecordBody
	}{
		{
			name: "an unknown kind",
			h:    format.RecordHeader{Kind: format.Kind(0x63), File: streamFileID(9), Tag: 7},
		},
		{
			name: "a payload on a kind that carries none",
			h:    format.RecordHeader{Kind: format.KindStamp, File: streamFileID(9), Tag: 7},
			body: format.RecordBody{Payload: payload, Cut: 4},
		},
		{
			name: "a length that differs from the bytes",
			h: format.RecordHeader{Kind: format.KindWrite, File: streamFileID(9),
				Extent: core.Extent{Off: 0, Len: 32}, Version: core.Version{Counter: 2}, Tag: 7},
			body: format.RecordBody{Payload: payload},
		},
		{
			name: "a negative file offset",
			h: format.RecordHeader{Kind: format.KindWrite, File: streamFileID(9),
				Extent: core.Extent{Off: -1, Len: 64}, Version: core.Version{Counter: 3}, Tag: 7},
			body: format.RecordBody{Payload: payload},
		},
	}
	for _, tc := range bad {
		if _, err := e.s.Append(tc.h, tc.body); !errors.Is(err, segment.ErrBadRecord) {
			t.Fatalf("%s: Append = %v, want ErrBadRecord", tc.name, err)
		}
		if got := e.seam.CallsFor(journaltest.OpWrite, streamSegName(1)); got != writes {
			t.Fatalf("%s made a seam write, want the refusal to perform none", tc.name)
		}
		if got := e.set.ShareBytes(7); got != share {
			t.Fatalf("%s moved the share bytes to %d, want the warm record's %d", tc.name, got, share)
		}
	}

	// The stream still works: the next good append lands past the warm
	// record and syncs.
	good := streamTestBody(64, 0x81)
	gh, gbody := streamWriteHeader(streamFileID(2), 64, good, 7, core.Version{Counter: 9})
	p := streamAppendWrite(t, e, gh, gbody)
	if p.Loc.Off != warmPlaced.Loc.Off+warmPlaced.Loc.Len {
		t.Fatalf("the append after the refusals landed at %d, want the warm record's end %d: a refusal moved the position", p.Loc.Off, warmPlaced.Loc.Off+warmPlaced.Loc.Len)
	}
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the Sync after the refusals: %v", err)
	}
}

// TestStreamSyncAtOfASegmentTheStreamSealedAndLeftIsDurable catches a SyncAt
// that answers a left segment with an error, or with a sync of the successor:
// a clean seal made every record of the segment durable.
func TestStreamSyncAtOfASegmentTheStreamSealedAndLeftIsDurable(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	h1, b1 := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x11), 7, core.Version{Counter: 1})
	first := streamAppendWrite(t, e, h1, b1)
	if _, err := e.s.Seal(false); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	h2, b2 := streamWriteHeader(streamFileID(1), 64, streamTestBody(64, 0x22), 7, core.Version{Counter: 2})
	second := streamAppendWrite(t, e, h2, b2)
	if second.Loc.Segment == first.Loc.Segment {
		t.Fatalf("the append after the seal stayed in segment %d, want a roll", first.Loc.Segment)
	}

	// A sync of the successor would be a sync call on its file: none may be
	// made on the strength of the first record's location.
	syncs := e.seam.CallsFor(journaltest.OpSync, streamSegName(second.Loc.Segment))
	if err := e.s.SyncAt(first.Loc); err != nil {
		t.Fatalf("SyncAt of a sealed, left segment = %v, want nil", err)
	}
	if got := e.seam.CallsFor(journaltest.OpSync, streamSegName(second.Loc.Segment)); got != syncs {
		t.Errorf("SyncAt synced the successor: %d sync calls, want still %d", got, syncs)
	}
}

// TestStreamSyncAtRefusesASegmentThatEndsBetweenItsChecksAndItsSync catches a
// SyncAt that reports a record durable because Sync returned nil, when a
// failed seal ended the record's segment and a roll moved the stream on in
// the gap: Sync then synced the successor, which says nothing of the record.
func TestStreamSyncAtRefusesASegmentThatEndsBetweenItsChecksAndItsSync(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	h1, b1 := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x11), 7, core.Version{Counter: 1})
	first := streamAppendWrite(t, e, h1, b1)

	ran := false
	segment.SetSyncAtGapHook(func() {
		segment.SetSyncAtGapHook(nil)
		ran = true
		if err := e.seam.Schedule(journaltest.Failing(journaltest.OpSync, streamSegName(1), 1, syscall.EIO)); err != nil {
			t.Errorf("Schedule: %v", err)
		}
		if _, err := e.s.Seal(false); err == nil {
			t.Errorf("the seal did not fail")
		}
		if err := e.seam.Schedule(); err != nil {
			t.Errorf("disarming the sync fault: %v", err)
		}
		h2, b2 := streamWriteHeader(streamFileID(1), 64, streamTestBody(64, 0x22), 7, core.Version{Counter: 2})
		if _, err := e.s.Append(h2, b2); err != nil {
			t.Errorf("the rolling append: %v", err)
		}
	})
	defer segment.SetSyncAtGapHook(nil)

	if err := e.s.SyncAt(first.Loc); !errors.Is(err, segment.ErrSegmentEnded) {
		t.Fatalf("SyncAt = %v, want ErrSegmentEnded", err)
	}
	if !ran {
		t.Fatal("the gap hook never ran")
	}
}
