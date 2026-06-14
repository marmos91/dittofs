package fs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// hashFor returns the BLAKE3-256 content hash of payload, matching the
// addressing scheme used by StoreChunk/CommitChunks.
func hashFor(payload []byte) block.ContentHash {
	sum := blake3.Sum256(payload)
	var h block.ContentHash
	copy(h[:], sum[:])
	return h
}

// seedChunks writes n distinct chunks into bc and returns their hashes
// in insertion order. Each chunk's bytes are deterministic in i so the
// test fixture is reproducible.
func seedChunks(t *testing.T, bc *FSStore, n int) []block.ContentHash {
	t.Helper()
	ctx := context.Background()
	hashes := make([]block.ContentHash, 0, n)
	for i := 0; i < n; i++ {
		data := []byte(fmt.Sprintf("ListUnsynced fixture chunk %d body bytes", i))
		h := hashFor(data)
		if err := bc.StoreChunk(ctx, h, data); err != nil {
			t.Fatalf("StoreChunk(%d): %v", i, err)
		}
		hashes = append(hashes, h)
	}
	return hashes
}

// collectIter drains the iterator into a (hashes, errs) tuple. Caller
// chooses what to assert.
func collectIter(it func(yield func(block.ContentHash, error) bool)) ([]block.ContentHash, []error) {
	var hashes []block.ContentHash
	var errs []error
	for h, err := range it {
		hashes = append(hashes, h)
		errs = append(errs, err)
	}
	return hashes, errs
}

// sortHashes returns a sorted copy of in for set-equality comparison.
func sortHashes(in []block.ContentHash) []block.ContentHash {
	out := make([]block.ContentHash, len(in))
	copy(out, in)
	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(out[i][:], out[j][:]) < 0
	})
	return out
}

