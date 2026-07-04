package fs

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// hashFromHex builds a ContentHash from a 64-char hex string for tests.
func hashFromHex(t *testing.T, s string) block.ContentHash {
	t.Helper()
	var h block.ContentHash
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != block.HashSize {
		t.Fatalf("bad test hex %q: %v", s, err)
	}
	copy(h[:], b)
	return h
}

func TestChunkStore_RoundTrip(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	h := hashFromHex(t, strings.Repeat("ab", 32))
	data := bytes.Repeat([]byte{0xAB}, 4096)
	ctx := context.Background()

	if err := bc.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}
	exists, err := bc.HasChunk(ctx, h)
	if err != nil || !exists {
		t.Fatalf("HasChunk: exists=%v err=%v", exists, err)
	}
	got, err := bc.ReadChunk(ctx, h)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("round-trip data mismatch")
	}
}

// TestChunkStore_RoundTrip_OddSize exercises the log-blob read path with a
// chunk whose length is odd and not a power of two, so the ReadChunk buffer
// must be sized exactly from the recorded location's RawLength. Bytes must be
// returned byte-identical.
func TestChunkStore_RoundTrip_OddSize(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	h := hashFromHex(t, strings.Repeat("3e", 32))
	// 7919 is prime: odd, not a power of two, larger than any round buffer.
	data := make([]byte, 7919)
	for i := range data {
		data[i] = byte(i*31 + 7)
	}
	ctx := context.Background()

	if err := bc.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}
	got, err := bc.ReadChunk(ctx, h)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if len(got) != len(data) {
		t.Fatalf("odd-size read length: got %d want %d", len(got), len(data))
	}
	if !bytes.Equal(got, data) {
		t.Fatal("odd-size round-trip data mismatch")
	}
}

func TestChunkStore_Idempotent(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	h := hashFromHex(t, strings.Repeat("cd", 32))
	data := bytes.Repeat([]byte{0xCD}, 256)
	ctx := context.Background()

	if err := bc.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("first StoreChunk: %v", err)
	}
	if err := bc.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("second StoreChunk (idempotent): %v", err)
	}
	// Verify bytes still match after the idempotent re-store.
	got, err := bc.ReadChunk(ctx, h)
	if err != nil {
		t.Fatalf("ReadChunk after idempotent store: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("data mismatch after idempotent store")
	}
}

func TestChunkStore_ReadMissing_ErrChunkNotFound(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	h := hashFromHex(t, strings.Repeat("ef", 32))
	_, err := bc.ReadChunk(context.Background(), h)
	if !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("want ErrChunkNotFound, got %v", err)
	}
}

func TestChunkStore_DeleteMissing_NoError(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	h := hashFromHex(t, strings.Repeat("12", 32))
	if err := bc.DeleteChunk(context.Background(), h); err != nil {
		t.Fatalf("DeleteChunk on missing: want nil, got %v", err)
	}
}

// ---: OnChunkComplete wire-in in StoreChunk. ---
//
// StoreChunk fires OnChunkComplete after the chunk's bytes are recorded in the
// index on every successful chunk write. The tests below pin the producer-side
// contract: exactly-once on success, never on error, and nil-safe. Log-blob
// chunks have no per-chunk on-disk path, so the callback's path argument is
// always empty.

// TestChunkstore_OnChunkComplete_FiresAfterSuccessfulStoreChunk asserts a
// single StoreChunk fires OnChunkComplete exactly once with the (hash, data)
// pair and an empty path (logblob chunks carry no per-chunk file path).
func TestChunkstore_OnChunkComplete_FiresAfterSuccessfulStoreChunk(t *testing.T) {
	var calls atomic.Int64
	var gotHash block.ContentHash
	var gotData []byte
	var gotPath string
	cb := func(h block.ContentHash, data []byte, path string) {
		calls.Add(1)
		gotHash = h
		// Copy the slice so a later overwrite by the caller does not
		// invalidate the capture (matches Cache.Put's heap-copy semantics).
		gotData = append([]byte(nil), data...)
		gotPath = path
	}
	bc := newFSStoreForTest(t, FSStoreOptions{OnChunkComplete: cb})
	h := hashFromHex(t, strings.Repeat("7a", 32))
	data := bytes.Repeat([]byte{0x7A}, 1024)

	if err := bc.StoreChunk(context.Background(), h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("OnChunkComplete fired %d times; want 1", got)
	}
	if gotHash != h {
		t.Fatalf("captured hash mismatch: got %s want %s", gotHash.String(), h.String())
	}
	if !bytes.Equal(gotData, data) {
		t.Fatalf("captured data byte-mismatch: len got %d want %d", len(gotData), len(data))
	}
	if gotPath != "" {
		t.Fatalf("captured path mismatch: got %q want empty (logblob chunk)", gotPath)
	}
}

