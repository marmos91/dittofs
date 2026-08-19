package block

import (
	"strconv"
	"strings"
)

// ParseChunkOffset extracts the trailing numeric component of a FileChunk
// ID of the form "<payloadID>/<chunkOffset>" and returns
// (chunkOffset, true) on success. Returns (0, false) for malformed IDs
// (no slash, trailing slash, non-digit characters after the slash, or an
// offset that does not fit in a uint64).
func ParseChunkOffset(id string) (uint64, bool) {
	slash := strings.LastIndexByte(id, '/')
	if slash < 0 || slash == len(id)-1 {
		return 0, false
	}
	v, err := strconv.ParseUint(id[slash+1:], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
