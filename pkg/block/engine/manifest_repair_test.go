package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// repairBackends runs a repair scenario on every metadata backend the check
// suite covers, so a repair proved on the in-memory store is also proved on a
// store that has to serialise the transaction.
var repairBackends = []string{"memory", "sqlite"}

// sqlOnlyBackends is for scenarios built around an unplaceable row. The
// in-memory store drops a row whose ID suffix is not numeric before returning
// it from ListFileChunks, so no caller there can see one — the same reason the
// scan's own unplaceable-row test is SQL-only.
var sqlOnlyBackends = []string{"sqlite"}

// resolveAt answers the question a read asks of the manifest: which row covers
// this offset? It is the read path's own resolver, so a test asserting on it
// asserts on what a reader would get rather than on a re-derived opinion.
func resolveAt(t *testing.T, store metadata.Store, payloadID string, off uint64) (*block.FileChunk, error) {
	t.Helper()
	rows, err := store.ListFileChunks(context.Background(), payloadID)
	if err != nil && !errors.Is(err, block.ErrFileChunkNotFound) {
		t.Fatalf("ListFileChunks: %v", err)
	}
	hit, err := findRowCoveringOffset(rows, off)
	if err != nil {
		return nil, err
	}
	if hit == nil {
		return nil, nil
	}
	return hit.fb, nil
}

// manifestSnapshot captures every row of a payload so a test can assert what a
// repair left behind as well as what it added.
func manifestSnapshot(t *testing.T, store metadata.Store, payloadID string) map[string]block.FileChunk {
	t.Helper()
	rows, err := store.ListFileChunks(context.Background(), payloadID)
	if err != nil && !errors.Is(err, block.ErrFileChunkNotFound) {
		t.Fatalf("ListFileChunks: %v", err)
	}
	out := make(map[string]block.FileChunk, len(rows))
	for _, r := range rows {
		if r != nil {
			out[r.ID] = *r
		}
	}
	return out
}

// assertNoBytesLost pins the property every repair has to hold: each hash that
// covered a byte range before the repair still covers a range after it. A
// repair adds coverage the file already asked for and moves a row that covered
// nothing; it must never drop one.
func assertNoBytesLost(t *testing.T, before, after map[string]block.FileChunk) {
	t.Helper()
	live := make(map[block.ContentHash]uint32, len(after))
	for _, r := range after {
		if r.DataSize > live[r.Hash] {
			live[r.Hash] = r.DataSize
		}
	}
	for id, r := range before {
		if r.Hash.IsZero() {
			continue
		}
		if live[r.Hash] < r.DataSize {
			t.Fatalf("repair lost coverage for row %s: hash %s claimed %d bytes before, %d after",
				id, r.Hash, r.DataSize, live[r.Hash])
		}
	}
}

func runRepair(t *testing.T, store metadata.Store, share string, opts ManifestCheckOptions) *ManifestCheckResult {
	t.Helper()
	res, err := CheckManifests(t.Context(), share, store, opts)
	if err != nil {
		t.Fatalf("CheckManifests: %v", err)
	}
	return res
}

