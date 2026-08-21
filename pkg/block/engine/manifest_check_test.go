package engine

import (
	"strconv"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	metadatasqlite "github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// seedRow describes one FileChunk row to plant: the suffix appended after
// "{payloadID}/", the row's DataSize, and a hash seed (0 means the zero hash,
// i.e. a pending row that holds no committed bytes).
type seedRow struct {
	suffix string
	size   uint32
	hash   byte
}

// seedRef describes one entry of the file's own block list.
type seedRef struct {
	off  uint64
	size uint32
	hash byte
}

func seedHash(b byte) block.ContentHash {
	var h block.ContentHash
	if b == 0 {
		return h
	}
	for i := range h {
		h[i] = b
	}
	return h
}

// newCheckStore builds an empty share on the named backend and returns the
// store plus its root handle.
func newCheckStore(t *testing.T, backend, share string) (metadata.Store, metadata.FileHandle) {
	t.Helper()
	ctx := t.Context()

	var store metadata.Store
	switch backend {
	case "memory":
		store = metadatamemory.NewMemoryMetadataStoreWithDefaults()
	case "sqlite":
		s, err := metadatasqlite.NewSQLiteMetadataStore(ctx, &metadatasqlite.SQLiteMetadataStoreConfig{
			Path:        t.TempDir() + "/m.db",
			AutoMigrate: true,
		}, metadata.FilesystemCapabilities{
			MaxFileSize:         1 << 40,
			MaxFilenameLen:      255,
			MaxPathLen:          4096,
			MaxHardLinkCount:    32767,
			CaseSensitive:       true,
			CasePreserving:      true,
			TimestampResolution: 1,
		})
		if err != nil {
			t.Fatalf("NewSQLiteMetadataStore: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		store = s
	default:
		t.Fatalf("unknown backend %q", backend)
	}

	if err := store.CreateShare(ctx, &metadata.Share{Name: share}); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	if _, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	}); err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}
	root, err := store.GetRootHandle(ctx, share)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	return store, root
}

// seedCheckFile plants one regular file under root with the given recorded
// size, FileChunk rows and block list, and returns its payloadID.
func seedCheckFile(
	t *testing.T,
	store metadata.Store,
	share string,
	root metadata.FileHandle,
	name string,
	size uint64,
	rows []seedRow,
	refs []seedRef,
) string {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()

	handle, err := store.GenerateHandle(ctx, share, "/"+name)
	if err != nil {
		t.Fatalf("GenerateHandle(%s): %v", name, err)
	}
	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle(%s): %v", name, err)
	}

	payloadID := "payload-" + name
	for _, r := range rows {
		if err := store.Put(ctx, &block.FileChunk{
			ID:         payloadID + "/" + r.suffix,
			Hash:       seedHash(r.hash),
			State:      block.BlockStatePending,
			DataSize:   r.size,
			LastAccess: now,
			CreatedAt:  now,
		}); err != nil {
			t.Fatalf("Put chunk %s/%s: %v", payloadID, r.suffix, err)
		}
	}

	blocks := make([]block.ChunkRef, 0, len(refs))
	for _, r := range refs {
		blocks = append(blocks, block.ChunkRef{Hash: seedHash(r.hash), Offset: r.off, Size: r.size})
	}

	file := &metadata.File{
		ID:        fileID,
		ShareName: share,
		FileAttr: metadata.FileAttr{
			Type:      metadata.FileTypeRegular,
			Mode:      0o644,
			Size:      size,
			Mtime:     now,
			Ctime:     now,
			Atime:     now,
			PayloadID: metadata.PayloadID(payloadID),
			Blocks:    blocks,
		},
	}
	if err := store.SetManifest(ctx, file); err != nil {
		t.Fatalf("UpdateAttrs(%s): %v", name, err)
	}
	if err := store.SetParent(ctx, handle, root); err != nil {
		t.Fatalf("SetParent(%s): %v", name, err)
	}
	if err := store.SetChild(ctx, root, name, handle); err != nil {
		t.Fatalf("SetChild(%s): %v", name, err)
	}
	if err := store.SetLinkCount(ctx, handle, 1); err != nil {
		t.Fatalf("SetLinkCount(%s): %v", name, err)
	}
	return payloadID
}

func findingFor(t *testing.T, res *ManifestCheckResult, payloadID string) *PayloadFinding {
	t.Helper()
	for i := range res.Findings {
		if res.Findings[i].PayloadID == payloadID {
			return &res.Findings[i]
		}
	}
	return nil
}

