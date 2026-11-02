package format

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// FuzzScan drives ScanSegment over two shapes of input and holds every scan
// to the invariants that make its result worth reading:
//
//   - the error nil, or matching ErrDamaged — only when size is below the
//     first record slot — ErrUnknownKind or ErrNonZeroEpoch;
//   - the records and ranges ascending, tiling [SegmentHeaderSize, End)
//     exactly;
//   - every attached record re-decoding out of the bytes it was attached
//     from, its body verifying — a record a scan hangs out "never yields
//     an extent the bytes do not contain";
//   - every handed-back refused header re-decoding at its own offset,
//     lying inside a RangeBad range, and its body failing to verify;
//   - every RangeZero all zeros, every RangeBad holding a byte not zero;
//   - no two ranges adjacent;
//   - Resynced set exactly when some RangeBad ends at an attached record's
//     own slot.
//
// In raw mode the mutation bytes are the segment whole. In planted mode
// plan derives a segment — records of the kinds 1–14 with zero gaps among
// them, an optional seal marker, an old life's bytes in the tail, trailing
// zeros — and mutate names the bytes to flip; the planted scan is held to
// the rules its planting makes too: an untouched planted record attaches at
// its own slot with its own header, nothing else attaches, an old life's
// record never attaches, and an untouched planted marker seals at its own
// slot.
func FuzzScan(f *testing.F) {
	// Raw seeds: nothing but the header's own letters, a header alone,
	// the clean walk of the kinds this format defines, the same walk
	// recycling an old life's bytes in its tail, and the shapes the unit
	// tests walk — the five-record segment sealed with a footer, and a
	// write whose payload holds a whole copy of another journal's bytes.
	f.Add([]byte{}, []byte{})
	f.Add(make([]byte, 256), []byte{})
	f.Add([]byte{}, scanAllKindsSegment())
	f.Add([]byte{}, recycledSegment())
	f.Add([]byte{}, sealedTestSegment(f))
	f.Add([]byte{}, payloadCopySegment(f))

	// Planted seeds: every kind in a row; a zero gap in the middle; a
	// marker past the last; a gap then a marker; an old life in the
	// tail; two gaps; a record planted past a marker. The marker and
	// old-life letters are the grammar's own, not record kinds — a seed
	// spelling them with kind letters plants nothing it claims to.
	for _, plan := range [][]byte{
		{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14},
		{1, 0, 3},
		{1, 2, fuzzMarkerLetter},
		{1, 0, fuzzMarkerLetter, 2},
		{1, 2, fuzzOldLifeLetter},
		{1, 0, 2, 0, 3},
		{1, fuzzMarkerLetter, 2},
	} {
		f.Add(plan, []byte{})
	}

	f.Fuzz(func(t *testing.T, plan, mutate []byte) {
		fuzzScan(t, plan, mutate)
	})
}

// fuzzScan runs one fuzz iteration. The raw mode scans the mutation bytes
// themselves as a segment. The planted mode plants a segment from plan —
// only when plan holds a letter the planting grammar knows — flips the
// bytes mutate names, and holds its scan to the planted rules too. A plan
// holding no letter stays raw alone, so no plan a fuzzer hands over is
// quietly dropped while its own bytes go unscanned.
func fuzzScan(t *testing.T, plan, mutate []byte) {
	identity, segment := testIdentity, testSegment

	raw, rawErr := scanWhole(mutate, identity, segment)
	fuzzAssertInvariants(t, "raw", mutate, raw, rawErr, identity, segment)

	pl, ok := fuzzPlant(plan)
	if !ok {
		return
	}
	seg, before := applyXORs(pl.seg, mutate)
	scan, err := scanWhole(seg, identity, segment)
	fuzzAssertInvariants(t, "planted", seg, scan, err, identity, segment)
	fuzzAssertPlanted(t, seg, before, scan, err, pl)
}

// scanWhole reads seg as a segment of exactly its own length.
func scanWhole(seg []byte, identity core.JournalID, segment core.SegmentID) (Scan, error) {
	return ScanSegment(bytes.NewReader(seg), int64(len(seg)), identity, segment)
}

// The planting grammar's own letters. A plan's byte is a letter: 0 a zero
// gap of one record's width, 1–14 a record of that kind, fuzzMarkerLetter
// a seal marker pasted at the next slot, and fuzzOldLifeLetter an old
// life's whole bytes in the tail. Anything else ends the plan's letters.
const (
	fuzzFirstKind     = 1
	fuzzLastKind      = 14
	fuzzMarkerLetter  = 253
	fuzzOldLifeLetter = 252
	fuzzMaxPayload    = 96
)

