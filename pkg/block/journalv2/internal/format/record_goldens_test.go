package format

import (
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/raw"
)

// wantRecord is one of the 14 record goldens: the kind, the header its hex
// file decodes to — every field at the value the file writes, both checksums
// read from its bytes — and the body its payload checksum covers. The
// encoder, the hex file and raw must agree byte for byte, and the decoder
// must return exactly this header and body.
type wantRecord struct {
	name string // the hex file's name, record_<name>
	n    uint8  // the kinds table's literal number
	h    RecordHeader
	body RecordBody
}

// goldenBase returns the header fields every record golden shares: the
// segment, the FileID, the sequence number, the version, the tag, the
// synced-through offset and the own offset. What differs per kind — the
// kind byte, the extent, the kinds the table gives as zero — the table
// sets on top of it, per the hex file.
func goldenBase(kind Kind) RecordHeader {
	return RecordHeader{
		Kind:          kind,
		Segment:       testSegment,
		File:          testRecordFile,
		Seq:           testRecordSeq,
		Version:       testRecordVersion,
		Tag:           testRecordTag,
		SyncedThrough: testRecordSyncedThrough,
		Offset:        testRecordOffset,
	}
}

// withExtent is goldenBase with a file offset and length, the kinds that
// carry an extent.
func withExtent(kind Kind, off, length int64) RecordHeader {
	h := goldenBase(kind)
	h.Extent = core.Extent{Off: off, Len: length}
	return h
}

// bare is goldenBase with the file offset and length zero, and for the
// kinds the table gives as zero entirely, the FileID and version zeroed too.
func bare(kind Kind, zeroFile bool) RecordHeader {
	h := withExtent(kind, 0, 0)
	if zeroFile {
		h.File = core.FileID{}
		h.Version = core.Version{}
	}
	return h
}

// lossFlags is the flag word the loss golden's body encodes.
func lossFlags(l Loss) uint64 {
	flags := uint64(0)
	if l.Synced {
		flags |= 1 << 0
	}
	if l.Offloaded {
		flags |= 1 << 1
	}
	return flags | uint64(l.Reason)<<8
}

// lossBodyBytes is the 16 bytes a loss record's body takes, so a test can
// checksum them without the codec's help.
func lossBodyBytes(l Loss) []byte {
	b := binary.LittleEndian.AppendUint64(nil, uint64(l.Dropped))
	return binary.LittleEndian.AppendUint64(b, lossFlags(l))
}

// cloneBodyBytes is the 56 bytes a clone target's body takes, so a test can
// checksum them without the codec's help.
func cloneBodyBytes(c CloneSource) []byte {
	file := c.File.Bytes()
	b := append(c.Share[:], file[:]...)
	b = binary.LittleEndian.AppendUint64(b, uint64(c.Off))
	b = binary.LittleEndian.AppendUint64(b, c.AsOf.Epoch)
	return binary.LittleEndian.AppendUint64(b, c.AsOf.Counter)
}

func cutBodyBytes(cut core.SnapshotCut) []byte {
	return binary.LittleEndian.AppendUint64(nil, uint64(cut))
}

// writeBodyBytes is the bytes a write's body takes: the modification time
// then the payload.
func writeBodyBytes(b RecordBody) []byte {
	out := binary.LittleEndian.AppendUint64(nil, uint64(b.MTime))
	return append(out, b.Payload...)
}

// bodyBytes is the bytes the record's body takes, by its kind, so a test can
// checksum what the golden covers without the codec's help.
func (w wantRecord) bodyBytes() []byte {
	switch w.h.Kind {
	case KindWrite:
		return writeBodyBytes(w.body)
	case KindFill:
		return w.body.Payload
	case KindLoss:
		return lossBodyBytes(w.body.Loss)
	case KindCloneTarget:
		return cloneBodyBytes(w.body.Clone)
	case KindHold, KindUnhold, KindStamp:
		return cutBodyBytes(w.body.Cut)
	}
	return nil
}