// TestRepairReplacesUnplaceableRow covers the one case where the store knows
// both the bytes and where they belong: a row whose offset was lost, and a
// claim naming exactly its hash and length. Before the repair the read path
// refuses the whole payload; after it, the row resolves.
func TestRepairReplacesUnplaceableRow(t *testing.T) {
	for _, backend := range sqlOnlyBackends {
		t.Run(backend, func(t *testing.T) {
			share := "/repair"
			store, root := newCheckStore(t, backend, share)
			payloadID := seedCheckFile(t, store, share, root, "moved", 4096,
				[]seedRow{{suffix: "not-an-offset", size: 4096, hash: 1}},
				[]seedRef{{off: 0, size: 4096, hash: 1}},
			)

			if _, err := resolveAt(t, store, payloadID, 0); !errors.Is(err, block.ErrManifestInconsistent) {
				t.Fatalf("read path should refuse before the repair, got %v", err)
			}
			before := manifestSnapshot(t, store, payloadID)

			plan := runRepair(t, store, share, ManifestCheckOptions{PlanRepairs: true})
			if plan.RepairsPlanned != 1 || len(plan.Repairs) != 1 {
				t.Fatalf("want 1 planned repair, got %d: %+v", plan.RepairsPlanned, plan.Repairs)
			}
			if got := plan.Repairs[0].Kind; got != RepairReplaceRow {
				t.Fatalf("want %s, got %s", RepairReplaceRow, got)
			}
			if snap := manifestSnapshot(t, store, payloadID); len(snap) != len(before) {
				t.Fatalf("planning wrote to the store: %d rows before, %d after", len(before), len(snap))
			}

			applied := runRepair(t, store, share, ManifestCheckOptions{ApplyRepairs: true, PlanRepairs: true})
			if applied.RepairsApplied != 1 || applied.RepairsSkipped != 0 {
				t.Fatalf("want 1 applied 0 skipped, got %d/%d", applied.RepairsApplied, applied.RepairsSkipped)
			}

			row, err := resolveAt(t, store, payloadID, 0)
			if err != nil {
				t.Fatalf("read path still refuses after the repair: %v", err)
			}
			if row == nil {
				t.Fatal("no row covers offset 0 after the repair")
			}
			if row.ID != payloadID+"/0" {
				t.Fatalf("row ID = %q, want %q", row.ID, payloadID+"/0")
			}
			if row.Hash != seedHash(1) || row.DataSize != 4096 {
				t.Fatalf("repaired row carries hash %s size %d, want the claim's", row.Hash, row.DataSize)
			}
			assertNoBytesLost(t, before, manifestSnapshot(t, store, payloadID))

			after := runRepair(t, store, share, ManifestCheckOptions{})
			if after.Damaged() {
				t.Fatalf("payload still reports damage after the repair: %+v", after)
			}
		})
	}
}

// TestRepairRecreatesRowForSyncedClaim covers the case the store can answer
// without any surviving row: the file claims a range, no row covers it, and the
// synced-hash store resolves the claim's hash, so the remote holds the bytes.
func TestRepairRecreatesRowForSyncedClaim(t *testing.T) {
	for _, backend := range repairBackends {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			share := "/repair"
			store, root := newCheckStore(t, backend, share)
			payloadID := seedCheckFile(t, store, share, root, "dropped", 8192,
				[]seedRow{{suffix: "4096", size: 4096, hash: 2}},
				[]seedRef{{off: 0, size: 4096, hash: 1}, {off: 4096, size: 4096, hash: 2}},
			)
			if err := store.MarkSynced(ctx, seedHash(1), block.ChunkLocator{}); err != nil {
				t.Fatalf("MarkSynced: %v", err)
			}
			if err := store.MarkSynced(ctx, seedHash(2), block.ChunkLocator{}); err != nil {
				t.Fatalf("MarkSynced: %v", err)
			}

			if row, err := resolveAt(t, store, payloadID, 0); err != nil || row != nil {
				t.Fatalf("offset 0 should read as a hole before the repair, got row=%v err=%v", row, err)
			}
			before := manifestSnapshot(t, store, payloadID)

			applied := runRepair(t, store, share, ManifestCheckOptions{
				CheckSynced: true, PlanRepairs: true, ApplyRepairs: true,
			})
			if applied.RepairsApplied != 1 {
				t.Fatalf("want 1 applied repair, got %d: %+v", applied.RepairsApplied, applied.Repairs)
			}
			if got := applied.Repairs[0].Kind; got != RepairRecreateRow {
				t.Fatalf("want %s, got %s", RepairRecreateRow, got)
			}

			row, err := resolveAt(t, store, payloadID, 0)
			if err != nil || row == nil {
				t.Fatalf("offset 0 unresolved after the repair: row=%v err=%v", row, err)
			}
			if row.Hash != seedHash(1) || row.DataSize != 4096 {
				t.Fatalf("recreated row carries hash %s size %d, want the claim's", row.Hash, row.DataSize)
			}
			assertNoBytesLost(t, before, manifestSnapshot(t, store, payloadID))

			after := runRepair(t, store, share, ManifestCheckOptions{CheckSynced: true})
			if after.Damaged() {
				t.Fatalf("payload still reports damage after the repair: %+v", after)
			}
		})
	}
}

