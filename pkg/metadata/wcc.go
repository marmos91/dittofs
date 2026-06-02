package metadata

// DirWcc carries the parent-directory attributes captured atomically with a
// mutating metadata operation, for protocol weak-cache-consistency (WCC) data.
//
// Before holds the directory attributes as they were immediately before the
// mutation; After holds them immediately after. Both are captured inside the
// same store transaction that performs the mutation, so the pair is guaranteed
// to bracket the operation with no intervening modification — closing the
// time-of-check/time-of-use window that exists when a handler reads the
// directory separately (a stale Before could otherwise describe a state that
// never preceded the operation, corrupting the client's cached view).
//
// Either field may be nil if the directory attributes could not be captured
// (e.g. an early error); callers must tolerate nil.
type DirWcc struct {
	// Before is a copy of the parent directory attributes prior to the mutation.
	Before *FileAttr

	// After is a copy of the parent directory attributes after the mutation.
	After *FileAttr
}

// RenameWcc carries the source- and destination-directory attributes captured
// atomically with a Move/rename, for protocol WCC data on both directories (H9).
// FromDir and ToDir are the same DirWcc instance when the move is intra-directory.
type RenameWcc struct {
	// FromDir holds the source parent directory's pre/post attributes.
	FromDir *DirWcc

	// ToDir holds the destination parent directory's pre/post attributes.
	ToDir *DirWcc
}
