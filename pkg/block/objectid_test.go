package block

import (
	"testing"

	"lukechampine.com/blake3"
)

// makeHash constructs a deterministic ContentHash filled with the given byte
// pattern starting from `seed`, useful for fixture inputs.
func makeHash(seed byte) ContentHash {
	var h ContentHash
	for i := 0; i < HashSize; i++ {
		h[i] = seed + byte(i)
	}
	return h
}

// TestComputeObjectID_Empty verifies that nil and empty input both yield the
// same canonical empty-file ObjectID — BLAKE3 of just the domain prefix
// . The result MUST NOT be all-zero (the all-zero value is
// the "never quiesced" sentinel and must be distinct from any real ObjectID).
func TestComputeObjectID_Empty(t *testing.T) {
	a := ComputeObjectID(nil)
	b := ComputeObjectID([]BlockRef{})
	if a != b {
		t.Fatalf("ComputeObjectID(nil) != ComputeObjectID(empty slice): %s vs %s", a.String(), b.String())
	}
	// Determinism: a second call must produce identical output.
	c := ComputeObjectID(nil)
	if a != c {
		t.Fatalf("ComputeObjectID(nil) not deterministic: %s vs %s", a.String(), c.String())
	}
	if a.IsZero() {
		t.Fatalf("empty-input ObjectID must not collide with the all-zero sentinel; got %s", a.String())
	}
	// It must equal BLAKE3 of just the prefix.
	hh := blake3.New(32, nil)
	_, _ = hh.Write([]byte(objectIDDomainPrefix))
	var want ObjectID
	hh.Sum(want[:0])
	if a != want {
		t.Fatalf("empty ObjectID != BLAKE3(prefix): got %s want %s", a.String(), want.String())
	}
}

// TestComputeObjectID_DomainSeparation verifies that a single-block ObjectID
// is NOT equal to that block's bare Hash. The domain prefix
// keeps the ObjectID output space disjoint from per-chunk ContentHash values.
func TestComputeObjectID_DomainSeparation(t *testing.T) {
	var h0 ContentHash
	for i := range h0 {
		h0[i] = 0xAA
	}
	got := ComputeObjectID([]BlockRef{{Hash: h0, Offset: 0, Size: 4096}})
	if got == h0 {
		t.Fatalf("ObjectID must not equal bare block hash (domain prefix missing?): %s", got.String())
	}
}

// TestComputeObjectID_SortStability verifies ComputeObjectID is deterministic
// two calls on the same BlockRef slice produce identical output.
func TestComputeObjectID_SortStability(t *testing.T) {
	blocks := []BlockRef{
		{Hash: makeHash(1), Offset: 0, Size: 4096},
		{Hash: makeHash(64), Offset: 4096, Size: 4096},
		{Hash: makeHash(128), Offset: 8192, Size: 4096},
		{Hash: makeHash(192), Offset: 12288, Size: 4096},
	}
	a := ComputeObjectID(blocks)
	b := ComputeObjectID(blocks)
	if a != b {
		t.Fatalf("ComputeObjectID not deterministic across calls: %s vs %s", a.String(), b.String())
	}
}

// TestComputeObjectID_OrderSensitivity verifies that permuting the BlockRef
// list produces a different ObjectID — catches a future bug where someone
// re-sorts inside the helper.
func TestComputeObjectID_OrderSensitivity(t *testing.T) {
	blocks := []BlockRef{
		{Hash: makeHash(1), Offset: 0, Size: 4096},
		{Hash: makeHash(64), Offset: 4096, Size: 4096},
		{Hash: makeHash(128), Offset: 8192, Size: 4096},
		{Hash: makeHash(192), Offset: 12288, Size: 4096},
	}
	original := ComputeObjectID(blocks)

	swapped := make([]BlockRef, len(blocks))
	copy(swapped, blocks)
	swapped[0], swapped[1] = swapped[1], swapped[0]
	permuted := ComputeObjectID(swapped)

	if original == permuted {
		t.Fatalf("permuted BlockRef list produced same ObjectID; helper must not re-sort: %s", original.String())
	}
}

// TestComputeObjectID_MutationDiff verifies that flipping a single byte in
// one of the BlockRef hashes changes the ObjectID. Primitive collision-
// resistance check on top of BLAKE3.
func TestComputeObjectID_MutationDiff(t *testing.T) {
	blocks := []BlockRef{
		{Hash: makeHash(1), Offset: 0, Size: 4096},
		{Hash: makeHash(64), Offset: 4096, Size: 4096},
		{Hash: makeHash(128), Offset: 8192, Size: 4096},
		{Hash: makeHash(192), Offset: 12288, Size: 4096},
	}
	before := ComputeObjectID(blocks)

	mutated := make([]BlockRef, len(blocks))
	copy(mutated, blocks)
	mutated[2].Hash[5] ^= 0xFF
	after := ComputeObjectID(mutated)

	if before == after {
		t.Fatalf("flipping one byte in blocks[2].Hash did not change ObjectID: %s", before.String())
	}
}

// TestComputeObjectID_PrefixVersionFreeze freezes the wire-level output for
// a deterministic single-block input. Any change to the domain prefix, hash
// algorithm, or input shape will fail this test loudly. If a versioned
// upgrade is intentional (`dittofs:objectid:v2\x00`), update the constant
// AND add a migration plan first.
func TestComputeObjectID_PrefixVersionFreeze(t *testing.T) {
	var h ContentHash
	for i := 0; i < HashSize; i++ {
		h[i] = byte(i + 1) // 0x01, 0x02, ..., 0x20
	}
	blocks := []BlockRef{{Hash: h, Offset: 0, Size: 1}}
	got := ComputeObjectID(blocks)

	// Frozen golden value computed from BLAKE3("dittofs:objectid:v1\x00" || [0x01..0x20]).
	const goldenHex = "fe8d8ffcd948084aa6448d4a84e9c34c0329fc624bbefc8118b5f596d445af2a"
	if got.String() != goldenHex {
		t.Fatalf("ObjectID prefix/version drift: got %s want %s", got.String(), goldenHex)
	}
}
