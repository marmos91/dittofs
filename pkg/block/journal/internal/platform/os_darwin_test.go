package platform

// closedAllocateOp is the Op of the error Allocate returns on a closed file:
// the call it would have made first.
const closedAllocateOp = "F_PREALLOCATE"
