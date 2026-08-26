package engine_test

import (
	"bytes"
	"context"
	"math/rand"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// overwrite is one [from, to) range to rewrite, chosen from the manifest as it
// stands when its turn comes so a test can aim at a row an earlier overwrite
// produced.
type overwrite func(refs []block.ChunkRef) (from, to uint64)

// clobberFixture carves and syncs an 8 MiB file, then applies each overwrite in
// turn — evicting before it so the write lands on cold bytes, carving after it,
// and evicting again — and finally reads the whole file back cold and compares.
//
// Evicting before each overwrite is what makes the carve run stop where it does:
// with the bytes past the write still warm, carve extends the run to the end of
// the row it lands in, and neither the clobber nor the straddle arises.
//
// The read covers the whole file rather than the written range, so a stranded
// stretch is caught wherever it ends up.
func clobberFixture(t *testing.T, ms metadata.Store, share string, writes ...overwrite) {
	t.Helper()
	ctx := context.Background()
	bs := newEngineWithRemote(t, ms, remotememory.New())
	root := createShare(t, ms, share)
	pid, _ := createRealFile(t, ms, share, share+".bin", root)

	const fileSize = 8 * 1024 * 1024
	seed := make([]byte, fileSize)
	rand.New(rand.NewSource(0x2136)).Read(seed) //nolint:gosec // deterministic fixture
	if _, err := bs.WriteAt(ctx, pid, nil, seed, 0); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	carve(t, bs, ctx, pid)

	want := append([]byte{}, seed...)
	rng := rand.New(rand.NewSource(0x2136BEEF)) //nolint:gosec // deterministic fixture
	for n, write := range writes {
		refs := manifestRefs(t, ms, pid)
		if len(refs) < 4 {
			t.Fatalf("overwrite %d: fixture needs at least 4 manifest rows, got %d", n, len(refs))
		}
		d0, d1 := write(refs)
		if _, err := bs.DrainLocalSynced(ctx); err != nil {
			t.Fatalf("overwrite %d: evict before write: %v", n, err)
		}
		ow := make([]byte, d1-d0)
		rng.Read(ow)
		copy(want[d0:d1], ow)
		if _, err := bs.WriteAt(ctx, pid, nil, ow, d0); err != nil {
			t.Fatalf("overwrite %d: write [%d, %d): %v", n, d0, d1, err)
		}
		carve(t, bs, ctx, pid)
	}
	if _, err := bs.DrainLocalSynced(ctx); err != nil {
		t.Fatalf("evict before read: %v", err)
	}

	got := make([]byte, fileSize)
	if _, err := bs.ReadAt(ctx, pid, got, 0); err != nil {
		t.Fatalf("cold read: %v — a range the manifest no longer covers reads as "+
			"an error rather than as bytes", err)
	}
	if bytes.Equal(got, want) {
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("cold read differs at offset %d: got %#x, want %#x (pre-overwrite byte was %#x)",
				i, got[i], want[i], seed[i])
		}
	}
}

// atRowStart overwrites the first 8 KiB of the row at index i, so the fresh row
// takes that row's key and stops far short of its end.
func atRowStart(i int) overwrite {
	return func(refs []block.ChunkRef) (uint64, uint64) {
		return refs[i].Offset, refs[i].Offset + 8192
	}
}

// runClobberedRowColdRead drives the defect. The overwrite starts EXACTLY on an
// old row's offset, so the fresh row takes that row's key, and Put is an upsert
// — the replaced row is gone before the run-end reap ever lists the manifest,
// and everything between the fresh row's end and the replaced row's end has no
// cover at all.
func runClobberedRowColdRead(t *testing.T, ms metadata.Store) {
	clobberFixture(t, ms, "clobber", atRowStart(1))
}

// runClobberedRemnantColdRead drives the same defect a second time at the same
// place: the first overwrite leaves the replaced row's tail re-keyed to the
// fresh row's end, and the second overwrite starts exactly THERE, so the row it
// clobbers is itself a remnant. What survives has to read the original chunk
// from further in still, which is what says the preserved in-chunk start
// accumulates rather than being recomputed from the chunk's head.
func runClobberedRemnantColdRead(t *testing.T, ms metadata.Store) {
	clobberFixture(t, ms, "remnant",
		atRowStart(1),
		// The remnant sits at index 2 once the fresh row has taken index 1's key.
		atRowStart(2),
	)
}

// runLongRunColdRead is a control, and it passes with or without the fix: the
// run starts on a row's offset but is long enough to tile past that row's end,
// so nothing of it is left to strand. It is here because it also ends inside a
// LATER row that starts inside the run, which the reap narrows off its head —
// the two moves have to compose, and this is what says they do.
func runLongRunColdRead(t *testing.T, ms metadata.Store) {
	clobberFixture(t, ms, "longrun", func(refs []block.ChunkRef) (uint64, uint64) {
		return refs[1].Offset, refs[3].Offset + 8192
	})
}

// runInteriorStartColdRead is a control, and it passes with or without the fix:
// the same overwrite shape but starting INSIDE a row rather than on its offset,
// so no key is taken over and nothing is clobbered.
func runInteriorStartColdRead(t *testing.T, ms metadata.Store) {
	clobberFixture(t, ms, "interiorstart", func(refs []block.ChunkRef) (uint64, uint64) {
		return refs[1].Offset + 4096, refs[3].Offset + 8192
	})
}

func TestMemoryColdRead_ClobberedRow(t *testing.T) { runClobberedRowColdRead(t, memStore()) }
func TestMemoryColdRead_ClobberedRemnant(t *testing.T) {
	runClobberedRemnantColdRead(t, memStore())
}
func TestMemoryColdRead_LongRun(t *testing.T)       { runLongRunColdRead(t, memStore()) }
func TestMemoryColdRead_InteriorStart(t *testing.T) { runInteriorStartColdRead(t, memStore()) }

func TestBadgerColdRead_ClobberedRow(t *testing.T) { runClobberedRowColdRead(t, badgerStore(t)) }
func TestBadgerColdRead_ClobberedRemnant(t *testing.T) {
	runClobberedRemnantColdRead(t, badgerStore(t))
}
func TestBadgerColdRead_LongRun(t *testing.T) { runLongRunColdRead(t, badgerStore(t)) }
func TestBadgerColdRead_InteriorStart(t *testing.T) {
	runInteriorStartColdRead(t, badgerStore(t))
}

func memStore() metadata.Store { return metadatamemory.NewMemoryMetadataStoreWithDefaults() }

func badgerStore(t *testing.T) metadata.Store {
	t.Helper()
	ms, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	// Cleanup (not defer) so the engine's Close joins the syncer workers before
	// the metadata store closes.
	t.Cleanup(func() { _ = ms.Close() })
	return ms
}
