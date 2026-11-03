package segment_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"runtime"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journal/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journal/internal/format"
	"github.com/marmos91/dittofs/pkg/block/journal/internal/segment"
	"github.com/marmos91/dittofs/pkg/block/journal/journaltest"
	"github.com/marmos91/dittofs/pkg/block/journal/journaltest/prof"
)

// streamCap is the cap of the sets these tests build, small enough that a
// modest record straddles it and large enough that many records fit.
const streamCap = int64(64 << 10)

// streamMax is the journal maximum the stream tests pass, so a take is never
// refused by it.
const streamMax = math.MaxInt64

// streamIdentity is one journal identity every stream test writes under.
var streamIdentity = core.JournalID{0xd, 0xe, 0xc, 0xa, 0xd, 0}

// streamEnv is one stream over the seam, its ledger and its cache: the
// building set of every stream check.
type streamEnv struct {
	seam   *journaltest.Seam
	set    *segment.Set
	maker  *segment.Maker
	cache  *segment.Descriptors
	budget *core.DescriptorBudget
	s      *segment.Stream
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
	maker := segment.NewMaker(seam.FS(), set, streamMax, 0, nil)
	budget := core.NewDescriptorBudget(64)
	cache := segment.NewDescriptors(seam.FS(), format.SegmentName, budget)

	e := streamEnv{seam: seam, set: set, maker: maker, cache: cache, budget: budget}
	setup(e)
	e.s = segment.NewStream(1, streamIdentity, set, maker, cache)
	return e
}

// streamSegName returns the segment file name of id, as the cache names it.
func streamSegName(id core.SegmentID) string { return format.SegmentName(id) }

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

// streamAppend writes one record through the stream, failing the test on any
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

// TestStreamAFailedWriteBoundsTheSyncedThroughOfTheRecordsAfterIt checks R10
// at the seam: one record lands and is synced, the next write fails with the
// seam's EIO, and the two appends after it — one past a Sync between them —
// each carry a synced-through at or below the failed slot's offset, read back
// from storage by decoding their headers. It catches a synced-through that
// outran a failed write.
func TestStreamAFailedWriteBoundsTheSyncedThroughOfTheRecordsAfterIt(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// One synced record, then a failed one: the fault is armed once the
	// first record has landed and been synced — Failing fires from a rule's
	// first matching call on, so arming it after the first record's write
	// makes the very next write of the segment the failed one.
	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	// The first write lands and is synced, so its record carries a
	// synced-through of zero and the next one has room to name the prefix.
	h1 := mustWrite(t, e, streamFileID(1), 0, make([]byte, 64), 7, 1)
	if h1.SyncedThrough != 0 {
		t.Fatalf("the first record carries SyncedThrough %d, want 0", h1.SyncedThrough)
	}
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

	// The failed slot's offset is the first record's end. The next two
	// records each carry a synced-through at or below it.
	failedOff := h1.Offset + h1.Size()
	for i, between := range []bool{false, true, false} {
		if between {
			if err := e.s.Sync(); err != nil {
				t.Fatalf("Sync between the appends: %v", err)
			}
		}
		payload := mustBody(t, 64)
		h, body := streamWriteHeader(streamFileID(2), int64(128+64*i), payload, 8, core.Version{Counter: uint64(3 + i)})
		p := streamAppendWrite(t, e, h, body)
		got, _ := streamReadRecord(t, e, p.Loc)
		if got.SyncedThrough > failedOff {
			t.Fatalf("record %d after the failed write carries SyncedThrough %d, want at most the failed slot's offset %d",
				i+1, got.SyncedThrough, failedOff)
		}
	}
}

// mustWrite appends one record and reads its own header back, so the caller
// holds the record's decoded header without the ceremony.
func mustWrite(t *testing.T, e streamEnv, fid core.FileID, off int64, payload []byte, tag core.ShareTag, version uint64) format.RecordHeader {
	t.Helper()

	h, body := streamWriteHeader(fid, off, payload, tag, core.Version{Counter: version})
	p := streamAppendWrite(t, e, h, body)
	got, _ := streamReadRecord(t, e, p.Loc)
	return got
}

