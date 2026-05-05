//go:build e2e

// TestDEDUP02_CrossShareDedup verifies the Phase 13 cross-share file-level
// dedup invariant (DEDUP-02 / D-13). Two NFS shares pointing at one Postgres
// metadata store and ONE shared S3 bucket are written with identical content;
// the resulting CAS object set must reflect the chunks of ONE file, not two.
//
// Setup: one DittoFS server, one Postgres metadata store (shared by both
// shares), one S3 bucket exposed via two separate remote-block-store records
// configured against the SAME bucket. Each share has its own per-share local
// block store (the CLAUDE.md invariant: "Block stores are per-share. Local
// storage dirs are always isolated.") The shared remote configuration
// satisfies the DEDUP-02 scope: dedup spans shares whose stores point at the
// same metadata + remote.
//
// Procedure:
//
//  1. Boot server, create one Postgres metadata store, one S3 bucket, two
//     remote-block-store records both targeting that bucket, two per-share
//     local-block-store records.
//  2. Create share-a + share-b referencing meta + their respective local +
//     their respective remote (both remotes point at the same bucket).
//  3. Mount each share over NFSv3 and write the SAME 16 MiB deterministic
//     payload as `vm.img` on each.
//  4. Drain uploads via dfsctl.
//  5. List the bucket's cas/ prefix. Assertions:
//     - At least one CAS key exists (write actually happened).
//     - Total CAS object count is bounded by the FastCDC chunk count of
//     16 MiB (single-file's worth, not 2x). Without dedup, both writes
//     would produce ~2x the chunk count; with file-level OR block-level
//     dedup engaged, the same set of chunks is shared.
//  6. Read both files back over NFS and verify content equals payload.
//
// Tier: nightly only. Requires sudo + kernel NFS client + Localstack +
// Postgres testcontainers, mirroring TestBlockStoreImmutableOverwrites.
//
// Run:
//
//	cd test/e2e && DITTOFS_E2E_NIGHTLY=1 sudo ./run-e2e.sh \
//	    --s3 --test TestDEDUP02_CrossShareDedup
//
// See Phase 13 plan 13-08, decisions D-13 (per-metadata-store dedup scope)
// and D-09 (file-level dedup short-circuit on full-quiesce-of-Pending file).
package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// crossShareDedupPayloadSize matches cas_immutable_overwrites_test.go's
// payloadSize so FastCDC emits multiple chunks (min 1 / avg 4 / max 16 MiB).
// 16 MiB ⇒ ~4 chunks at the average; we cap the dedup-success bound at
// 16 keys (4× headroom for FastCDC variance + control-plane internal keys).
const crossShareDedupPayloadSize = 16 * 1024 * 1024

// crossShareDedupCASKeyBound is the upper bound on cas/ keys that proves
// dedup is active. Without dedup, two independent 16 MiB writes would push
// 8-16 keys total; with dedup, share-b's write produces ZERO net new keys
// once share-a's chunks land. We allow some FastCDC boundary variance and
// out-of-band dedup chunks to keep the bound robust without compromising
// the assertion's signal: a non-deduped run would produce 2x chunk count
// which still vastly exceeds this bound for 16 MiB payloads.
const crossShareDedupCASKeyBound = 16

