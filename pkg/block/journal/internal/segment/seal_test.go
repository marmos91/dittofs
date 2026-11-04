package segment_test

import (
	"bytes"
	"errors"
	"io/fs"
	"slices"
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
	defer f.Close()
	size, err := f.Size()
	if err != nil {
		t.Fatalf("the size of segment %d's file: %v", id, err)
	}
	return size
}

// sealRecorderFault is a recording fault: it logs every seam call on the
// segment's file, and can fail chosen ops the first times they arrive.
type sealRecorderFault struct {
	log   *[]string
	seg   string
	fails map[string][]error // op -> the errors to fail in order
}

func (f *sealRecorderFault) At(op string, key []byte) (dl time.Duration, tear int, err error) {
	if string(key) == f.seg {
		*f.log = append(*f.log, op)
		if errs := f.fails[op]; len(errs) > 0 {
			f.fails[op] = errs[1:]
			return 0, 0, errs[0]
		}
	}
	return 0, 0, nil
}

// fail arms op to fail with errs, in the order given, once each.
func (f *sealRecorderFault) fail(op string, errs ...error) {
	if f.fails == nil {
		f.fails = map[string][]error{}
	}
	f.fails[op] = errs
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
	defer r.Close()
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
	defer r.Close()
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
// and passes everything else: the shape the failure injections need, since a
// schedule rule counts from the first call of the op, not from the seal.
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
