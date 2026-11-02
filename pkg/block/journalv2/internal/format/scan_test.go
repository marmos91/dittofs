package format

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// This file tests the scan over segments built by hand: records planted at
// known slots, their bytes edited with the checksum helpers so only the
// edited rule can refuse, and the outcomes asserted on the Scan's own
// fields. The scanner classifies by bytes, so the tests build the bytes the
// classification rules spell: zeros, junk, a record's own bytes, a marker's
// own — each holding exactly the evidence its rule names.
//
// plantedHeadersEqual compares a planted and an attached header: the
// planted one carries no Offset or PayloadCRC — AppendRecord fills them —
// so the comparison takes the attached one's own.
func plantedHeadersEqual(planted, attached RecordHeader) bool {
	want := planted
	want.Offset = attached.Offset
	want.PayloadCRC = attached.PayloadCRC
	return want == attached
}

// scanTestRecords returns one record of five kinds: a write and a fill with
// small payloads, a loss, a clone target, and the header-only unmark. Every
// field the decoder returns is patterned, as the golden tests' values are.
func scanTestRecords() []scanRecord {
	var recs []scanRecord
	for _, k := range []Kind{KindWrite, KindFill, KindLoss, KindCloneTarget, KindUnmark} {
		h, body := scanKindBody(k)
		recs = append(recs, scanRecord{h: h, body: body})
	}
	return recs
}

// scanKindBody is the planted header and body of one kind: every field the
// decoder hands back carries the golden tests' patterns, and the fields
// the kinds table gives as 0 are zero, as the table spells them.
func scanKindBody(kind Kind) (RecordHeader, RecordBody) {
	h := RecordHeader{
		Kind:          kind,
		Segment:       testSegment,
		File:          testRecordFile,
		Seq:           testRecordSeq,
		Version:       testRecordVersion,
		Tag:           testRecordTag,
		SyncedThrough: testRecordSyncedThrough,
		Extent:        core.Extent{Off: testRecordFileOffset, Len: testRecordLength},
	}
	var body RecordBody
	switch kind {
	case KindWrite:
		h.Extent = core.Extent{Off: testRecordFileOffset, Len: 13}
		body = RecordBody{MTime: testWriteMTime, Payload: scanPayload(13)}
	case KindFill:
		h.Extent = core.Extent{Off: testRecordFileOffset, Len: 21}
		body = RecordBody{Payload: scanPayload(21)}
	case KindLoss:
		body = RecordBody{Loss: Loss{Dropped: testLoss.Dropped, Synced: true, Reason: LossStale}}
	case KindCloneTarget:
		body = RecordBody{Clone: CloneSource{Share: testClone.Share, File: testClone.File, Off: testClone.Off, AsOf: testClone.AsOf}}
	case KindHold, KindStamp:
		h.Extent = core.Extent{}
		body = RecordBody{Cut: testCut}
	}
	return h, body
}

// TestScanAttachesEveryKindAndSeals is the clean walk: every kind's record at
// its known slot attaches with its full header — its own offset and its
// synced-through offset included — its body decoded with the payload
// dropped, no ranges, and a verifying seal marker past the last record ends
// the scan at its own offset, the bytes after it never read.
func TestScanAttachesEveryKindAndSeals(t *testing.T) {
	recs := scanTestRecords()
	seg, offs := scanSegment(t, testIdentity, recs...)
	seal := scanSeal(t, &seg, testIdentity, testSegment)
	seg = append(seg, make([]byte, 64)...) // footer bytes the scan must never read

	got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
	if err != nil {
		t.Fatalf("ScanSegment = %v, want no error", err)
	}
	if len(got.Records) != len(recs) {
		t.Fatalf("attached %d records, want %d", len(got.Records), len(recs))
	}
	for i, r := range got.Records {
		want := recs[i]
		if r.Header.Offset != offs[i] {
			t.Errorf("record %d attached at %d, want its planted slot %d", i, r.Header.Offset, offs[i])
			continue
		}
		if !plantedHeadersEqual(want.h, r.Header) {
			t.Errorf("record %d header = %+v, want the planted %+v", i, r.Header, want.h)
		}
		wantBody := want.body
		wantBody.Payload = nil
		if !recordBodiesEqual(r.Body, wantBody) {
			t.Errorf("record %d body = %+v, want %+v with the payload dropped", i, r.Body, wantBody)
		}
		if r.Body.Payload != nil {
			t.Errorf("record %d kept a %d-byte payload, want nil", i, len(r.Body.Payload))
		}
	}
	if len(got.Ranges) != 0 {
		t.Errorf("a clean segment gave ranges %+v, want none", got.Ranges)
	}
	if !got.Sealed || got.End != seal {
		t.Errorf("sealed %v at %d, want sealed at the marker's %d", got.Sealed, got.End, seal)
	}
	if got.Resynced {
		t.Errorf("a clean scan resynced, want it did not")
	}
}

// TestScanCorruptedLengthResynchronises is the issue's own done-when: a
// record's length bytes edited — the checksums left stale, or redone so only
// the length's own rule can refuse — is passed over as bad bytes, the
// records around it attach, and the scan counts itself as one that
// resynchronised.
func TestScanCorruptedLengthResynchronises(t *testing.T) {
	for _, c := range []struct {
		name  string
		reset func(rec []byte)
	}{
		{"length corrupted, checksums stale", func(rec []byte) {
			binary.LittleEndian.PutUint64(rec[40:48], 4<<20)
		}},
		{"length corrupted, header rechecksummed", func(rec []byte) {
			binary.LittleEndian.PutUint64(rec[40:48], 4<<20)
			rechecksumRecord(rec, testIdentity)
		}},
	} {
		recs := scanTestRecords()
		seg, offs := scanSegment(t, testIdentity, recs...)
		size := int64(len(seg))
		seg = append(seg, make([]byte, 16)...) // a tail the planted records do not reach

		// The second record's whole bytes, at its own slot: the corruption
		// edits its length, whatever its checksums say about it.
		c.reset(seg[offs[1]:offs[2]])

		got, err := ScanSegment(bytes.NewReader(seg), size, testIdentity, testSegment)
		if err != nil {
			t.Fatalf("%s: ScanSegment = %v, want no error: a record that does not verify is walked over, never returned", c.name, err)
		}
		if len(got.Records) != 4 {
			t.Fatalf("%s: attached %d records, want the 4 around the bad one", c.name, len(got.Records))
		}
		for i, idx := range []int{0, 2, 3, 4} {
			if got.Records[i].Header.Offset != offs[idx] {
				t.Errorf("%s: record %d attached at %d, want planted slot %d", c.name, i, got.Records[i].Header.Offset, offs[idx])
			}
		}
		want := ScanRange{Off: offs[1], End: offs[2], Kind: RangeBad}
		if len(got.Ranges) != 1 || got.Ranges[0] != want {
			t.Errorf("%s: ranges = %+v, want the one RangeBad %+v", c.name, got.Ranges, want)
		}
		if !got.Resynced {
			t.Errorf("%s: want the scan to count itself resynchronised", c.name)
		}
		if got.End != size {
			t.Errorf("%s: end %d, want the segment's size %d", c.name, got.End, size)
		}
	}
}