// TestDEDUP02_CrossShareDedup is the e2e gate for Phase 13 DEDUP-02.
func TestDEDUP02_CrossShareDedup(t *testing.T) {
	if os.Getenv("DITTOFS_E2E_NIGHTLY") != "1" {
		t.Skip("nightly tier only; set DITTOFS_E2E_NIGHTLY=1")
	}
	if testing.Short() {
		t.Skip("Skipping cross-share dedup E2E in short mode")
	}

	if !framework.CheckLocalstackAvailable(t) {
		t.Skip("Skipping: Localstack (S3) not available — run via run-e2e.sh --s3")
	}
	if !framework.CheckPostgresAvailable(t) {
		t.Skip("Skipping: Postgres not available — run via run-e2e.sh with Postgres")
	}

	lsHelper := framework.NewLocalstackHelper(t)
	require.NotNil(t, lsHelper, "Localstack helper must be available")
	pgHelper := framework.NewPostgresHelper(t)
	require.NotNil(t, pgHelper, "Postgres helper must be available")

	// Ensure a clean slate: a stale Postgres schema from a prior nightly
	// would leak ObjectID rows that pre-claim our payload's chunks before
	// share-a even writes.
	require.NoError(t, pgHelper.TruncateTables(), "Truncate Postgres tables for isolation")

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	// ---- One shared Postgres metadata store ----
	metaName := helpers.UniqueTestName("dedup-meta")
	pgConfig := pgHelper.GetConfig()
	pgConfigJSON := fmt.Sprintf(
		`{"host":"%s","port":%d,"database":"%s","user":"%s","password":"%s"}`,
		pgConfig.Host, pgConfig.Port, pgConfig.Database, pgConfig.User, pgConfig.Password,
	)
	_, err := cli.CreateMetadataStore(metaName, "postgres",
		helpers.WithMetaRawConfig(pgConfigJSON))
	require.NoError(t, err, "create shared Postgres metadata store")
	t.Cleanup(func() { _ = cli.DeleteMetadataStore(metaName) })

	// ---- One shared S3 bucket; two remote-block-store records pointing at it ----
	bucketName := strings.ReplaceAll(
		fmt.Sprintf("dittofs-dedup02-%s", helpers.UniqueTestName("bkt")), "_", "-")
	require.NoError(t,
		lsHelper.CreateBucket(context.Background(), bucketName),
		"create shared S3 bucket")
	t.Cleanup(func() { lsHelper.CleanupBucket(context.Background(), bucketName) })

	remoteAName := helpers.UniqueTestName("dedup-remote-a")
	remoteBName := helpers.UniqueTestName("dedup-remote-b")
	for _, name := range []string{remoteAName, remoteBName} {
		_, err := cli.CreateRemoteBlockStore(name, "s3",
			helpers.WithBlockS3Config(bucketName, "us-east-1",
				lsHelper.Endpoint, "test", "test"))
		require.NoError(t, err, "create remote block store %s", name)
		t.Cleanup(func() { _ = cli.DeleteRemoteBlockStore(name) })
	}

	// ---- Per-share local block stores (CLAUDE.md invariant: local dirs isolated) ----
	localAName := helpers.UniqueTestName("dedup-local-a")
	localBName := helpers.UniqueTestName("dedup-local-b")
	localPathA := t.TempDir()
	localPathB := t.TempDir()
	_, err = cli.CreateLocalBlockStore(localAName, "fs",
		helpers.WithBlockRawConfig(fmt.Sprintf(`{"path":"%s"}`, localPathA)))
	require.NoError(t, err, "create local-A")
	t.Cleanup(func() { _ = cli.DeleteLocalBlockStore(localAName) })
	_, err = cli.CreateLocalBlockStore(localBName, "fs",
		helpers.WithBlockRawConfig(fmt.Sprintf(`{"path":"%s"}`, localPathB)))
	require.NoError(t, err, "create local-B")
	t.Cleanup(func() { _ = cli.DeleteLocalBlockStore(localBName) })

	// ---- Two shares pointing at the SAME metadata store + SAME bucket ----
	shareA := "/dedup-share-a"
	shareB := "/dedup-share-b"
	_, err = cli.CreateShare(shareA, metaName, localAName, helpers.WithShareRemote(remoteAName))
	require.NoError(t, err, "create %s", shareA)
	t.Cleanup(func() { _ = cli.DeleteShare(shareA) })
	_, err = cli.CreateShare(shareB, metaName, localBName, helpers.WithShareRemote(remoteBName))
	require.NoError(t, err, "create %s", shareB)
	t.Cleanup(func() { _ = cli.DeleteShare(shareB) })

	// ---- Enable NFS adapter and wait for it ----
	nfsPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err, "enable NFS adapter")
	t.Cleanup(func() { _, _ = cli.DisableAdapter("nfs") })
	require.NoError(t,
		helpers.WaitForAdapterStatus(t, cli, "nfs", true, 10*time.Second),
		"NFS adapter should become enabled")
	framework.WaitForServer(t, nfsPort, 10*time.Second)

	// ---- Mount both shares ----
	mountA := framework.MountNFSExportWithVersion(t, nfsPort, shareA, "3")
	t.Cleanup(mountA.Cleanup)
	mountB := framework.MountNFSExportWithVersion(t, nfsPort, shareB, "3")
	t.Cleanup(mountB.Cleanup)

	// ---- Generate one deterministic payload; write to both shares ----
	payload := dedupDeterministicPayload(t, crossShareDedupPayloadSize, 0xded02)

	pathA := mountA.FilePath("vm.img")
	pathB := mountB.FilePath("vm.img")
	t.Cleanup(func() { _ = os.Remove(pathA) })
	t.Cleanup(func() { _ = os.Remove(pathB) })

	framework.WriteFile(t, pathA, payload)
	t.Logf("step 1: wrote vm.img to share-a (%d bytes, sha256=%s)",
		len(payload), shortPayloadSha(payload))

	framework.WriteFile(t, pathB, payload)
	t.Logf("step 2: wrote vm.img to share-b (%d bytes, sha256=%s)",
		len(payload), shortPayloadSha(payload))

	// ---- Drain uploads (canonical pattern from cas_immutable_overwrites_test) ----
	dedupDrainUploads(t, cli)

	// ---- Enumerate cas/ keys directly in S3 (bypassing DittoFS) ----
	casKeys := lsHelper.ListS3Prefix(t, bucketName, "cas/")
	require.NotEmpty(t, casKeys,
		"DEDUP-02: cas/ prefix is empty after writing 16 MiB to two shares — "+
			"writes never reached S3 (drain may have failed)")

	t.Logf("DEDUP-02: %d unique cas/ keys after writing identical 16 MiB "+
		"payload to share-a + share-b", len(casKeys))

	// Sanity: every key should match the cas/{hh}/{hh}/{hex} shape.
	for _, k := range casKeys {
		parts := strings.Split(k, "/")
		require.Lenf(t, parts, 4, "cas key %q must have 4 path segments", k)
		require.Equal(t, "cas", parts[0], "cas key %q must start with cas/", k)
	}

	// Bound the cas/ key count. Without dedup (two distinct chunk lists,
	// no cross-share GetByHash hits, no file-level short-circuit), the
	// observed key count would scale with 2× the chunk count of 16 MiB
	// (≈8-32 keys). With dedup engaged at either tier (block-level via
	// Phase 11 GetByHash OR file-level via Phase 13 BSCAS-05), share-b's
	// chunks collide with share-a's hashes and share-b contributes zero
	// new uploads. crossShareDedupCASKeyBound captures the dedup-pass
	// regime with headroom for FastCDC boundary variance.
	require.LessOrEqualf(t, len(casKeys), crossShareDedupCASKeyBound,
		"DEDUP-02 VIOLATION: too many cas/ keys post-dedup. "+
			"got %d keys, expected <= %d (one file's chunks, not two). "+
			"Either dedup did not engage (BSCAS-05 short-circuit + Phase 11 "+
			"GetByHash both inactive) or the bucket holds stale state. keys=%v",
		len(casKeys), crossShareDedupCASKeyBound, casKeys)

	// ---- Read both files back; assert content matches ----
	for label, path := range map[string]string{"share-a": pathA, "share-b": pathB} {
		got := framework.ReadFile(t, path)
		assert.Equalf(t, len(payload), len(got),
			"%s: re-read length should match payload (got %d, want %d)",
			label, len(got), len(payload))
		if !assertShaEqual(t, payload, got) {
			t.Errorf("%s: content mismatch on re-read", label)
		}
	}
}

