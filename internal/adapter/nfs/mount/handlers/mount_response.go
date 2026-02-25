package handlers

// MountResponseBase is the base response type embedded in Mount protocol response structures.
//
// Similar to NFSResponseBase, this provides a consistent Status field for Mount protocol
// responses that return status codes (primarily the MNT procedure).
//
// Not all Mount procedures return status:
//   - MNT: Returns status (MountOK, MountErrNoEnt, etc.)
//   - NULL, DUMP, EXPORT, UMNT, UMNTALL: Return void or data without status
//
// For procedures that don't traditionally have status, we embed this anyway and
// set Status to MountOK (0) to maintain consistency with our metrics tracking.
//
// Usage:
//
//	type MountResponse struct {
//	    MountResponseBase  // Embeds Status and GetStatus()
//	    FileHandle []byte
//	    AuthFlavors []int32
//	}
//
// This pattern provides:
//   - Consistent status tracking across all mount operations
//   - Single implementation of GetStatus() for all responses
//   - Simplified metrics collection
//   - Easy addition of new mount procedures
type MountResponseBase struct {
	// Status is the Mount protocol status code.
	// For procedures that return status (MNT), this is the actual result.
	// For procedures that don't (NULL, DUMP, etc.), this is always MountOK (0).
	Status uint32
}

// GetStatus returns the Mount protocol status code.
//
// This method is automatically available on all response types that embed MountResponseBase.
func (r *MountResponseBase) GetStatus() uint32 {
	return r.Status
}