// TestCheckManifests_HolesAndUnknownHashes exercises the three reportable
// conditions that every metadata backend surfaces identically: a range no row
// covers that the file's own block list claims (damage), a range nothing
// claims (indistinguishable from sparseness), and a row whose hash the
// synced-hash store does not know. A healthy file must produce no finding at
// all — a scan that reports nothing everywhere proves nothing.
func TestCheckManifests_HolesAndUnknownHashes(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			const share = "checkshare"
			store, root := newCheckStore(t, backend, share)

			// Healthy: two rows covering [0,8192), both claimed and both synced.
			healthy := seedCheckFile(t, store, share, root, "healthy", 8192,
				[]seedRow{{"0", 4096, 1}, {"4096", 4096, 2}},
				[]seedRef{{0, 4096, 1}, {4096, 4096, 2}})

			// Damaged: the file claims data at [0,4096) but only the second
			// row survives, so nothing covers the leading page — the #1879
			// shape.
			damaged := seedCheckFile(t, store, share, root, "damaged", 8192,
				[]seedRow{{"4096", 4096, 3}},
				[]seedRef{{0, 4096, 4}, {4096, 4096, 3}})

			// Sparse: a real hole at [0,4096) that the file does not claim.
			sparse := seedCheckFile(t, store, share, root, "sparse", 8192,
				[]seedRow{{"4096", 4096, 5}},
				[]seedRef{{4096, 4096, 5}})

			// Every hash above is marked synced except hash 2, which stays
			// unknown to the synced-hash store.
			for _, h := range []byte{1, 3, 5} {
				if err := store.MarkSynced(ctx, seedHash(h), block.ChunkLocator{}); err != nil {
					t.Fatalf("MarkSynced(%d): %v", h, err)
				}
			}

			res, err := CheckManifests(ctx, share, store, ManifestCheckOptions{CheckSynced: true})
			if err != nil {
				t.Fatalf("CheckManifests: %v", err)
			}
			if res.FilesScanned != 3 {
				t.Fatalf("FilesScanned = %d, want 3", res.FilesScanned)
			}
			if !res.Damaged() {
				t.Fatal("Damaged() = false, want true")
			}

			// The healthy file's only finding is its unknown hash; it has no
			// uncovered range.
			hf := findingFor(t, res, healthy)
			if hf == nil {
				t.Fatal("healthy payload: want a finding for the unknown hash")
			}
			if len(hf.Uncovered) != 0 {
				t.Fatalf("healthy payload: Uncovered = %v, want none", hf.Uncovered)
			}
			if want := []string{healthy + "/4096"}; len(hf.UnknownHashRows) != 1 || hf.UnknownHashRows[0] != want[0] {
				t.Fatalf("healthy payload: UnknownHashRows = %v, want %v", hf.UnknownHashRows, want)
			}

			df := findingFor(t, res, damaged)
			if df == nil {
				t.Fatal("damaged payload: no finding")
			}
			if len(df.Uncovered) != 1 || df.Uncovered[0] != (ByteRange{Start: 0, End: 4096, Claimed: true}) {
				t.Fatalf("damaged payload: Uncovered = %v, want one claimed [0,4096)", df.Uncovered)
			}
			if !df.Damaged {
				t.Fatal("damaged payload: Damaged() = false")
			}

			sf := findingFor(t, res, sparse)
			if sf == nil {
				t.Fatal("sparse payload: no finding")
			}
			if len(sf.Uncovered) != 1 || sf.Uncovered[0] != (ByteRange{Start: 0, End: 4096, Claimed: false}) {
				t.Fatalf("sparse payload: Uncovered = %v, want one unclaimed [0,4096)", sf.Uncovered)
			}
			if sf.Damaged {
				t.Fatal("sparse payload: Damaged() = true, want false — an unclaimed hole is not damage")
			}

			if res.ClaimedUncoveredBytes != 4096 || res.UncoveredBytes != 8192 {
				t.Fatalf("ClaimedUncoveredBytes=%d UncoveredBytes=%d, want 4096 and 8192",
					res.ClaimedUncoveredBytes, res.UncoveredBytes)
			}
			if res.UnknownHashRows != 1 {
				t.Fatalf("UnknownHashRows = %d, want 1", res.UnknownHashRows)
			}
			if res.DamagedPayloads != 2 {
				t.Fatalf("DamagedPayloads = %d, want 2 (damaged + the unknown-hash file)", res.DamagedPayloads)
			}
		})
	}
}