// TestScanTwoBadRecordsCountOneResync: two corrupted records in one segment
// each leave one RangeBad, and the scan's resync contribution stays one bool
// — the counter counts scans, not resyncs.
func TestScanTwoBadRecordsCountOneResync(t *testing.T) {
	recs := scanTestRecords()
	seg, offs := scanSegment(t, testIdentity, recs...)

	binary.LittleEndian.PutUint64(seg[offs[0]:offs[1]][40:48], 4<<20)
	binary.LittleEndian.PutUint64(seg[offs[2]:offs[3]][40:48], 4<<20)

	got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
	if err != nil {
		t.Fatalf("ScanSegment = %v, want no error", err)
	}
	if len(got.Records) != 3 {
		t.Fatalf("attached %d records, want the 3 around two bad slots", len(got.Records))
	}
	if len(got.Ranges) != 2 || got.Ranges[0] != (ScanRange{Off: offs[0], End: offs[1], Kind: RangeBad}) || got.Ranges[1] != (ScanRange{Off: offs[2], End: offs[3], Kind: RangeBad}) {
		t.Fatalf("ranges = %+v, want two RangeBad over the corrupted slots", got.Ranges)
	}
	if !got.Resynced {
		t.Errorf("want the scan to count itself resynchronised")
	}
	if got.End != int64(len(seg)) {
		t.Errorf("end %d, want the size %d", got.End, len(seg))
	}
}

// TestScanZeroedMiddleRecordStaysAZeroRange: a whole record zeroed in place
// is a zero slot — a write that failed and was never acknowledged — and the
// scan steps over it as zeros, attaches the records around it and never
// counts a resync: no bad bytes were passed to find a record.
func TestScanZeroedMiddleRecordStaysAZeroRange(t *testing.T) {
	recs := scanTestRecords()
	seg, offs := scanSegment(t, testIdentity, recs...)

	clear(seg[offs[1]:offs[2]])

	got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
	if err != nil {
		t.Fatalf("ScanSegment = %v, want no error", err)
	}
	if len(got.Records) != 4 {
		t.Fatalf("attached %d records, want the 4 around the zero slot", len(got.Records))
	}
	want := ScanRange{Off: offs[1], End: offs[2], Kind: RangeZero}
	if len(got.Ranges) != 1 || got.Ranges[0] != want {
		t.Fatalf("ranges = %+v, want the one zero range %+v", got.Ranges, want)
	}
	if got.Resynced {
		t.Errorf("a zero slot stepped over counts no resync")
	}
	if got.End != int64(len(seg)) {
		t.Errorf("end %d, want the size %d", got.End, len(seg))
	}
}

// TestScanBadLastRecordReachesTheSize: a bad record with nothing verifying
// after it leaves one bad range running to the segment's end: the search
// found no header anywhere past it, so the tail is the range and the scan
// ends at the cap.
func TestScanBadLastRecordReachesTheSize(t *testing.T) {
	recs := scanTestRecords()[:2]
	seg, offs := scanSegment(t, testIdentity, recs...)

	binary.LittleEndian.PutUint64(seg[offs[1]:][40:48], 4<<20)

	size := int64(len(seg))
	got, err := ScanSegment(bytes.NewReader(seg), size, testIdentity, testSegment)
	if err != nil {
		t.Fatalf("ScanSegment = %v, want no error", err)
	}
	if len(got.Records) != 1 {
		t.Fatalf("attached %d records, want the first alone", len(got.Records))
	}
	want := ScanRange{Off: offs[1], End: size, Kind: RangeBad}
	if len(got.Ranges) != 1 || got.Ranges[0] != want {
		t.Fatalf("ranges = %+v, want the one bad range %+v", got.Ranges, want)
	}
	if got.Resynced || got.Sealed {
		t.Errorf("a bad tail to the cap resynced %v sealed %v, want neither", got.Resynced, got.Sealed)
	}
	if got.End != size {
		t.Errorf("end %d, want the size %d", got.End, size)
	}
}

// TestScanTrailingZerosAreAZeroRange: records then zeros to the cap — the
// plain unwritten tail — reads one zero range to the size and ends there,
// not sealed: there is no marker to seal it.
func TestScanTrailingZerosAreAZeroRange(t *testing.T) {
	recs := scanTestRecords()[:2]
	seg, _ := scanSegment(t, testIdentity, recs...)
	tail := int64(len(seg))
	seg = append(seg, make([]byte, 32)...)
	size := int64(len(seg))

	got, err := ScanSegment(bytes.NewReader(seg), size, testIdentity, testSegment)
	if err != nil {
		t.Fatalf("ScanSegment = %v, want no error", err)
	}
	if len(got.Records) != 2 {
		t.Fatalf("attached %d records, want 2", len(got.Records))
	}
	want := ScanRange{Off: tail, End: size, Kind: RangeZero}
	if len(got.Ranges) != 1 || got.Ranges[0] != want {
		t.Fatalf("ranges = %+v, want the one zero range %+v", got.Ranges, want)
	}
	if got.Sealed || got.Resynced {
		t.Errorf("sealed %v resynced %v, want neither", got.Sealed, got.Resynced)
	}
	if got.End != size {
		t.Errorf("end %d, want the size %d", got.End, size)
	}
}

