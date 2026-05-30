//go:build integration

package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// ds3RemoteConfig builds a remote s3 BlockStoreConfig from the DS3_* env
// vars (see /tmp/ds3.env). Each test gets a unique key prefix so parallel
// runs and reruns don't collide on the bucket. Skips when DS3_ENDPOINT is
// unset. force_path_style is left to the production default
// (CreateRemoteStoreFromConfig forces it true when a custom endpoint is set)
// so this test exercises the SAME wiring the live `dfsctl store block remote
// add` path produces.
func ds3RemoteConfig(t *testing.T, prefix string) *models.BlockStoreConfig {
	t.Helper()
	endpoint := os.Getenv("DS3_ENDPOINT")
	if endpoint == "" {
		t.Skip("DS3_ENDPOINT not set; skipping DS3 durable snapshot test")
	}
	if !strings.HasPrefix(endpoint, "http") {
		endpoint = "https://" + endpoint
	}
	region := os.Getenv("DS3_REGION")
	if region == "" {
		region = "eu-central-1"
	}
	cfg := &models.BlockStoreConfig{
		Name: "ds3-bv",
		Kind: models.BlockStoreKindRemote,
		Type: "s3",
	}
	if err := cfg.SetConfig(map[string]any{
		"bucket":            os.Getenv("DS3_BUCKET"),
		"region":            region,
		"endpoint":          endpoint,
		"prefix":            prefix,
		"access_key_id":     os.Getenv("DS3_ACCESS_KEY"),
		"secret_access_key": os.Getenv("DS3_SECRET_KEY"),
	}); err != nil {
		t.Fatalf("SetConfig(remote): %v", err)
	}
	return cfg
}

// TestSnapshotByteVerify_DS3Durable drives the FULL remote-durable path
// against a real Cubbit/DS3 bucket: write a multi-chunk file through the
// engine -> DrainRollups -> DrainAllUploads -> VerifyRemoteDurability, then
// mutate + ResetLocalState (drop ALL local CAS so reads MUST pull from DS3)
// + restore, and assert byte-identical recovery.
//
// Crucially the snapshot is created WITHOUT NoVerify, so CreateSnapshot runs
// the actual remote durability gate that produced the "verify chunk not
// found" failure. A miss here reproduces the bug. RemoteDurable MUST be true.
//
// Repeated subtests hammer the upload->verify window to catch any
// nondeterministic miss.
func TestSnapshotByteVerify_DS3Durable(t *testing.T) {
	const iterations = 3
	for i := 0; i < iterations; i++ {
		i := i
		t.Run(fmt.Sprintf("iter-%d", i), func(t *testing.T) {
			prefix := fmt.Sprintf("bvtest/%s/iter-%d/", t.Name(), i)
			remoteCfg := ds3RemoteConfig(t, prefix)
			meta := metadatamemory.NewMemoryMetadataStoreWithDefaults()
			fx := newByteVerifyFixtureOpts(t, meta, "memory", remoteCfg)
			defer fx.close()
			runDS3DurableCycle(t, fx)
		})
	}
}