// fuzzPlanted is a segment planted from a plan, with everything its rules
// assert on: the planted records' slots and headers, the marker's own slot
// when one was planted, and the old life's byte range when the plan holds
// one.
type fuzzPlanted struct {
	seg     []byte
	records []int64        // the planted records' slots, ascending
	headers []RecordHeader // the planted records' headers, the records' own order
	seal    int64          // the planted marker's slot, -1 when none was planted
	// oldOff, oldEnd hold the old life's byte range and oldSeal its
	// marker's own slot, or -1s when the plan planted none.
	oldOff, oldEnd, oldSeal int64
}

// fuzzPlant builds a segment from a plan's bytes, each a letter of the
// grammar above. A gap plants zeros of the width the next planted record of
// the next letter takes, so the planted records stay on the 8-byte grid
// whatever the letters spell; the trailing zeros count off the plan's own
// first letter, so their width stays small. A plan holding no letter —
// every byte 0 is a gap and a plan of only gaps plants a whole segment of
// zeros, so a gap is a letter too — plants a segment of its segment header
// and the zeros the gaps spell. The grammar reads only the letters at the
// plan's head, the rest of a plan past the first letter it does not know
// ignored.
func fuzzPlant(plan []byte) (fuzzPlanted, bool) {
	if len(plan) == 0 {
		return fuzzPlanted{}, false
	}

	pl := fuzzPlanted{seal: -1, oldOff: -1, oldEnd: -1, oldSeal: -1}
	seg := AppendSegmentHeader(nil, testSegmentHeader)
	droppedOld := false
	seenMarker := false

	for _, letter := range plan {
		switch {
		case letter == 0:
			// A zero gap one whole record wide: the smallest width
			// that holds a slot's own bytes and the next letter's
			// header start on the grid.
			seg = append(seg, make([]byte, 96)...)
		case letter >= fuzzFirstKind && letter <= fuzzLastKind:
			off := int64(len(seg))
			h, body := fuzzKindBody(Kind(letter), off)
			seg = AppendRecord(seg, testIdentity, h, body)
			pl.records = append(pl.records, off)
			pl.headers = append(pl.headers, h)
		case letter == fuzzMarkerLetter:
			if seenMarker {
				continue // a second marker plants nothing, like a sealed segment's own tail
			}
			seenMarker = true
			off := int64(len(seg))
			seg = AppendSealMarker(seg, testIdentity, SealMarker{Segment: testSegment, Offset: off})
			pl.seal = off
		case letter == fuzzOldLifeLetter:
			if droppedOld {
				continue // one old life planted per plan
			}
			droppedOld = true
			off := int64(len(seg))
			old := fuzzOldLifeBytes(off)
			seg = append(seg, old...)
			pl.oldOff, pl.oldEnd = off, off+int64(len(old))
			pl.oldSeal = off + int64(len(old)) - SealMarkerSize
		default:
			// A letter the grammar does not know ends the plan.
			goto done
		}
	}
done:
	// Trailing zeros, small: the plan's own bytes count them modulo a
	// few slots, so a tail range costs little to cover.
	{
		n := 0
		if len(plan) > 0 {
			for _, b := range plan {
				n = (n + int(b)) % 5
			}
		}
		seg = append(seg, make([]byte, n*8)...)
	}
	pl.seg = seg
	return pl, true
}

