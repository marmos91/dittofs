package journal

import "github.com/cubbit/dittofs/pkg/block/journal/internal/core"

// decision: the value types are declared in internal/core and re-exported
// here as aliases. The journal's internal packages need them, and this package
// imports those internal packages, so declaring the types here would be an
// import cycle. The cost is that documentation shows them as declared in
// internal/core.
type (
	Version        = core.Version
	FileID         = core.FileID
	Extent         = core.Extent
	Held           = core.Held
	ShareTag       = core.ShareTag
	ShareID        = core.ShareID
	SnapshotCut    = core.SnapshotCut
	JournalID      = core.JournalID
	InstallationID = core.InstallationID
	SegmentID      = core.SegmentID
	CloneSpec      = core.CloneSpec
	ChangeKind     = core.ChangeKind
	LossEvent      = core.LossEvent
	LossReason     = core.LossReason
)

// The loss reasons; see LossReason.
const (
	LossCorrupt      = core.LossCorrupt
	LossStale        = core.LossStale
	LossFailedWindow = core.LossFailedWindow
	LossSuperseded   = core.LossSuperseded
	LossAcknowledged = core.LossAcknowledged
)

// NewFileID returns the FileID of the file whose identifier is id. It is the
// only way to build a FileID other than the zero value.
func NewFileID(id [16]byte) FileID {
	return core.NewFileID(id)
}

// VersionFromBytes is the inverse of Version.Bytes.
func VersionFromBytes(b [16]byte) Version {
	return core.VersionFromBytes(b)
}