// TestScanZeroSlotJunkThenRecordIsOneBadRange: a slot of zeros — the first
// four bytes zero, whatever its next bytes hold — followed by junk, then a
// verifying record, is one RangeBad and never a zero range: the range's
// kind is decided by its bytes, and the scan resynchronised past them.
func TestScanZeroSlotJunkThenRecordIsOneBadRange(t *testing.T) {
	recs := scanTestRecords()[:2]
	seg, _ := scanSegment(t, testIdentity, recs...)
	badStart := int64(len(seg))
	seg = append(seg, make([]byte, 8)...)                // the zero slot
	seg = append(seg, bytes.Repeat([]byte{0xa5}, 16)...) // the junk
	seg, later := scanAppend(t, seg, testIdentity, recs[1])
	got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
	if err != nil {
		t.Fatalf("ScanSegment = %v, want no error", err)
	}
	if len(got.Ranges) != 1 {
		t.Fatalf("ranges = %+v, want one", got.Ranges)
	}
	r := got.Ranges[0]
	if r.Off != badStart || r.End != later || r.Kind != RangeBad {
		t.Fatalf("range = %+v, want one bad range [%d, %d)", r, badStart, later)
	}
	if len(got.Records) != 3 || got.Records[2].Header.Offset != later {
		t.Fatalf("records = %d long, last at %+v, want the record past the junk at %d", len(got.Records), got.Records, later)
	}
	if !got.Resynced {
		t.Errorf("want the scan to count itself resynchronised past bad bytes")
	}
	if got.End != int64(len(seg)) {
		t.Errorf("end %d, want the size %d", got.End, len(seg))
	}
}

// TestScanSealMarkerInPayloadDoesNotStopTheScan covers two ways a seal
// marker's bytes can appear where no marker is: a copy of this journal's
// own marker planted inside a write's payload — a client file holding a
// copy of a journal segment — and a marker written at a slot it does not
// name — its own offset wrong, for "a copy of one inside a payload never
// ends a scan" and a wrong-place one never either.
func TestScanSealMarkerInPayloadDoesNotStopTheScan(t *testing.T) {
	t.Run("a marker copied into a write's payload", func(t *testing.T) {
		// A write whose payload holds a copy of this journal's own marker
		// for a real slot: the marker's bytes name a place inside the
		// payload, so they hold the wrong place wherever they sit and the
		// write verifies whole with them in it.
		marker := AppendSealMarker(nil, testIdentity, SealMarker{Segment: testSegment, Offset: int64(testOffset%8*8) + 48})
		h, body := scanKindBody(KindWrite)
		h.Extent = core.Extent{Off: testRecordFileOffset, Len: 96}
		body = RecordBody{MTime: testWriteMTime, Payload: slices.Concat(marker, scanPayload(64))}

		recs := scanTestRecords()[:2]
		seg, offs := scanSegment(t, testIdentity, recs...)
		seg, third := scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})
		seg, fourth := scanAppend(t, seg, testIdentity, scanTestRecords()[1])

		got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		if err != nil {
			t.Fatalf("ScanSegment = %v, want no error", err)
		}
		if got.Sealed {
			t.Fatalf("the scan sealed at %d: a copy of a marker inside a payload never ends a scan", got.End)
		}
		wantOffs := []int64{offs[0], offs[1], third, fourth}
		if len(got.Records) != len(wantOffs) {
			t.Fatalf("attached %d records, want %d", len(got.Records), len(wantOffs))
		}
		for i, r := range got.Records {
			if r.Header.Offset != wantOffs[i] {
				t.Errorf("record %d attached at %d, want planted slot %d", i, r.Header.Offset, wantOffs[i])
			}
		}
		if len(got.Ranges) != 0 {
			t.Errorf("ranges = %+v, want none: every record verified", got.Ranges)
		}
		if got.End != int64(len(seg)) {
			t.Errorf("end %d, want the size %d", got.End, len(seg))
		}
	})

	t.Run("a marker for another offset at a slot", func(t *testing.T) {
		recs := scanTestRecords()[:2]
		seg, offs := scanSegment(t, testIdentity, recs...)
		size := int64(len(seg))

		// A marker that names the first record's slot, planted at the
		// second's: it verifies its checksum but holds the wrong place, so
		// the slot is bad bytes and the scan walks on.
		bad := AppendSealMarker(nil, testIdentity, SealMarker{Segment: testSegment, Offset: offs[0]})
		copy(seg[offs[1]:offs[1]+SealMarkerSize], bad)

		got, err := ScanSegment(bytes.NewReader(seg), size, testIdentity, testSegment)
		if err != nil {
			t.Fatalf("ScanSegment = %v, want no error", err)
		}
		if got.Sealed {
			t.Fatalf("the scan sealed at %d: a marker at the wrong place never ends a scan", got.End)
		}
		if len(got.Records) != 1 || got.Records[0].Header.Offset != offs[0] {
			t.Fatalf("records = %+v, want the one before the marker alone", got.Records)
		}
		want := ScanRange{Off: offs[1], End: size, Kind: RangeBad}
		if len(got.Ranges) != 1 || got.Ranges[0] != want {
			t.Fatalf("ranges = %+v, want the one bad range %+v", got.Ranges, want)
		}
	})
}