func hashSetEqual(a, b []block.ContentHash) bool {
	if len(a) != len(b) {
		return false
	}
	as := sortHashes(a)
	bs := sortHashes(b)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// TestFSStore_ListUnsynced exercises the contract on every behavioral
// edge: empty store, nil-syncedStore, all-synced, all-unsynced
// partial, and mid-iter ctx cancellation.
func TestFSStore_ListUnsynced(t *testing.T) {
	t.Run("EmptyStore", func(t *testing.T) {
		shs := memory.NewMemoryMetadataStoreWithDefaults()
		bc := newFSStoreForTest(t, FSStoreOptions{SyncedHashStore: shs})

		got, errs := collectIter(bc.ListUnsynced(context.Background()))
		if len(got) != 0 {
			t.Fatalf("expected zero items on empty store, got %d", len(got))
		}
		for _, e := range errs {
			if e != nil {
				t.Fatalf("unexpected error on empty store: %v", e)
			}
		}
	})

	t.Run("NilSyncedHashStore", func(t *testing.T) {
		// No SyncedHashStore wired — strict-subset invariant says
		// "no synced markers" collapses to empty iterator.
		bc := newFSStoreForTest(t, FSStoreOptions{})
		_ = seedChunks(t, bc, 5)

		got, errs := collectIter(bc.ListUnsynced(context.Background()))
		if len(got) != 0 {
			t.Fatalf("expected zero items with nil SyncedHashStore, got %d", len(got))
		}
		for _, e := range errs {
			if e != nil {
				t.Fatalf("unexpected error with nil SyncedHashStore: %v", e)
			}
		}
	})

	t.Run("AllSynced", func(t *testing.T) {
		shs := memory.NewMemoryMetadataStoreWithDefaults()
		bc := newFSStoreForTest(t, FSStoreOptions{SyncedHashStore: shs})
		hashes := seedChunks(t, bc, 4)
		ctx := context.Background()
		for _, h := range hashes {
			if err := shs.MarkSynced(ctx, h); err != nil {
				t.Fatalf("MarkSynced: %v", err)
			}
		}

		got, errs := collectIter(bc.ListUnsynced(ctx))
		if len(got) != 0 {
			t.Fatalf("expected zero unsynced items when all marked, got %d", len(got))
		}
		for _, e := range errs {
			if e != nil {
				t.Fatalf("unexpected error: %v", e)
			}
		}
	})

	t.Run("AllUnsynced", func(t *testing.T) {
		shs := memory.NewMemoryMetadataStoreWithDefaults()
		bc := newFSStoreForTest(t, FSStoreOptions{SyncedHashStore: shs})
		hashes := seedChunks(t, bc, 4)

		got, errs := collectIter(bc.ListUnsynced(context.Background()))
		for _, e := range errs {
			if e != nil {
				t.Fatalf("unexpected error: %v", e)
			}
		}
		if !hashSetEqual(got, hashes) {
			t.Fatalf("AllUnsynced: yielded set differs from inserted set\n got: %v\nwant: %v", got, hashes)
		}
	})

	t.Run("Partial", func(t *testing.T) {
		shs := memory.NewMemoryMetadataStoreWithDefaults()
		bc := newFSStoreForTest(t, FSStoreOptions{SyncedHashStore: shs})
		hashes := seedChunks(t, bc, 5)
		ctx := context.Background()

		// Mark two as synced; remaining three must be the yielded set.
		synced := []block.ContentHash{hashes[1], hashes[3]}
		for _, h := range synced {
			if err := shs.MarkSynced(ctx, h); err != nil {
				t.Fatalf("MarkSynced: %v", err)
			}
		}
		want := []block.ContentHash{hashes[0], hashes[2], hashes[4]}

		got, errs := collectIter(bc.ListUnsynced(ctx))
		for _, e := range errs {
			if e != nil {
				t.Fatalf("unexpected error: %v", e)
			}
		}
		if !hashSetEqual(got, want) {
			t.Fatalf("Partial: yielded set differs from expected\n got: %v\nwant: %v", got, want)
		}
	})

	t.Run("CtxCancelMidIter", func(t *testing.T) {
		shs := memory.NewMemoryMetadataStoreWithDefaults()
		bc := newFSStoreForTest(t, FSStoreOptions{SyncedHashStore: shs})
		_ = seedChunks(t, bc, 10)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var seen int
		var sawCtxErr bool
		for h, err := range bc.ListUnsynced(ctx) {
			_ = h
			if err != nil {
				if errors.Is(err, context.Canceled) {
					sawCtxErr = true
				} else {
					t.Fatalf("unexpected non-ctx error mid-iter: %v", err)
				}
				break
			}
			seen++
			if seen == 1 {
				// Cancel after first successful yield; the next loop
				// iteration must observe ctx.Err() and yield it.
				cancel()
			}
		}
		if seen < 1 {
			t.Fatalf("expected at least one item before cancellation, got %d", seen)
		}
		if !sawCtxErr {
			t.Fatalf("expected context.Canceled to be yielded after cancel, got none")
		}
	})
}

// TestFSStore_Walk_StopWalkSentinel pins the public Walk contract:
//
//   - returning block.ErrStopWalk from the callback (raw or wrapped
//     via %w) causes Walk to exit cleanly (nil).
//   - returning io.EOF from the callback must NOT be mistaken for a
//     clean exit — io.EOF is just another error and must halt Walk with
//     the wrapped "walk halted at %s: %w" form.
//   - returning any other non-nil error halts Walk and propagates the
//     wrapped error.
//
// Regression guard for the I-7 bug: using io.EOF as the internal
// short-circuit token silently swallowed legitimate io.EOF returns
// from callbacks.
func TestFSStore_Walk_StopWalkSentinel(t *testing.T) {
	t.Run("CleanExitOnErrStopWalk", func(t *testing.T) {
		bc := newFSStoreForTest(t, FSStoreOptions{})
		_ = seedChunks(t, bc, 5)

		var seen int
		err := bc.Walk(context.Background(), func(_ block.ContentHash, _ block.Meta) error {
			seen++
			if seen == 2 {
				return block.ErrStopWalk
			}
			return nil
		})
		if err != nil {
			t.Fatalf("ErrStopWalk should cause clean nil exit; got %v", err)
		}
		if seen != 2 {
			t.Fatalf("expected callback to stop after 2 calls, got %d", seen)
		}
	})

	t.Run("CleanExitOnWrappedErrStopWalk", func(t *testing.T) {
		// Callers idiomatically wrap ErrStopWalk via fmt.Errorf("%w", ...)
		// (see pkg/block/errors.go). errors.Is must detect through
		// the wrap and still trigger clean exit.
		bc := newFSStoreForTest(t, FSStoreOptions{})
		_ = seedChunks(t, bc, 3)

		err := bc.Walk(context.Background(), func(h block.ContentHash, _ block.Meta) error {
			return fmt.Errorf("gc target %s: %w", h, block.ErrStopWalk)
		})
		if err != nil {
			t.Fatalf("wrapped ErrStopWalk should cause clean nil exit; got %v", err)
		}
	})

	t.Run("EOFHaltsWithWrappedError", func(t *testing.T) {
		// Regression guard: a callback returning io.EOF must NOT be
		// confused with the public ErrStopWalk sentinel. It is just
		// another error and must halt Walk with the wrapped form.
		bc := newFSStoreForTest(t, FSStoreOptions{})
		_ = seedChunks(t, bc, 3)

		err := bc.Walk(context.Background(), func(_ block.ContentHash, _ block.Meta) error {
			return io.EOF
		})
		if err == nil {
			t.Fatalf("io.EOF from callback must halt Walk with a wrapped error, got nil")
		}
		if !errors.Is(err, io.EOF) {
			t.Fatalf("returned error must wrap io.EOF; got %v", err)
		}
		if errors.Is(err, block.ErrStopWalk) {
			t.Fatalf("returned error must NOT match ErrStopWalk; got %v", err)
		}
	})

	t.Run("ArbitraryErrorHaltsWithWrappedError", func(t *testing.T) {
		bc := newFSStoreForTest(t, FSStoreOptions{})
		_ = seedChunks(t, bc, 3)

		boom := errors.New("boom")
		err := bc.Walk(context.Background(), func(_ block.ContentHash, _ block.Meta) error {
			return boom
		})
		if err == nil {
			t.Fatalf("non-ErrStopWalk error must halt Walk, got nil")
		}
		if !errors.Is(err, boom) {
			t.Fatalf("returned error must wrap the callback's error; got %v", err)
		}
	})
}
