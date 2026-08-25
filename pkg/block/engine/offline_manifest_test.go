package engine_test

import (
	"context"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	localfs "github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// keepOpenRemote hands the engine a remote it will not tear down, so the same
// uploaded blocks are still there after the store is closed and reopened over
// the same journal directory.
type keepOpenRemote struct{ *remotememory.Store }

func (keepOpenRemote) Close() error { return nil }

var _ remote.RemoteStore = keepOpenRemote{}

// openOfflineEngine builds a journal-backed engine over dir. Called twice per
// case with the same dir and metadata store, so the second call is a restart.
func openOfflineEngine(t *testing.T, dir string, ms metadata.Store, mem *remotememory.Store) (*engine.Store, *localfs.FSStore) {
	t.Helper()
	shs, ok := ms.(metadata.SyncedHashStore)
	if !ok {
		t.Fatalf("metadata store %T does not implement metadata.SyncedHashStore", ms)
	}
	local, err := localfs.NewWithOptions(dir, 100*1024*1024, ms, localfs.FSStoreOptions{
		MaxLogBytes: 128 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	syncer := engine.NewSyncer(local, mem, ms, engine.DefaultConfig())
	syncer.SetSyncedHashStore(shs)
	syncer.SetRemoteBlockStore(mem)
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           local,
		Remote:          keepOpenRemote{mem},
		Syncer:          syncer,
		FileChunkStore:  ms,
		Coordinator:     &testCoordinator{store: ms},
		SyncedHashStore: shs,
		ReadBufferBytes: 16 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	return bs, local
}

// findColdLog returns the journal's cold.log path under dir. It fails the test
// when there is none, so a rig that silently stopped producing cold markers
// cannot pass by removing nothing.
func findColdLog(t *testing.T, dir string) string {
	t.Helper()
	var found []string
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "cold.log" {
			found = append(found, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d cold.log files under %s, want exactly 1: %v", len(found), dir, found)
	}
	return found[0]
}

// TestOfflineReadiness_LostIntervalIsNotSafe puts the readiness answer in front
// of the three states it has to tell apart, all reached the same way — write,
// carve, restart — so the only thing that varies is what the journal still
// knows on the way back up.
//
//   - resident: nothing evicted. Every byte serves locally, and the share is
//     safe. This is the control: the cross-check must not refuse a healthy
//     share.
//   - evicted: the local bytes are gone but the cold markers replayed, so the
//     index still describes the ranges. Not safe, and the existing count says
//     by how much. This is what the cross-check must not confuse with a loss.
//   - lost: the same eviction, but the cold log did not survive the restart —
//     #2084's shape, where a marker never reached the log before the segment
//     holding the only copy was unlinked. The index describes nothing, so the
//     remote-only count is zero and the pre-cross-check answer was a confident
//     "provably offline safe" for a share that can no longer serve those bytes
//     at all.
func TestOfflineReadiness_LostIntervalIsNotSafe(t *testing.T) {
	const fileSize = 8 * 1024 * 1024

	for _, tc := range []struct {
		name        string
		evict       bool
		loseColdLog bool
		wantKnown   bool
		wantSafe    bool
		wantBytes   int64
	}{
		{name: "resident", wantKnown: true, wantSafe: true},
		{name: "evicted", evict: true, wantKnown: true, wantBytes: fileSize},
		{name: "lost", evict: true, loseColdLog: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
			mem := remotememory.New()

			bs, local := openOfflineEngine(t, dir, ms, mem)
			root := createShare(t, ms, "offline")
			pid, _ := createRealFile(t, ms, "offline", "data.bin", root)

			data := make([]byte, fileSize)
			rand.New(rand.NewSource(0x0FF11E)).Read(data) //nolint:gosec // deterministic fixture
			if _, err := bs.WriteAt(ctx, pid, nil, data, 0); err != nil {
				t.Fatalf("WriteAt: %v", err)
			}
			carve(t, bs, ctx, pid)
			if tc.evict {
				if _, err := bs.DrainLocalSynced(ctx); err != nil {
					t.Fatalf("DrainLocalSynced: %v", err)
				}
			}
			// The seed marker is what the readiness gate checks before it
			// trusts the index at all; without it every case below would
			// refuse for the wrong reason.
			if err := local.MarkColdSeeded(); err != nil {
				t.Fatalf("MarkColdSeeded: %v", err)
			}

			// The manifest is the denominator the cross-check works against.
			// Assert it is non-empty before the restart, so a rig whose carve
			// stopped writing rows fails here instead of passing everything.
			rows, err := ms.ListFileChunks(ctx, pid)
			if err != nil {
				t.Fatalf("ListFileChunks: %v", err)
			}
			var placed int64
			for _, row := range rows {
				if row != nil && !row.Hash.IsZero() {
					placed += int64(row.DataSize)
				}
			}
			if placed != fileSize {
				t.Fatalf("manifest places %d bytes across %d rows, want %d", placed, len(rows), fileSize)
			}

			coldLog := ""
			if tc.evict {
				coldLog = findColdLog(t, dir)
			}
			if err := bs.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if tc.loseColdLog {
				if err := os.Remove(coldLog); err != nil {
					t.Fatalf("remove %s: %v", coldLog, err)
				}
			}

			bs, local = openOfflineEngine(t, dir, ms, mem)
			t.Cleanup(func() { _ = bs.Close() })

			// The whole input the pre-cross-check answer was built from. Read
			// it here rather than assuming it, so the "was safe before" half of
			// this test is an observation.
			coldBytes, coldRanges, err := local.ColdExtents(ctx)
			if err != nil {
				t.Fatalf("ColdExtents: %v", err)
			}
			described, err := local.DataExtents(ctx, pid, fileSize)
			if err != nil {
				t.Fatalf("DataExtents: %v", err)
			}
			var describedBytes int64
			for _, e := range described {
				describedBytes += int64(e[1] - e[0])
			}
			priorAnswer := engine.OfflineReadiness{
				RemoteOnlyBytes:  coldBytes,
				RemoteOnlyRanges: coldRanges,
				Known:            true,
			}
			t.Logf("manifest places %d bytes in %d rows; index describes %d bytes in %d extents "+
				"(%d of them cold in %d ranges); answer without the cross-check: safe=%v",
				placed, len(rows), describedBytes, len(described), coldBytes, coldRanges, priorAnswer.Safe())

			got := bs.OfflineReadiness(ctx)
			if got.Known != tc.wantKnown {
				t.Errorf("Known = %v, want %v (reason %q)", got.Known, tc.wantKnown, got.Reason)
			}
			if got.Safe() != tc.wantSafe {
				t.Errorf("Safe() = %v, want %v (reason %q)", got.Safe(), tc.wantSafe, got.Reason)
			}
			if got.RemoteOnlyBytes != tc.wantBytes {
				t.Errorf("RemoteOnlyBytes = %d, want %d", got.RemoteOnlyBytes, tc.wantBytes)
			}

			switch tc.name {
			case "lost":
				// Both halves of the discrimination, on the same run: the index
				// has nothing to say about a range the manifest still places,
				// and that silence used to read as proof of safety.
				if describedBytes != 0 {
					t.Errorf("index still describes %d bytes after the cold log was lost; "+
						"the rig did not reproduce a lost interval", describedBytes)
				}
				if !priorAnswer.Safe() {
					t.Errorf("the pre-cross-check answer was already unsafe (%d remote-only bytes), "+
						"so this case proves nothing about the new check", coldBytes)
				}
				if !strings.Contains(got.Reason, "manifest places") {
					t.Errorf("refused for some other reason than the shortfall: %q", got.Reason)
				}
			case "evicted":
				// A cold range is described, just not resident: the cross-check
				// must count it as covered, or it would refuse for every share
				// that has ever evicted and never discriminate at all.
				if describedBytes != fileSize {
					t.Errorf("index describes %d bytes of the evicted file, want %d", describedBytes, fileSize)
				}
			}
		})
	}
}

// TestOfflineReadiness_CloneIsIndeterminate pins the other way a share reaches
// a shortfall, so nobody reads the refusal as proof of a lost interval.
//
// A server-side copy writes the destination's manifest rows and creates no
// interval for them — the clone's bytes are the source's, already on the
// remote, and the read path hydrates them on demand. From the index that is
// indistinguishable from a range whose interval was lost, and the offline
// answer is the same either way: those bytes need the remote, and the index
// cannot say which case it is looking at.
//
// If the copy path ever seeds cold intervals for its destination rows, this
// test is what says so: the share would then report a remote-only count
// instead, and the refusal would be left meaning a genuine loss.
func TestOfflineReadiness_CloneIsIndeterminate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	mem := remotememory.New()
	bs, local := openOfflineEngine(t, dir, ms, mem)

	root := createShare(t, ms, "clone")
	src, _ := createRealFile(t, ms, "clone", "src.bin", root)
	dst, _ := createRealFile(t, ms, "clone", "dst.bin", root)

	const size = 4 * 1024 * 1024
	data := make([]byte, size)
	rand.New(rand.NewSource(7)).Read(data) //nolint:gosec // deterministic fixture
	if _, err := bs.WriteAt(ctx, src, nil, data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	carve(t, bs, ctx, src)
	if err := local.MarkColdSeeded(); err != nil {
		t.Fatalf("MarkColdSeeded: %v", err)
	}

	// The source alone is resident and accounted for.
	if got := bs.OfflineReadiness(ctx); !got.Safe() {
		t.Fatalf("before the clone: Safe() = false, want true (reason %q)", got.Reason)
	}

	srcRows, err := ms.ListFileChunks(ctx, src)
	if err != nil {
		t.Fatalf("ListFileChunks: %v", err)
	}
	var refs []block.ChunkRef
	for _, r := range srcRows {
		off, ok := block.ParseChunkOffset(r.ID)
		if !ok || r.Hash.IsZero() {
			continue
		}
		refs = append(refs, block.ChunkRef{Hash: r.Hash, Offset: off, Size: r.DataSize})
	}
	if len(refs) == 0 {
		t.Fatal("the source carved no placeable rows, so the clone would copy nothing")
	}
	if _, err := bs.CopyPayload(ctx, src, dst, refs); err != nil {
		t.Fatalf("CopyPayload: %v", err)
	}

	described, err := local.DataExtents(ctx, dst, size)
	if err != nil {
		t.Fatalf("DataExtents: %v", err)
	}
	if len(described) != 0 {
		t.Fatalf("the copy created %d index extents for the destination; "+
			"the shortfall this test describes no longer happens", len(described))
	}

	// The memo was filled by the safe answer above, so the store is reopened
	// over the same journal — same seed marker, same index, empty memo — rather
	// than replaced, which would refuse for want of a seed instead.
	if err := bs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	bs, local = openOfflineEngine(t, dir, ms, mem)
	t.Cleanup(func() { _ = bs.Close() })
	if !local.ColdSeeded() {
		t.Fatal("the reopened tier lost its seed marker, so the refusal below would not be the shortfall")
	}

	got := bs.OfflineReadiness(ctx)
	if got.Known || got.Safe() {
		t.Errorf("a share holding an unhydrated clone reported known=%v safe=%v", got.Known, got.Safe())
	}
	if !strings.Contains(got.Reason, "manifest places") {
		t.Errorf("refused for some other reason than the shortfall: %q", got.Reason)
	}
	t.Logf("reason: %s", got.Reason)
}