// TestScanJournalBytesInAPayloadAttachNothing: a write whose payload holds a
// copy of a segment of this journal — records at slots of its copy, a seal
// marker included — attaches none of the copy's interior, whether the write
// itself verifies (the walked record's own boundaries span the whole copy)
// or its header is damaged so the forward search reads through the copy.
// Every record inside names the offset it carried in the old segment, so
// none verifies at the place the copy holds it.
func TestScanJournalBytesInAPayloadAttachNothing(t *testing.T) {
	// The copy: two records and a seal marker, planted at the slots of
	// their own copy — its first record at 48 — and the outer write's
	// payload shifts them deeper into this segment, so none of their
	// stored own offsets names the place this segment holds them at.
	copySeg, _ := scanSegment(t, testIdentity, scanTestRecords()[:2]...)
	copySeal := scanSeal(t, &copySeg, testIdentity, testSegment)

	build := func() ([]byte, []int64, int64, int64) {
		h, body := scanKindBody(KindWrite)
		h.Extent = core.Extent{Off: testRecordFileOffset, Len: int64(len(copySeg))}
		body = RecordBody{MTime: testWriteMTime, Payload: copySeg}

		recs := scanTestRecords()[:1]
		seg, offs := scanSegment(t, testIdentity, recs...)
		seg, writeOff := scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})
		seg, after := scanAppend(t, seg, testIdentity, scanTestRecords()[1])
		return seg, offs, writeOff, after
	}

	t.Run("the write intact", func(t *testing.T) {
		seg, offs, writeOff, after := build()
		got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		if err != nil {
			t.Fatalf("ScanSegment = %v, want no error", err)
		}
		if len(got.Records) != 3 {
			t.Fatalf("attached %d records, want the 3 around the copy: %+v", len(got.Records), got.Records)
		}
		for i, want := range []int64{offs[0], writeOff, after} {
			if got.Records[i].Header.Offset != want {
				t.Errorf("record %d attached at %d, want planted slot %d", i, got.Records[i].Header.Offset, want)
			}
		}
		if len(got.Ranges) != 0 {
			t.Errorf("ranges = %+v, want none", got.Ranges)
		}
	})

	t.Run("the write's header damaged so the search runs through the copy", func(t *testing.T) {
		seg, offs, writeOff, after := build()
		// The write's length bytes corrupted, checksums stale: the walk
		// refuses the record and the forward search runs through the copy
		// looking for a header that verifies at its own place.
		binary.LittleEndian.PutUint64(seg[writeOff : writeOff+96][40:48], 4<<20)

		got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		if err != nil {
			t.Fatalf("ScanSegment = %v, want no error", err)
		}
		if got.Sealed {
			t.Fatalf("the scan sealed at %d: the copy's marker names its own copy's slot %d, never this segment's", got.End, copySeal)
		}
		for _, r := range got.Records {
			if r.Header.Offset == writeOff {
				t.Errorf("attached the damaged write at its own slot %d", writeOff)
			}
		}
		want := []int64{offs[0], after}
		if len(got.Records) != len(want) {
			t.Fatalf("attached %d records, want the %d around the copy: %+v", len(got.Records), len(want), got.Records)
		}
		for i, w := range want {
			if got.Records[i].Header.Offset != w {
				t.Errorf("record %d attached at %d, want planted slot %d", i, got.Records[i].Header.Offset, w)
			}
		}
		one := ScanRange{Off: writeOff, End: after, Kind: RangeBad}
		if len(got.Ranges) != 1 || got.Ranges[0] != one {
			t.Errorf("ranges = %+v, want the one bad range %+v", got.Ranges, one)
		}
		if !got.Resynced {
			t.Errorf("want the scan to count itself resynchronised")
		}
	})
}

// TestScanRecycledSegmentKeepsOnlyTheNewLife: a segment's earlier life,
// written under another segment id, lies under the new life's records —
// none of its records or its seal marker verifies here, so none is attached
// and none ends the scan; the old bytes past the new life's last record
// stay one bad range to the cap, which a scan over a fresh segment's zeros
// would read as the tail instead.
func TestScanRecycledSegmentKeepsOnlyTheNewLife(t *testing.T) {
	// The old life: two header-only records under the previous segment id,
	// a seal marker under it, planted from the first record slot as the
	// old segment itself was. Only the first record's slot at 48 and the
	// second's at 144 are used below; the marker's sits at 240.
	below := func(segment core.SegmentID) []scanRecord {
		var recs []scanRecord
		for range 2 {
			h, body := scanKindBody(KindUnmark)
			h.Segment = segment
			recs = append(recs, scanRecord{h: h, body: body})
		}
		return recs
	}
	oldSeg, _ := scanSegment(t, testIdentity, below(testSegment-1)...)
	oldSeal := scanSeal(t, &oldSeg, testIdentity, testSegment-1)

	// The new life's record, of the same header-only kind so its slot is
	// the same 96 bytes wide as the old life's first: overwriting the
	// start leaves the old second record and marker on the device.
	newLife := func(offset int64) scanRecord {
		h, body := scanKindBody(KindUnmark)
		h.Segment = testSegment
		h.Offset = offset
		return scanRecord{h: h, body: body}
	}

	t.Run("old bytes past the new record are one bad range", func(t *testing.T) {
		seg := slices.Concat(oldSeg[:48], AppendRecord(nil, testIdentity, newLife(48).h, RecordBody{}), oldSeg[144:])
		size := int64(len(seg))

		got, err := ScanSegment(bytes.NewReader(seg), size, testIdentity, testSegment)
		if err != nil {
			t.Fatalf("ScanSegment = %v, want no error", err)
		}
		if got.Sealed {
			t.Fatalf("the scan sealed at %d: the old life's marker at %d names the old id and never ends a new life's scan", got.End, oldSeal)
		}
		if len(got.Records) != 1 || got.Records[0].Header.Offset != 48 {
			t.Fatalf("records = %+v, want the new life's first alone", got.Records)
		}
		want := ScanRange{Off: 144, End: size, Kind: RangeBad}
		if len(got.Ranges) != 1 || got.Ranges[0] != want {
			t.Fatalf("ranges = %+v, want the old bytes past the new record one bad range %+v", got.Ranges, want)
		}
		if got.Resynced {
			t.Errorf("a range that found no record counts no resync")
		}
	})

	t.Run("a new record past old bytes is found and attached", func(t *testing.T) {
		// The new life's A at 48, the second record's slot keeping the old
		// life's second record — whose header names the old id — and the
		// new life's C at 240, over the old marker's slot. The forward
		// search reads the old bytes over and finds C.
		var seg []byte
		seg = append(seg, oldSeg[:48]...)
		a := newLife(48)
		seg = AppendRecord(seg, testIdentity, a.h, a.body)
		if int64(len(seg)) != 144 {
			t.Fatalf("record A ends at %d, want 144: the old second record's own slot", len(seg))
		}
		seg = append(seg, oldSeg[144:240]...)
		c := newLife(240)
		seg = AppendRecord(seg, testIdentity, c.h, c.body)
		size := int64(len(seg))

		got, err := ScanSegment(bytes.NewReader(seg), size, testIdentity, testSegment)
		if err != nil {
			t.Fatalf("ScanSegment = %v, want no error", err)
		}
		if len(got.Records) != 2 || got.Records[0].Header.Offset != 48 || got.Records[1].Header.Offset != 240 {
			t.Fatalf("records = %+v, want the new life's at 48 and 240", got.Records)
		}
		want := ScanRange{Off: 144, End: 240, Kind: RangeBad}
		if len(got.Ranges) != 1 || got.Ranges[0] != want {
			t.Fatalf("ranges = %+v, want the old second record one bad range %+v", got.Ranges, want)
		}
		if !got.Resynced {
			t.Errorf("want the scan to count itself resynchronised past the old bytes")
		}
		if got.End != size {
			t.Errorf("end %d, want the size %d", got.End, size)
		}
	})
}

