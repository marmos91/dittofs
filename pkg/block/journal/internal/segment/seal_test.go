package segment_test

import (
	"bytes"
	"errors"
	"io/fs"
	"math"
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

// sealAppended appends about five records through the stream and returns
// their places, the warm shape every seal check starts from.
func sealAppended(t *testing.T, e streamEnv) []segment.Placed {
	t.Helper()

	var placed []segment.Placed
	for i := range 5 {
		payload := streamTestBody(64, byte(0x30+i))
		h, body := streamWriteHeader(streamFileID(i+1), int64(i)*64, payload, 7, core.Version{Counter: uint64(i + 1)})
		placed = append(placed, streamAppendWrite(t, e, h, body))
	}
	return placed
}

// sealCounters snapshots every seam counter a seal can move, so a check can
// assert a refusal or a second seal made no call at all.
type sealCounters struct {
	writes, syncs, truncates, opens int
}

func sealCountersOf(e streamEnv, seg string) sealCounters {
	return sealCounters{
		writes:    e.seam.CallsFor(journaltest.OpWrite, seg),
		syncs:     e.seam.CallsFor(journaltest.OpSync, seg),
		truncates: e.seam.CallsFor(journaltest.OpTruncate, seg),
		opens:     e.seam.CallsFor(journaltest.OpOpen, seg),
	}
}

// sealFileSize returns the size of the segment's file through the seam's
// durable directory: the one source of truth for every size a seal check
// asserts, read where the cache cannot be asked.
func sealFileSize(t *testing.T, e streamEnv, id core.SegmentID) int64 {
	t.Helper()

	f, err := e.seam.FS().Open(streamSegName(id))
	if err != nil {
		t.Fatalf("opening segment %d's file: %v", id, err)
	}
	defer func() { _ = f.Close() }()
	size, err := f.Size()
	if err != nil {
		t.Fatalf("the size of segment %d's file: %v", id, err)
	}
	return size
}

// sealRecorderFault is a recording fault: it logs every seam call on the
// segment's file, and can fail chosen ops the first times they arrive.
type sealRecorderFault struct {
	log *[]string
	seg string
}

func (f *sealRecorderFault) At(op string, key []byte) (dl time.Duration, tear int, err error) {
	if string(key) == f.seg {
		*f.log = append(*f.log, op)
	}
	return 0, 0, nil
}

// sealRecordFault installs a recorder over the whole seam and returns it.
func sealRecordFault(t *testing.T, e streamEnv, seg string) *sealRecorderFault {
	t.Helper()

	f := &sealRecorderFault{seg: seg, log: &[]string{}}
	e.seam.SetFault(f)
	return f
}

// entries returns the calls the recorder logged, in their order.
func (f *sealRecorderFault) entries() []string { return *f.log }

// sealEntriesOf builds the catalog entries the appended records describe, in
// offset order, the way the seal must hand them to the footer.
//
// sealEntriesOf decodes the record header at each place, so what it builds
// names what landed rather than what the caller believed.
func sealEntriesOf(t *testing.T, e streamEnv, placed []segment.Placed) []format.CatalogEntry {
	t.Helper()

	entries := make([]format.CatalogEntry, 0, len(placed))
	for _, p := range placed {
		h, _ := streamReadRecord(t, e, p.Loc)
		entries = append(entries, format.CatalogEntry{
			File:         h.File,
			Extent:       h.Extent,
			RecordOffset: p.Loc.Off,
			Seq:          h.Seq,
			Version:      h.Version,
			Kind:         h.Kind,
			Tag:          h.Tag,
		})
	}
	slices.SortFunc(entries, func(a, b format.CatalogEntry) int {
		return int(a.RecordOffset - b.RecordOffset)
	})
	return entries
}

// TestSealFullWritesTheTrailerAtTheCapInTwoSyncs checks the full seal's
// contract: after five records the seal issues exactly two syncs, no
// truncation, at least one write; the file stays at its cap with the trailer
// in its last 32 bytes; the footer verifies through the format's own reader
// and names every record; the segment is Sealed in the ledger; and the
// footprint did not move. It catches an extra flush (the floor D11 pin), a
// trailer written after the footer on a full seal, and a full seal that
// truncates (R26, R27, R31, R35, R72).
func TestSealFullWritesTheTrailerAtTheCapInTwoSyncs(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	placed := sealAppended(t, e)
	marker := placed[len(placed)-1].Loc.Off + placed[len(placed)-1].Loc.Len
	before := sealCountersOf(e, seg)

	res, err := e.s.Seal(false)
	if err != nil {
		t.Fatalf("the full Seal: %v", err)
	}
	if res.Marker != marker {
		t.Fatalf("the seal's marker is at %d, want the last record's end %d", res.Marker, marker)
	}
	if res.Size != streamCap {
		t.Fatalf("a full seal is accounted at %d, want the cap %d", res.Size, streamCap)
	}
	if res.TailErr != nil {
		t.Fatalf("a full seal's TailErr is %v, want nil", res.TailErr)
	}

	after := sealCountersOf(e, seg)
	if got := after.syncs - before.syncs; got != 2 {
		t.Fatalf("the full seal issued %d syncs, want exactly 2", got)
	}
	if got := after.truncates - before.truncates; got != 0 {
		t.Fatalf("the full seal made %d truncations, want none", got)
	}
	if after.writes < before.writes+1 {
		t.Fatalf("the full seal wrote nothing, want at least the marker, footer and trailer")
	}

	// The file is at its cap, its trailer in the last 32 bytes.
	size := sealFileSize(t, e, 1)
	if size != streamCap {
		t.Fatalf("after the full seal the file is %d bytes, want the cap %d", size, streamCap)
	}

	// The footer verifies and describes every record. A Ref is an
	// io.ReaderAt, so the format's reader runs over it directly.
	r := sealReaderOf(t, e, 1)
	defer func() { _ = r.Close() }()
	footer, err := format.ReadFooter(r, size, streamIdentity, 1)
	if err != nil {
		t.Fatalf("the sealed segment's footer does not verify: %v", err)
	}
	if want := marker + format.SealMarkerSize; footer.Offset != want {
		t.Fatalf("the footer starts at %d, want the marker's end %d", footer.Offset, want)
	}
	want := sealEntriesOf(t, e, placed)
	if len(footer.Entries) != len(want) {
		t.Fatalf("the footer holds %d entries, want the %d records appended", len(footer.Entries), len(want))
	}
	for i := range want {
		if footer.Entries[i] != want[i] {
			t.Fatalf("entry %d is %+v, want %+v", i, footer.Entries[i], want[i])
		}
	}

	// The segment is sealed in the ledger, at its cap, with no share charged
	// a byte it does not hold.
	if state, err := e.set.State(1); err != nil || state != segment.Sealed {
		t.Fatalf("the segment's state after the seal is %v, %v, want Sealed", state, err)
	}
	c := e.set.Counters()
	if c.Sealed != 1 {
		t.Fatalf("the ledger counts %d sealed segments, want 1", c.Sealed)
	}
	if c.UsedBytes != streamCap {
		t.Fatalf("the footprint after the full seal is %d, want the cap %d", c.UsedBytes, streamCap)
	}
	if err := e.set.Check(); err != nil {
		t.Fatalf("the ledger after the seal: %v", err)
	}
}

// sealReaderOf acquires the segment and hands back a reader that releases
// the reference on Close, so format.ReadFooter runs over a held descriptor
// and the reference never leaks.
func sealReaderOf(t *testing.T, e streamEnv, id core.SegmentID) sealReader {
	t.Helper()

	ref, err := e.cache.Acquire(id)
	if err != nil {
		t.Fatalf("acquiring segment %d: %v", id, err)
	}
	return sealReader{ref: ref}
}

// sealReader is the segment's whole file as an io.ReaderAt: a Ref already
// reads at offsets, and the closer releases it.
type sealReader struct {
	ref segment.Ref
}

func (r sealReader) ReadAt(p []byte, off int64) (int, error) {
	return r.ref.ReadAt(p, off)
}

func (r sealReader) Close() error {
	r.ref.Release()
	return nil
}

// TestSealEarlyReturnsTheTailInThreeSyncs checks the early seal's contract:
// three syncs, one truncation, the file ends at its trailer, the footer
// verifies at the truncated size, and the ledger carries the segment at the
// new size — the footprint falls by exactly the returned tail. It catches a
// truncation never accounted, or a footer outside the accounted size (R26,
// R53, R72).
func TestSealEarlyReturnsTheTailInThreeSyncs(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	placed := sealAppended(t, e)
	last := placed[len(placed)-1].Loc
	marker := last.Off + last.Len
	before := sealCountersOf(e, seg)

	res, err := e.s.Seal(true)
	if err != nil {
		t.Fatalf("the early Seal: %v", err)
	}
	if res.Marker != marker {
		t.Fatalf("the seal's marker is at %d, want the last record's end %d", res.Marker, marker)
	}
	want := marker + format.SealMarkerSize + int64(len(placed))*format.CatalogEntrySize + format.TrailerSize
	if res.Size != want {
		t.Fatalf("the early seal is accounted at %d, want the trailer's end %d", res.Size, want)
	}
	if res.TailErr != nil {
		t.Fatalf("the early seal's TailErr is %v, want nil", res.TailErr)
	}

	after := sealCountersOf(e, seg)
	if got := after.syncs - before.syncs; got != 3 {
		t.Fatalf("the early seal issued %d syncs, want exactly 3", got)
	}
	if got := after.truncates - before.truncates; got != 1 {
		t.Fatalf("the early seal made %d truncations, want exactly 1", got)
	}

	size := sealFileSize(t, e, 1)
	if size != want {
		t.Fatalf("after the early seal the file is %d bytes, want the trailer's end %d", size, want)
	}

	// The footer verifies at the truncated size, with every record in it.
	r := sealReaderOf(t, e, 1)
	defer func() { _ = r.Close() }()
	footer, err := format.ReadFooter(r, size, streamIdentity, 1)
	if err != nil {
		t.Fatalf("the early-sealed segment's footer does not verify: %v", err)
	}
	entries := sealEntriesOf(t, e, placed)
	if len(footer.Entries) != len(entries) {
		t.Fatalf("the footer holds %d entries, want the %d records appended", len(footer.Entries), len(entries))
	}
	for i := range entries {
		if footer.Entries[i] != entries[i] {
			t.Fatalf("entry %d is %+v, want %+v", i, footer.Entries[i], entries[i])
		}
	}

	// The ledger carries the segment at its new size (R53): the footprint
	// fell by exactly the returned tail and the check still holds.
	c := e.set.Counters()
	if wantFoot := streamCap - (streamCap - size); c.UsedBytes != wantFoot {
		t.Fatalf("the footprint after the early seal is %d, want the segment's new size %d", c.UsedBytes, size)
	}
	if state, err := e.set.State(1); err != nil || state != segment.Sealed {
		t.Fatalf("the segment's state after the early seal is %v, %v, want Sealed", state, err)
	}
	if err := e.set.Check(); err != nil {
		t.Fatalf("the ledger after the early seal: %v", err)
	}
}

// TestSealStepsRunInOrder pins the order the seal's steps run in, which the
// sync count alone cannot see: the log of a recording fault must be exactly
// one sync, then the step-2 writes, then the second sync — and, for an early
// seal only, one truncation and the third sync after them. It catches a
// merged or reordered step: a marker written before the records are durable,
// or a truncation before the catalog is (R26, R29).
func TestSealStepsRunInOrder(t *testing.T) {
	for _, early := range []bool{false, true} {
		name := "full"
		if early {
			name = "early"
		}
		t.Run(name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			seg := streamSegName(1)
			e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
			defer func() { _ = e.s.Close() }()

			placed := sealAppended(t, e)
			f := sealRecordFault(t, e, seg)

			if _, err := e.s.Seal(early); err != nil {
				t.Fatalf("the Seal: %v", err)
			}
			_ = placed

			// The seal's calls, in their order: one sync, then at
			// least one write, then the second sync, then — an early
			// seal only — one truncation and the third sync. Nothing
			// may run before the first sync, and no truncation before
			// the second.
			log := f.entries()
			var syncs, writes, truncates []int
			for i, op := range log {
				switch op {
				case journaltest.OpSync:
					syncs = append(syncs, i)
				case journaltest.OpWrite:
					writes = append(writes, i)
				case journaltest.OpTruncate:
					truncates = append(truncates, i)
				default:
					t.Fatalf("an unexpected op in the seal's log: %q in %v", op, log)
				}
			}
			want := 2
			if early {
				want = 3
			}
			if len(syncs) != want {
				t.Fatalf("the seal's log holds %d syncs at %v, want %d: %v", len(syncs), syncs, want, log)
			}
			if len(writes) == 0 {
				t.Fatalf("the seal wrote nothing: %v", log)
			}
			if syncs[0] > writes[0] {
				t.Fatalf("a marker write ran before the records were made durable: %v", log)
			}
			if last := writes[len(writes)-1]; last > syncs[1] {
				t.Fatalf("a write ran past the second sync: %v", log)
			}
			if !early {
				if len(truncates) != 0 {
					t.Fatalf("a full seal truncated: %v", log)
				}
			} else {
				if len(truncates) != 1 {
					t.Fatalf("the early seal's log holds %d truncations, want one: %v", len(truncates), log)
				}
				if truncates[0] < syncs[1] || syncs[2] < truncates[0] {
					t.Fatalf("the truncation ran outside the second and third syncs: %v", log)
				}
			}
			_ = placed
		})
	}
}

