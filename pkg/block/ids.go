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

// ChunkOffsetFor extracts the chunk offset from a FileChunk ID that belongs to
// payloadID, i.e. an ID of the exact form "<payloadID>/<chunkOffset>".
//
// It is stricter than ParseChunkOffset, which splits on the last slash and so
// cannot tell a chunk of payloadID from a chunk of a payload nested beneath it.
// PayloadIDs are built from share name and file path and therefore contain
// slashes, so "<payloadID>/<more>/<chunkOffset>" is the ID of a different
// file's chunk; it returns (0, false) here, as does any ID that is not
// prefixed by payloadID or whose trailing component is not a decimal offset.
func ChunkOffsetFor(id, payloadID string) (uint64, bool) {
	rest, ok := strings.CutPrefix(id, payloadID+"/")
	if !ok || strings.IndexByte(rest, '/') >= 0 {
		return 0, false
	}
	v, err := strconv.ParseUint(rest, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