// fuzzKindBody builds a planted record of one kind at its own slot: the
// extent and the zero-field rules from the kinds table, a file for every
// kind but unhold and forget, versions at the bounds the table allows, and
// payloads small. The version epoch stays zero — this format's own rule —
// so a planted header stops no scan.
func fuzzKindBody(kind Kind, off int64) (RecordHeader, RecordBody) {
	h := RecordHeader{
		Kind:          kind,
		Segment:       testSegment,
		File:          testRecordFile,
		Seq:           testRecordSeq,
		Version:       testRecordVersion,
		Tag:           testRecordTag,
		SyncedThrough: testRecordSyncedThrough,
		Offset:        off,
	}
	var body RecordBody
	switch kind {
	case KindWrite:
		h.Extent = core.Extent{Off: testRecordFileOffset, Len: fuzzMaxPayload}
		body = RecordBody{MTime: testWriteMTime, Payload: scanPayload(fuzzMaxPayload)}
	case KindFill:
		h.Extent = core.Extent{Off: testRecordFileOffset, Len: fuzzMaxPayload + 13}
		body = RecordBody{Payload: scanPayload(fuzzMaxPayload + 13)}
	case KindLoss:
		h.Extent = core.Extent{Off: testRecordFileOffset, Len: testRecordLength}
		body = RecordBody{Loss: Loss{Dropped: testLoss.Dropped, Synced: true, Reason: LossStale}}
	case KindCloneTarget:
		h.Extent = core.Extent{Off: testRecordFileOffset, Len: testRecordLength}
		body = RecordBody{Clone: CloneSource{Share: testClone.Share, File: testClone.File, Off: testClone.Off, AsOf: testClone.AsOf}}
	case KindHold, KindStamp:
		h.Extent = core.Extent{}
		body = RecordBody{Cut: testCut}
	case KindUnhold:
		// The kinds table gives an unhold a zero file, version and
		// extent, so a planted one carries them zeroed.
		h.File = core.FileID{}
		h.Version = core.Version{}
		h.Extent = core.Extent{}
		body = RecordBody{Cut: testCut}
	case KindForget:
		h.File = core.FileID{}
		h.Version = core.Version{}
		h.Extent = core.Extent{}
		body = RecordBody{}
	case KindTruncate:
		h.Extent = core.Extent{Off: testRecordFileOffset, Len: 0}
	case KindDeallocate, KindRelease, KindOffloaded, KindUnmark:
		h.Extent = core.Extent{Off: testRecordFileOffset, Len: testRecordLength}
	}
	return h, body
}

// fuzzOldLifeBytes plants an old life's records and marker under the
// segment id before this one, as its own segment was. base is the slot
// the bytes will be pasted at in the recycled segment, so every header
// names the slot it will really sit at — as a recycled segment's own old
// bytes do — and only the segment id tells this scan to refuse them.
func fuzzOldLifeBytes(base int64) []byte {
	var seg []byte
	for range 2 {
		h, body := fuzzKindBody(KindUnmark, base+int64(len(seg)))
		h.Segment = testSegment - 1
		seg = AppendRecord(seg, testIdentity, h, body)
	}
	oldSeal := base + int64(len(seg))
	seg = AppendSealMarker(seg, testIdentity, SealMarker{Segment: testSegment - 1, Offset: oldSeal})
	return seg
}

// The mutating grammar: every nine bytes of mutate name one flip — an
// offset in its first eight, little-endian, and a value to XOR in its
// ninth. A pair past the segment's end flips nothing, and two flips of one
// byte cancel, so the fuzzer's own mutations can leave a planted record
// whole again.
func applyXORs(seg, mutate []byte) ([]byte, []byte) {
	flipped := slices.Clone(seg)
	before := slices.Clone(seg)
	for i := 0; i+9 <= len(mutate); i += 9 {
		off := int64(binary.LittleEndian.Uint64(mutate[i : i+8]))
		val := mutate[i+8]
		if off < int64(len(flipped)) && off >= 0 {
			flipped[off] ^= val
		}
	}
	return flipped, before
}