// sealNthFault fails the n-th call (1-based) of an op on the segment's file,
// counted from the fault's installation, and passes everything else: the
// shape the failure injections need, since a schedule rule counts from the
// first call of the op, which the appends made long before the seal.
type sealNthFault struct {
	seg string
	op  string
	n   int
	err error
}

func (f *sealNthFault) At(op string, key []byte) (time.Duration, int, error) {
	if op == f.op && string(key) == f.seg {
		f.n--
		if f.n == 0 {
			return 0, 0, f.err
		}
	}
	return 0, 0, nil
}

// sealImage returns the segment's durable image: the bytes a power loss
// would leave, read from the seam. A Crash closes every open file handle, so
// the checks read the image directly rather than through a stream whose
// descriptors the crash invalidated.
func sealImage(t *testing.T, e streamEnv, id core.SegmentID) []byte {
	t.Helper()

	img, err := e.seam.Durable(streamSegName(id))
	if err != nil {
		t.Fatalf("the durable image: %v", err)
	}
	return img
}

// sealCrashBuilt registers the cache's Shutdown the way a crashed
// environment needs: a crash closes every seam handle, so the cache's close
// of a dead descriptor answers fs.ErrClosed — the error a dead process would
// never see, which the check tolerates as the fixture's own drop-dead path
// does. Every other error still fails the test, for a leaked ref shows in it.
func sealCrashBuilt(t *testing.T, e streamEnv) streamEnv {
	t.Helper()
	t.Cleanup(func() {
		err := e.cache.Shutdown()
		if err != nil && !errors.Is(err, fs.ErrClosed) {
			t.Errorf("the cache's Shutdown after the crash = %v, want nil or a dead handle's fs.ErrClosed", err)
		}
	})
	return e
}

// TestSealACrashAtEveryDurableStateLeavesThePromisedBytes crashes the seal
// between its steps and checks every durable state the RFC promises: a crash
// before the second sync leaves no footer and the records still decoding; a
// crash keeping only part of step 2's writes is no footer too; a crash after
// step 2 of a full seal leaves the footer verifiable; a crash after step 2
// of an early seal, with the truncation dropped, leaves the file at its cap
// with a valid marker at its place and no footer a reader at the cap would
// find; a crash after step 3 leaves the footer verifiable at the truncated
// size (R26).
func TestSealACrashAtEveryDurableStateLeavesThePromisedBytes(t *testing.T) {
	// (a) Fail step 2's own sync, then crash: the records are durable and
	// decode; the marker, footer and trailer the failed sync covered never
	// became durable, so no footer reads, and the segment is ended, never
	// sealed.
	t.Run("failed step-2 sync then crash", func(t *testing.T) {
		prof.Watch(t, prof.Options{})

		seg := streamSegName(1)
		e := sealCrashBuilt(t, streamOver(t, streamCap, func(streamEnv) {}))
		defer func() { _ = e.s.Close() }()

		placed := sealAppended(t, e)
		f := &sealNthFault{seg: seg, op: journaltest.OpSync, n: 2, err: syscall.EIO}
		e.seam.SetFault(f)
		if _, err := e.s.Seal(true); !errors.Is(err, segment.ErrSegmentEnded) || !errors.Is(err, syscall.EIO) {
			t.Fatalf("the Seal whose step-2 sync failed = %v, want ErrSegmentEnded carrying the EIO", err)
		}
		if err := e.seam.Crash(); err != nil {
			t.Fatalf("the crash: %v", err)
		}

		img := sealImage(t, e, 1)
		if _, err := format.ReadFooter(bytes.NewReader(img), int64(len(img)), streamIdentity, 1); err == nil {
			t.Fatal("the durable state carries a footer after a failed step-2 sync, want none")
		}
		for _, p := range placed {
			raw := make([]byte, format.RecordHeaderSize)
			copy(raw, img[p.Loc.Off:p.Loc.Off+format.RecordHeaderSize])
			if _, err := format.DecodeRecordHeader(raw, streamIdentity, 1, p.Loc.Off); err != nil {
				t.Fatalf("the record at %d does not decode after the crash: %v", p.Loc.Off, err)
			}
		}
	})

	// (b) A crash keeping only part of step 2's writes: no footer, for the
	// marker with the footer and without the trailer, and for the trailer
	// without the marker. The full seal's two step-2 writes are staged
	// apart; an early seal's single write is torn instead.
	t.Run("part of step 2 kept", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			early bool
			torn  bool
			mask  uint64
		}{
			{name: "full: marker and footer, no trailer", mask: 0b01},
			{name: "full: trailer, no marker", mask: 0b10},
			{name: "early: single write torn", early: true, torn: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				prof.Watch(t, prof.Options{})

				seg := streamSegName(1)
				e := sealCrashBuilt(t, streamOver(t, streamCap, func(streamEnv) {}))
				// The crash closes every open handle, so the stream's
				// own Close after it returns the crashed files' errors
				// without touching storage; it must still run, for the
				// ref it holds is what the cache's Shutdown waits out.
				defer func() { _ = e.s.Close() }()

				placed := sealAppended(t, e)

				if !tc.torn {
					// The full seal splits step 2 in two writes. Park the
					// seal at its first sync, let it pass, then park it at
					// its second: at that point both step-2 writes are
					// issued and unsynced, so the crash keeps one and
					// loses the other by the mask.
					h := e.seam.HoldFirst(t, journaltest.OpSync, seg)
					synced := make(chan error, 1)
					go func() {
						_, err := e.s.Seal(false)
						synced <- err
					}()
					<-h.Reached()
					h.Release()
					h2 := e.seam.HoldFirst(t, journaltest.OpSync, seg)
					<-h2.Reached()
					if n := e.seam.Unsynced(); n != 2 {
						t.Fatalf("the seal left %d unsynced writes, want the two of step 2", n)
					}
					if err := e.seam.Crash(journaltest.Keeping(tc.mask)); err != nil {
						t.Fatalf("the crash keeping %#x: %v", tc.mask, err)
					}
					h2.Release()
					<-synced // the crashed sync returns the fs.ErrClosed of its park
				} else {
					// The early seal writes its marker, footer and
					// trailer as one call. Park the seal at its first
					// sync, let it pass, then park it at its second:
					// the single step-2 write is issued and unsynced
					// there, so the crash tears it inside itself.
					h := e.seam.HoldFirst(t, journaltest.OpSync, seg)
					synced := make(chan error, 1)
					go func() {
						_, err := e.s.Seal(true)
						synced <- err
					}()
					<-h.Reached()
					h.Release()
					h2 := e.seam.HoldFirst(t, journaltest.OpSync, seg)
					<-h2.Reached()
					if n := e.seam.Unsynced(); n != 1 {
						t.Fatalf("the seal left %d unsynced writes, want the one write of step 2", n)
					}
					if err := e.seam.Crash(journaltest.TornIn(seg, 40)); err != nil {
						t.Fatalf("the torn crash: %v", err)
					}
					h2.Release()
					<-synced // the crashed sync returns the fs.ErrClosed of its park
				}

				img := sealImage(t, e, 1)
				if _, err := format.ReadFooter(bytes.NewReader(img), int64(len(img)), streamIdentity, 1); err == nil {
					t.Fatalf("the durable state holds a footer through only part of step 2, want none")
				}
				for _, p := range placed {
					raw := make([]byte, format.RecordHeaderSize)
					copy(raw, img[p.Loc.Off:p.Loc.Off+format.RecordHeaderSize])
					if _, err := format.DecodeRecordHeader(raw, streamIdentity, 1, p.Loc.Off); err != nil {
						t.Fatalf("the record at %d does not decode after the crash: %v", p.Loc.Off, err)
					}
				}
			})
		}
	})

	// (c) A crash after step 2 of a full seal: the footer verifies. A full
	// seal writes nothing past its second sync, so the durable state at its
	// return is the state after step 2.
	t.Run("after step 2 of a full seal", func(t *testing.T) {
		prof.Watch(t, prof.Options{})

		e := sealCrashBuilt(t, streamOver(t, streamCap, func(streamEnv) {}))
		defer func() { _ = e.s.Close() }()

		sealAppended(t, e)
		if _, err := e.s.Seal(false); err != nil {
			t.Fatalf("the full Seal: %v", err)
		}
		if err := e.seam.Crash(); err != nil {
			t.Fatalf("the crash: %v", err)
		}
		img := sealImage(t, e, 1)
		if int64(len(img)) != streamCap {
			t.Fatalf("the durable file is %d bytes, want the cap %d", len(img), streamCap)
		}
		if _, err := format.ReadFooter(bytes.NewReader(img), int64(len(img)), streamIdentity, 1); err != nil {
			t.Fatalf("the footer after step 2 of the full seal does not verify: %v", err)
		}
	})

	// (d) A crash after step 2 of an early seal with the truncation dropped:
	// the file is still at its cap, ReadFooter at the cap reports no footer,
	// and the marker verifies at its own offset.
	t.Run("after step 2 of an early seal, truncation dropped", func(t *testing.T) {
		prof.Watch(t, prof.Options{})

		seg := streamSegName(1)
		e := sealCrashBuilt(t, streamOver(t, streamCap, func(streamEnv) {}))
		// The crash closes every open handle; the stream's own Close must
		// still run after it, for the ref it holds is what the cache's
		// Shutdown waits out.
		defer func() { _ = e.s.Close() }()

		placed := sealAppended(t, e)
		marker := pEnd(placed[len(placed)-1])

		// Park the seal inside its truncation and crash: the cut is
		// dropped, so the durable file keeps its cap. The seal still
		// returns with a nil error and TailErr carrying the truncation's
		// crashed-handle failure — the seal is complete after its second
		// step, and a failed truncation does not end the segment.
		h := e.seam.HoldFirst(t, journaltest.OpTruncate, seg)
		res := make(chan segment.SealResult, 1)
		serr := make(chan error, 1)
		go func() {
			r, err := e.s.Seal(true)
			res <- r
			serr <- err
		}()
		<-h.Reached()
		if err := e.seam.Crash(); err != nil {
			t.Fatalf("the crash: %v", err)
		}
		// The release comes first: the parked truncation returns with the
		// crashed handle's fs.ErrClosed, and only then does the seal
		// answer — with a nil error and TailErr carrying it (R34).
		h.Release()
		if err := <-serr; err != nil {
			t.Fatalf("the seal across a crash inside its truncation returned %v, want nil: the truncation's failure is the tail's (R34)", err)
		}
		r := <-res
		if r.TailErr == nil {
			t.Fatal("the seal across the crash reports no TailErr, want the truncation's crashed-handle failure")
		}

		img := sealImage(t, e, 1)
		if int64(len(img)) != streamCap {
			t.Fatalf("with the truncation dropped the durable file is %d bytes, want the cap %d", len(img), streamCap)
		}
		if _, err := format.ReadFooter(bytes.NewReader(img), int64(len(img)), streamIdentity, 1); err == nil {
			t.Fatal("ReadFooter at the cap found a footer through the untruncated tail, want no footer")
		}
		mbuf := make([]byte, format.SealMarkerSize)
		copy(mbuf, img[marker:marker+format.SealMarkerSize])
		if _, err := format.DecodeSealMarker(mbuf, streamIdentity, 1, marker); err != nil {
			t.Fatalf("the seal marker at %d does not verify: %v", marker, err)
		}
	})

	// (e) A crash after step 3 of an early seal: the footer verifies at the
	// truncated size.
	t.Run("after step 3 of an early seal", func(t *testing.T) {
		prof.Watch(t, prof.Options{})

		e := sealCrashBuilt(t, streamOver(t, streamCap, func(streamEnv) {}))
		defer func() { _ = e.s.Close() }()

		placed := sealAppended(t, e)
		res, err := e.s.Seal(true)
		if err != nil || res.TailErr != nil {
			t.Fatalf("the early Seal: %v, %v", err, res.TailErr)
		}
		if err := e.seam.Crash(); err != nil {
			t.Fatalf("the crash: %v", err)
		}
		img := sealImage(t, e, 1)
		if int64(len(img)) != res.Size {
			t.Fatalf("the durable file is %d bytes, want the truncated %d", len(img), res.Size)
		}
		footer, err := format.ReadFooter(bytes.NewReader(img), int64(len(img)), streamIdentity, 1)
		if err != nil {
			t.Fatalf("the footer after the truncation does not verify: %v", err)
		}
		if len(footer.Entries) != len(placed) {
			t.Fatalf("the footer holds %d entries, want the %d records appended", len(footer.Entries), len(placed))
		}
	})
}

