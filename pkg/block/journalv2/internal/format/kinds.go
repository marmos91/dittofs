package format

import (
	"fmt"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// Kind is a record's kind: what the record does — stages bytes, removes or
// marks them. Values are stable: a value is never reassigned, and 0 is never
// a kind, so a slot of zeros never reads as one. 15–127 are reserved for
// later core kinds and 128–255 for extensions, each assigned with the format
// version that introduces it.
type Kind uint8

const (
	KindWrite       Kind = 1  // stages the extent's bytes
	KindFill        Kind = 2  // stages the extent's bytes as filled
	KindRelease     Kind = 3  // stops holding one released extent
	KindTruncate    Kind = 4  // drops everything past the file offset
	KindDeallocate  Kind = 5  // punches the extent out of the file
	KindDelete      Kind = 6  // drops the whole file, the extent is 0, 0
	KindCloneTarget Kind = 7  // carries a clone's spec, the extent is DstOff, Len
	KindOffloaded   Kind = 8  // marks one extent offloaded
	KindUnmark      Kind = 9  // unmarks one extent
	KindLoss        Kind = 10 // names the exact record a loss dropped
	KindHold        Kind = 11 // holds a snapshot's state at a cut
	KindUnhold      Kind = 12 // releases the hold a cut held
	KindStamp       Kind = 13 // stamps the cut the existence commit is for
	KindForget      Kind = 14 // forgets a share, naming only its tag
)

// known reports whether k is one of the kinds this format version defines.
func (k Kind) known() bool {
	return k >= KindWrite && k <= KindForget
}

// bodySize returns the bytes a record of kind k leaves after its header and
// before its padding: write an 8-byte modification time plus the length's
// payload bytes; fill the length's payload bytes; loss 16, clone target 56,
// hold, unhold and stamp 8; and none otherwise. panics on an unknown kind —
// no caller may ask how much room to step over a record it cannot name.
func bodySize(k Kind, length int64) int64 {
	switch k {
	case KindWrite:
		return 8 + length
	case KindFill:
		return length
	case KindLoss:
		return 16
	case KindCloneTarget:
		return 56
	case KindHold, KindUnhold, KindStamp:
		return 8
	case KindRelease, KindTruncate, KindDeallocate, KindDelete, KindOffloaded, KindUnmark, KindForget:
		return 0
	default:
		panic(fmt.Sprintf("format: body size of unknown kind %d", uint8(k)))
	}
}

// LossReason is why a loss record drops the record it names: corruption, a
// stale offload mark, a failed sync window, or a superseding record. The
// 0 it would need to name no reason is refused as damage, so no loss record
// ever reads as reasonless.
type LossReason uint8

const (
	LossCorrupt    LossReason = 1 // the record was found corrupt
	LossStale      LossReason = 2 // the record's offload mark was stale
	LossFailedSync LossReason = 3 // a failed sync window failed the record
	// decision: reason 4 is accepted ahead of the format table, which
	// documents reasons 1 to 3 and leaves the rest zero. A superseding
	// record is a fourth way a record becomes unreachable, this codec
	// already writes it here, and refusing it would strand a record this
	// very format produced. The bound is LossReason.known's ceiling and the
	// refusal text "%w: loss reason %d, want one of 1-4" and its tests: if
	// the format's owner rules otherwise — rejects reason 4, renumbers it,
	// or widens the range — known, the refusal text and those tests all
	// change with it.
	LossSuperseded LossReason = 4 // a superseding record replaced it
)

// known reports whether r is a reason a loss record can name.
func (r LossReason) known() bool {
	return r >= LossCorrupt && r <= LossSuperseded
}

// Loss is a loss record's body: which record it dropped and why. Its flag
// word carries the synced bit and the offloaded bit of the record dropped,
// and the reason in its second byte.
type Loss struct {
	Dropped   core.Seq // the sequence number of the record dropped
	Synced    bool     // whether the dropped record was synced when dropped
	Offloaded bool     // whether the dropped record's offloaded bit was set
	Reason    LossReason
}

// CloneSource is a clone target record's body: where the cloned bytes come
// from. The header's file offset and length are the spec's DstOff and Len.
type CloneSource struct {
	Share core.ShareID // the source share
	File  core.FileID  // the source file
	Off   int64        // offset of the source extent
	AsOf  core.Version // content version of the source to copy from
}

// RecordBody is a record's body, of whichever fields its kind carries; every
// other field is zero. A kind never reads a body a later format version adds.
type RecordBody struct {
	// MTime is the modification time a write sets, nanoseconds since the
	// Unix epoch, or zero when the write's open suspended time updates.
	MTime int64

	// Payload is the content a write or fill stages, the header's length
	// in bytes. For a write it is the bytes after the modification time.
	Payload []byte

	// Loss is a loss record's dropped record and flags.
	Loss Loss

	// Clone is a clone target's source.
	Clone CloneSource

	// Cut is the cut number a hold, unhold or stamp records.
	Cut core.SnapshotCut
}

// checkFields refuses a header whose fields the kinds table gives as 0, and
// whose required extents are not what its kind spells. A field the table
// gives as 0 is written as zero, so a header holding more is not one the
// journal could have written.
//
// decision: the zero-field rules are refused as damage, the stricter
// reading — the kinds table writes a field as "0" and says no more about a
// reader's duty. Relax them, field by field, if the format's owner rules
// that a reader ignores them.
func checkFields(h RecordHeader) error {
	switch h.Kind {
	case KindTruncate:
		if h.Extent.Len != 0 {
			return fmt.Errorf("%w: truncate length %d, want 0", ErrDamaged, h.Extent.Len)
		}
	case KindDeallocate, KindCloneTarget:
		// 0 is how truncate and delete are spelled, so a zero-length one
		// here cannot mean "the whole rest of the file" — it is refused
		// at the API and never written.
		if h.Extent.Len == 0 {
			return fmt.Errorf("%w: %s length 0, want more", ErrDamaged, h.Kind)
		}
	case KindDelete, KindHold, KindStamp:
		if h.Extent.Off != 0 {
			return fmt.Errorf("%w: %s file offset %d, want 0", ErrDamaged, h.Kind, h.Extent.Off)
		}
		if h.Extent.Len != 0 {
			return fmt.Errorf("%w: %s length %d, want 0", ErrDamaged, h.Kind, h.Extent.Len)
		}
	case KindUnhold, KindForget:
		if h.Extent.Off != 0 {
			return fmt.Errorf("%w: %s file offset %d, want 0", ErrDamaged, h.Kind, h.Extent.Off)
		}
		if h.Extent.Len != 0 {
			return fmt.Errorf("%w: %s length %d, want 0", ErrDamaged, h.Kind, h.Extent.Len)
		}
		if h.File != (core.FileID{}) {
			return fmt.Errorf("%w: %s names the FileID % x, want zeros", ErrDamaged, h.Kind, h.File.Bytes())
		}
		if h.Version != (core.Version{}) {
			return fmt.Errorf("%w: %s names version %+v, want zeros", ErrDamaged, h.Kind, h.Version)
		}
	case KindWrite, KindFill, KindRelease, KindOffloaded, KindUnmark, KindLoss:
		// Lengths and sequence numbers are unconstrained.
	}

	// Every kind but unhold and forget names a file.
	if h.Kind != KindUnhold && h.Kind != KindForget && h.File == (core.FileID{}) {
		return fmt.Errorf("%w: %s names no FileID, want one", ErrDamaged, h.Kind)
	}
	return nil
}

// String names the kinds, so a failing test and a refusal log read which kind
// a record held.
func (k Kind) String() string {
	switch k {
	case KindWrite:
		return "write"
	case KindFill:
		return "fill"
	case KindRelease:
		return "release"
	case KindTruncate:
		return "truncate"
	case KindDeallocate:
		return "deallocate"
	case KindDelete:
		return "delete"
	case KindCloneTarget:
		return "clone target"
	case KindOffloaded:
		return "offloaded"
	case KindUnmark:
		return "unmark"
	case KindLoss:
		return "loss"
	case KindHold:
		return "hold"
	case KindUnhold:
		return "unhold"
	case KindStamp:
		return "stamp"
	case KindForget:
		return "forget"
	}
	return fmt.Sprintf("unknown %d", uint8(k))
}