// fuzzAssertInvariants holds one scan to the invariants both modes share.
func fuzzAssertInvariants(t *testing.T, mode string, seg []byte, scan Scan, err error, identity core.JournalID, segment core.SegmentID) {
	t.Helper()
	size := int64(len(seg))

	if err != nil {
		switch {
		case size < SegmentHeaderSize:
			if !errors.Is(err, ErrDamaged) {
				t.Errorf("%s: error %v on a short segment, want ErrDamaged", mode, err)
			}
			return
		case errors.Is(err, ErrDamaged):
			t.Errorf("%s: error %v holds ErrDamaged over a whole segment, want only ErrUnknownKind or ErrNonZeroEpoch", mode, err)
		case !errors.Is(err, ErrUnknownKind) && !errors.Is(err, ErrNonZeroEpoch):
			t.Errorf("%s: error %v matches no rule the format stops for", mode, err)
		}
		// A stop's own Scan is everything before its slot; the invariants
		// check it the same as an error-free one's, over [48, End).
	} else if !scan.Sealed && scan.End != size {
		t.Errorf("%s: end %d, want the size %d the walk reads to, or the slot of the marker that sealed it", mode, scan.End, size)
	} else if scan.Sealed && scan.End+int64(SealMarkerSize) > size {
		t.Errorf("%s: sealed at %d, a marker past the size %d", mode, scan.End, size)
	}

	// The tiling: what the scan attached and what it stepped over cover
	// everything from the first record slot to End, in ascending order.
	at := int64(SegmentHeaderSize)
	ri, qi := 0, 0
	for at < scan.End {
		switch {
		case ri < len(scan.Records) && scan.Records[ri].Header.Offset == at:
			rec := scan.Records[ri]
			ri++
			if at+int64(RecordHeaderSize) > size {
				t.Fatalf("%s: record at %d holds a header past the size %d", mode, at, size)
			}
			h, decErr := DecodeRecordHeader(seg[at:at+int64(RecordHeaderSize)], identity, segment, at)
			if decErr != nil {
				t.Fatalf("%s: record at %d does not re-decode: %v", mode, at, decErr)
			}
			if h != rec.Header {
				t.Errorf("%s: record at %d attached as %+v, want its own decoded header %+v", mode, at, rec.Header, h)
			}
			end := at + h.Size()
			if end > size {
				t.Fatalf("%s: record at %d runs past the size %d", mode, at, size)
			}
			if rec.Body.Payload != nil {
				t.Errorf("%s: record at %d attached with a payload, want it dropped", mode, at)
			}
			// The body the bytes hold re-verifies and matches the one
			// the scan attached, fixed fields alone — what a scan
			// hangs out is never an extent the bytes do not contain.
			body := make([]byte, h.BodySize())
			copy(body, seg[at+int64(RecordHeaderSize):at+int64(RecordHeaderSize)+h.BodySize()])
			verified, verErr := VerifyRecordBody(body, identity, h)
			if verErr != nil {
				t.Fatalf("%s: record at %d body does not verify: %v", mode, at, verErr)
			}
			verified.Payload = nil
			if !recordBodiesEqual(verified, rec.Body) {
				t.Errorf("%s: record at %d attached the body %+v, want the verified one %+v", mode, at, rec.Body, verified)
			}
			at = end
		case qi < len(scan.Ranges) && scan.Ranges[qi].Off == at:
			r := scan.Ranges[qi]
			qi++
			if r.End <= r.Off {
				t.Errorf("%s: range %+v holds no bytes", mode, r)
			}
			if r.End > scan.End {
				t.Errorf("%s: range %+v ends past the scan's own end %d", mode, r, scan.End)
			}
			if r.Kind == RangeZero {
				if !zero(seg[r.Off:r.End]) {
					t.Errorf("%s: range %+v is RangeZero holding a byte not zero", mode, r)
				}
			} else if r.Kind == RangeBad {
				if zero(seg[r.Off:r.End]) {
					t.Errorf("%s: range %+v is RangeBad holding only zeros", mode, r)
				}
			} else {
				t.Errorf("%s: range %+v holds kind %d, want zero or bad", mode, r, r.Kind)
			}
			at = r.End
		default:
			t.Fatalf("%s: the records and ranges leave the bytes at %d uncovered, up to end %d", mode, at, scan.End)
		}
	}
	if ri < len(scan.Records) {
		t.Errorf("%s: %d records below the covered end %d", mode, len(scan.Records)-ri, scan.End)
	}
	if qi < len(scan.Ranges) {
		t.Errorf("%s: %d ranges below the covered end %d", mode, len(scan.Ranges)-qi, scan.End)
	}

	// No two ranges share an edge: a zero run and a bad one side by side
	// are one range of bad bytes, never two.
	for i := 1; i < len(scan.Ranges); i++ {
		if scan.Ranges[i].Off == scan.Ranges[i-1].End {
			t.Errorf("%s: ranges %+v and %+v are adjacent, want one", mode, scan.Ranges[i-1], scan.Ranges[i])
		}
	}

	// Every handed-back refused header is a real refused record: its
	// header re-decodes at its own offset under the scan's own identity
	// and segment — a handed-back header never yields terms its bytes
	// do not hold — it lies inside a bad range, the records its bytes
	// fold into, and its body does not verify, for it is handed back
	// exactly because it does not.
	for i, h := range scan.Refused {
		off := h.Offset
		if off < SegmentHeaderSize || off >= size {
			t.Fatalf("%s: refused header %d at %d, past the segment's size %d", mode, i, off, size)
		}
		if off+int64(RecordHeaderSize) > size {
			t.Fatalf("%s: refused header %d at %d holds a header past the size %d", mode, i, off, size)
		}
		decoded, decErr := DecodeRecordHeader(seg[off:off+int64(RecordHeaderSize)], identity, segment, off)
		if decErr != nil {
			t.Fatalf("%s: refused header %d at %d does not re-decode: %v", mode, i, off, decErr)
		}
		if decoded != h {
			t.Errorf("%s: refused header %d at %d = %+v, want its own decoded one %+v", mode, i, off, h, decoded)
		}
		if i > 0 && scan.Refused[i-1].Offset >= off {
			t.Errorf("%s: refused headers at %d then %d, want them ascending by offset", mode, scan.Refused[i-1].Offset, off)
		}
		inside := false
		for _, r := range scan.Ranges {
			if r.Kind == RangeBad && r.Off <= off && off < r.End {
				inside = true
				break
			}
		}
		if !inside {
			t.Errorf("%s: refused header %d at %d lies in no bad range, want its record's bytes folded into one", mode, i, off)
		}
		if off+decoded.Size() > size {
			t.Fatalf("%s: refused header %d at %d runs past the size %d", mode, i, off, size)
		}
		body := make([]byte, decoded.BodySize())
		copy(body, seg[off+int64(RecordHeaderSize):off+int64(RecordHeaderSize)+decoded.BodySize()])
		if _, verErr := VerifyRecordBody(body, identity, decoded); verErr == nil {
			t.Errorf("%s: refused header %d at %d has a body that verifies, want it handed back exactly because it does not", mode, i, off)
		}
	}

	// Resynced holds exactly when a bad range ends at an attached record's
	// own slot.
	resync := false
	for _, r := range scan.Ranges {
		if r.Kind != RangeBad {
			continue
		}
		for _, rec := range scan.Records {
			if rec.Header.Offset == r.End {
				resync = true
			}
		}
	}
	if scan.Resynced != resync {
		t.Errorf("%s: resynced %v, want %v for the ranges %+v", mode, scan.Resynced, resync, scan.Ranges)
	}

	// Sealed says End names a marker that verifies here: re-reading the
	// bytes holds the same.
	if scan.Sealed {
		if _, err := DecodeSealMarker(seg[scan.End:scan.End+int64(SealMarkerSize)], identity, segment, scan.End); err != nil {
			t.Errorf("%s: sealed at %d on a marker that does not verify: %v", mode, scan.End, err)
		}
	}
}