// pEnd returns the end offset of the last placed record: where the seal's
// marker sits.
func pEnd(p segment.Placed) int64 {
	return p.Loc.Off + p.Loc.Len
}

// TestSealAFailedSyncOrWriteEndsTheSegment checks R33: a failure in
// either of the seal's first two steps — the record sync, the step-2
// write, the step-2 sync — ends the segment rather than sealing it. Each
// answers an error matching ErrSegmentEnded and the store's error, leaves
// the segment Ended in the ledger, unsealed, accounted at its cap, and
// refuses every later append, second seal and sync. It catches a seal that
// leaves a failed segment Active, or that later pretends it sealed.
func TestSealAFailedSyncOrWriteEndsTheSegment(t *testing.T) {
	for _, tc := range []struct {
		name  string
		op    string
		nth   int // which of the seal's own calls of op the fault fails
		err   error
		early bool
	}{
		{"the record sync", journaltest.OpSync, 1, syscall.EIO, true},
		// The step-2 write: the full seal writes the marker and footer,
		// then the trailer, so its first step-2 write is the first write
		// after the fault is armed.
		{"the step-2 write", journaltest.OpWrite, 1, syscall.EIO, false},
		{"the step-2 sync", journaltest.OpSync, 2, syscall.EIO, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			seg := streamSegName(1)
			e := sealCrashBuilt(t, streamOver(t, streamCap, func(streamEnv) {}))
			defer func() { _ = e.s.Close() }()

			placed := sealAppended(t, e)
			// The nth-call fault fails the named call of the seal's own,
			// counted from the appends: the fault is armed once they are
			// done, so n=1 is the seal's first sync, n=2 its second, and
			// the write the seal's first marker write — a failed first
			// step is told from a failed second by which call the fault
			// refused.
			f := &sealNthFault{seg: seg, op: tc.op, n: tc.nth, err: tc.err}
			e.seam.SetFault(f)

			if _, err := e.s.Seal(tc.early); !errors.Is(err, segment.ErrSegmentEnded) || !errors.Is(err, tc.err) {
				t.Fatalf("the Seal whose %s failed = %v, want ErrSegmentEnded carrying %v", tc.name, err, tc.err)
			}
			_ = placed

			// The segment is ended in the ledger — never sealed — and
			// kept at its cap with every share-byte charged.
			if state, err := e.set.State(1); err != nil || state != segment.Ended {
				t.Fatalf("the segment's state after the failed %s is %v, %v, want Ended", tc.name, state, err)
			}
			if c := e.set.Counters(); c.Sealed != 0 {
				t.Fatalf("the ledger counts %d sealed segments after the failed %s, want none", c.Sealed, tc.name)
			}
			if c := e.set.Counters(); c.UsedBytes != streamCap {
				t.Fatalf("the footprint after the failed %s is %d, want the cap %d", tc.name, c.UsedBytes, streamCap)
			}
			if err := e.set.Check(); err != nil {
				t.Fatalf("the ledger after the failed %s: %v", tc.name, err)
			}

			// Every later append is refused and reserves nothing: the
			// roll signal carries the end, so the caller can tell a
			// segment it must roll on from one whose successor names
			// none.
			payload := streamTestBody(64, 0x77)
			h, body := streamWriteHeader(streamFileID(9), 512, payload, 7, core.Version{Counter: 99})
			if _, err := e.s.Append(h, body); !errors.Is(err, segment.ErrSegmentFull) || !errors.Is(err, segment.ErrSegmentEnded) {
				t.Fatalf("the append after the failed %s = %v, want ErrSegmentFull carrying ErrSegmentEnded", tc.name, err)
			}
			// A second seal writes nothing and answers ErrSegmentEnded.
			returned := sealResult{writes: e.seam.CallsFor(journaltest.OpWrite, seg), syncs: e.seam.CallsFor(journaltest.OpSync, seg), truncates: e.seam.CallsFor(journaltest.OpTruncate, seg)}
			if _, err := e.s.Seal(true); !errors.Is(err, segment.ErrSegmentEnded) {
				t.Fatalf("the second Seal after the failed %s = %v, want ErrSegmentEnded", tc.name, err)
			}
			now := sealResult{writes: e.seam.CallsFor(journaltest.OpWrite, seg), syncs: e.seam.CallsFor(journaltest.OpSync, seg), truncates: e.seam.CallsFor(journaltest.OpTruncate, seg)}
			if now != returned {
				t.Fatalf("the second Seal after the failed %s moved the seam: %+v, want %+v", tc.name, now, returned)
			}
			// A sync after the end is refused too.
			if err := e.s.Sync(); !errors.Is(err, segment.ErrSegmentEnded) {
				t.Fatalf("the Sync after the failed %s = %v, want ErrSegmentEnded", tc.name, err)
			}
		})
	}
}