// TestScanStopsAtAKindOrEpochItCannotRead: the walk and the forward search
// both stop at a header this format version cannot read over — an unknown
// kind, or a non-zero version epoch — returning the records attached before
// it, the error the decode made, and End at the slot itself, never stepping
// the record over by any length. A clone target's AsOf epoch stops the scan
// the same way, in VerifyRecordBody where the body's own bytes are read.
func TestScanStopsAtAKindOrEpochItCannotRead(t *testing.T) {
	cases := func() ([]byte, []int64) {
		return scanSegment(t, testIdentity, scanTestRecords()...)
	}

	t.Run("unknown kind on the walk", func(t *testing.T) {
		seg, offs := cases()
		for _, kind := range []byte{15, 128} {
			s := slices.Clone(seg) // a fresh copy each pass, the offsets unchanged
			s[offs[1]+4] = kind
			rechecksumRecord(s[offs[1]:offs[1]+96], testIdentity)

			got, err := ScanSegment(bytes.NewReader(s), int64(len(s)), testIdentity, testSegment)
			if !errors.Is(err, ErrUnknownKind) {
				t.Fatalf("kind %d: ScanSegment = %v, want ErrUnknownKind", kind, err)
			}
			if len(got.Records) != 1 || got.Records[0].Header.Offset != offs[0] {
				t.Errorf("kind %d: records = %+v, want the first alone before the stop", kind, got.Records)
			}
			if got.End != offs[1] {
				t.Errorf("kind %d: end %d, want the stopping slot %d itself", kind, got.End, offs[1])
			}
			if got.Sealed || got.Resynced {
				t.Errorf("kind %d: sealed %v resynced %v, want neither", kind, got.Sealed, got.Resynced)
			}
		}
	})

	t.Run("unknown kind reached by the forward search", func(t *testing.T) {
		seg, offs := cases()
		// The first record's length damaged so the search runs from it,
		// the second made an unknown kind: the search reads the bad slot
		// at the first record's own offset, then the unknown kind's, and
		// stops there — with the bad range before it reported.
		binary.LittleEndian.PutUint64(seg[offs[0] : offs[0]+96][40:48], 4<<20)
		seg[offs[1]+4] = 15
		rechecksumRecord(seg[offs[1]:offs[1]+96], testIdentity)

		got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		if !errors.Is(err, ErrUnknownKind) {
			t.Fatalf("ScanSegment = %v, want ErrUnknownKind", err)
		}
		if got.End != offs[1] {
			t.Fatalf("end %d, want the stopping slot %d itself", got.End, offs[1])
		}
		want := ScanRange{Off: offs[0], End: offs[1], Kind: RangeBad}
		if len(got.Ranges) != 1 || got.Ranges[0] != want {
			t.Errorf("ranges = %+v, want the one before the stop, %+v", got.Ranges, want)
		}
		if len(got.Records) != 0 {
			t.Errorf("records = %+v, want none attached", got.Records)
		}
		if got.Resynced {
			t.Error("resynced, want none: nothing was attached after the range")
		}
	})

	t.Run("a non-zero version epoch in a header", func(t *testing.T) {
		seg, offs := cases()
		binary.LittleEndian.PutUint64(seg[offs[1] : offs[1]+96][56:64], 1)
		rechecksumRecord(seg[offs[1]:offs[1]+96], testIdentity)

		got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		if !errors.Is(err, ErrNonZeroEpoch) {
			t.Fatalf("ScanSegment = %v, want ErrNonZeroEpoch", err)
		}
		if len(got.Records) != 1 || got.Records[0].Header.Offset != offs[0] {
			t.Errorf("records = %+v, want the first alone before the stop", got.Records)
		}
		if got.End != offs[1] {
			t.Errorf("end %d, want the stopping slot %d itself", got.End, offs[1])
		}
	})

	t.Run("a non-zero AsOf epoch in a clone target's body", func(t *testing.T) {
		// The clone target is appended with the epoch this format
		// requires of it, zero — a record holding a non-zero one cannot
		// be built — and its body's AsOf epoch bytes are then set, with
		// both checksums redone: VerifyRecordBody is the only place left
		// to refuse it, and the stop it makes is the record's own slot.
		h, body := scanKindBody(KindCloneTarget)
		seg, _ := scanSegment(t, testIdentity, scanTestRecords()[:2]...)
		seg, at := scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})
		atBody := seg[at+96 : at+96+56]
		binary.LittleEndian.PutUint64(atBody[40:48], 1)
		rechecksumRecordBody(seg[at:at+96+56], testIdentity, 56)

		got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		if !errors.Is(err, ErrNonZeroEpoch) {
			t.Fatalf("ScanSegment = %v, want ErrNonZeroEpoch: the AsOf epoch is read in VerifyRecordBody", err)
		}
		if got.End != at {
			t.Errorf("end %d, want the clone target's own slot %d", got.End, at)
		}
		for _, r := range got.Records {
			if r.Header.Offset >= at {
				t.Errorf("attached a record at %d, past the stop at %d", r.Header.Offset, at)
			}
		}
		if len(got.Ranges) != 0 || got.Resynced {
			t.Errorf("ranges = %+v resynced %v, want a clean walk before the stop", got.Ranges, got.Resynced)
		}
	})
}