// fuzzAssertPlanted holds the rules a planting makes: every planted record
// left untouched that lies before the scan's own end attaches at its own
// slot with an equal header; nothing attaches that was not planted; an old
// life's record never attaches; an untouched planted marker seals the scan
// at its own slot.
func fuzzAssertPlanted(t *testing.T, seg, before []byte, scan Scan, err error, pl fuzzPlanted) {
	t.Helper()

	// A stop's scan ends at its own slot; the rules hold for everything
	// planted before that, the same as an error-free scan's.
	untouched := func(at, size int64) bool {
		return at+size <= int64(len(seg)) && bytes.Equal(seg[at:at+size], before[at:at+size])
	}

	end := scan.End
	for i, at := range pl.records {
		h := pl.headers[i]
		if at >= end {
			break
		}
		if !untouched(at, h.Size()) {
			continue
		}
		found := false
		for _, rec := range scan.Records {
			if rec.Header.Offset == at {
				found = true
				// The planted header carries no PayloadCRC — AppendRecord
				// fills it — so the comparison takes the attached one's
				// own, as the scan tests' helper does.
				if !plantedHeadersEqual(h, rec.Header) {
					t.Errorf("planted: the untouched record at %d attached as %+v, want its planted header %+v", at, rec.Header, h)
				}
				break
			}
		}
		if !found {
			t.Errorf("planted: the untouched record at %d did not attach before the end %d (error %v)", at, end, err)
		}
	}

	// Every attached record is a planted new-life one at its planted slot.
	for _, rec := range scan.Records {
		if !slices.Contains(pl.records, rec.Header.Offset) {
			t.Errorf("planted: a record attached at %d, where the plan planted nothing", rec.Header.Offset)
		}
	}

	// An untouched planted record whose header verifies never comes back
	// refused, and a refused header is a planted record's own at its
	// planted slot — only a planted record's bytes decode here at all.
	for i, at := range pl.records {
		if at >= end {
			break
		}
		h := pl.headers[i]
		if untouched(at, h.Size()) {
			for _, ref := range scan.Refused {
				if ref.Offset == at {
					t.Errorf("planted: the untouched record at %d came back refused, want its bytes whole and verified", at)
				}
			}
		}
	}
	for _, ref := range scan.Refused {
		if !slices.Contains(pl.records, ref.Offset) {
			t.Errorf("planted: a refused header at %d, where the plan planted nothing", ref.Offset)
		}
	}

	// No allocated record lies in the old life's range: its records name
	// the segment this scan is not of, so none verifies here.
	if pl.oldOff >= 0 {
		for _, rec := range scan.Records {
			if rec.Header.Offset >= pl.oldOff && rec.Header.Offset < pl.oldEnd {
				t.Errorf("planted: an old-life record attached at %d, by its own bytes never verifiable here", rec.Header.Offset)
			}
		}
		// The old life's own marker, at its own real slot and naming the
		// segment this scan is not of, never seals: it only decodes under
		// the old segment id, which this scan does not scan as.
		if scan.Sealed && scan.End == pl.oldSeal {
			t.Errorf("planted: sealed at %d on the old life's marker, which names another segment and cannot verify here", scan.End)
		}
	}

	// An untouched planted marker seals the scan at its own slot.
	if pl.seal >= 0 && pl.seal < end && untouched(pl.seal, int64(SealMarkerSize)) {
		if err != nil {
			return // a stop ended the scan before the marker was reached
		}
		if !scan.Sealed {
			t.Errorf("planted: the untouched marker at %d did not seal the scan, which ended at %d", pl.seal, scan.End)
		} else if scan.End != pl.seal {
			t.Errorf("planted: sealed at %d, want the untouched marker's own slot %d", scan.End, pl.seal)
		}
	}
}