// TestSealAFailedTruncationLeavesItSealedAtItsCap checks R34: a failure of
// the truncation or of its sync is the tail's alone — the seal returns a nil
// error with TailErr carrying the failure, the segment is Sealed and
// accounted at its cap, no Shrink ran, and a successor may be taken. It
// catches a truncation failure treated as an end, or an accounting that cut
// the footprint before the cut was durable.
func TestSealAFailedTruncationLeavesItSealedAtItsCap(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   string
	}{
		{"the truncation", journaltest.OpTruncate},
		{"the truncation's sync", journaltest.OpSync},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			seg := streamSegName(1)
			e := sealCrashBuilt(t, streamOver(t, streamCap, func(streamEnv) {}))
			defer func() { _ = e.s.Close() }()

			sealAppended(t, e)
			before := e.set.Counters()
			if tc.op == journaltest.OpSync {
				// Fail the truncation's own sync — the seal's third,
				// counted from the appends: the first two pass and the
				// truncation runs, so the fault fires on the third sync
				// it sees.
				e.seam.SetFault(&sealNthFault{seg: seg, op: journaltest.OpSync, n: 3, err: syscall.EIO})
				r, err := e.s.Seal(true)
				if err != nil {
					t.Fatalf("the early Seal whose tail sync failed = %v, want nil: the truncation is the tail's", err)
				}
				if r.TailErr == nil || !errors.Is(r.TailErr, syscall.EIO) {
					t.Fatalf("the failed tail sync in TailErr = %v, want the EIO", r.TailErr)
				}
				checkSealedAtCap(t, e, before, r)
				return
			}

			// The truncation itself: the fault fails the seal's only
			// truncation call.
			e.seam.SetFault(&sealNthFault{seg: seg, op: journaltest.OpTruncate, n: 1, err: syscall.EIO})

			r, err := e.s.Seal(true)
			if err != nil {
				t.Fatalf("the early Seal whose %s failed = %v, want nil: the truncation's failure is the tail's", tc.name, err)
			}
			if r.TailErr == nil || !errors.Is(r.TailErr, syscall.EIO) {
				t.Fatalf("the failed %s in TailErr = %v, want the EIO", tc.name, r.TailErr)
			}
			checkSealedAtCap(t, e, before, r)
		})
	}
}

// checkSealedAtCap asserts what a failed truncation leaves: the segment is
// Sealed in the ledger, counted once, accounted at its cap with the
// footprint unmoved, no Shrink served, and the ledger's check nil.
func checkSealedAtCap(t *testing.T, e streamEnv, before segment.Counters, r segment.SealResult) {
	t.Helper()

	if r.Size != streamCap {
		t.Fatalf("the sealed segment is accounted at %d, want its cap %d", r.Size, streamCap)
	}
	if state, err := e.set.State(1); err != nil || state != segment.Sealed {
		t.Fatalf("the segment's state after the failed tail is %v, %v, want Sealed", state, err)
	}
	c := e.set.Counters()
	if c.Sealed != 1 {
		t.Fatalf("the ledger counts %d sealed segments, want 1", c.Sealed)
	}
	if c.UsedBytes != before.UsedBytes {
		t.Fatalf("the footprint after the failed tail is %d, want the before-seal %d", c.UsedBytes, before.UsedBytes)
	}
	if err := e.set.Check(); err != nil {
		t.Fatalf("the ledger after the failed tail: %v", err)
	}
	// A failed-tail seal is complete: a Shrink is not needed, and the
	// segment's accounted size is the cap, so a later append is refused
	// and a second seal writes nothing (R32).
	if _, err := e.s.Seal(true); err == nil {
		t.Fatal("a second seal of the sealed segment succeeded, want ErrWrongState")
	}
}

// TestSealASecondSealWritesNothing checks R32: after a full seal and after
// an early one, every later Seal is refused with ErrWrongState and makes no
// seam call and no ledger change; two Seals racing each other leave exactly
// one sealed, the other refused, and the footer written once.
func TestSealASecondSealWritesNothing(t *testing.T) {
	for _, early := range []bool{false, true} {
		name := "full"
		if early {
			name = "early"
		}
		t.Run(name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			seg := streamSegName(1)
			e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
			defer func() { _ = e.s.Close() }()

			sealAppended(t, e)
			if _, err := e.s.Seal(early); err != nil {
				t.Fatalf("the first Seal: %v", err)
			}
			counters := sealCountersOf(e, seg)
			c := e.set.Counters()

			// Both refusals of the sealed stream.
			for _, again := range []bool{false, true} {
				if _, err := e.s.Seal(again); !errors.Is(err, segment.ErrWrongState) {
					t.Fatalf("the second Seal(%v) = %v, want ErrWrongState", again, err)
				}
			}
			if now := sealCountersOf(e, seg); now != counters {
				t.Fatalf("the second seals moved the seam: %+v, want %+v", now, counters)
			}
			if after := e.set.Counters(); after != c {
				t.Fatalf("the second seals moved the ledger: %+v, want %+v", after, c)
			}
		})
	}

	t.Run("two racing seals leave one", func(t *testing.T) {
		prof.Watch(t, prof.Options{})

		e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
		defer func() { _ = e.s.Close() }()

		sealAppended(t, e)
		results := make(chan sealRace, 2)
		for range 2 {
			go func() {
				r, err := e.s.Seal(false)
				results <- sealRace{r: r, err: err}
			}()
		}
		first, second := <-results, <-results
		sealed, refused := first, second
		if first.err != nil {
			sealed, refused = second, first
		}
		if sealed.err != nil {
			t.Fatalf("neither racing seal succeeded: %v and %v", first.err, second.err)
		}
		if !errors.Is(refused.err, segment.ErrWrongState) {
			t.Fatalf("the refused racing seal = %v, want ErrWrongState", refused.err)
		}
		// The footer is written once: the sealed segment verifies, and its
		// marker, footer and trailer were written by one write at least
		// but the segment was sealed once.
		if c := e.set.Counters(); c.Sealed != 1 {
			t.Fatalf("the ledger counts %d sealed segments after the race, want 1", c.Sealed)
		}
		if err := e.set.Check(); err != nil {
			t.Fatalf("the ledger after the race: %v", err)
		}
		size := sealFileSize(t, e, 1)
		r := sealReaderOf(t, e, 1)
		defer func() { _ = r.Close() }()
		footer, err := format.ReadFooter(r, size, streamIdentity, 1)
		if err != nil {
			t.Fatalf("the raced footer does not verify: %v", err)
		}
		if len(footer.Entries) != 5 {
			t.Fatalf("the raced footer holds %d entries, want the 5 appended", len(footer.Entries))
		}
	})
}

// sealRace is one racing seal's outcome.
type sealRace struct {
	r   segment.SealResult
	err error
}

// sealResult is a snapshot of the seam's counters on one file, comparable
// with ==.
type sealResult struct {
	writes, syncs, truncates int
}

// TestSealWaitsForTheInFlightAppends checks R85's wait: a seal started while
// a record write is parked does not reach its own first sync until the write
// lands; once it does, the marker sits at the parked record's end and the
// footer lists it. It catches a seal that places its marker over an
// in-flight slot.
func TestSealWaitsForTheInFlightAppends(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	// One landed record warms the stream, then the next write parks.
	warm := streamTestBody(64, 0x5a)
	wh, wbody := streamWriteHeader(streamFileID(1), 0, warm, 7, core.Version{Counter: 1})
	warmPlaced := streamAppendWrite(t, e, wh, wbody)

	h := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	heldDone := make(chan segment.Placed, 1)
	go func() {
		payload := streamTestBody(64, 0x3c)
		hh, body := streamWriteHeader(streamFileID(2), 64, payload, 7, core.Version{Counter: 2})
		p, err := e.s.Append(hh, body)
		if err != nil {
			t.Errorf("the held append: %v", err)
		}
		heldDone <- p
	}()
	<-h.Reached()

	// While the write is parked, the seal must not run: no sync of the
	// segment may reach the seam before the append lands.
	sealed := make(chan sealRace, 1)
	go func() {
		r, err := e.s.Seal(false)
		sealed <- sealRace{r: r, err: err}
	}()
	syncsBefore := e.seam.CallsFor(journaltest.OpSync, seg)
	time.Sleep(50 * time.Millisecond)
	if got := e.seam.CallsFor(journaltest.OpSync, seg); got != syncsBefore {
		t.Fatalf("the seal made a sync while an append was still in flight: %d syncs, want still %d", got, syncsBefore)
	}

	// The write lands; the seal passes the drain and places the marker at
	// the held record's end, whose entry the footer carries.
	h.Release()
	held := <-heldDone
	race := <-sealed
	if race.err != nil {
		t.Fatalf("the Seal after the held append: %v", race.err)
	}
	if want := held.Loc.Off + held.Loc.Len; race.r.Marker != want {
		t.Fatalf("the seal's marker is at %d, want the held record's end %d", race.r.Marker, want)
	}
	size := sealFileSize(t, e, 1)
	rd := sealReaderOf(t, e, 1)
	defer func() { _ = rd.Close() }()
	footer, err := format.ReadFooter(rd, size, streamIdentity, 1)
	if err != nil {
		t.Fatalf("the footer after the waited seal does not verify: %v", err)
	}
	if len(footer.Entries) != 2 {
		t.Fatalf("the footer holds %d entries, want the two records, the warm one and the held one", len(footer.Entries))
	}
	last := footer.Entries[len(footer.Entries)-1]
	if last.RecordOffset != held.Loc.Off {
		t.Fatalf("the footer's last entry sits at %d, want the held record's %d", last.RecordOffset, held.Loc.Off)
	}
	_ = warmPlaced
}

// TestSealAppendsRacingItLandBeforeItsMarkerOrAreRefused checks R1 and R85
// under the race detector: sixteen goroutines append while one Seal runs.
// Every success lands at or below the marker, every failure matches
// ErrSegmentFull, after the seal returns every append fails, the footer
// lists exactly the successes in offset order — entries kept in completion
// order must have been sorted — and the share bytes sum to the placed
// records' whole sizes, so no charge was refused its record.
func TestSealAppendsRacingItLandBeforeItsMarkerOrAreRefused(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	const appenders = 16
	const perAppender = 4

	type outcome struct {
		p   segment.Placed
		len int64
	}
	outcomes := make(chan outcome, appenders*perAppender)

	// A warm record takes the segment before the race, so the Seal and
	// the appends compete over one taken segment rather than its take.
	warm := streamTestBody(64, 0x5a)
	wh, wbody := streamWriteHeader(streamFileID(1), 0, warm, 7, core.Version{Counter: 1})
	warmPlaced := streamAppendWrite(t, e, wh, wbody)

	sealed := make(chan sealRace, 1)
	go func() {
		r, err := e.s.Seal(false)
		sealed <- sealRace{r: r, err: err}
	}()

	var wg sync.WaitGroup
	for i := range appenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range perAppender {
				payload := streamTestBody(32, byte(i*16+j))
				h, body := streamWriteHeader(streamFileID(i), int64(j)*32, payload, 7, core.Version{Counter: uint64(i*perAppender + j + 1)})
				p, err := e.s.Append(h, body)
				if err != nil {
					if !errors.Is(err, segment.ErrSegmentFull) {
						t.Errorf("appender %d's append %d: %v, want ErrSegmentFull or nil", i, j, err)
					}
					continue
				}
				outcomes <- outcome{p: p, len: p.Loc.Len}
			}
		}(i)
	}
	wg.Wait()
	close(outcomes)

	race := <-sealed
	if race.err != nil {
		t.Fatalf("the raced Seal: %v", race.err)
	}

	var placed []segment.Placed
	var sum int64
	placed = append(placed, warmPlaced)
	sum += warmPlaced.Loc.Len
	for o := range outcomes {
		if end := o.p.Loc.Off + o.p.Loc.Len; end > race.r.Marker {
			t.Fatalf("a record ended at %d, past the seal's marker %d", end, race.r.Marker)
		}
		placed = append(placed, o.p)
		sum += o.len
	}
	if len(placed) == 0 {
		t.Fatal("every racing append was refused, want at least one to land before the marker")
	}

	// After the seal, every append is refused.
	payload := streamTestBody(64, 0x77)
	h, body := streamWriteHeader(streamFileID(99), 0, payload, 7, core.Version{Counter: 99})
	if _, err := e.s.Append(h, body); !errors.Is(err, segment.ErrSegmentFull) {
		t.Fatalf("the append after the seal = %v, want ErrSegmentFull", err)
	}

	// The footer lists exactly the successes, ascending by offset.
	size := sealFileSize(t, e, 1)
	rd := sealReaderOf(t, e, 1)
	defer func() { _ = rd.Close() }()
	footer, err := format.ReadFooter(rd, size, streamIdentity, 1)
	if err != nil {
		t.Fatalf("the raced footer does not verify: %v", err)
	}
	if len(footer.Entries) != len(placed) {
		t.Fatalf("the footer holds %d entries, want the %d successes", len(footer.Entries), len(placed))
	}
	offs := make([]int64, 0, len(placed))
	for _, p := range placed {
		offs = append(offs, p.Loc.Off)
	}
	slices.Sort(offs)
	for i, want := range offs {
		if footer.Entries[i].RecordOffset != want {
			t.Fatalf("the footer's entry %d sits at %d, want the placed %d", i, footer.Entries[i].RecordOffset, want)
		}
	}
	if got := e.set.ShareBytes(7); got != sum {
		t.Fatalf("the share holds %d record bytes, want the %d placed", got, sum)
	}
	if err := e.set.Check(); err != nil {
		t.Fatalf("the ledger after the race: %v", err)
	}
}