// TestChunkstore_NilOnChunkComplete_NoOp asserts StoreChunk succeeds and
// does NOT panic when no callback is installed (nil-safety contract
// gated at the firing site — chunkstore.go is the producer).
func TestChunkstore_NilOnChunkComplete_NoOp(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	if cb := bc.onChunkComplete.Load(); cb == nil || cb.fn != nil {
		t.Fatalf("precondition: onChunkComplete.fn must start nil; holder=%v", cb)
	}
	h := hashFromHex(t, strings.Repeat("8b", 32))
	data := bytes.Repeat([]byte{0x8B}, 512)
	ctx := context.Background()

	if err := bc.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk with nil OnChunkComplete: %v", err)
	}
	exists, err := bc.HasChunk(ctx, h)
	if err != nil || !exists {
		t.Fatalf("chunk missing after StoreChunk: exists=%v err=%v", exists, err)
	}
}

// TestChunkstore_OnChunkComplete_FiresExactlyOnce_PerSuccessfulCall asserts
//   - Two StoreChunk calls with DIFFERENT hashes → counter == 2.
//   - A second StoreChunk for an already-stored hash short-circuits via
//     HasChunk (idempotent) and does NOT fire again — the callback fires only
//     when a fresh chunk is actually recorded.
func TestChunkstore_OnChunkComplete_FiresExactlyOnce_PerSuccessfulCall(t *testing.T) {
	var calls atomic.Int64
	cb := func(_ block.ContentHash, _ []byte, _ string) {
		calls.Add(1)
	}
	bc := newFSStoreForTest(t, FSStoreOptions{OnChunkComplete: cb})
	ctx := context.Background()

	hA := hashFromHex(t, strings.Repeat("9c", 32))
	hB := hashFromHex(t, strings.Repeat("ad", 32))
	dataA := bytes.Repeat([]byte{0x9C}, 256)
	dataB := bytes.Repeat([]byte{0xAD}, 256)

	if err := bc.StoreChunk(ctx, hA, dataA); err != nil {
		t.Fatalf("StoreChunk A: %v", err)
	}
	if err := bc.StoreChunk(ctx, hB, dataB); err != nil {
		t.Fatalf("StoreChunk B: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("after 2 distinct StoreChunk: callback fired %d times; want 2", got)
	}
	// Second store of the same hash is the idempotent path: HasChunk returns
	// true and StoreChunk returns nil before reaching the callback site. The
	// counter must NOT advance.
	if err := bc.StoreChunk(ctx, hA, dataA); err != nil {
		t.Fatalf("StoreChunk A (idempotent): %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("after idempotent re-store: callback fired %d times; want still 2", got)
	}
}

// TestChunkstore_OnChunkComplete_DoesNotFireOnError asserts that error paths
// through StoreChunk leave the callback un-invoked (never fire on error). A
// pre-cancelled context makes StoreChunk return early, before the callback
// site.
func TestChunkstore_OnChunkComplete_DoesNotFireOnError(t *testing.T) {
	var calls atomic.Int64
	cb := func(_ block.ContentHash, _ []byte, _ string) {
		calls.Add(1)
	}
	bc := newFSStoreForTest(t, FSStoreOptions{OnChunkComplete: cb})
	h := hashFromHex(t, strings.Repeat("be", 32))
	data := bytes.Repeat([]byte{0xBE}, 256)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := bc.StoreChunk(ctx, h, data); err == nil {
		t.Fatal("StoreChunk: expected error from cancelled context; got nil")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("OnChunkComplete fired %d times on error path; want 0", got)
	}
	// And the chunk must NOT be recorded.
	exists, err := bc.HasChunk(context.Background(), h)
	if err != nil {
		t.Fatalf("HasChunk: %v", err)
	}
	if exists {
		t.Fatal("chunk recorded after failed StoreChunk")
	}
}