// scanAllKindsSegment builds a raw seed: every kind's record at its own
// slot, then a marker past the last, then a short tail of zeros.
func scanAllKindsSegment() []byte {
	seg := AppendSegmentHeader(nil, testSegmentHeader)
	for k := fuzzFirstKind; k <= fuzzLastKind; k++ {
		h, body := fuzzKindBody(Kind(k), int64(len(seg)))
		seg = AppendRecord(seg, testIdentity, h, body)
	}
	seg = AppendSealMarker(seg, testIdentity, SealMarker{Segment: testSegment, Offset: int64(len(seg))})
	return append(seg, make([]byte, 16)...)
}

// recycledSegment builds a raw seed: the segment header, a new life's one
// record, then an old life's whole bytes pasted past it — the recycled
// segment a scan tells its new life's records from.
func recycledSegment() []byte {
	seg := AppendSegmentHeader(nil, testSegmentHeader)
	h, body := fuzzKindBody(KindUnmark, int64(len(seg)))
	seg = AppendRecord(seg, testIdentity, h, body)
	return append(seg, fuzzOldLifeBytes(int64(len(seg)))...)
}

// sealedTestSegment builds a raw seed from the unit tests' own five-record
// segment: the records, a verifying seal marker, and a zero footer tail —
// the shape every fallback-open scan starts from.
func sealedTestSegment(f *testing.F) []byte {
	seg, _ := scanSegment(f, testIdentity, scanTestRecords()...)
	seg = AppendSealMarker(seg, testIdentity, SealMarker{Segment: testSegment, Offset: int64(len(seg))})
	return append(seg, make([]byte, 64)...)
}

// payloadCopySegment builds a raw seed from the unit tests' own copy-bearing
// write: a write whose payload holds a whole sealed two-record segment of
// this same journal, with records of its own around it — the shape whose
// marker must not seal and whose records must not attach from inside the
// payload.
func payloadCopySegment(f *testing.F) []byte {
	copySeg, _ := scanSegment(f, testIdentity, scanTestRecords()[:2]...)
	copySeg = AppendSealMarker(copySeg, testIdentity, SealMarker{Segment: testSegment, Offset: int64(len(copySeg))})
	h, body := scanKindBody(KindWrite)
	h.Extent = core.Extent{Off: testRecordFileOffset, Len: int64(len(copySeg))}
	body = RecordBody{MTime: testWriteMTime, Payload: copySeg}
	seg, _ := scanSegment(f, testIdentity, scanTestRecords()[:1]...)
	seg, _ = scanAppend(f, seg, testIdentity, scanRecord{h: h, body: body})
	seg, _ = scanAppend(f, seg, testIdentity, scanTestRecords()[1])
	return seg
}
