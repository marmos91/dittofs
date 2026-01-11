package content

// ============================================================================
// Capability Detection Helpers
// ============================================================================

// AsReadAtStore returns the store as a ReadAtContentStore if it supports
// efficient random reads, otherwise returns nil.
//
// Usage:
//
//	if readAtStore := content.AsReadAtStore(store); readAtStore != nil {
//	    n, err := readAtStore.ReadAt(ctx, id, buf, offset)
//	} else {
//	    // Fall back to sequential read
//	}
func AsReadAtStore(store ContentStore) ReadAtContentStore {
	if readAt, ok := store.(ReadAtContentStore); ok {
		return readAt
	}
	return nil
}

// AsIncrementalWriteStore returns the store as an IncrementalWriteStore if it
// supports incremental writes (S3 multipart), otherwise returns nil.
//
// Usage:
//
//	if incStore := content.AsIncrementalWriteStore(store); incStore != nil {
//	    flushed, err := incStore.FlushIncremental(ctx, id, cache)
//	} else {
//	    // Use regular WriteAt
//	}
func AsIncrementalWriteStore(store ContentStore) IncrementalWriteStore {
	if inc, ok := store.(IncrementalWriteStore); ok {
		return inc
	}
	return nil
}

// StoreCapabilities describes the optional capabilities of a content store.
type StoreCapabilities struct {
	// SupportsReadAt indicates efficient random-access reads are available.
	// When true, the store implements ReadAtContentStore.
	SupportsReadAt bool

	// SupportsIncrementalWrite indicates incremental writes are available.
	// When true, the store implements IncrementalWriteStore.
	// This is typically true for S3-backed stores.
	SupportsIncrementalWrite bool
}

// GetCapabilities returns the optional capabilities of a content store.
//
// Usage:
//
//	caps := content.GetCapabilities(store)
//	if caps.SupportsReadAt {
//	    // Use range reads
//	}
func GetCapabilities(store ContentStore) StoreCapabilities {
	return StoreCapabilities{
		SupportsReadAt:           AsReadAtStore(store) != nil,
		SupportsIncrementalWrite: AsIncrementalWriteStore(store) != nil,
	}
}