// TestScanFindsASealMarkerBySearchAfterBadBytes: the forward search resumes
// past a zero slot and past a bad one at a verifying seal marker, because the
// marker proves everything before it synced — the range it reports ends at
// the marker's own offset, the scan seals there, and neither recovery counts
// a resync, for nothing was attached past the damage.
func TestScanFindsASealMarkerBySearchAfterBadBytes(t *testing.T) {
	for _, name := range []string{"zero", "bad"} {
		t.Run("after a "+name+" slot", func(t *testing.T) {
			recs := scanTestRecords()[:2]
			seg, _ := scanSegment(t, testIdentity, recs...)
			damaged := int64(len(seg))
			if name == "zero" {
				seg = append(seg, make([]byte, 16)...)
			} else {
				junk := make([]byte, 16)
				for i := range junk {
					junk[i] = 0xa5
				}
				seg = append(seg, junk...)
			}
			seal := scanSeal(t, &seg, testIdentity, testSegment)

			got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
			if err != nil {
				t.Fatalf("ScanSegment = %v, want no error", err)
			}
			kind := RangeZero
			if name == "bad" {
				kind = RangeBad
			}
			want := ScanRange{Off: damaged, End: seal, Kind: kind}
			if len(got.Ranges) != 1 || got.Ranges[0] != want {
				t.Fatalf("ranges = %+v, want one, %+v", got.Ranges, want)
			}
			if !got.Sealed || got.End != seal {
				t.Errorf("sealed %v end %d, want sealed at the marker's own offset %d", got.Sealed, got.End, seal)
			}
			if got.Resynced {
				t.Error("resynced, want none: nothing attached past the damage")
			}
			if len(got.Records) != 2 {
				t.Errorf("records = %d, want the two before the damage", len(got.Records))
			}
		})
	}
}

// TestScanReturnsReadErrorsAsThemselves pins that a read that fails is
// returned as itself, never as damage and never as a zero slot — unreadable
// never counts as empty — with the partial Scan it had built: everything
// attached before the failing read, every range closed before it, End at the
// offset the read was made at. The scan reads a record's body by its own
// readFull call, and reads its slots through a window the first offending
// slot refills, so an injected failure lands where the scan asks the reader
// for the bytes: at the body's own read, or at the edge of a window the
// walk has crossed — the header slot a later refill starts at, or the slot
// a forward search steps to — each of which the subtests name. A bodyless
// unmark record, 96 bytes and no more, is the cheapest way to walk a
// segment past one whole window of a megabyte, so that is what the two
// window-crossing subtests build.
func TestScanReturnsReadErrorsAsThemselves(t *testing.T) {
	injected := errors.New("format: an injected read failure")

	t.Run("at a body", func(t *testing.T) {
		// The first record is a write with a 13-byte payload, so its
		// 21-byte body read starts at its header's own end: a reader
		// failing there fails the record under examination, nothing
		// attached yet and no range open, End at the record's slot.
		seg, offs := scanSegment(t, testIdentity, scanTestRecords()...)
		bodyAt := offs[0] + RecordHeaderSize // the write's own body read starts here

		got, err := ScanSegment(readAtFailer{b: seg, off: bodyAt, err: injected}, int64(len(seg)), testIdentity, testSegment)
		if !errors.Is(err, injected) {
			t.Fatalf("ScanSegment = %v, want the injected error", err)
		}
		if errors.Is(err, ErrDamaged) {
			t.Error("the injected error washed into ErrDamaged, want it kept as itself")
		}
		if len(got.Records) != 0 || got.End != offs[0] {
			t.Errorf("records %d end %d, want none attached and end at the record's own slot %d", len(got.Records), got.End, offs[0])
		}
		if len(got.Ranges) != 0 || got.Sealed || got.Resynced {
			t.Errorf("ranges %d sealed %v resynced %v, want no range open for a record still under examination", len(got.Ranges), got.Sealed, got.Resynced)
		}
	})

	t.Run("inside the forward search", func(t *testing.T) {
		// The write at the first slot with its length damaged opens a
		// search, and nothing after it verifies — zeros the whole
		// megabyte the search must cross. The first read the search
		// cannot be served from the refill the damaged write was judged
		// under is the slot whose own header crosses that window's edge,
		// so the reader fails exactly there. The search dies mid-way:
		// End at the read's own offset, no range closed, for the bad
		// slot's range closes only at a recovery the scan can read to.
		var seg []byte
		seg = AppendSegmentHeader(seg, testSegmentHeader)
		h, body := scanKindBody(KindWrite)
		seg, _ = scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})
		binary.LittleEndian.PutUint64(seg[48 : 48+96][40:48], 4<<20)
		// the first slot whose header read crosses the window [48, 48+scanWindow)
		failAt := int64(SegmentHeaderSize + scanWindow - RecordHeaderSize + 8)
		seg = append(seg, make([]byte, failAt+96-int64(len(seg)))...)

		got, err := ScanSegment(readAtFailer{b: seg, off: failAt, err: injected}, int64(len(seg)), testIdentity, testSegment)
		if !errors.Is(err, injected) {
			t.Fatalf("ScanSegment = %v, want the injected error", err)
		}
		if got.End != failAt {
			t.Errorf("end %d, want the read's own offset %d", got.End, failAt)
		}
		if len(got.Records) != 0 || got.Resynced || got.Sealed {
			t.Error("records, seal or resync from a search whose read failed, want nothing")
		}
		// The bad write's range closes at the failing read and is kept —
		// every range closed before the failing offset is — as bad, for
		// the damaged record's own bytes are not zeros.
		want := ScanRange{Off: SegmentHeaderSize, End: failAt, Kind: RangeBad}
		if len(got.Ranges) != 1 || got.Ranges[0] != want {
			t.Errorf("ranges = %+v, want the one closed at the failing read, %+v", got.Ranges, want)
		}
	})

	t.Run("at the first slot of a window refill", func(t *testing.T) {
		// The window is refilled at the slot being examined, so a
		// reader failing at the refill's own start fails the walk at
		// that slot with everything attached before it kept. One
		// refill covers a whole megabyte, so the subtest walks unmark
		// records — 96 bytes each and bodyless, the cheapest there
		// are — one window's worth and then one more: every one the
		// first refill holds attaches, and the record past its reach
		// is the one the failing refill is made for.
		var seg []byte
		seg = AppendSegmentHeader(seg, testSegmentHeader)
		const unmarks = scanWindow / 96 // exactly one window of records
		for i := 0; i < unmarks+1; i++ {
			h, body := scanKindBody(KindUnmark)
			seg, _ = scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})
		}
		refillAt := int64(len(seg)) - 96 // the first record past the first refill's reach

		got, err := ScanSegment(readAtFailer{b: seg, off: refillAt, err: injected}, int64(len(seg)), testIdentity, testSegment)
		if !errors.Is(err, injected) {
			t.Fatalf("ScanSegment = %v, want the injected error", err)
		}
		if got.End != refillAt {
			t.Errorf("end %d, want the refill's own start %d, the offset the read was made at", got.End, refillAt)
		}
		if len(got.Records) != unmarks {
			t.Errorf("records %d, want the %d the first window holds, not the record examined under the failing refill", len(got.Records), unmarks)
		}
		if len(got.Ranges) != 0 || got.Sealed || got.Resynced {
			t.Errorf("ranges %d sealed %v resynced %v, want a clean walk before the failure", len(got.Ranges), got.Sealed, got.Resynced)
		}
	})

	t.Run("a reader shorter than size", func(t *testing.T) {
		seg, _ := scanSegment(t, testIdentity, scanTestRecords()...)
		got, err := ScanSegment(readAtShort{b: seg[:len(seg)-1]}, int64(len(seg)), testIdentity, testSegment)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("ScanSegment = %v, want io.ErrUnexpectedEOF", err)
		}
		if errors.Is(err, ErrDamaged) {
			t.Error("the short read washed into ErrDamaged, want it kept as itself")
		}
		if got.Sealed || got.Resynced {
			t.Errorf("sealed %v resynced %v, want neither from a short read", got.Sealed, got.Resynced)
		}
		if got.End < SegmentHeaderSize || got.End > int64(len(seg)) {
			t.Errorf("end %d, want one inside the segment", got.End)
		}
		for _, r := range got.Records {
			if r.Header.Offset+RecordHeaderSize > got.End {
				t.Errorf("attached a record at %d over the end %d", r.Header.Offset, got.End)
			}
		}
	})
}