// TestRepairLeavesUnprovableCasesAlone pins the half of the report the command
// must not act on. Each case is damage the scan reports and the repair has no
// evidence for, so the store has to come back untouched.
func TestRepairLeavesUnprovableCasesAlone(t *testing.T) {
	cases := []struct {
		name string
		size uint64
		rows []seedRow
		refs []seedRef
		// synced names the hash seeds the synced-hash store knows.
		synced []byte
		// backends narrows the case to the stores that can host it.
		backends []string
	}{{
		// The claim's hash is not on the remote and no row carries it, so
		// nothing can serve those bytes. A row here would turn an uncovered
		// range into a failing read and recover nothing.
		name: "claim whose hash nothing resolves",
		size: 4096,
		refs: []seedRef{{off: 0, size: 4096, hash: 1}},
	}, {
		// The row's bytes exist but nothing says where they belong.
		name:     "unplaceable row matching no claim",
		size:     4096,
		rows:     []seedRow{{suffix: "not-an-offset", size: 4096, hash: 9}},
		refs:     []seedRef{{off: 0, size: 4096, hash: 1}},
		backends: sqlOnlyBackends,
	}, {
		// Two rows and two claims share a hash and length, so no pairing is
		// better founded than any other and neither hash is on the remote.
		name: "ambiguous one-to-many match",
		size: 8192,
		rows: []seedRow{
			{suffix: "not-an-offset-a", size: 4096, hash: 1},
			{suffix: "not-an-offset-b", size: 4096, hash: 1},
		},
		refs:     []seedRef{{off: 0, size: 4096, hash: 1}, {off: 4096, size: 4096, hash: 1}},
		backends: sqlOnlyBackends,
	}, {
		// A pending row already owns the offset. Writing there would drop the
		// bytes the rollup that created it is about to commit.
		name: "pending row already at the offset",
		size: 4096,
		rows: []seedRow{{suffix: "0", size: 4096, hash: 0}},
		refs: []seedRef{{off: 0, size: 4096, hash: 1}},
		// The hash is on the remote, so only the occupied offset holds
		// this case back.
		synced: []byte{1},
	}, {
		// A placeable row whose hash the synced-hash store does not know is
		// reported, but its bytes are what is missing, not its metadata.
		name: "row with an unknown hash",
		size: 4096,
		rows: []seedRow{{suffix: "0", size: 4096, hash: 3}},
		refs: []seedRef{{off: 0, size: 4096, hash: 3}},
	}}

	for _, tc := range cases {
		backends := tc.backends
		if backends == nil {
			backends = repairBackends
		}
		for _, backend := range backends {
			t.Run(tc.name+"/"+backend, func(t *testing.T) {
				ctx := t.Context()
				share := "/repair"
				store, root := newCheckStore(t, backend, share)
				payloadID := seedCheckFile(t, store, share, root, "f", tc.size, tc.rows, tc.refs)
				for _, h := range tc.synced {
					if err := store.MarkSynced(ctx, seedHash(h), block.ChunkLocator{}); err != nil {
						t.Fatalf("MarkSynced: %v", err)
					}
				}
				before := manifestSnapshot(t, store, payloadID)

				res := runRepair(t, store, share, ManifestCheckOptions{
					CheckSynced: true, PlanRepairs: true, ApplyRepairs: true,
				})
				if res.RepairsPlanned != 0 || len(res.Repairs) != 0 {
					t.Fatalf("planned %d repair(s) with no evidence: %+v", res.RepairsPlanned, res.Repairs)
				}
				if !res.Damaged() {
					t.Fatal("scan stopped reporting damage it cannot repair")
				}

				after := manifestSnapshot(t, store, payloadID)
				if len(after) != len(before) {
					t.Fatalf("manifest changed: %d rows before, %d after", len(before), len(after))
				}
				for id, row := range before {
					got, ok := after[id]
					if !ok {
						t.Fatalf("row %s was removed", id)
					}
					if got.Hash != row.Hash || got.DataSize != row.DataSize {
						t.Fatalf("row %s was rewritten: %+v -> %+v", id, row, got)
					}
				}
			})
		}
	}
}

