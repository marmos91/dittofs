package core

// CloneSpec describes the range a clone copies into its destination: Len bytes
// of Src, in share SrcShare, from SrcOff, as of Src's change version AsOf,
// placed at DstOff of the destination. A clone target's removal marker keeps
// it, so a restart that re-applies the marker resumes the clone rather than
// leaving the destination reading zeros.
type CloneSpec struct {
	SrcShare       ShareID
	Src            FileID
	SrcOff, DstOff int64
	Len            int64
	AsOf           Version
}