// TestScanRefusesASegmentTooShortForASlot: a size below the first record
// slot cannot hold one record, so the scan refuses it — wrapped in
// ErrDamaged, as a segment too short for anything its rules speak of — and
// returns an empty Scan, for nothing was read at all. At exactly the
// header's own size the walk has nothing to walk: an empty scan ends there.
func TestScanRefusesASegmentTooShortForASlot(t *testing.T) {
	for _, size := range []int64{0, 1, 47} {
		got, err := ScanSegment(bytes.NewReader(nil), size, testIdentity, testSegment)
		if !errors.Is(err, ErrDamaged) {
			t.Errorf("size %d: ScanSegment = %v, want ErrDamaged", size, err)
		}
		if len(got.Records) != 0 || len(got.Ranges) != 0 || got.Sealed || got.Resynced || got.End != 0 {
			t.Errorf("size %d: scan = %+v, want the zero Scan: nothing was read", size, got)
		}
	}

	seg := AppendSegmentHeader(nil, testSegmentHeader)
	got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
	if err != nil {
		t.Fatalf("ScanSegment = %v, want no error", err)
	}
	want := Scan{End: SegmentHeaderSize}
	if len(got.Records) != 0 || len(got.Ranges) != 0 || got.Sealed || got.Resynced || got.End != want.End {
		t.Errorf("scan = %+v, want the empty one ending at the first slot, %+v", got, want)
	}
}

// TestScanEdges: a record slot cut short by the segment's end — a header
// fewer than 96 bytes, a marker fewer than 32 — is bad, walked into the
// range the forward search reports, never read as the record or the marker
// it promised; and a size not a multiple of 8 ends the walk on a slot fewer
// than 8 bytes long, whose bytes join the range they end in.
func TestScanEdges(t *testing.T) {
	t.Run("a header the segment ends short of", func(t *testing.T) {
		// A whole unmark record then a DJR1 prefix with only 40 bytes
		// left: the truncated header is bad, the search finds nothing
		// after it, and the one range runs to the size — bad by its
		// bytes, the truncated header among them.
		recs := scanTestRecords()[:1]
		seg, offs := scanSegment(t, testIdentity, recs...)
		cut := int64(len(seg))
		seg = append(seg, seg[offs[0]:offs[0]+40]...) // a header's prefix, its slice cut short
		copy(seg[cut:cut+4], "DJR1")
		rechecksumRecord(seg[cut:cut+40], testIdentity)

		got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		if err != nil {
			t.Fatalf("ScanSegment = %v, want no error: a short header is bad, not a stop", err)
		}
		want := ScanRange{Off: cut, End: int64(len(seg)), Kind: RangeBad}
		if len(got.Ranges) != 1 || got.Ranges[0] != want {
			t.Errorf("ranges = %+v, want the short header reported bad to the size, %+v", got.Ranges, want)
		}
		if len(got.Records) != 1 || got.Records[0].Header.Offset != offs[0] {
			t.Errorf("records = %+v, want the whole record before the cut alone", got.Records)
		}
	})

	t.Run("a marker the segment ends short of", func(t *testing.T) {
		// A whole record then a DJS1 prefix with 8 bytes left: the
		// truncated marker is bad too, so the segment is not sealed and
		// the tail range runs to the size.
		recs := scanTestRecords()[:1]
		seg, offs := scanSegment(t, testIdentity, recs...)
		cut := int64(len(seg))
		seg = append(seg, make([]byte, 8)...)
		copy(seg[cut:cut+4], "DJS1")

		got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		if err != nil {
			t.Fatalf("ScanSegment = %v, want no error", err)
		}
		if got.Sealed {
			t.Error("sealed, want not: a marker cut short of its 32 bytes is not a marker")
		}
		want := ScanRange{Off: cut, End: int64(len(seg)), Kind: RangeBad}
		if len(got.Ranges) != 1 || got.Ranges[0] != want {
			t.Errorf("ranges = %+v, want the short marker reported bad to the size, %+v", got.Ranges, want)
		}
		_ = offs
	})

	t.Run("a residue under the telling four bytes", func(t *testing.T) {
		// One, two and three bytes past the header: fewer than the
		// four a slot is told by, so the slot holds neither a prefix
		// to read nor a record, and its bytes end in the range the
		// search reports to the size — zero bytes a zero range, a
		// byte not zero a bad one.
		for _, residue := range []int{1, 2, 3} {
			for _, byte_ := range []struct {
				name string
				b    byte
				kind ScanRangeKind
			}{{"zero", 0, RangeZero}, {"junk", 0xa5, RangeBad}} {
				seg := AppendSegmentHeader(nil, testSegmentHeader)
				seg = append(seg, bytes.Repeat([]byte{byte_.b}, residue)...)

				got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
				if err != nil {
					t.Fatalf("residue %d: ScanSegment = %v, want no error", residue, err)
				}
				want := ScanRange{Off: SegmentHeaderSize, End: int64(len(seg)), Kind: byte_.kind}
				if len(got.Ranges) != 1 || got.Ranges[0] != want {
					t.Errorf("residue %d, %s: ranges = %+v, want the residue in one range %+v", residue, byte_.name, got.Ranges, want)
				}
				if got.End != int64(len(seg)) {
					t.Errorf("residue %d, %s: end %d, want the size %d", residue, byte_.name, got.End, len(seg))
				}
				if len(got.Records) != 0 || got.Sealed || got.Resynced {
					t.Errorf("residue %d, %s: records %d sealed %v resynced %v, want nothing but the range", residue, byte_.name, len(got.Records), got.Sealed, got.Resynced)
				}
			}
		}
	})

	t.Run("a size not a multiple of 8", func(t *testing.T) {
		// Four bytes past a whole record's end: the walk's last slot
		// holds fewer bytes than the grid it walks on, so it holds
		// neither a prefix nor a record; its bytes end in the range
		// the search reports to the size.
		recs := scanTestRecords()[:1]
		seg, _ := scanSegment(t, testIdentity, recs...)
		tail := int64(len(seg))
		seg = append(seg, 0xa5, 0xa5, 0xa5, 0xa5)

		got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		if err != nil {
			t.Fatalf("ScanSegment = %v, want no error", err)
		}
		want := ScanRange{Off: tail, End: int64(len(seg)), Kind: RangeBad}
		if len(got.Ranges) != 1 || got.Ranges[0] != want {
			t.Errorf("ranges = %+v, want the residue reported bad to the size, %+v", got.Ranges, want)
		}
		if got.End != int64(len(seg)) {
			t.Errorf("end %d, want the size %d", got.End, len(seg))
		}
	})
}