// TestRepairSkipsWhenEvidenceMoved covers the write transaction re-establishing
// what the plan relied on. The plan is computed outside the transaction against
// a live store, so each precondition has to be able to fail there — and fail as
// a skip carrying a reason, never as a forced write.
func TestRepairSkipsWhenEvidenceMoved(t *testing.T) {
	const payloadID = "payload-f"
	hash := seedHash(1)

	base := func() (*metadata.File, map[string]*block.FileChunk) {
		return &metadata.File{
				FileAttr: metadata.FileAttr{
					Size:      4096,
					PayloadID: metadata.PayloadID(payloadID),
					Blocks:    []block.ChunkRef{{Hash: hash, Offset: 0, Size: 4096}},
				},
			}, map[string]*block.FileChunk{
				payloadID + "/x": {ID: payloadID + "/x", Hash: hash, DataSize: 4096},
			}
	}

	cases := []struct {
		name   string
		mutate func(f *metadata.File, rows map[string]*block.FileChunk, covered *[][2]uint64)
		reason string
		// kind defaults to RepairReplaceRow; the recreate branch rests on
		// the synced marker instead of a source row.
		kind RepairKind
		// synced marks the claim's hash in the synced-hash store.
		synced  bool
		applies bool
	}{{
		name:    "evidence intact",
		mutate:  func(*metadata.File, map[string]*block.FileChunk, *[][2]uint64) {},
		applies: true,
	}, {
		name:   "claim withdrawn",
		mutate: func(f *metadata.File, _ map[string]*block.FileChunk, _ *[][2]uint64) { f.Blocks = nil },
		reason: "the file no longer claims this range",
	}, {
		name:   "file truncated",
		mutate: func(f *metadata.File, _ map[string]*block.FileChunk, _ *[][2]uint64) { f.Size = 2048 },
		reason: "the file is now shorter than the range",
	}, {
		name: "offset taken",
		mutate: func(_ *metadata.File, rows map[string]*block.FileChunk, _ *[][2]uint64) {
			rows[payloadID+"/0"] = &block.FileChunk{ID: payloadID + "/0"}
		},
		reason: "a manifest row now occupies the target offset",
	}, {
		name: "range covered",
		mutate: func(_ *metadata.File, _ map[string]*block.FileChunk, covered *[][2]uint64) {
			*covered = [][2]uint64{{0, 4096}}
		},
		reason: "another row now covers the range",
	}, {
		name: "source row gone",
		mutate: func(_ *metadata.File, rows map[string]*block.FileChunk, _ *[][2]uint64) {
			delete(rows, payloadID+"/x")
		},
		reason: "the unplaceable row is gone",
	}, {
		name: "source row rewritten",
		mutate: func(_ *metadata.File, rows map[string]*block.FileChunk, _ *[][2]uint64) {
			rows[payloadID+"/x"].DataSize = 1024
		},
		reason: "the unplaceable row no longer matches the claim",
	}, {
		name:    "recreate with the marker intact",
		kind:    RepairRecreateRow,
		synced:  true,
		mutate:  func(*metadata.File, map[string]*block.FileChunk, *[][2]uint64) {},
		applies: true,
	}, {
		// The refcount cascade clears the marker of a hash nothing references,
		// and a hash whose row went missing is exactly that — so the one piece
		// of evidence a recreate rests on can disappear under the plan.
		name:   "recreate after the marker was cleared",
		kind:   RepairRecreateRow,
		mutate: func(*metadata.File, map[string]*block.FileChunk, *[][2]uint64) {},
		reason: "the claim's hash is no longer marked synced",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, rows := base()
			var covered [][2]uint64
			tc.mutate(f, rows, &covered)

			claimed := map[refKey]map[uint64]struct{}{}
			for _, ref := range f.Blocks {
				k := refKey{ref.Hash, ref.Size}
				if claimed[k] == nil {
					claimed[k] = map[uint64]struct{}{}
				}
				claimed[k][ref.Offset] = struct{}{}
			}

			kind := tc.kind
			if kind == "" {
				kind = RepairReplaceRow
			}
			action := RepairAction{
				Kind:      kind,
				PayloadID: payloadID,
				Offset:    0,
				Size:      4096,
				Hash:      hash.String(),
				ToRowID:   payloadID + "/0",
			}
			if kind == RepairReplaceRow {
				action.FromRowID = payloadID + "/x"
			}

			store, _ := newCheckStore(t, "memory", "/repair")
			if tc.synced {
				if err := store.MarkSynced(t.Context(), hash, block.ChunkLocator{}); err != nil {
					t.Fatalf("MarkSynced: %v", err)
				}
			}
			var row *block.FileChunk
			err := store.WithTransaction(t.Context(), func(tx metadata.Transaction) error {
				var terr error
				row, terr = repairRow(t.Context(), tx, &action, f, rows, claimed, covered, f.Mtime)
				return terr
			})
			if err != nil {
				t.Fatalf("repairRow: %v", err)
			}
			if tc.applies {
				if row == nil {
					t.Fatalf("intact evidence was skipped: %q", action.SkipReason)
				}
				if row.ID != action.ToRowID || row.Hash != hash || row.DataSize != 4096 {
					t.Fatalf("repaired row = %+v, want the claim under the new ID", row)
				}
				return
			}
			if row != nil {
				t.Fatal("wrote a row whose evidence had moved")
			}
			if action.SkipReason != tc.reason {
				t.Fatalf("skip reason = %q, want %q", action.SkipReason, tc.reason)
			}
		})
	}
}