// TestCheckManifests_LocalOnlySkipsSyncedCheck pins the gate that keeps a
// share with no remote store from reporting every one of its rows: nothing is
// ever marked synced there, so the unknown-hash check must not run.
func TestCheckManifests_LocalOnlySkipsSyncedCheck(t *testing.T) {
	ctx := t.Context()
	const share = "localonly"
	store, root := newCheckStore(t, "memory", share)
	seedCheckFile(t, store, share, root, "f", 4096,
		[]seedRow{{"0", 4096, 1}}, []seedRef{{0, 4096, 1}})

	res, err := CheckManifests(ctx, share, store, ManifestCheckOptions{})
	if err != nil {
		t.Fatalf("CheckManifests: %v", err)
	}
	if res.SyncedHashesChecked {
		t.Fatal("SyncedHashesChecked = true, want false")
	}
	if res.UnknownHashRows != 0 || len(res.Findings) != 0 {
		t.Fatalf("local-only scan reported %d unknown-hash rows and %d findings, want none",
			res.UnknownHashRows, len(res.Findings))
	}
	if res.Damaged() {
		t.Fatal("Damaged() = true on a healthy local-only share")
	}
}

// TestCheckManifests_UnplaceableRow plants a row whose ID carries no parseable
// chunk offset — the row class that read paths refuse with
// ErrManifestInconsistent and that cold seeding cannot place at all.
//
// It runs on every backend the check harness can build: the row belongs to the
// payload and is merely damaged, so ListFileChunks must hand it to the scan
// rather than filter it out. A backend that dropped it would make this damage
// class invisible to the scan, which is the whole thing the scan exists to
// report.
func TestCheckManifests_UnplaceableRow(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			checkManifestsUnplaceableRow(t, backend)
		})
	}
}