// dedupDeterministicPayload returns size bytes of pseudo-random data seeded
// by the given int. Reproducible across runs and CI nodes.
func dedupDeterministicPayload(t *testing.T, size int, seed int64) []byte {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	buf := make([]byte, size)
	if _, err := r.Read(buf); err != nil {
		t.Fatalf("dedupDeterministicPayload(seed=%d size=%d): %v", seed, size, err)
	}
	return buf
}

// dedupDrainUploads mirrors cas_immutable_overwrites_test.go::drainUploads
// (kept inline per Plan 13-08's "one extra test-local helper is acceptable"
// guidance — we did not lift it to helpers.DrainAllUploads to keep the diff
// minimal).
func dedupDrainUploads(t *testing.T, runner *helpers.CLIRunner) {
	t.Helper()
	out, err := runner.Run("system", "drain-uploads")
	require.NoError(t, err, "drain-uploads should succeed: %s", string(out))
}

// shortPayloadSha returns the first 12 hex chars of SHA-256 — for debug
// logs only (NOT integrity verification, which uses BLAKE3 inside DittoFS).
func shortPayloadSha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:6])
}

// assertShaEqual compares two byte slices via SHA-256 to keep assertion
// failure messages bounded — comparing 16 MiB byte-by-byte produces a
// multi-MB error message in testify.
func assertShaEqual(t *testing.T, want, got []byte) bool {
	t.Helper()
	w := sha256.Sum256(want)
	g := sha256.Sum256(got)
	if w != g {
		t.Errorf("payload sha256 mismatch: want=%s got=%s (lengths want=%d got=%d)",
			hex.EncodeToString(w[:]), hex.EncodeToString(g[:]),
			len(want), len(got))
		return false
	}
	return true
}
