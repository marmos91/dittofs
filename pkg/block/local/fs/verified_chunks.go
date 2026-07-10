package fs

import (
	"sync"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
)

// verifiedChunkCap bounds one generation of the verified-chunk set. Entries are
// 32-byte content hashes; two generations at this cap cost ~a few MiB per store
// and cover a working set far larger than any single payload's chunk count (a
// 64 GiB file at the ~1 MiB FastCDC average is only ~64k chunks). Per-store, so
// it does not multiply with concurrent reads, only with shares.
const verifiedChunkCap = 1 << 16

// verifiedChunkSet is a bounded set of CAS chunk hashes whose on-disk bytes have
// already been BLAKE3-verified in this process. CAS chunks are content-addressed
// and immutable: a hash that verified once stays valid for the identical bytes,
// so re-hashing the whole (~1 MiB) covering chunk on every 4 KiB read — the warm
// random-read integrity gate — is redundant work. Membership lets the read path
// verify each chunk once and skip it thereafter.
//
// Eviction is two-generation, not per-entry LRU: when the current generation
// fills, it becomes the previous one and a fresh generation starts, dropping the
// oldest half wholesale. This keeps add/contains O(1) with no list bookkeeping.
// A chunk dropped from the set is simply re-verified on its next read.
//
// ponytail: two-generation approx-LRU; swap for a real LRU only if a
// pathological churn pattern shows re-verify thrash in a profile.
type verifiedChunkSet struct {
	mu   sync.Mutex
	cur  map[block.ContentHash]struct{}
	prev map[block.ContentHash]struct{}
	max  int
}

func newVerifiedChunkSet(max int) *verifiedChunkSet {
	return &verifiedChunkSet{cur: make(map[block.ContentHash]struct{}), max: max}
}

// contains reports whether h was verified recently enough to still be tracked.
// A nil set (an FSStore built outside newFSStore, e.g. a bare test literal)
// always reports false, so callers fall back to a full verify.
func (v *verifiedChunkSet) contains(h block.ContentHash) bool {
	if v == nil {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.cur[h]; ok {
		return true
	}
	_, ok := v.prev[h]
	return ok
}

// add records h as verified, rotating generations when the current one is full.
// A nil set is a no-op (see contains).
func (v *verifiedChunkSet) add(h block.ContentHash) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.cur[h]; ok {
		return
	}
	if len(v.cur) >= v.max {
		v.prev = v.cur
		v.cur = make(map[block.ContentHash]struct{}, v.max)
	}
	v.cur[h] = struct{}{}
}

// chunkTrusted reports whether data may be served for CAS hash without a fresh
// integrity check. It short-circuits true when hash is already in the verified
// set; otherwise it BLAKE3-hashes data, records a match, and returns whether it
// matched. A false result means the local bytes are corrupt and MUST NOT be
// served — the caller routes the read to the remote-verified heal path instead.
//
// The tradeoff of skipping re-verification: once a chunk verifies, later reads
// serve it without re-hashing, so on-disk bit-rot that develops after the first
// read of a still-tracked chunk goes undetected until the entry is evicted (or
// the chunk re-fetched). In practice the first read pulls the chunk into the OS
// page cache, so subsequent reads come from RAM rather than re-touching the
// platter, and competitors serving from page cache verify nothing at all — this
// keeps a per-chunk first-touch integrity gate they lack.
func (bc *FSStore) chunkTrusted(hash block.ContentHash, data []byte) bool {
	if bc.verified.contains(hash) {
		return true
	}
	if block.ContentHash(blake3.Sum256(data)) != hash {
		return false
	}
	bc.verified.add(hash)
	return true
}