// rawOf is the golden's record as raw builds it: the same fields, the same
// body, laid out by raw's own account of the format's tables.
func (w wantRecord) rawOf() raw.Record {
	r := raw.Record{
		Identity:      testIdentity,
		Kind:          uint8(w.h.Kind),
		Segment:       uint64(w.h.Segment),
		FileID:        w.h.File.Bytes(),
		Offset:        uint64(w.h.Extent.Off),
		Length:        uint64(w.h.Extent.Len),
		Seq:           uint64(w.h.Seq),
		Version:       raw.Version{Epoch: w.h.Version.Epoch, Counter: w.h.Version.Counter},
		Tag:           uint32(w.h.Tag),
		SyncedThrough: uint64(w.h.SyncedThrough),
		At:            uint32(w.h.Offset),
	}
	switch w.h.Kind {
	case KindWrite:
		r.Write = &raw.WriteBody{MTime: w.body.MTime, Payload: w.body.Payload}
	case KindFill:
		r.Fill = &raw.FillBody{Payload: w.body.Payload}
	case KindLoss:
		r.Loss = &raw.LossBody{Dropped: uint64(w.body.Loss.Dropped), Flags: lossFlags(w.body.Loss)}
	case KindCloneTarget:
		clone := w.body.Clone
		r.Clone = &raw.CloneBody{Share: clone.Share, File: clone.File.Bytes(), Off: uint64(clone.Off), AsOf: raw.Version{Epoch: clone.AsOf.Epoch, Counter: clone.AsOf.Counter}}
	case KindHold, KindUnhold, KindStamp:
		r.Cut = &raw.CutBody{Cut: uint64(w.body.Cut)}
	}
	return r
}

// recordGoldens returns the 14 records the golden files hold, one per kind,
// in the kinds table's order.
func recordGoldens(t *testing.T) map[string]wantRecord {
	t.Helper()

	// The release, deallocate, offloaded, unmark and loss goldens share
	// one extent; the clone target the same at DstOff.
	extent := core.Extent{Off: testRecordFileOffset, Len: testRecordLength}

	write := withExtent(KindWrite, testRecordFileOffset, int64(len(testWritePayload)))
	writeBody := RecordBody{MTime: testWriteMTime, Payload: testWritePayload}

	fill := withExtent(KindFill, testRecordFileOffset, int64(len(testFillPayload)))
	fillBody := RecordBody{Payload: testFillPayload}

	cloneTarget := withExtent(KindCloneTarget, testRecordFileOffset, testRecordLength)
	cloneTargetBody := RecordBody{Clone: testClone}

	goldens := map[string]wantRecord{
		"write":        {write.Kind.String(), 1, write, writeBody},
		"fill":         {fill.Kind.String(), 2, fill, fillBody},
		"release":      {"release", 3, withExtent(KindRelease, extent.Off, extent.Len), RecordBody{}},
		"truncate":     {"truncate", 4, withExtent(KindTruncate, testRecordFileOffset, 0), RecordBody{}},
		"deallocate":   {"deallocate", 5, withExtent(KindDeallocate, extent.Off, extent.Len), RecordBody{}},
		"delete":       {"delete", 6, bare(KindDelete, false), RecordBody{}},
		"clone_target": {"clone_target", 7, cloneTarget, cloneTargetBody},
		"offloaded":    {"offloaded", 8, withExtent(KindOffloaded, extent.Off, extent.Len), RecordBody{}},
		"unmark":       {"unmark", 9, withExtent(KindUnmark, extent.Off, extent.Len), RecordBody{}},
		"loss":         {"loss", 10, withExtent(KindLoss, extent.Off, extent.Len), RecordBody{Loss: testLoss}},
		"hold":         {"hold", 11, bare(KindHold, false), RecordBody{Cut: testCut}},
		"unhold":       {"unhold", 12, bare(KindUnhold, true), RecordBody{Cut: testCut}},
		"stamp":        {"stamp", 13, bare(KindStamp, false), RecordBody{Cut: testCut}},
		"forget":       {"forget", 14, bare(KindForget, true), RecordBody{}},
	}
	for name, g := range goldens {
		if uint8(g.h.Kind) != g.n {
			t.Fatalf("golden %s: kind is %d, want the table's %d", name, uint8(g.h.Kind), g.n)
		}
	}
	return goldens
}