// TestSealAnEarlierFailureEndsInsteadOfSealing checks the seal's refusal over
// a segment that already failed: a failed record write, or a failed sync
// before the seal, makes Seal answer ErrSegmentEnded, end the segment in the
// ledger and make no seam call of its own — the failure already ended the
// stream's involvement, and the seal must not write over a failed slot or a
// failed window.
func TestSealAnEarlierFailureEndsInsteadOfSealing(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(t *testing.T, e streamEnv, seg string)
	}{
		{"a failed record write", func(t *testing.T, e streamEnv, seg string) {
			// The next record's write fails with the seam's EIO: the
			// fault is armed once the warm record has landed, and
			// fires on the write that follows its own arming.
			e.seam.SetFault(&sealNthFault{seg: seg, op: journaltest.OpWrite, n: 1, err: syscall.EIO})
			payload := streamTestBody(64, 0x31)
			h, body := streamWriteHeader(streamFileID(2), 64, payload, 7, core.Version{Counter: 2})
			if _, err := e.s.Append(h, body); !errors.Is(err, syscall.EIO) {
				t.Fatalf("the faulted append = %v, want the EIO", err)
			}
		}},
		{"a failed sync", func(t *testing.T, e streamEnv, seg string) {
			if err := e.s.Sync(); err != nil {
				t.Fatalf("the warm Sync: %v", err)
			}
			e.seam.SetFault(&sealNthFault{seg: seg, op: journaltest.OpSync, n: 1, err: syscall.EIO})
			if err := e.s.Sync(); !errors.Is(err, syscall.EIO) {
				t.Fatalf("the faulted Sync = %v, want the EIO", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			seg := streamSegName(1)
			e := sealCrashBuilt(t, streamOver(t, streamCap, func(streamEnv) {}))
			defer func() { _ = e.s.Close() }()

			// One landed record warms the stream, then the failure runs.
			warm := streamTestBody(64, 0x5a)
			wh, wbody := streamWriteHeader(streamFileID(1), 0, warm, 7, core.Version{Counter: 1})
			streamAppendWrite(t, e, wh, wbody)
			tc.fail(t, e, seg)

			// The seal over the failed segment is refused and makes no
			// seam call: the counters it would move have not moved past
			// the failure's own.
			after := sealCountersOf(e, seg)
			if _, err := e.s.Seal(true); !errors.Is(err, segment.ErrSegmentEnded) {
				t.Fatalf("the Seal over %s = %v, want ErrSegmentEnded", tc.name, err)
			}
			if now := sealCountersOf(e, seg); now != after {
				t.Fatalf("the Seal over %s moved the seam: %+v, want %+v", tc.name, now, after)
			}

			if state, err := e.set.State(1); err != nil || state != segment.Ended {
				t.Fatalf("the segment's state after the seal over %s is %v, %v, want Ended", tc.name, state, err)
			}
			if c := e.set.Counters(); c.Sealed != 0 {
				t.Fatalf("the ledger counts %d sealed segments after the seal over %s, want none", c.Sealed, tc.name)
			}
		})
	}
}

// TestSealWriteSealTakesEntriesAsData checks the recovery path: WriteSeal
// accepts the entries a scan of an appended segment rebuilds, through a
// second reference straight from the cache, and the footer verifies where the
// stream's own entries' footer did. Then its refusals: entries out of order,
// a last record that does not end at the marker, a marker off the grid, no
// room before the cap and a segment that is not Active — each answers
// ErrBadCatalog (or the ledger's own refusal) with no seam call and no
// ledger change.
func TestSealWriteSealTakesEntriesAsData(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	placed := sealAppended(t, e)
	marker := pEnd(placed[len(placed)-1])
	entries := sealEntriesOf(t, e, placed)

	// The refusals: each hands the writer entries it cannot encode into a
	// footer the reader accepts, on an Active segment of a second set. The
	// writer is the one recovery calls too, so every refusal must hold with
	// no seam call made and no ledger change (R32 at the writer, R11 at
	// the writer, and the encoder's own contract).
	refuse := []struct {
		name    string
		entries func() []format.CatalogEntry
		marker  int64
	}{
		{"entries out of order", func() []format.CatalogEntry {
			out := slices.Clone(entries)
			out[0], out[len(out)-1] = out[len(out)-1], out[0]
			return out
		}, marker},
		{"a duplicated entry", func() []format.CatalogEntry {
			// The last entry twice: the copy's record still ends at
			// the marker — so the end check passes — and its offset
			// equals the one before it, which only the ascent rule's
			// strict half refuses. A reader without it takes back a
			// footer that names one record twice.
			out := slices.Clone(entries)
			out = append(out, out[len(out)-1])
			return out
		}, marker},
		{"a last record not ending at the marker", func() []format.CatalogEntry {
			out := slices.Clone(entries[:len(entries)-1])
			return out
		}, marker},
		{"a marker off the grid", func() []format.CatalogEntry { return entries }, marker + 4},
		{"a record offset off the grid", func() []format.CatalogEntry {
			out := slices.Clone(entries)
			out[0].RecordOffset += 4
			return out
		}, marker},
		{"a record offset below the first slot", func() []format.CatalogEntry {
			out := slices.Clone(entries)
			out[0].RecordOffset -= 8
			return out
		}, marker},
		{"an entry of an unknown kind", func() []format.CatalogEntry {
			out := slices.Clone(entries)
			out[1].Kind = format.Kind(0x63)
			return out
		}, marker},
		{"an entry with a non-zero version epoch", func() []format.CatalogEntry {
			out := slices.Clone(entries)
			out[1].Version = core.Version{Epoch: 1, Counter: 2}
			return out
		}, marker},
		{"an entry with a negative file offset", func() []format.CatalogEntry {
			out := slices.Clone(entries)
			out[1].Extent = core.Extent{Off: -1, Len: 64}
			return out
		}, marker},
		{"no room before the cap", func() []format.CatalogEntry {
			// A segment whose single record ends near the cap, with the
			// marker sitting so close that the marker, its one footer
			// entry and the trailer do not fit before it — a recovery
			// caller's hand-off the writer must refuse rather than write
			// over the trailer's slot. The record is a real one the
			// header decoder accepts, so only the room check refuses it.
			marker := streamCap - format.SealMarkerSize - format.TrailerSize
			reclen := format.RecordHeader{Kind: format.KindWrite, Extent: core.Extent{Len: 64}}.Size()
			return []format.CatalogEntry{{
				File:         streamFileID(1),
				Extent:       core.Extent{Off: 0, Len: 64},
				RecordOffset: marker - reclen,
				Seq:          1,
				Version:      core.Version{Counter: 1},
				Kind:         format.KindWrite,
				Tag:          7,
			}}
		}, streamCap - format.SealMarkerSize - format.TrailerSize},
	}
	for _, tc := range refuse {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh Active segment of its own set, taken the way a
			// stream takes one, so the refusals run against a segment
			// the writer could otherwise seal.
			set := segment.NewSet(streamCap, nil)
			budget := core.NewDescriptorBudget(8)
			cache := segment.NewDescriptors(e.seam.FS(), format.SegmentName, budget)
			maker := segment.NewMaker(e.seam.FS(), set, streamMax, 0, nil)
			t.Cleanup(func() {
				if err := cache.Shutdown(); err != nil {
					t.Errorf("the refusal's cache Shutdown: %v", err)
				}
			})
			id, err := maker.Make()
			if err != nil {
				t.Fatalf("the refusal's Make: %v", err)
			}
			if err := set.Take(id); err != nil {
				t.Fatalf("the refusal's Take: %v", err)
			}
			ref, err := cache.Acquire(id)
			if err != nil {
				t.Fatalf("the refusal's Acquire: %v", err)
			}
			defer ref.Release()
			buf := format.AppendSegmentHeader(nil, format.SegmentHeader{Identity: streamIdentity, ID: id})
			if _, err := ref.WriteAt(buf, 0); err != nil {
				t.Fatalf("the refusal's header write: %v", err)
			}

			// The refusal is a whole answer: no seam call made and
			// no ledger change, the counters a writer that had gone
			// ahead would have moved.
			calls := sealCountersOf(e, format.SegmentName(id))
			counters := set.Counters()
			if _, err := segment.WriteSeal(set, ref, streamIdentity, id, tc.marker, tc.entries(), false); !errors.Is(err, segment.ErrBadCatalog) {
				t.Fatalf("the refused WriteSeal = %v, want ErrBadCatalog", err)
			}
			if state, err := set.State(id); err != nil || state != segment.Active {
				t.Fatalf("the refused WriteSeal left the segment %v, %v, want Active", state, err)
			}
			if now := sealCountersOf(e, format.SegmentName(id)); now != calls {
				t.Fatalf("the refused WriteSeal moved the seam: %+v, want %+v", now, calls)
			}
			if now := set.Counters(); now != counters {
				t.Fatalf("the refused WriteSeal moved the ledger: %+v, want %+v", now, counters)
			}
		})
	}

	// The writer's own R32 guard: a segment the ledger holds Sealed, and
	// one it holds Ended, take no seal — the footer is never appended
	// twice and an ended segment is never sealed — with no seam call made
	// and no ledger change either.
	for _, tc := range []struct {
		name  string
		state segment.State
	}{
		{"a sealed segment", segment.Sealed},
		{"an ended segment", segment.Ended},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := segment.NewSet(streamCap, nil)
			budget := core.NewDescriptorBudget(8)
			cache := segment.NewDescriptors(e.seam.FS(), format.SegmentName, budget)
			maker := segment.NewMaker(e.seam.FS(), set, streamMax, 0, nil)
			t.Cleanup(func() {
				if err := cache.Shutdown(); err != nil {
					t.Errorf("the refusal's cache Shutdown: %v", err)
				}
			})
			id, err := maker.Make()
			if err != nil {
				t.Fatalf("the refusal's Make: %v", err)
			}
			if err := set.Take(id); err != nil {
				t.Fatalf("the refusal's Take: %v", err)
			}
			ref, err := cache.Acquire(id)
			if err != nil {
				t.Fatalf("the refusal's Acquire: %v", err)
			}
			defer ref.Release()
			buf := format.AppendSegmentHeader(nil, format.SegmentHeader{Identity: streamIdentity, ID: id})
			if _, err := ref.WriteAt(buf, 0); err != nil {
				t.Fatalf("the refusal's header write: %v", err)
			}
			switch tc.state {
			case segment.Sealed:
				if err := set.Seal(id, true); err != nil {
					t.Fatalf("sealing the segment for the refusal: %v", err)
				}
			case segment.Ended:
				if err := set.End(id); err != nil {
					t.Fatalf("ending the segment for the refusal: %v", err)
				}
			}

			calls := sealCountersOf(e, format.SegmentName(id))
			counters := set.Counters()
			if _, err := segment.WriteSeal(set, ref, streamIdentity, id, marker, entries, false); !errors.Is(err, segment.ErrWrongState) {
				t.Fatalf("the WriteSeal of a %s = %v, want the ledger's ErrWrongState", tc.name, err)
			}
			if now := sealCountersOf(e, format.SegmentName(id)); now != calls {
				t.Fatalf("the refused WriteSeal moved the seam: %+v, want %+v", now, calls)
			}
			if now := set.Counters(); now != counters {
				t.Fatalf("the refused WriteSeal moved the ledger: %+v, want %+v", now, counters)
			}
		})
	}

	// The segment cap the writer serves is bounded by the record header's
	// own 32-bit offset column, so a Set whose cap is above 4 GiB is a
	// misconfigured one the writer must still police: the entries pass
	// every other check — they ascent, their last record ends at the
	// marker, the marker, the footer and the trailer fit the cap — and
	// only the column's bound refuses the record offset it cannot carry.
	// The refusal makes no I/O, so no 4 GiB file is ever built: the
	// segment lives in the ledger alone, and the writer hands back its
	// answer before it could touch a reference.
	t.Run("a record offset above the 32-bit column", func(t *testing.T) {
		// The ledger alone: a Set whose cap is above 4 GiB is a
		// misconfigured one the writer must still police, and the
		// refusal it answers makes no I/O, so no 4 GiB file is ever
		// built. The reference is a real one over a small file of its
		// own, for the refusal runs before the writer could touch it
		// and the test asserts exactly that.
		set := segment.NewSet(1<<34, nil)
		if err := set.AddSpare(1, streamMax); err != nil {
			t.Fatalf("the spare for the oversized-cap refusal: %v", err)
		}
		if err := set.Take(1); err != nil {
			t.Fatalf("the take of the oversized-cap refusal: %v", err)
		}

		// One record whose offset sits above MaxUint32 and whose end
		// names the marker, so only the column's bound refuses it. The
		// offset is the first 8-aligned one past the column's top —
		// MaxUint32 itself is off the grid — and the record's end at
		// the marker passes the end check the grid and the column
		// alone would leave standing.
		reclen := format.RecordHeader{Kind: format.KindWrite, Extent: core.Extent{Len: 64}}.Size()
		above := (int64(math.MaxUint32) + 8) &^ 7
		cross := []format.CatalogEntry{{
			File:         streamFileID(1),
			Extent:       core.Extent{Off: 0, Len: 64},
			RecordOffset: above,
			Seq:          1,
			Version:      core.Version{Counter: 1},
			Kind:         format.KindWrite,
			Tag:          7,
		}}

		small := segment.NewSet(streamCap, nil)
		smallMaker := segment.NewMaker(e.seam.FS(), small, streamMax, 0, nil)
		smallID, err := smallMaker.Make()
		if err != nil {
			t.Fatalf("the small file for the oversized-cap refusal: %v", err)
		}
		budget := core.NewDescriptorBudget(2)
		smallCache := segment.NewDescriptors(e.seam.FS(), format.SegmentName, budget)
		t.Cleanup(func() {
			if err := smallCache.Shutdown(); err != nil {
				t.Errorf("the refusal's cache Shutdown: %v", err)
			}
		})
		ref, err := smallCache.Acquire(smallID)
		if err != nil {
			t.Fatalf("the refusal's reference: %v", err)
		}
		defer ref.Release()

		calls := sealCountersOf(e, format.SegmentName(smallID))
		counters := set.Counters()
		if _, err := segment.WriteSeal(set, ref, streamIdentity, 1, above+reclen, cross, false); !errors.Is(err, segment.ErrBadCatalog) {
			t.Fatalf("the WriteSeal over the 32-bit column = %v, want ErrBadCatalog: the record header cannot carry the offset %d", err, above)
		}
		if state, err := set.State(1); err != nil || state != segment.Active {
			t.Fatalf("the refused WriteSeal left the segment %v, %v, want Active", state, err)
		}
		if now := set.Counters(); now != counters {
			t.Fatalf("the refused WriteSeal moved the ledger: %+v, want %+v", now, counters)
		}
		if now := sealCountersOf(e, format.SegmentName(smallID)); now != calls {
			t.Fatalf("the refused WriteSeal moved the seam: %+v, want %+v", now, calls)
		}
	})

	// The acceptance, on a second segment the refusals' own shape built:
	// entries as data, scanned from the records of a segment a fresh
	// stream appended, seal the segment whose header names it.
	t.Run("entries as data", func(t *testing.T) {
		// A fresh stream of its own over the same directory appends
		// three records; the entries a scan of its segment rebuilds are
		// what WriteSeal takes, through a reference the cache hands
		// out, and the footer verifies at the truncated size.
		set := segment.NewSet(streamCap, nil)
		budget := core.NewDescriptorBudget(8)
		cache := segment.NewDescriptors(e.seam.FS(), format.SegmentName, budget)
		stream := segment.NewStream(1, streamIdentity, set, segment.NewMaker(e.seam.FS(), set, streamMax, 0, nil), cache)
		defer func() { _ = stream.Close() }()
		defer func() {
			if err := cache.Shutdown(); err != nil {
				t.Errorf("the acceptance's cache Shutdown: %v", err)
			}
		}()

		var placed2 []segment.Placed
		for i := range 3 {
			payload := streamTestBody(64, byte(0x40+i))
			h, body := streamWriteHeader(streamFileID(i+1), int64(i)*64, payload, 7, core.Version{Counter: uint64(i + 1)})
			p, err := stream.Append(h, body)
			if err != nil {
				t.Fatalf("the acceptance's append %d: %v", i, err)
			}
			placed2 = append(placed2, p)
		}
		id := placed2[0].Loc.Segment

		// Recovery's shape: the entries rebuilt from the records
		// themselves, in the order the scan read them.
		scanned := sealEntriesViaScan(t, cache, id)
		et := make([]format.CatalogEntry, 0, len(placed2))
		for _, p := range placed2 {
			et = append(et, scanned[p.Loc.Off])
		}

		// The stream is done with the segment: WriteSeal seals it
		// from a second reference, with only the scanned facts.
		if err := stream.Close(); err != nil {
			t.Fatalf("the acceptance's stream Close: %v", err)
		}
		ref, err := cache.Acquire(id)
		if err != nil {
			t.Fatalf("the acceptance's Acquire: %v", err)
		}
		defer ref.Release()
		mk := pEnd(placed2[len(placed2)-1])
		res, err := segment.WriteSeal(set, ref, streamIdentity, id, mk, et, true)
		if err != nil {
			t.Fatalf("the WriteSeal over scanned entries: %v", err)
		}
		want := footerEndSize(mk, len(et), false)
		if res.Size != want {
			t.Fatalf("the scanned seal is accounted at %d, want the trailer's end %d", res.Size, want)
		}

		// The footer verifies and lists every scanned record, and the
		// ledger carries the segment at its truncated size.
		rd := sealReaderOf(t, streamEnv{cache: cache}, id)
		defer func() { _ = rd.Close() }()
		footer, err := format.ReadFooter(rd, want, streamIdentity, id)
		if err != nil {
			t.Fatalf("the scanned footer does not verify: %v", err)
		}
		if len(footer.Entries) != len(et) {
			t.Fatalf("the scanned footer holds %d entries, want the %d scanned", len(footer.Entries), len(et))
		}
		if err := set.Check(); err != nil {
			t.Fatalf("the scanned seal's ledger: %v", err)
		}
	})
}

