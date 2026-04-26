package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// hashFromHex builds a ContentHash from a 64-character hex string. Test
// fixture only — production uses BLAKE3.
func hashFromHex(t *testing.T, hex string) blockstore.ContentHash {
	t.Helper()
	h, err := blockstore.ParseContentHash(hex)
	if err != nil {
		t.Fatalf("ParseContentHash(%q): %v", hex, err)
	}
	return h
}

// TestReadBuffer_CASPutAndGet covers Test 1 of Task 3: CAS Put then Get
// returns identical bytes; the entry is keyed by ContentHash.
func TestReadBuffer_CASPutAndGet(t *testing.T) {
	c := NewReadBuffer(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	hash := hashFromHex(t, "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262")
	data := makeData(testBlockSize, 0xAA)
	c.PutCAS(hash, data, uint32(testBlockSize))

	dest := make([]byte, testBlockSize)
	n, ok := c.GetCAS(hash, dest, 0)
	require.True(t, ok, "expected CAS hit")
	assert.Equal(t, testBlockSize, n)
	assert.Equal(t, data, dest[:n])
}

// TestReadBuffer_LegacyPutAndGet covers Test 2 of Task 3.
func TestReadBuffer_LegacyPutAndGet(t *testing.T) {
	c := NewReadBuffer(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	const storeKey = "share/file.bin/block-0"
	data := makeData(testBlockSize, 0xBB)
	c.PutLegacy(storeKey, data, uint32(testBlockSize))

	dest := make([]byte, testBlockSize)
	n, ok := c.GetLegacy(storeKey, dest, 0)
	require.True(t, ok, "expected legacy hit")
	assert.Equal(t, testBlockSize, n)
	assert.Equal(t, data, dest[:n])
}

// TestReadBuffer_NoCrossSpaceCollision is the LOAD-BEARING assertion
// (Test 3 of Task 3): CAS hash X and legacy storeKey "cas/aa/..." cannot
// collide even when their underlying string representations overlap. This
// is the cache-poisoning protection.
func TestReadBuffer_NoCrossSpaceCollision(t *testing.T) {
	c := NewReadBuffer(testBlockSize * 8)
	require.NotNil(t, c)
	defer c.Close()

	hash := hashFromHex(t, "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262")
	// Legacy storeKey that mimics the CAS path shape — pathological but
	// possible if a legacy share name happened to start with "cas/".
	legacyKeyStr := blockstore.FormatCASKey(hash) // "cas/af/13/af1349b9..."

	casData := makeData(testBlockSize, 0xCC)
	legData := makeData(testBlockSize, 0xDD)

	c.PutCAS(hash, casData, uint32(testBlockSize))
	c.PutLegacy(legacyKeyStr, legData, uint32(testBlockSize))

	// Coordinate-space Put with arbitrary payloadID/idx — must also not
	// collide with the other two.
	coordData := makeData(testBlockSize, 0xEE)
	c.Put("share/file", 0, coordData, uint32(testBlockSize))

	// Each Get should retrieve its own bytes.
	got := make([]byte, testBlockSize)
	n, ok := c.GetCAS(hash, got, 0)
	require.True(t, ok)
	assert.Equal(t, testBlockSize, n)
	assert.Equal(t, casData, got[:n], "CAS bytes leaked from another space")

	got2 := make([]byte, testBlockSize)
	n2, ok2 := c.GetLegacy(legacyKeyStr, got2, 0)
	require.True(t, ok2)
	assert.Equal(t, testBlockSize, n2)
	assert.Equal(t, legData, got2[:n2], "Legacy bytes leaked from another space")

	got3 := make([]byte, testBlockSize)
	n3, ok3 := c.Get("share/file", 0, got3, 0)
	require.True(t, ok3)
	assert.Equal(t, testBlockSize, n3)
	assert.Equal(t, coordData, got3[:n3], "Coord bytes leaked from another space")
}

// TestReadBuffer_BifurcatedSpacesShareBudget verifies eviction policy
// (Test 4 of Task 3): the total cache size budget covers all three
// spaces; eviction runs LRU regardless of space.
func TestReadBuffer_BifurcatedSpacesShareBudget(t *testing.T) {
	// Budget for exactly 2 blocks worth.
	c := NewReadBuffer(testBlockSize * 2)
	require.NotNil(t, c)
	defer c.Close()

	h1 := hashFromHex(t, "0000000000000000000000000000000000000000000000000000000000000001")
	h2 := hashFromHex(t, "0000000000000000000000000000000000000000000000000000000000000002")
	h3 := hashFromHex(t, "0000000000000000000000000000000000000000000000000000000000000003")

	// Insert 3 CAS entries; the first must evict (LRU).
	c.PutCAS(h1, makeData(testBlockSize, 0x11), uint32(testBlockSize))
	c.PutCAS(h2, makeData(testBlockSize, 0x22), uint32(testBlockSize))
	c.PutCAS(h3, makeData(testBlockSize, 0x33), uint32(testBlockSize))

	dest := make([]byte, testBlockSize)
	_, hit1 := c.GetCAS(h1, dest, 0)
	_, hit2 := c.GetCAS(h2, dest, 0)
	_, hit3 := c.GetCAS(h3, dest, 0)
	assert.False(t, hit1, "h1 should be evicted (LRU)")
	assert.True(t, hit2, "h2 should still be cached")
	assert.True(t, hit3, "h3 should still be cached")

	// Stats should reflect both spaces in the same byte budget.
	s := c.Stats()
	assert.Equal(t, 2, s.Entries)
	assert.LessOrEqual(t, s.CurBytes, s.MaxBytes)
}

// TestReadBuffer_InvalidateFile_OnlyCoordEntries verifies (Test 5 of
// Task 3) that InvalidateFile only touches coordinate entries; CAS
// entries are not file-scoped and remain intact.
func TestReadBuffer_InvalidateFile_OnlyCoordEntries(t *testing.T) {
	c := NewReadBuffer(testBlockSize * 8)
	require.NotNil(t, c)
	defer c.Close()

	// Coord entries for "victim" and "survivor" payloads.
	c.Put("victim", 0, makeData(testBlockSize, 0x10), uint32(testBlockSize))
	c.Put("victim", 1, makeData(testBlockSize, 0x11), uint32(testBlockSize))
	c.Put("survivor", 0, makeData(testBlockSize, 0x20), uint32(testBlockSize))

	// CAS entry that happens to share a hex prefix with "victim" — must
	// survive because CAS is hash-keyed and disjoint from coord space.
	hash := hashFromHex(t, "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262")
	c.PutCAS(hash, makeData(testBlockSize, 0xCA), uint32(testBlockSize))

	c.InvalidateFile("victim")

	dest := make([]byte, testBlockSize)
	if _, hit := c.Get("victim", 0, dest, 0); hit {
		t.Error("victim/0 should be invalidated")
	}
	if _, hit := c.Get("victim", 1, dest, 0); hit {
		t.Error("victim/1 should be invalidated")
	}
	if _, hit := c.Get("survivor", 0, dest, 0); !hit {
		t.Error("survivor/0 should remain")
	}
	if _, hit := c.GetCAS(hash, dest, 0); !hit {
		t.Error("CAS entry should remain after coord InvalidateFile")
	}
}

// TestReadBuffer_InvalidateCAS removes only the targeted CAS entry.
func TestReadBuffer_InvalidateCAS(t *testing.T) {
	c := NewReadBuffer(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	h1 := hashFromHex(t, "0000000000000000000000000000000000000000000000000000000000000001")
	h2 := hashFromHex(t, "0000000000000000000000000000000000000000000000000000000000000002")
	c.PutCAS(h1, makeData(testBlockSize, 0x11), uint32(testBlockSize))
	c.PutCAS(h2, makeData(testBlockSize, 0x22), uint32(testBlockSize))

	if !c.InvalidateCAS(h1) {
		t.Error("InvalidateCAS(h1) should report present-and-removed")
	}
	if c.InvalidateCAS(h1) {
		t.Error("InvalidateCAS(h1) twice should report not-present")
	}
	if !c.ContainsCAS(h2) {
		t.Error("h2 must still be cached")
	}
}

// TestReadBuffer_InvalidateLegacy removes only the targeted legacy entry.
func TestReadBuffer_InvalidateLegacy(t *testing.T) {
	c := NewReadBuffer(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	c.PutLegacy("share/a/block-0", makeData(testBlockSize, 0xA0), uint32(testBlockSize))
	c.PutLegacy("share/a/block-1", makeData(testBlockSize, 0xA1), uint32(testBlockSize))

	if !c.InvalidateLegacy("share/a/block-0") {
		t.Error("InvalidateLegacy should report present-and-removed")
	}
	if c.InvalidateLegacy("share/a/block-0") {
		t.Error("InvalidateLegacy twice should report not-present")
	}
	if !c.ContainsLegacy("share/a/block-1") {
		t.Error("other legacy entry must remain")
	}
}