// TestScanAttachesRecordsAroundAndPastTheWindow: the walk attaches the
// records the scan's window alone cannot serve — a body read larger than a
// whole window, and a record whose header sits on one side of a refill and
// whose body stretches past it — because a record's body is read by its own
// readFull into a buffer grown for it, never through the slot window.
func TestScanAttachesRecordsAroundAndPastTheWindow(t *testing.T) {
	// bigWrite builds a write of n payload bytes with a header whose
	// patterns the golden tests carry.
	bigWrite := func(n int64) (RecordHeader, RecordBody) {
		h := RecordHeader{
			Kind:          KindWrite,
			Segment:       testSegment,
			File:          testRecordFile,
			Seq:           testRecordSeq,
			Version:       testRecordVersion,
			Tag:           testRecordTag,
			SyncedThrough: testRecordSyncedThrough,
			Extent:        core.Extent{Off: testRecordFileOffset, Len: n},
		}
		return h, RecordBody{MTime: testWriteMTime, Payload: scanPayload(int(n))}
	}

	t.Run("a body larger than the window", func(t *testing.T) {
		// A write of a window and a half of payload: both the one
		// before it and it itself attach, its 21-byte record and its
		// one-and-a-half-megabyte body read whole by one call that
		// grows the scan's own body buffer past the window.
		var seg []byte
		seg = AppendSegmentHeader(seg, testSegmentHeader)
		h, body := scanKindBody(KindWrite)
		seg, first := scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})
		h, body = bigWrite(int64(scanWindow) + scanWindow/2)
		seg, big := scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})

		got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		if err != nil {
			t.Fatalf("ScanSegment = %v, want no error", err)
		}
		if len(got.Records) != 2 {
			t.Fatalf("records = %d, want the small write and the one past the window, both", len(got.Records))
		}
		for i, want := range []int64{first, big} {
			if got.Records[i].Header.Offset != want {
				t.Errorf("record %d at %d, want %d", i, got.Records[i].Header.Offset, want)
			}
		}
		if got.Records[1].Header.Extent.Len != int64(scanWindow)+int64(scanWindow)/2 {
			t.Errorf("the big write's length = %d, want it whole", got.Records[1].Header.Extent.Len)
		}
		if got.Records[1].Body.Payload != nil {
			t.Error("the big write survived with a payload, want it dropped")
		}
		if got.End != int64(len(seg)) || len(got.Ranges) != 0 || got.Sealed || got.Resynced {
			t.Errorf("end %d ranges %d sealed %v resynced %v, want a clean walk to the size", got.End, len(got.Ranges), got.Sealed, got.Resynced)
		}
	})

	t.Run("a record straddling a window refill", func(t *testing.T) {
		// Unmarks up to a refill's edge, then a write whose header
		// sits at the first slot of the next window and whose body
		// stretches two windows past it: the header asks for a refill,
		// the body for a read the window never serves, and both hold.
		var seg []byte
		seg = AppendSegmentHeader(seg, testSegmentHeader)
		const unmarks = scanWindow / 96
		for i := 0; i < unmarks; i++ {
			h, body := scanKindBody(KindUnmark)
			seg, _ = scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})
		}
		h, body := bigWrite(int64(2*scanWindow) + 56)
		seg, _ = scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})

		got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		if err != nil {
			t.Fatalf("ScanSegment = %v, want no error", err)
		}
		if len(got.Records) != unmarks+1 {
			t.Fatalf("records = %d, want the %d unmarks and the straddling write", len(got.Records), unmarks+1)
		}
		last := got.Records[len(got.Records)-1]
		if last.Header.Kind != KindWrite || last.Header.Extent.Len != int64(2*scanWindow)+56 {
			t.Errorf("the straddling write came back as %d, length %d", last.Header.Kind, last.Header.Extent.Len)
		}
		if last.Body.Payload != nil {
			t.Error("the straddling write survived with a payload, want it dropped")
		}
		if got.End != int64(len(seg)) || len(got.Ranges) != 0 || got.Sealed || got.Resynced {
			t.Errorf("end %d ranges %d sealed %v resynced %v, want a clean walk to the size", got.End, len(got.Ranges), got.Sealed, got.Resynced)
		}
	})
}