// sealEntriesViaScan rebuilds the catalog entries of a segment from its
// records, the way recovery does, keyed by the record's offset.
func sealEntriesViaScan(t *testing.T, cache *segment.Descriptors, id core.SegmentID) map[int64]format.CatalogEntry {
	t.Helper()

	ref, err := cache.Acquire(id)
	if err != nil {
		t.Fatalf("the scan's Acquire: %v", err)
	}
	defer ref.Release()
	entries := map[int64]format.CatalogEntry{}
	off := int64(format.SegmentHeaderSize)
	for {
		raw := make([]byte, format.RecordHeaderSize)
		if _, err := ref.ReadAt(raw, off); err != nil {
			t.Fatalf("the scan's read at %d: %v", off, err)
		}
		h, err := format.DecodeRecordHeader(raw, streamIdentity, id, off)
		if err != nil {
			break
		}
		entries[off] = format.CatalogEntry{
			File:         h.File,
			Extent:       h.Extent,
			RecordOffset: off,
			Seq:          h.Seq,
			Version:      h.Version,
			Kind:         h.Kind,
			Tag:          h.Tag,
		}
		off += h.Size()
	}
	return entries
}

// footerEndSize names where an early seal's file ends: the marker, the
// entries and the trailer.
func footerEndSize(marker int64, n int, full bool) int64 {
	size := marker + format.SealMarkerSize + int64(n)*format.CatalogEntrySize + format.TrailerSize
	if full {
		return streamCap
	}
	return size
}

