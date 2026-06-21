//go:build e2e

// TestDedupRace_NFSv4_ConcurrentIdenticalWrites is the explicit NFSv4.1
// wire-path regression gate for the #1245 dedup race.
//
// Background (#1245): bulk concurrent byte-identical writes to a REMOTE-backed
// (S3) share could hit a dedup short-circuit that targeted a donor whose
// FileBlock rows were still Pending — yielding
// "increment refcount … no FileBlock with hash X" → EIO/wedge on COMMIT/close,
// plus a rollup "Transaction Conflict" non-converging loop. Fixed on develop by
// #1254 (Pending-donor → MISS, not EIO) and #1256 (wrap ErrConflict + a
// converging persister).
//
// The fix lives in the dedup coordinator BELOW the protocol adapter, so every
// protocol shares it by construction. Existing coverage exercised the race
// over NFSv3 (dedup_cross_share_test.go / dedup_vmfleet_test.go) and SMB plus
// the protocol-agnostic engine tests (pkg/block/engine/
// concurrent_identical_drain_test.go, dedup_pending_donor_test.go). NFSv4 was
// never explicitly exercised. This test adds that proof.
//
// Procedure (mirrors the documented #1245 trigger):
//
//  1. Boot one server, one Postgres metadata store, one S3 bucket, one remote
//     block store + one per-share local block store, one share (copied from
//     dedup_cross_share_test.go's remote-S3 setup).
//  2. Mount the share over NFSv4.1.
//  3. Prime dedup with a single donor file written first, then write N
//     additional byte-identical files CONCURRENTLY (goroutines + WaitGroup).
//     Concurrent identical content maximises contention on the hot dedup keys
//     (the obj:<hash> object_id index + the content-hash FileBlock rows) and
//     reproduces the Pending-donor window.
//  4. Assert the FIX holds: every concurrent write/close (which flushes +
//     COMMITs over NFSv4.1) succeeds with NO EIO, and every file reads back
//     byte-identical to the source payload.
//
// Tier: nightly only (DITTOFS_E2E_NIGHTLY=1) — also requires sudo + kernel NFS
// client + Localstack + Postgres, mirroring dedup_cross_share_test.go.
//
// Run:
//
//	cd test/e2e && DITTOFS_E2E_NIGHTLY=1 sudo ./run-e2e.sh \
//	    --s3 --test TestDedupRace_NFSv4_ConcurrentIdenticalWrites
package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// dedupRaceNFSv4Concurrent is the number of byte-identical files written
	// CONCURRENTLY after the donor. Sits in the documented 8-16 window: high
	// enough to provoke overlapping rollup passes on the hot dedup keys, low
	// enough to keep the nightly run-time bounded.
	dedupRaceNFSv4Concurrent = 12

	// dedupRaceNFSv4PayloadSize matches crossShareDedupPayloadSize (16 MiB) so
	// FastCDC emits multiple chunks per file — the multi-chunk path is where
	// the Pending-donor short-circuit and the rollup conflict loop manifested.
	dedupRaceNFSv4PayloadSize = 16 * 1024 * 1024
)