// runDS3DurableCycle is the remote-durable analogue of runByteVerifyCycle.
func runDS3DurableCycle(t *testing.T, fx *byteVerifyFixture) {
	ctx := context.Background()
	const mib = 1 << 20

	// Multi-chunk file (4 MiB non-repeating) + a small one.
	origA := distinctBytes(4*mib, 0xD53)
	origB := distinctBytes(9000, 0xB0B)

	pidA := fx.createEmptyFile(ctx, "fileA.bin")
	fx.writeFile(ctx, pidA, origA)
	pidB := fx.createEmptyFile(ctx, "fileB.bin")
	fx.writeFile(ctx, pidB, origB)

	if fa := fx.getFile(ctx, "fileA.bin"); len(fa.Blocks) < 2 {
		t.Fatalf("fileA produced %d CAS block(s), want >= 2 (multi-chunk path)", len(fa.Blocks))
	}

	// Sanity: the engine HAS a remote wired.
	if fx.bs.RemoteStore() == nil {
		t.Fatal("share has no remote store — DS3 not wired into engine")
	}

	// (2) DURABLE snapshot: NoVerify=false runs DrainAllUploads +
	// VerifyRemoteDurability against real DS3. This is the path that
	// produced "verify chunk not found".
	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{NoVerify: false})
	if err != nil {
		t.Fatalf("CreateSnapshot(durable): %v", err)
	}
	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}
	if snap.State != models.StateReady {
		t.Fatalf("snap.State = %q, want ready (durable verify failed)", snap.State)
	}
	if !snap.RemoteDurable {
		t.Fatalf("snap.RemoteDurable = false, want true — remote durability gate did not pass")
	}

	// Independently re-verify every manifest hash is HEAD-visible on DS3
	// (belt-and-braces beyond the orchestration's own gate).
	if err := reverifyManifestOnRemote(ctx, fx, snap); err != nil {
		t.Fatalf("post-create manifest re-verify on DS3 failed: %v", err)
	}

	// (3) Mutate: overwrite A in place (same length), delete B, add C.
	mutA := distinctBytes(4*mib, 0xD54)
	fx.writeFile(ctx, pidA, mutA)
	fx.deleteFile(ctx, "fileB.bin")
	pidC := fx.createEmptyFile(ctx, "fileC.bin")
	fx.writeFile(ctx, pidC, distinctBytes(32*1024, 0xC0C))

	// (4) Drop ALL local CAS state so restore-time reads MUST pull bytes
	// back from DS3 — the true remote round-trip.
	if err := fx.bs.ResetLocalState(ctx); err != nil {
		t.Fatalf("ResetLocalState: %v", err)
	}

	// (5) Restore. Durable snapshot -> no AllowNonDurable needed.
	if err := fx.rt.DisableShare(ctx, fx.shareName); err != nil {
		t.Fatalf("DisableShare: %v", err)
	}
	if _, err := fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{}); err != nil {
		t.Fatalf("RestoreSnapshot(durable): %v", err)
	}

	// (6) Byte-verify recovery — reads resolve through DS3 (local was reset).
	if !fx.fileExists(ctx, "fileA.bin") {
		t.Fatal("fileA missing after restore")
	}
	gotA := fx.readFile(ctx, pidA, len(origA))
	if !bytes.Equal(gotA, origA) {
		t.Fatalf("RESTORE fileA NOT byte-identical from DS3: %s", firstDiff(origA, gotA))
	}
	if !fx.fileExists(ctx, "fileB.bin") {
		t.Fatal("fileB not recovered after restore")
	}
	pidB2 := fx.getFile(ctx, "fileB.bin").PayloadID
	gotB := fx.readFile(ctx, pidB2, len(origB))
	if !bytes.Equal(gotB, origB) {
		t.Fatalf("RESTORE fileB NOT byte-identical from DS3: %s", firstDiff(origB, gotB))
	}
	if fx.fileExists(ctx, "fileC.bin") {
		t.Fatal("fileC still present after restore (created post-snapshot)")
	}
}

// reverifyManifestOnRemote re-reads the snapshot manifest from disk and HEADs
// every hash against the live remote, independent of the orchestration's own
// verify pass. Returns the first miss (wrapping ErrChunkNotFound) so a
// nondeterministic gap is surfaced even if the create-time gate passed.
func reverifyManifestOnRemote(ctx context.Context, fx *byteVerifyFixture, snap *models.Snapshot) error {
	manifestPath := snap.ManifestPath(fx.localStoreDir)
	f, err := os.Open(manifestPath)
	if err != nil {
		return fmt.Errorf("open manifest %s: %w", manifestPath, err)
	}
	defer func() { _ = f.Close() }()
	hs, err := snapshot.ReadManifest(f)
	if err != nil {
		return fmt.Errorf("read manifest %s: %w", manifestPath, err)
	}
	rs := fx.bs.RemoteStore()
	if rs == nil {
		return errors.New("no remote store")
	}
	hctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for _, h := range hs.Sorted() {
		if _, herr := rs.Head(hctx, h); herr != nil {
			return fmt.Errorf("manifest hash %s HEAD failed (%w)", h, herr)
		}
	}
	return nil
}