// TestSealTheStreamEdges checks the edge refusals: a Seal on a stream that
// never appended answers ErrNoSegment and makes no seam call; a segment with
// no records seals through WriteSeal with a zero-entry footer that verifies;
// and a Sync after a full seal makes no fsync and answers nil.
func TestSealTheStreamEdges(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("a stream that never appended", func(t *testing.T) {
		e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
		defer func() { _ = e.s.Close() }()

		if _, err := e.s.Seal(true); !errors.Is(err, segment.ErrNoSegment) {
			t.Fatalf("the Seal of a segment-less stream = %v, want ErrNoSegment", err)
		}
		if got := e.seam.Calls(journaltest.OpSync); got != 0 {
			t.Fatalf("the refusal made %d syncs, want none", got)
		}
		if got := e.seam.Calls(journaltest.OpWrite); got != 0 {
			t.Fatalf("the refusal made %d writes, want none", got)
		}
	})

	t.Run("a segment with no records", func(t *testing.T) {
		// A stream only takes a segment on an Append, so the empty
		// footer goes through WriteSeal on a taken, header-written
		// segment — the shape recovery seals a scanned tail with.
		e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
		defer func() { _ = e.s.Close() }()

		// A second set with its own Maker over the same directory, so
		// the spare the Make draws is one this set accounts for.
		set := segment.NewSet(streamCap, nil)
		maker := segment.NewMaker(e.seam.FS(), set, streamMax, 0, nil)
		id, err := maker.Make()
		if err != nil {
			t.Fatalf("the empty seal's Make: %v", err)
		}
		if err := set.Take(id); err != nil {
			t.Fatalf("the empty seal's Take: %v", err)
		}
		ref, err := e.cache.Acquire(id)
		if err != nil {
			t.Fatalf("the empty seal's Acquire: %v", err)
		}
		defer ref.Release()
		buf := format.AppendSegmentHeader(nil, format.SegmentHeader{Identity: streamIdentity, ID: id})
		if _, err := ref.WriteAt(buf, 0); err != nil {
			t.Fatalf("the empty seal's header write: %v", err)
		}

		marker := int64(format.SegmentHeaderSize)
		res, err := segment.WriteSeal(set, ref, streamIdentity, id, marker, nil, true)
		if err != nil {
			t.Fatalf("the empty WriteSeal: %v", err)
		}
		if want := footerEndSize(marker, 0, false); res.Size != want {
			t.Fatalf("the empty early seal is accounted at %d, want the trailer's end %d", res.Size, want)
		}
		size := sealFileSize(t, e, id)
		if size != res.Size {
			t.Fatalf("the empty early seal's file is %d bytes, want %d", size, res.Size)
		}
		footerRd := sealReaderOf(t, e, id)
		defer func() { _ = footerRd.Close() }()
		got, err := format.ReadFooter(footerRd, size, streamIdentity, id)
		if err != nil {
			t.Fatalf("the empty footer does not verify: %v", err)
		}
		if len(got.Entries) != 0 {
			t.Fatalf("the empty footer holds %d entries, want none", len(got.Entries))
		}
		if err := set.Check(); err != nil {
			t.Fatalf("the empty seal's ledger: %v", err)
		}
	})

	t.Run("Sync after a full seal", func(t *testing.T) {
		e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
		defer func() { _ = e.s.Close() }()

		sealAppended(t, e)
		if _, err := e.s.Seal(false); err != nil {
			t.Fatalf("the full Seal: %v", err)
		}
		syncs := e.seam.CallsFor(journaltest.OpSync, streamSegName(1))
		if err := e.s.Sync(); err != nil {
			t.Fatalf("the Sync after the seal: %v", err)
		}
		if got := e.seam.CallsFor(journaltest.OpSync, streamSegName(1)); got != syncs {
			t.Fatalf("the Sync after the seal made an fsync: %d syncs, want still %d", got, syncs)
		}
	})
}

// TestSealNoSeamCallUnderTheAppendLock checks R12 through a seal: no
// storage call of the appends, the sync or the seal runs while the stream's
// append lock is held, so the leaf-lock contract of the segment append domain
// survives the seal's new lock sections. It catches a seal step moved under
// appendMu.
func TestSealNoSeamCallUnderTheAppendLock(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := streamOver(t, streamCap, func(streamEnv) {})
	ok := true
	fault := &streamAppendHeldFault{s: e.s, ok: &ok}
	e.seam.SetFault(fault)
	t.Cleanup(func() {
		if err := e.cache.Shutdown(); err != nil {
			t.Errorf("the cache's Shutdown under the lock fault = %v", err)
		}
	})
	defer func() { _ = e.s.Close() }()

	for i := range 3 {
		payload := streamTestBody(64, byte(0x30+i))
		h, body := streamWriteHeader(streamFileID(i+1), int64(i)*64, payload, 7, core.Version{Counter: uint64(i + 1)})
		streamAppendWrite(t, e, h, body)
	}
	if _, err := e.s.Seal(true); err != nil {
		t.Fatalf("the early Seal under the lock fault: %v", err)
	}

	if !ok {
		t.Fatal("a seam call was made while the stream's append lock was held: the seal or an append held the segment append domain across I/O")
	}
}

// TestSealCloseWaitsForAnInFlightSeal checks the in-flight operation Close
// must wait out: the seal's step-2 sync parked at a seam hold keeps Close
// from returning — releasing the active segment's ref first would hand the
// descriptor the cache or a reclaim could close under the seal's write — and
// once released the seal completes and the cache's Shutdown runs clean.
func TestSealCloseWaitsForAnInFlightSeal(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {})
	e.cacheBuilt(t)
	defer func() { _ = e.s.Close() }() // before the cache's Shutdown: the stream holds a ref

	sealAppended(t, e)

	// The take gave the segment a header write, the appends five records,
	// and the seal's first sync runs before its step-2 writes, so the
	// second hold catches the seal's step-2 sync: arm the first sync's
	// hold, let it pass, then hold the next.
	h := e.seam.HoldFirst(t, journaltest.OpSync, seg)
	sealed := make(chan sealRace, 1)
	go func() {
		r, err := e.s.Seal(true)
		sealed <- sealRace{r: r, err: err}
	}()
	<-h.Reached()
	h.Release()
	h2 := e.seam.HoldFirst(t, journaltest.OpSync, seg)
	<-h2.Reached()

	closed := make(chan error, 1)
	go func() { closed <- e.s.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v while a seal was still in flight, want it to wait the seal out", err)
	case <-time.After(100 * time.Millisecond):
	}

	// The seal completes, and only then does Close return and the cache's
	// Shutdown run clean behind it.
	h2.Release()
	select {
	case race := <-sealed:
		if race.err != nil {
			t.Fatalf("the parked Seal failed: %v", race.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parked Seal did not return after the hold was released")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close after its in-flight seal = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after its in-flight seal finished")
	}
	if err := e.cache.Shutdown(); err != nil {
		t.Fatalf("the cache's Shutdown after the stream's Close = %v, want nil: the wait released the ref under the seal", err)
	}
	if err := e.set.Check(); err != nil {
		t.Fatalf("the ledger after the raced close: %v", err)
	}
}

// TestSealRefusesAnAppendWhileItRuns checks the sealing half of R85's gate:
// once a seal has taken the segment — while it waits the drain of the
// appends reserved before it, and while its own steps run — the segment is
// closed to new reservations, so an Append arriving now answers
// ErrSegmentFull, reserves nothing and writes nothing. It catches a seal
// whose marker an in-flight append can overwrite, or an acknowledged record
// the footer never lists.
func TestSealRefusesAnAppendWhileItRuns(t *testing.T) {
	// While the seal park itself inside WriteSeal's first sync: the
	// marker is chosen and the writes are under way.
	t.Run("inside its own first sync", func(t *testing.T) {
		prof.Watch(t, prof.Options{})

		seg := streamSegName(1)
		e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
		defer func() { _ = e.s.Close() }()

		placed := sealAppended(t, e)
		wantMarker := pEnd(placed[len(placed)-1])

		h := e.seam.HoldFirst(t, journaltest.OpSync, seg)
		sealed := make(chan sealRace, 1)
		go func() {
			r, err := e.s.Seal(false)
			sealed <- sealRace{r: r, err: err}
		}()
		<-h.Reached()

		payload := streamTestBody(64, 0x77)
		hh, body := streamWriteHeader(streamFileID(9), 512, payload, 7, core.Version{Counter: 99})
		writes := e.seam.CallsFor(journaltest.OpWrite, seg)
		if _, err := e.s.Append(hh, body); !errors.Is(err, segment.ErrSegmentFull) {
			t.Fatalf("the append while the seal ran in its own steps = %v, want ErrSegmentFull: the segment is closed to reservations", err)
		}
		if got := e.seam.CallsFor(journaltest.OpWrite, seg); got != writes {
			t.Fatalf("the refused append made %d writes, want none: nothing is written past a seal", got-writes)
		}

		h.Release()
		race := <-sealed
		if race.err != nil {
			t.Fatalf("the Seal: %v", race.err)
		}
		if race.r.Marker != wantMarker {
			t.Fatalf("the seal's marker is at %d, want the last pre-seal record's end %d: the refused append moved the position", race.r.Marker, wantMarker)
		}
		size := sealFileSize(t, e, 1)
		rd := sealReaderOf(t, e, 1)
		defer func() { _ = rd.Close() }()
		footer, err := format.ReadFooter(rd, size, streamIdentity, 1)
		if err != nil {
			t.Fatalf("the footer does not verify: %v", err)
		}
		if len(footer.Entries) != len(placed) {
			t.Fatalf("the footer holds %d entries, want the %d records appended before the seal, nobody after it", len(footer.Entries), len(placed))
		}
	})

	// While the seal waits the drain of a parked append: the segment is
	// closed the moment the seal takes it, so a fresh Append is refused
	// even though the seal has not chosen its marker yet.
	t.Run("while it waits the drain", func(t *testing.T) {
		prof.Watch(t, prof.Options{})

		seg := streamSegName(1)
		e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
		defer func() { _ = e.s.Close() }()

		warm := streamTestBody(64, 0x5a)
		wh, wbody := streamWriteHeader(streamFileID(1), 0, warm, 7, core.Version{Counter: 1})
		warmPlaced := streamAppendWrite(t, e, wh, wbody)

		// Park a record write the seal must wait out, then start the
		// seal: it takes the segment and parks on the drain.
		h := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
		heldDone := make(chan segment.Placed, 1)
		go func() {
			payload := streamTestBody(64, 0x3c)
			hh, body := streamWriteHeader(streamFileID(2), 64, payload, 7, core.Version{Counter: 2})
			p, err := e.s.Append(hh, body)
			if err != nil {
				t.Errorf("the held append: %v", err)
			}
			heldDone <- p
		}()
		<-h.Reached()
		sealed := make(chan sealRace, 1)
		go func() {
			r, err := e.s.Seal(false)
			sealed <- sealRace{r: r, err: err}
		}()
		time.Sleep(50 * time.Millisecond) // the seal has taken the segment and parked on the drain

		payload := streamTestBody(64, 0x77)
		hh, body := streamWriteHeader(streamFileID(9), 128, payload, 7, core.Version{Counter: 99})
		if _, err := e.s.Append(hh, body); !errors.Is(err, segment.ErrSegmentFull) {
			t.Fatalf("the append while the seal waited the drain = %v, want ErrSegmentFull: the segment is closed from the seal's take", err)
		}

		// The seal then places its marker at the held record's end and
		// lists it — the one that was already reserved, never the one
		// refused above.
		h.Release()
		held := <-heldDone
		race := <-sealed
		if race.err != nil {
			t.Fatalf("the Seal: %v", race.err)
		}
		if want := held.Loc.Off + held.Loc.Len; race.r.Marker != want {
			t.Fatalf("the seal's marker is at %d, want the held record's end %d", race.r.Marker, want)
		}
		size := sealFileSize(t, e, 1)
		rd := sealReaderOf(t, e, 1)
		defer func() { _ = rd.Close() }()
		footer, err := format.ReadFooter(rd, size, streamIdentity, 1)
		if err != nil {
			t.Fatalf("the footer does not verify: %v", err)
		}
		if len(footer.Entries) != 2 {
			t.Fatalf("the footer holds %d entries, want the warm and the held records alone", len(footer.Entries))
		}
		_ = warmPlaced
	})
}

