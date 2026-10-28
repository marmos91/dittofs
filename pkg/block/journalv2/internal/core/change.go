package core

// ChangeKind is what a change Since reports is: a held extent, the marker of
// one of the four removals, or a loss. The zero ChangeKind names none.
type ChangeKind uint8

const (
	// ChangeWrite is a held extent, written or filled.
	ChangeWrite ChangeKind = iota + 1
	// ChangeTruncate is the marker of a Truncate.
	ChangeTruncate
	// ChangeDeallocate is the marker of a Deallocate.
	ChangeDeallocate
	// ChangeDelete is the marker of a Delete.
	ChangeDelete
	// ChangeCloneTarget is the marker of a CloneTarget, which keeps its
	// clone's spec.
	ChangeCloneTarget
	// ChangeLoss is a synced write the journal lost: the range reads as lost
	// rather than as the file was before the write.
	ChangeLoss
)
