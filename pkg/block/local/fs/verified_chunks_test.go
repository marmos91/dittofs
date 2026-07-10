package fs

import (
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
)

func hashOf(b []byte) block.ContentHash { return block.ContentHash(blake3.Sum256(b)) }

func TestVerifiedChunkSet_AddContainsAndTwoGenEviction(t *testing.T) {
	v := newVerifiedChunkSet(2) // tiny cap so we can force a generation rotation

	a, b, c, d := hashOf([]byte("a")), hashOf([]byte("b")), hashOf([]byte("c")), hashOf([]byte("d"))

	v.add(a)
	v.add(b)
	if !v.contains(a) || !v.contains(b) {
		t.Fatal("a and b should be tracked after add")
	}

	// cur is full (len 2 == max). Adding c rotates: cur{a,b} -> prev, cur = {c}.
	v.add(c)
	if !v.contains(c) {
		t.Fatal("c should be tracked")
	}
	if !v.contains(a) || !v.contains(b) {
		t.Fatal("a,b should still be reachable in prev generation")
	}

	// cur now {c}. Fill it then rotate again: adding d then e drops the {a,b} gen.
	v.add(d) // cur {c,d}
	e := hashOf([]byte("e"))
	v.add(e) // cur full -> {c,d} becomes prev, cur = {e}; {a,b} generation dropped
	if !v.contains(e) || !v.contains(c) || !v.contains(d) {
		t.Fatal("e (cur) and c,d (prev) should be tracked")
	}
	if v.contains(a) || v.contains(b) {
		t.Fatal("a,b should have been evicted with the oldest generation")
	}
}

func TestChunkTrusted_VerifiesOnceThenTrusts(t *testing.T) {
	bc := &FSStore{verified: newVerifiedChunkSet(verifiedChunkCap)}
	data := []byte("some immutable CAS chunk bytes")
	h := hashOf(data)

	// First call: not yet tracked -> hashes, matches, records, returns true.
	if !bc.chunkTrusted(h, data) {
		t.Fatal("matching hash should be trusted")
	}
	if !bc.verified.contains(h) {
		t.Fatal("a verified chunk must be recorded so later reads skip re-hashing")
	}
	// Second call: served from the set (still true) — the point of the cache.
	if !bc.chunkTrusted(h, data) {
		t.Fatal("cached chunk should stay trusted")
	}

	// A hash that does not match its bytes must never be trusted or recorded.
	wrong := hashOf([]byte("different bytes"))
	if bc.chunkTrusted(wrong, data) {
		t.Fatal("mismatched hash/bytes must not be trusted")
	}
	if bc.verified.contains(wrong) {
		t.Fatal("a failed verify must not be recorded")
	}
}
