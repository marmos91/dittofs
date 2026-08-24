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
	// Compared in place rather than against payloadID+"/": this runs per row
	// and per sort comparison, and building the prefix would allocate on each.
	if len(id) <= len(payloadID)+1 || id[:len(payloadID)] != payloadID || id[len(payloadID)] != '/' {
		return "", false
	}
	rest := id[len(payloadID)+1:]
	if strings.IndexByte(rest, '/') >= 0 {
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

// PayloadPrefixRange returns the half-open ID range [lo, hi) holding every
// FileChunk row of payloadID, for backends that locate a file's chunks by
// seeking an ordered index of row IDs.
//
// Row IDs are "<payloadID>/<chunkOffset>", so the range runs from the
// separator to the byte after it: '/' is 0x2F, '0' is 0x30, and nothing sorts
// between them, so [payloadID+"/", payloadID+"0") is exactly the set of IDs
// carrying the prefix.
//
// That equality holds only when the range is compared in BYTE order, so
// callers must pin a byte-ordered comparison rather than take the database
// default: a linguistic collation may treat punctuation as ignorable and sort
// a row out of the range.
//
// The range locates rows, it does not decide membership: it still spans
// payloads nested beneath this one, and ChunksForPayload settles what belongs.
func PayloadPrefixRange(payloadID string) (lo, hi string) {
	return payloadID + "/", payloadID + "0"
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
	// Offsets are parsed once and sorted alongside their row rather than
	// re-parsed inside the comparator, which would repeat the work O(n log n)
	// times. A row of this payload with no placeable offset keys as 0.
	type keyed struct {
		row *FileChunk
		off uint64
	}
	keys := make([]keyed, 0, len(rows))
	for _, r := range rows {
		suffix, ok := chunkSuffixFor(r.ID, payloadID)
		if !ok {
			continue
		}
		off, _ := strconv.ParseUint(suffix, 10, 64)
		keys = append(keys, keyed{row: r, off: off})
	}
	sort.SliceStable(keys, func(i, j int) bool { return keys[i].off < keys[j].off })

	out := make([]*FileChunk, len(keys))
	for i, k := range keys {
		out[i] = k.row
	}
	return out
}