func checkManifestsUnplaceableRow(t *testing.T, backend string) {
	ctx := t.Context()
	const share = "unplaceable"
	store, root := newCheckStore(t, backend, share)

	payloadID := seedCheckFile(t, store, share, root, "f", 8192,
		[]seedRow{{"block-0", 4096, 1}, {"4096", 4096, 2}},
		[]seedRef{{0, 4096, 1}, {4096, 4096, 2}})

	// Only the placeable row's hash is known to the synced-hash store, so
	// the unplaceable row must still be put to it: where its bytes belong is
	// unknown, but whether the remote holds them is still answerable.
	if err := store.MarkSynced(ctx, seedHash(2), block.ChunkLocator{}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	res, err := CheckManifests(ctx, share, store, ManifestCheckOptions{CheckSynced: true})
	if err != nil {
		t.Fatalf("CheckManifests: %v", err)
	}
	if res.UnplaceableRows != 1 {
		t.Fatalf("UnplaceableRows = %d, want 1", res.UnplaceableRows)
	}
	if res.UnknownHashRows != 1 {
		t.Fatalf("UnknownHashRows = %d, want 1 (the unplaceable row's hash)", res.UnknownHashRows)
	}
	f := findingFor(t, res, payloadID)
	if f == nil {
		t.Fatal("no finding for the payload holding an unplaceable row")
	}
	want := payloadID + "/block-0"
	if len(f.UnplaceableRows) != 1 || f.UnplaceableRows[0] != want {
		t.Fatalf("UnplaceableRows = %v, want [%s]", f.UnplaceableRows, want)
	}
	if len(f.UnknownHashRows) != 1 || f.UnknownHashRows[0] != want {
		t.Fatalf("UnknownHashRows = %v, want [%s]", f.UnknownHashRows, want)
	}
	// The unplaceable row contributes no coverage, so the range it should
	// have held is also reported as claimed-but-uncovered.
	if len(f.Uncovered) != 1 || !f.Uncovered[0].Claimed || f.Uncovered[0] != (ByteRange{0, 4096, true}) {
		t.Fatalf("Uncovered = %v, want one claimed [0,4096)", f.Uncovered)
	}
	if !res.Damaged() {
		t.Fatal("Damaged() = false with an unplaceable row present")
	}
}

// TestCheckManifests_PendingRowHoldsNoBytes pins that a zero-hash row
// contributes no coverage — it holds no committed bytes, the same reading
// DataExtents takes — and is not put to the synced-hash store.
func TestCheckManifests_PendingRowHoldsNoBytes(t *testing.T) {
	ctx := t.Context()
	const share = "pending"
	store, root := newCheckStore(t, "memory", share)
	payloadID := seedCheckFile(t, store, share, root, "f", 4096,
		[]seedRow{{"0", 4096, 0}}, nil)

	res, err := CheckManifests(ctx, share, store, ManifestCheckOptions{CheckSynced: true})
	if err != nil {
		t.Fatalf("CheckManifests: %v", err)
	}
	f := findingFor(t, res, payloadID)
	if f == nil || len(f.Uncovered) != 1 || f.Uncovered[0].Claimed {
		t.Fatalf("Findings = %+v, want one unclaimed uncovered range", res.Findings)
	}
	if res.UnknownHashRows != 0 {
		t.Fatalf("UnknownHashRows = %d, want 0 — a pending row has no hash to look up", res.UnknownHashRows)
	}
}

func TestSplitByClaim(t *testing.T) {
	claimed := [][2]uint64{{100, 200}, {300, 400}}
	got := splitByClaim([2]uint64{50, 350}, claimed)
	want := []ByteRange{
		{50, 100, false},
		{100, 200, true},
		{200, 300, false},
		{300, 350, true},
	}
	if len(got) != len(want) {
		t.Fatalf("splitByClaim = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitByClaim[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestUncoveredRanges(t *testing.T) {
	got := uncoveredRanges([][2]uint64{{0, 10}, {20, 30}}, 40)
	want := [][2]uint64{{10, 20}, {30, 40}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("uncoveredRanges = %v, want %v", got, want)
	}
	if n := len(uncoveredRanges([][2]uint64{{0, 40}}, 40)); n != 0 {
		t.Fatalf("fully covered payload reported %d uncovered ranges", n)
	}
	// A row overhanging the recorded size still covers everything below it.
	if n := len(uncoveredRanges([][2]uint64{{0, 100}}, 40)); n != 0 {
		t.Fatalf("overhanging coverage reported %d uncovered ranges", n)
	}
}

// TestCheckManifests_CapDoesNotHideDamage pins that the per-payload display
// cap bounds only what is listed. A file fragmented past the cap by holes
// nothing claims must still report the claimed range that follows them —
// dropping it would make the scan exit clean on a damaged store, the exact
// silence this command exists to end.
func TestCheckManifests_CapDoesNotHideDamage(t *testing.T) {
	ctx := t.Context()
	const share = "fragmented"
	store, root := newCheckStore(t, "memory", share)

	// Alternate covered and uncovered 4 KiB pages so the file opens with
	// more unclaimed holes than the per-payload list can hold, then claim a
	// final page that no row covers.
	const holes = maxManifestCheckRangesPerPayload + 4
	var (
		rows []seedRow
		refs []seedRef
	)
	for i := 0; i < holes; i++ {
		off := uint64(i)*8192 + 4096
		rows = append(rows, seedRow{strconv.FormatUint(off, 10), 4096, 1})
		refs = append(refs, seedRef{off, 4096, 1})
	}
	damagedOff := uint64(holes) * 8192
	refs = append(refs, seedRef{damagedOff, 4096, 2})
	payloadID := seedCheckFile(t, store, share, root, "f", damagedOff+4096, rows, refs)

	res, err := CheckManifests(ctx, share, store, ManifestCheckOptions{})
	if err != nil {
		t.Fatalf("CheckManifests: %v", err)
	}
	if res.ClaimedUncoveredRanges != 1 || res.ClaimedUncoveredBytes != 4096 {
		t.Fatalf("ClaimedUncoveredRanges=%d ClaimedUncoveredBytes=%d, want 1 and 4096",
			res.ClaimedUncoveredRanges, res.ClaimedUncoveredBytes)
	}
	if res.UncoveredRanges != holes+1 {
		t.Fatalf("UncoveredRanges = %d, want %d", res.UncoveredRanges, holes+1)
	}
	if res.DamagedPayloads != 1 || !res.Damaged() {
		t.Fatalf("DamagedPayloads=%d Damaged=%v, want 1 and true", res.DamagedPayloads, res.Damaged())
	}
	f := findingFor(t, res, payloadID)
	if f == nil || !f.Damaged {
		t.Fatalf("finding = %+v, want Damaged", f)
	}
	if !f.Truncated || len(f.Uncovered) != maxManifestCheckRangesPerPayload {
		t.Fatalf("Truncated=%v len(Uncovered)=%d, want true and %d",
			f.Truncated, len(f.Uncovered), maxManifestCheckRangesPerPayload)
	}
}
