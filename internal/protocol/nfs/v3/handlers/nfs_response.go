package handlers

// NFSResponseBase is the base response type embedded in all NFS v3 response structures.
//
// All NFS v3 responses must include a status code as the first field in their XDR encoding.
// By embedding this base type, we:
//   - Eliminate duplicate Status field definitions across 20+ response types
//   - Provide a single implementation of GetStatus() for all responses
//   - Ensure consistency in how status codes are handled
//   - Simplify adding new response types (just embed NFSResponseBase)
//
// Usage in response types:
//
//	type ReadResponse struct {
//	    NFSResponseBase     // Embeds Status field and GetStatus() method
//	    Attr *types.NFSFileAttr
//	    Data []byte
//	    EOF  bool
//	}
//
// The embedded Status field is automatically accessible:
//
//	resp := &ReadResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3OK}}
//	status := resp.Status  // Direct field access
//	status = resp.GetStatus()  // Or via method
//
// This pattern is Go's approach to composition-based "inheritance".
type NFSResponseBase struct {
	// Status is the NFS v3 status code for this operation.
	// This must be the first field to match XDR encoding requirements.
	//
	// Common values:
	//   - types.NFS3OK (0): Success
	//   - types.NFS3ErrNoEnt (2): File not found
	//   - types.NFS3ErrAcces (13): Permission denied
	//   - types.NFS3ErrStale (70): Stale file handle
	//   - types.NFS3ErrBadHandle (10001): Invalid file handle
	Status uint32
}

// GetStatus returns the NFS status code from the response.
//
// This method is automatically available on all response types that embed NFSResponseBase,
// satisfying the rpcResponse interface requirement without duplicating code.
func (r *NFSResponseBase) GetStatus() uint32 {
	return r.Status
}
