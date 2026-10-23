package journalv2

// Hold records, for snapshot cut cut of share, a hold mark per file: the
// version each file's existence has committed up to. Content at or below a
// file's mark whose offloaded bit is unset is kept, and offered as
// superseded, until it is offloaded under the cut, whatever supersedes it.
// The marks are durable before Hold returns, and recovery rebuilds them.
//
// Its record is refused with a NoSpaceError naming LimitHeadroom once less
// than half the reserved headroom remains.
func (h *Handle) Hold(share ShareID, cut SnapshotCut, marks map[FileID]Version) error {
	return ErrNotImplemented
}

// Unhold releases cut's hold of share, when the cut is aborted or its
// snapshot deleted, keeping what another live cut's hold still covers.
// Released content nothing else keeps is reclaimable like any superseded
// record.
func (h *Handle) Unhold(share ShareID, cut SnapshotCut) error {
	return ErrNotImplemented
}

// Held returns the bytes share's holds keep, from a maintained counter.
//
// decision: the stub returns 0 because the signature has no error to carry
// ErrNotImplemented. Zero reads as "nothing held", so no caller may depend on
// it until holds land.
func (h *Handle) Held(share ShareID) int64 {
	return 0
}

// Stamp records the cut an existence commit read, for every version of file
// id it covered up to through. An offer carries it with each extent, as
// OfferedExtent.Cut, for the offload commit to copy into the ref as born. It is a record made durable by the file's next
// Sync. It carries a version already issued, so the version limit never
// refuses it; it is refused with a NoSpaceError naming LimitHeadroom once less
// than half the reserved headroom remains, and a refused or failed Stamp
// records nothing.
func (h *Handle) Stamp(id FileID, through Version, cut SnapshotCut) error {
	return ErrNotImplemented
}