// TestDedupRace_NFSv4_ConcurrentIdenticalWrites is the e2e gate proving the
// #1245 fix holds over the NFSv4.1 wire path.
func TestDedupRace_NFSv4_ConcurrentIdenticalWrites(t *testing.T) {
	if os.Getenv("DITTOFS_E2E_NIGHTLY") != "1" {
		t.Skip("nightly tier only; set DITTOFS_E2E_NIGHTLY=1")
	}
	if testing.Short() {
		t.Skip("Skipping NFSv4.1 dedup-race E2E in short mode")
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

	// Clean slate: a stale Postgres schema from a prior nightly would leak
	// ObjectID rows that pre-claim our payload's chunks before the donor write,
	// masking the Pending-donor window we are trying to reproduce.
	require.NoError(t, pgHelper.TruncateTables(), "Truncate Postgres tables for isolation")

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	// ---- One Postgres metadata store ----
	metaName := helpers.UniqueTestName("dedup-nfsv4-meta")
	pgConfig := pgHelper.GetConfig()
	pgConfigJSON := fmt.Sprintf(
		`{"host":"%s","port":%d,"database":"%s","user":"%s","password":"%s"}`,
		pgConfig.Host, pgConfig.Port, pgConfig.Database, pgConfig.User, pgConfig.Password,
	)
	_, err := cli.CreateMetadataStore(metaName, "postgres",
		helpers.WithMetaRawConfig(pgConfigJSON))
	require.NoError(t, err, "create Postgres metadata store")
	t.Cleanup(func() { _ = cli.DeleteMetadataStore(metaName) })

	// ---- One S3 bucket + remote block store ----
	bucketName := strings.ReplaceAll(
		fmt.Sprintf("dittofs-dedup-nfsv4-%s", helpers.UniqueTestName("bkt")), "_", "-")
	require.NoError(t,
		lsHelper.CreateBucket(context.Background(), bucketName),
		"create S3 bucket")
	t.Cleanup(func() { lsHelper.CleanupBucket(context.Background(), bucketName) })

	remoteName := helpers.UniqueTestName("dedup-nfsv4-remote")
	_, err = cli.CreateRemoteBlockStore(remoteName, "s3",
		helpers.WithBlockS3Config(bucketName, "us-east-1",
			lsHelper.Endpoint, "test", "test"))
	require.NoError(t, err, "create remote block store")
	t.Cleanup(func() { _ = cli.DeleteRemoteBlockStore(remoteName) })

	// ---- Per-share local block store (CLAUDE.md invariant: local dirs isolated) ----
	localName := helpers.UniqueTestName("dedup-nfsv4-local")
	localPath := t.TempDir()
	_, err = cli.CreateLocalBlockStore(localName, "fs",
		helpers.WithBlockRawConfig(fmt.Sprintf(`{"path":"%s"}`, localPath)))
	require.NoError(t, err, "create local block store")
	t.Cleanup(func() { _ = cli.DeleteLocalBlockStore(localName) })

	// ---- One remote-backed share ----
	shareName := "/dedup-nfsv4-race"
	_, err = cli.CreateShare(shareName, metaName, localName, helpers.WithShareRemote(remoteName))
	require.NoError(t, err, "create share %s", shareName)
	t.Cleanup(func() { _ = cli.DeleteShare(shareName) })

	// ---- Enable NFS adapter and wait for it ----
	nfsPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err, "enable NFS adapter")
	t.Cleanup(func() { _, _ = cli.DisableAdapter("nfs") })
	require.NoError(t,
		helpers.WaitForAdapterStatus(t, cli, "nfs", true, 10*time.Second),
		"NFS adapter should become enabled")
	framework.WaitForServer(t, nfsPort, 10*time.Second)

	// ---- Mount over NFSv4.1 (the wire path under test) ----
	mount := framework.MountNFSExportWithVersion(t, nfsPort, shareName, "4.1")
	t.Cleanup(mount.Cleanup)

	// One deterministic byte-identical payload shared by donor + all racers.
	payload := dedupDeterministicPayload(t, dedupRaceNFSv4PayloadSize, 0x1245f1)

	// ---- Prime dedup with a single donor file written first ----
	// The donor lands the canonical chunk hashes; the concurrent racers below
	// then collide on those same hashes — including the window where the
	// donor's FileBlock rows may still be Pending (the #1245 trigger).
	donorPath := mount.FilePath("donor.img")
	t.Cleanup(func() { _ = os.Remove(donorPath) })
	framework.WriteFile(t, donorPath, payload)
	t.Logf("primed donor.img (%d bytes, sha256=%s) over NFSv4.1",
		len(payload), shortPayloadSha(payload))

	// ---- Write N byte-identical files CONCURRENTLY ----
	// os.WriteFile creates+writes+closes; the close triggers the flush/COMMIT
	// over NFSv4.1 where #1245 surfaced EIO. We collect per-file errors instead
	// of failing inside the goroutine so a single EIO does not abort the whole
	// fleet before we can report the aggregate.
	racerPaths := make([]string, dedupRaceNFSv4Concurrent)
	writeErrs := make([]error, dedupRaceNFSv4Concurrent)
	var wg sync.WaitGroup
	wg.Add(dedupRaceNFSv4Concurrent)
	for i := 0; i < dedupRaceNFSv4Concurrent; i++ {
		racerPaths[i] = mount.FilePath(fmt.Sprintf("racer-%02d.img", i))
		t.Cleanup(func() { _ = os.Remove(racerPaths[i]) })
		go func(idx int) {
			defer wg.Done()
			// os.WriteFile here (not framework.WriteFile) so a write/close EIO
			// is captured as an error rather than a require.FailNow inside the
			// goroutine — that is the precise #1245 failure surface.
			writeErrs[idx] = os.WriteFile(racerPaths[idx], payload, 0o644)
		}(i)
	}
	wg.Wait()

	// ---- Assert the FIX: zero EIO across all concurrent writes/closes ----
	for i, e := range writeErrs {
		require.NoErrorf(t, e,
			"#1245 REGRESSION: concurrent byte-identical write/close of racer-%02d.img "+
				"over NFSv4.1 must not error (EIO indicates the Pending-donor dedup "+
				"short-circuit or a non-converging rollup conflict has resurfaced)", i)
	}
	t.Logf("all %d concurrent byte-identical writes succeeded over NFSv4.1 (no EIO)",
		dedupRaceNFSv4Concurrent)

	// Drain the syncer so every block is flushed to S3 before we read back —
	// this makes the content check below a genuine server→S3→client round-trip
	// rather than a hit served from the NFSv4.1 local write-back cache.
	dedupDrainUploads(t, cli)

	// ---- Assert content correctness: every file reads back byte-identical ----
	allPaths := append([]string{donorPath}, racerPaths...)
	for _, path := range allPaths {
		got := framework.ReadFile(t, path)
		assert.Equalf(t, len(payload), len(got),
			"%s: re-read length should match payload (got %d, want %d)",
			path, len(got), len(payload))
		if !assertShaEqual(t, payload, got) {
			t.Errorf("%s: content mismatch on re-read after concurrent dedup race", path)
		}
	}
	t.Logf("all %d files (donor + %d racers) read back byte-identical over NFSv4.1",
		len(allPaths), dedupRaceNFSv4Concurrent)
}
