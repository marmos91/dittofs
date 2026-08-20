package block

import (
	"sort"
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

// chunkSuffixFor returns the trailing component of a FileChunk ID that belongs
// to payloadID, i.e. the single path component after "<payloadID>/", and
// reports whether the ID belongs to payloadID at all.
//
// Membership is decided by the component boundary alone, not by the component
// being a number. A row whose trailing component is not a decimal offset is
// still this payload's row — it is a damaged one, and hiding it from callers
// hides the damage. Conversely, payloadIDs are built from a share name and a
// file path and so contain slashes, so "<payloadID>/<more>/<offset>" names a
// chunk of a payload nested beneath this one and does not belong here.
func chunkSuffixFor(id, payloadID string) (string, bool) {
	rest, ok := strings.CutPrefix(id, payloadID+"/")
	if !ok || rest == "" || strings.IndexByte(rest, '/') >= 0 {
		return "", false
	}
	return rest, true
}

// ChunkOffsetFor extracts the chunk offset from a FileChunk ID belonging to
// payloadID, i.e. an ID of the exact form "<payloadID>/<chunkOffset>".
//
// It is stricter than ParseChunkOffset, which splits on the last slash and so
// cannot tell a chunk of payloadID from a chunk of a payload nested beneath
// it. It returns (0, false) for an ID that belongs to another payload and for
// one that belongs to this payload but carries no placeable offset.
func ChunkOffsetFor(id, payloadID string) (uint64, bool) {
	rest, ok := chunkSuffixFor(id, payloadID)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseUint(rest, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// ChunksForPayload keeps the rows that belong to payloadID and orders them by
// chunk offset. The returned slice is never nil.
//
// Metadata backends locate a file's chunks with a coarse prefix scan — a SQL
// LIKE on the ID, a map walk — and hand the result here; this is the
// membership test, the scan is not. A prefix over-matches two ways: SQL LIKE
// reads "_" and "%" inside the payloadID as wildcards, and a plain prefix also
// spans payloads nested beneath this one, since payloadIDs are built from a
// share name and a file path and so contain slashes. Consumers read a row's
// trailing component as its offset, so an unfiltered foreign row is credited
// to this file at that offset.
//
// A row of this payload whose trailing component is not a decimal offset is
// kept: it is this file's row and it is damaged, and dropping it would hide
// the damage from the callers whose job is to report it. It sorts as offset 0.
func ChunksForPayload(rows []*FileChunk, payloadID string) []*FileChunk {
	out := make([]*FileChunk, 0, len(rows))
	for _, r := range rows {
		if _, ok := chunkSuffixFor(r.ID, payloadID); ok {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := ChunkOffsetFor(out[i].ID, payloadID)
		b, _ := ChunkOffsetFor(out[j].ID, payloadID)
		return a < b
	})
	return out
}