// TestSealADrainedSyncFailureEndsTheSegment checks the drain's other half:
// a Stream.Sync that started before the seal is waited out, so its failure
// reaches the seal's ended-check and the segment is never sealed over a
// failed sync window. It catches a seal whose own step-1 sync can succeed
// over pages a concurrent stream sync failed to write (RFC 1 §5.3).
func TestSealADrainedSyncFailureEndsTheSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := sealCrashBuilt(t, streamOver(t, streamCap, func(streamEnv) {}))
	defer func() { _ = e.s.Close() }()

	sealAppended(t, e)

	// Park a stream Sync inside its fsync, then start the seal: with the
	// sync in flight, the seal parks in its drain, never reaching its own
	// steps — the window the drain must close. The one-shot EIO fails the
	// held Sync; the drain then empties, the seal's ended-check sees the
	// failure, and the segment is ended before anything is written.
	h := e.seam.HoldFirst(t, journaltest.OpSync, seg)
	syncErr := make(chan error, 1)
	go func() { syncErr <- e.s.Sync() }()
	<-h.Reached()

	sealed := make(chan sealRace, 1)
	go func() {
		r, err := e.s.Seal(false)
		sealed <- sealRace{r: r, err: err}
	}()
	time.Sleep(50 * time.Millisecond) // the seal is parked in its drain
	writes := e.seam.CallsFor(journaltest.OpWrite, seg)

	e.seam.SetFault(&sealNthFault{seg: seg, op: journaltest.OpSync, n: 1, err: syscall.EIO})
	h.Release()
	if err := <-syncErr; !errors.Is(err, syscall.EIO) {
		t.Fatalf("the parked Sync = %v, want the fault's EIO", err)
	}

	race := <-sealed
	if race.err == nil || !errors.Is(race.err, segment.ErrSegmentEnded) {
		t.Fatalf("the Seal over a failed sync window = %+v, %v, want ErrSegmentEnded: the segment is ended, never sealed", race.r, race.err)
	}
	if got := e.seam.CallsFor(journaltest.OpWrite, seg); got != writes {
		t.Fatalf("the refused Seal made %d writes, want none: nothing is written over a failed sync window", got-writes)
	}
	if state, err := e.set.State(1); err != nil || state != segment.Ended {
		t.Fatalf("the segment's state after the drained failure is %v, %v, want Ended", state, err)
	}
	if c := e.set.Counters(); c.Sealed != 0 {
		t.Fatalf("the ledger counts %d sealed segments, want none", c.Sealed)
	}
	payload := streamTestBody(64, 0x77)
	hh, body := streamWriteHeader(streamFileID(9), 512, payload, 7, core.Version{Counter: 99})
	if _, err := e.s.Append(hh, body); !errors.Is(err, segment.ErrSegmentFull) {
		t.Fatalf("the append after the ended segment = %v, want ErrSegmentFull", err)
	}
	if err := e.s.Sync(); !errors.Is(err, segment.ErrSegmentEnded) {
		t.Fatalf("the Sync after the ended segment = %v, want ErrSegmentEnded", err)
	}
}

// TestSealASyncOnItParksUntilItCompletes checks what a Stream.Sync does
// when a seal is in flight: it may not race its own fsync against the
// seal's — the platform reports a writeback error once per file, so a
// concurrent sync could eat the failure the seal's check or its own steps
// needed — and when the seal completes, the records are durable by its
// first step, so the Sync answers nil without an fsync of its own.
func TestSealASyncOnItParksUntilItCompletes(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	sealAppended(t, e)

	// Park the seal inside its first sync, then start a Sync: it must not
	// reach the seam while the seal runs.
	h := e.seam.HoldFirst(t, journaltest.OpSync, seg)
	sealed := make(chan sealRace, 1)
	go func() {
		r, err := e.s.Seal(true)
		sealed <- sealRace{r: r, err: err}
	}()
	<-h.Reached()
	syncsAtPark := e.seam.CallsFor(journaltest.OpSync, seg)

	syncErr := make(chan error, 1)
	go func() { syncErr <- e.s.Sync() }()
	time.Sleep(50 * time.Millisecond)
	if got := e.seam.CallsFor(journaltest.OpSync, seg); got != syncsAtPark {
		t.Fatalf("a Sync racing the seal issued %d syncs of its own, want none: its fsync must not run beside the seal's", got-syncsAtPark)
	}

	// The seal completes — and with its two remaining syncs alone, the
	// whole early seal shows its three, no more — and the parked Sync
	// answers nil, for the records are durable by the seal's first step.
	h.Release()
	race := <-sealed
	if race.err != nil {
		t.Fatalf("the early Seal under the parked Sync: %v", race.err)
	}
	if err := <-syncErr; err != nil {
		t.Fatalf("the parked Sync after the seal = %v, want nil: the seal's first step made the records durable", err)
	}
	if got := e.seam.CallsFor(journaltest.OpSync, seg); got != syncsAtPark+2 {
		t.Fatalf("the whole early seal plus the parked Sync issued %d syncs past the held one, want the seal's two alone", got-syncsAtPark)
	}
}

// TestSealAFullSealThatFillsTheSegmentWritesOnce checks the one-write branch
// of the full seal: a segment whose records end exactly where the trailer
// must begin writes the marker, the footer and the trailer as a single
// write — the two ranges meet — and the footer still verifies. It catches a
// gap left between the footer and the trailer that the ordinary two-write
// path would paper over.
func TestSealAFullSealThatFillsTheSegmentWritesOnce(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// A cap of 3072 and ten records of a 113-byte payload fill the segment
	// exactly: each record takes 224 bytes (a 96-byte header, the 8-byte
	// modification time and the payload, padded to 8), so the last record
	// ends at 48 + 10*224 = 2296, and the seal of ten entries takes 32 +
	// 10*72 + 32 = 784 more, which is the cap exactly.
	const cap = int64(3072)
	const records = 10
	seg := streamSegName(1)
	e := streamOver(t, cap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	var placed []segment.Placed
	for i := range records {
		payload := streamTestBody(113, byte(i))
		h, body := streamWriteHeader(streamFileID(i+1), 0, payload, 7, core.Version{Counter: uint64(i + 1)})
		p, err := e.s.Append(h, body)
		if err != nil {
			t.Fatalf("the filling append %d: %v (the cap must hold the sealed whole)", i, err)
		}
		placed = append(placed, p)
	}
	wantEnd := format.SegmentHeaderSize + int64(records)*224
	if got := pEnd(placed[len(placed)-1]); got != wantEnd {
		t.Fatalf("the last record ends at %d, want the computed fill %d", got, wantEnd)
	}
	if room := cap - wantEnd; room != format.SealMarkerSize+int64(records)*format.CatalogEntrySize+format.TrailerSize {
		t.Fatalf("the fill leaves %d bytes of room, want the seal's %d: the trailer must begin where the footer ends", room, format.SealMarkerSize+records*format.CatalogEntrySize+format.TrailerSize)
	}

	syncs := e.seam.CallsFor(journaltest.OpSync, seg)
	writes := e.seam.CallsFor(journaltest.OpWrite, seg)
	if _, err := e.s.Seal(false); err != nil {
		t.Fatalf("the full Seal of a filled segment: %v", err)
	}
	if got := e.seam.CallsFor(journaltest.OpSync, seg) - syncs; got != 2 {
		t.Fatalf("the full seal issued %d syncs, want the two of its floor", got)
	}
	if got := e.seam.CallsFor(journaltest.OpWrite, seg) - writes; got != 1 {
		t.Fatalf("the full seal of a filled segment issued %d writes, want the one its contiguous ranges allow", got)
	}

	// The file keeps its cap and the footer verifies with every record.
	size := sealFileSize(t, e, 1)
	if size != cap {
		t.Fatalf("the filled segment's file is %d bytes, want the cap %d", size, cap)
	}
	rd := sealReaderOf(t, e, 1)
	defer func() { _ = rd.Close() }()
	footer, err := format.ReadFooter(rd, size, streamIdentity, 1)
	if err != nil {
		t.Fatalf("the filled segment's footer does not verify: %v", err)
	}
	if len(footer.Entries) != len(placed) {
		t.Fatalf("the footer holds %d entries, want the %d records filling the segment", len(footer.Entries), len(placed))
	}
	entries := sealEntriesOf(t, e, placed)
	for i := range entries {
		if footer.Entries[i] != entries[i] {
			t.Fatalf("entry %d is %+v, want %+v", i, footer.Entries[i], entries[i])
		}
	}
}