// TestRepairToleratesAVanishedFile pins that a file unlinked between the scan
// and the write costs its own repairs and nothing else. Aborting the whole run
// there would let one unlink during a long scan strand every other payload's
// repair.
func TestRepairToleratesAVanishedFile(t *testing.T) {
	share := "/repair"
	store, _ := newCheckStore(t, "memory", share)

	gone := &metadata.File{
		ID:        uuid.New(),
		ShareName: share,
		FileAttr: metadata.FileAttr{
			Size:      4096,
			PayloadID: "payload-gone",
			Blocks:    []block.ChunkRef{{Hash: seedHash(1), Offset: 0, Size: 4096}},
		},
	}
	actions := []RepairAction{{
		Kind:      RepairRecreateRow,
		PayloadID: "payload-gone",
		Size:      4096,
		Hash:      seedHash(1).String(),
		ToRowID:   "payload-gone/0",
	}}

	if err := applyPayloadRepairs(t.Context(), store, gone, actions); err != nil {
		t.Fatalf("a vanished file must not fail the run: %v", err)
	}
	if actions[0].Applied {
		t.Fatal("wrote a row for a file that no longer exists")
	}
	if actions[0].SkipReason != "the file is gone" {
		t.Fatalf("skip reason = %q", actions[0].SkipReason)
	}
	if rows := manifestSnapshot(t, store, "payload-gone"); len(rows) != 0 {
		t.Fatalf("manifest gained %d row(s) for a deleted file", len(rows))
	}
}

// TestRepairDoesNotOverwriteItsOwnWrite pins that actions within one
// transaction are checked against each other's writes. Two block-list entries
// claiming the same range would otherwise both target one manifest key, and the
// second put would silently replace the first.
//
// Only the in-memory store can host it: the SQL backends key a file's block
// list by offset, so the duplicate entry never survives the write that seeds it.
func TestRepairDoesNotOverwriteItsOwnWrite(t *testing.T) {
	for _, backend := range []string{"memory"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			share := "/repair"
			store, root := newCheckStore(t, backend, share)
			payloadID := seedCheckFile(t, store, share, root, "twice", 4096, nil,
				[]seedRef{{off: 0, size: 4096, hash: 1}, {off: 0, size: 4096, hash: 2}},
			)
			for _, h := range []byte{1, 2} {
				if err := store.MarkSynced(ctx, seedHash(h), block.ChunkLocator{}); err != nil {
					t.Fatalf("MarkSynced: %v", err)
				}
			}

			res := runRepair(t, store, share, ManifestCheckOptions{
				CheckSynced: true, PlanRepairs: true, ApplyRepairs: true,
			})
			if res.RepairsApplied != 1 || res.RepairsSkipped != 1 {
				t.Fatalf("want 1 applied 1 skipped, got %d/%d: %+v",
					res.RepairsApplied, res.RepairsSkipped, res.Repairs)
			}
			if rows := manifestSnapshot(t, store, payloadID); len(rows) != 1 {
				t.Fatalf("want a single row at the offset, got %d", len(rows))
			}
		})
	}
}
