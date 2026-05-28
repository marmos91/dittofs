package blockstore

import (
	"lukechampine.com/blake3"
)

// ObjectID is the BLAKE3 Merkle root over a file's BlockRef.Hash values
// sorted by Offset (the canonical FileAttr.Blocks invariant). Same width
// (32 bytes) and wire form as ContentHash.
//
// All-zero is the "never quiesced" sentinel: legacy files,
// partially-flushed files (some blocks Pending), and freshly-mutated
// files awaiting next quiesce. The ObjectID lookup index MUST skip
// zero-valued ObjectIDs entirely.
type ObjectID = ContentHash

// objectIDDomainPrefix is the domain-separation tag prepended to the
// BLAKE3 input so the ObjectID output space cannot collide with per-chunk
// ContentHash values. Bump to v2/v3/... if the input shape ever changes
const objectIDDomainPrefix = "dittofs:objectid:v1\x00"

// ComputeObjectID returns the BLAKE3 Merkle root over the BlockRef list
//
//	ObjectID = BLAKE3(prefix || h0 || h1 || ... || hN-1)
//
// where hi = blocks[i].Hash. The slice MUST already be sorted by Offset
// — this is the canonical FileAttr.Blocks invariant.
// ComputeObjectID does NOT re-sort: misordered input is a caller bug
// caught by the storetest "sort-stability" conformance scenario.
//
// Empty input (nil or len==0) yields the canonical "empty-file"
// ObjectID — BLAKE3 of the prefix alone. Every empty file dedups
// to one constant. Callers that wish to retain the all-zero sentinel
// for "never quiesced" semantics MUST check len(blocks)==0
// themselves before calling.
func ComputeObjectID(blocks []BlockRef) ObjectID {
	h := blake3.New(32, nil)
	_, _ = h.Write([]byte(objectIDDomainPrefix))
	for i := range blocks {
		_, _ = h.Write(blocks[i].Hash[:])
	}
	var out ObjectID
	h.Sum(out[:0])
	return out
}