// mustBody returns a payload of n bytes the writer can use.
func mustBody(t *testing.T, n int) []byte {
	t.Helper()
	payload := make([]byte, n)
	for i := range payload {
		payload[i] = byte(i)
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
	close(reads)
	// The reader drains what it can until stopped; what it does not read the
	// buffered channel keeps without blocking the appenders.
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
// piece. The following append of the rest answers ErrSegmentFull (no roll
// yet) and makes no seam call. A header-only kind that does not fit is
// refused the same way.
func TestStreamAStraddlingWriteIsSplitAtTheCap(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// A cap the first record nearly fills: one record of 2048 payload
	// bytes, then a straddling write.
	e := streamOver(t, 4096, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	payload1 := make([]byte, 1024)
	h1, b1 := mustHeader(streamFileID(1), 0, payload1)
	_ = h1
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

	// The rest does not fit, so with no roll the next append is refused
	// with no write made.
	writesBefore := e.seam.CallsFor(journaltest.OpWrite, streamSegName(1))
	rest := payload2[p2.N:]
	h3, b3 := streamWriteHeader(streamFileID(2), 4096+p2.N, rest, 7, core.Version{Counter: 10})
	if _, err := e.s.Append(h3, b3); !errors.Is(err, segment.ErrSegmentFull) {
		t.Fatalf("the append of the rest = %v, want ErrSegmentFull", err)
	}
	if got := e.seam.CallsFor(journaltest.OpWrite, streamSegName(1)); got != writesBefore {
		t.Fatalf("the refusal made %d more writes, want none", got-writesBefore)
	}

	// A header-only kind that does not fit is refused too.
	h4 := format.RecordHeader{Kind: format.KindStamp, File: streamFileID(3), Tag: 7}
	if _, err := e.s.Append(h4, format.RecordBody{Cut: 4}); !errors.Is(err, segment.ErrSegmentFull) {
		t.Fatalf("the stamp that does not fit = %v, want ErrSegmentFull", err)
	}
}

// mustHeader returns the write header and body of payload in one step.
func mustHeader(fid core.FileID, off int64, payload []byte) (format.RecordHeader, format.RecordBody) {
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
	if got, b := streamReadRecord(t, e, p.Loc); !bytes.Equal(b.Payload, payload) {
		_ = got
		t.Fatal("the exactly-filling record does not read back byte for byte")
	}

	// Nothing more fits: the next append is refused without a write.
	writes := e.seam.CallsFor(journaltest.OpWrite, streamSegName(1))
	h2, body2 := streamWriteHeader(streamFileID(2), 0, make([]byte, 1), 7, core.Version{Counter: 2})
	if _, err := e.s.Append(h2, body2); !errors.Is(err, segment.ErrSegmentFull) {
		t.Fatalf("the append after the exact fill = %v, want ErrSegmentFull", err)
	}
	if got := e.seam.CallsFor(journaltest.OpWrite, streamSegName(1)); got != writes {
		t.Fatalf("the refusal after the exact fill made %d writes, want none", got-writes)
	}
}

// TestStreamTheSealReserveSurvivesTheAppends checks R11 for several record
// sizes: after appends fill the segment, the space left past next still holds
// the marker, a footer entry per record and the trailer. It catches a room
// check that forgets the catalog entry of the record being placed.
func TestStreamTheSealReserveSurvivesTheAppends(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, payloadLen := range []int64{16, 128, 1024} {
		t.Run(fmt.Sprintf("payload %d", payloadLen), func(t *testing.T) {
			e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
			defer func() { _ = e.s.Close() }()

			var count int64
			for i := 0; ; i++ {
				payload := make([]byte, payloadLen)
				h, body := streamWriteHeader(streamFileID(1), int64(i)*payloadLen, payload, 7, core.Version{Counter: uint64(i + 1)})
				_, err := e.s.Append(h, body)
				if errors.Is(err, segment.ErrSegmentFull) {
					break
				}
				if err != nil {
					t.Fatalf("append %d: %v", i, err)
				}
				count++
			}
			if count == 0 {
				t.Fatal("no record fit at all")
			}

			next := streamScanNext(t, e)
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
	// footprint (the bytes are inside storage the segment already accounts).
	if c := e.set.Counters(); c.UsedBytes != streamCap {
		t.Fatalf("the footprint is %d after the appends, want the cap %d alone", c.UsedBytes, streamCap)
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

	// Park the take's header write: the first OpWrite on the segment name
	// is the header, so the parked append is one that has not landed at all.
	h := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	appended := make(chan segment.Placed, 1)
	appErr := make(chan error, 1)
	go func() {
		payload := make([]byte, 64)
		hh, body := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 1})
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
		t.Fatalf("the append returned %v while its record write was parked, want it held", p.Loc)
	case err := <-appErr:
		t.Fatalf("the append returned %v while its record write was parked", err)
	default:
	}
	if got := e.set.ShareBytes(7); got != 0 {
		t.Fatalf("the share holds %d bytes while the record write is parked, want 0: the bytes are charged only once written", got)
	}
	// The footprint holds the segment alone, the take's cap.
	if c := e.set.Counters(); c.UsedBytes != streamCap {
		t.Fatalf("the footprint is %d, want the take's cap %d", c.UsedBytes, streamCap)
	}

	h.Release()
	select {
	case p := <-appended:
		if p.Loc.Off == 0 {
			t.Fatal("the released append placed its record at offset 0, want a record slot")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the append did not return after the hold was released")
	}
	if err := <-appErr; err != nil {
		t.Fatalf("the released append failed: %v", err)
	}
	if got := e.set.ShareBytes(7); got == 0 {
		t.Fatal("the share holds nothing after the append returned, want the record charged")
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

	// Exactly one segment file was created, and its header was written
	// once: the segment's OpWrites are one header plus the records.
	segs := 0
	for _, n := range streamListNames(t, e) {
		if kind, _ := format.ClassifyName(n); kind == format.NameSegment {
			segs++
		}
	}
	if segs != 1 {
		t.Fatalf("the concurrent first appends created %d segment files, want exactly 1", segs)
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
	c := e.set.Counters()
	if c.UsedBytes != streamCap {
		t.Fatalf("the footprint after the failed take is %d, want the failed segment's cap %d held to its retirement", c.UsedBytes, streamCap)
	}
	if c.Segments != 1 || c.Spares != 0 {
		t.Fatalf("the ledger holds %d segments and %d spares, want the failed take's segment and no spares", c.Segments, c.Spares)
	}
	if err := e.set.Check(); err != nil {
		t.Fatalf("the ledger after the failed take: %v", err)
	}

	// The next append retries the take: a fresh segment with a higher id.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, streamSegName(1), 0, syscall.EIO)); err != nil {
		t.Fatalf("disarming the header fault: %v", err)
	}
	h2, body2 := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 2})
	p := streamAppendWrite(t, e, h2, body2)
	if p.Loc.Segment != 2 {
		t.Fatalf("the retry landed in segment %d, want a fresh id above the failed take's 1", p.Loc.Segment)
	}
	c2 := e.set.Counters()
	if c2.UsedBytes != 2*streamCap {
		t.Fatalf("the footprint after the retry is %d, want both caps %d", c2.UsedBytes, 2*streamCap)
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
	if _, err := e.s.ReadAt(1, buf, p.Loc.Off); !errors.Is(err, segment.ErrSegmentGone) {
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
	if err := e.s.Sync(); err != nil {
		t.Fatalf("Sync after Close = %v, want nil", err)
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
