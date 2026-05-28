package blockstore

// ParseChunkOffset extracts the trailing numeric component of a FileBlock
// ID of the form "<payloadID>/<chunkOffset>" and returns
// (chunkOffset, true) on success. Returns (0, false) for malformed IDs
// (no slash, trailing slash, or non-digit characters after the slash).
func ParseChunkOffset(id string) (uint64, bool) {
	slash := -1
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '/' {
			slash = i
			break
		}
	}
	if slash < 0 || slash == len(id)-1 {
		return 0, false
	}
	var v uint64
	for _, c := range id[slash+1:] {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + uint64(c-'0')
	}
	return v, true
}
