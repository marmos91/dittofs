//go:build e2e

// TestObjectIDPopulation_NFSWriteQuiesce is the goal-backward verification
// gate: after a real NFSv3 write quiesces through the runtime,
// FileAttr.ObjectID for that file MUST be a non-zero BLAKE3 Merkle root
// equal to ComputeObjectID(Blocks).
//
// The ObjectID-population path now lives inside the local store's
// rollup-completion callback (the ObjectIDPersister installed by
// engine.New). The test asserts that a real NFSv3 write quiesces
// through the runtime and lands a non-zero BLAKE3 Merkle root in
// FileAttr.ObjectID equal to ComputeObjectID(Blocks).
//
// Tier: nightly only. Mirrors dedup_cross_share_test.go and dedup_vmfleet_test.go
// — requires sudo + kernel NFS client + Localstack (S3) + Postgres testcontainer.
//
// Run:
//
//	cd test/e2e && DITTOFS_E2E_NIGHTLY=1 sudo ./run-e2e.sh \
//	    --s3 --test TestObjectIDPopulation_NFSWriteQuiesce
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/require"
)

// objectIDPopulationPayloadSize is intentionally 4 MiB — small enough to keep
// the nightly run quick, large enough that FastCDC produces multiple chunks
// (min=1 MiB / avg=4 MiB / max=16 MiB) so the resulting Merkle root is
// non-trivial. Deterministic content (Repeat 0xAB) makes ObjectID reproducible
// across runs and machines for any future regression spot-check.
const objectIDPopulationPayloadSize = 4 << 20

// TestObjectIDPopulation_NFSWriteQuiesce is the e2e RED gate for VERIFICATION
// must-have #1: FileAttr.ObjectID is populated post-Flush.
func TestObjectIDPopulation_NFSWriteQuiesce(t *testing.T) {
	if os.Getenv("DITTOFS_E2E_NIGHTLY") != "1" {
		t.Skip("nightly tier only; set DITTOFS_E2E_NIGHTLY=1")
	}
	if testing.Short() {
		t.Skip("Skipping ObjectID population E2E in short mode")
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
	// ObjectID rows that pre-claim our payload's chunks before our write
	// even lands (false-negative regime).
	require.NoError(t, pgHelper.TruncateTables(),
		"Truncate Postgres tables for isolation")

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	// ---- Postgres metadata store (same DSN we open directly below) ----
	metaName := helpers.UniqueTestName("objid-meta")
	pgConfig := pgHelper.GetConfig()
	pgConfigJSON := fmt.Sprintf(
		`{"host":"%s","port":%d,"database":"%s","user":"%s","password":"%s"}`,
		pgConfig.Host, pgConfig.Port, pgConfig.Database, pgConfig.User, pgConfig.Password,
	)
	_, err := cli.CreateMetadataStore(metaName, "postgres",
		helpers.WithMetaRawConfig(pgConfigJSON))
	require.NoError(t, err, "create Postgres metadata store")
	t.Cleanup(func() { _ = cli.DeleteMetadataStore(metaName) })

	// ---- One S3 bucket + remote-block-store record ----
	bucketName := strings.ReplaceAll(
		fmt.Sprintf("dittofs-objid-%s", helpers.UniqueTestName("bkt")), "_", "-")
	require.NoError(t,
		lsHelper.CreateBucket(context.Background(), bucketName),
		"create S3 bucket")
	t.Cleanup(func() { lsHelper.CleanupBucket(context.Background(), bucketName) })

	remoteName := helpers.UniqueTestName("objid-remote")
	_, err = cli.CreateRemoteBlockStore(remoteName, "s3",
		helpers.WithBlockS3Config(bucketName, "us-east-1",
			lsHelper.Endpoint, "test", "test"))
	require.NoError(t, err, "create remote block store")
	t.Cleanup(func() { _ = cli.DeleteRemoteBlockStore(remoteName) })

	// ---- Per-share local block store (CLAUDE.md invariant: dirs isolated) ----
	localName := helpers.UniqueTestName("objid-local")
	localPath := t.TempDir()
	_, err = cli.CreateLocalBlockStore(localName, "fs",
		helpers.WithBlockRawConfig(fmt.Sprintf(`{"path":"%s"}`, localPath)))
	require.NoError(t, err, "create local block store")
	t.Cleanup(func() { _ = cli.DeleteLocalBlockStore(localName) })

	// ---- One share ----
	shareName := "/objid-pop"
	_, err = cli.CreateShare(shareName, metaName, localName, helpers.WithShareRemote(remoteName))
	require.NoError(t, err, "create share %s", shareName)
	t.Cleanup(func() { _ = cli.DeleteShare(shareName) })

	// ---- Enable NFS adapter ----
	nfsPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err, "enable NFS adapter")
	t.Cleanup(func() { _, _ = cli.DisableAdapter("nfs") })
	require.NoError(t,
		helpers.WaitForAdapterStatus(t, cli, "nfs", true, 10*time.Second),
		"NFS adapter should become enabled")
	framework.WaitForServer(t, nfsPort, 10*time.Second)

	// ---- Mount over NFSv3 and write the deterministic payload ----
	mount := framework.MountNFSExportWithVersion(t, nfsPort, shareName, "3")
	t.Cleanup(mount.Cleanup)

	data := bytes.Repeat([]byte{0xAB}, objectIDPopulationPayloadSize)
	dst := mount.FilePath("test.bin")
	t.Cleanup(func() { _ = os.Remove(dst) })
	framework.WriteFile(t, dst, data)
	t.Logf("wrote %d bytes to %s%s", len(data), shareName, "/test.bin")

	// ---- Drain uploads to force Pending -> Remote + post-Flush hook ----
	objIDDrainUploads(t, cli)

	// ---- Open a second handle to the same Postgres backend ----
	//
	// The plan permits this pattern when no in-tree helper exists for
	// resolving the server-internal *PostgresMetadataStore: we open our
	// own with the same DSN. Read-only at the moment of query; safe to
	// run concurrently with the server's pool.
	caps := metadata.FilesystemCapabilities{
		MaxReadSize:         1048576,
		PreferredReadSize:   1048576,
		MaxWriteSize:        1048576,
		PreferredWriteSize:  1048576,
		MaxFileSize:         9223372036854775807,
		MaxFilenameLen:      255,
		MaxPathLen:          4096,
		MaxHardLinkCount:    32767,
		SupportsHardLinks:   true,
		SupportsSymlinks:    true,
		CaseSensitive:       true,
		CasePreserving:      true,
		TimestampResolution: 1,
	}
	mdsCfg := &postgres.PostgresMetadataStoreConfig{
		Host:     pgConfig.Host,
		Port:     pgConfig.Port,
		Database: pgConfig.Database,
		User:     pgConfig.User,
		Password: pgConfig.Password,
		SSLMode:  "disable",
		// AutoMigrate intentionally false: the server owns schema management;
		// we attach in read-only intent.
	}
	mds, err := postgres.NewPostgresMetadataStore(t.Context(), mdsCfg, caps)
	require.NoError(t, err, "open second Postgres handle for verification")
	t.Cleanup(func() { _ = mds.Close() })

	// ---- Resolve the file row ----
	//
	// Files created after #1166 PR-3 get a UUID-based PayloadID, so we cannot
	// reconstruct the content_id from the path. Walk the namespace from the
	// share root to the file, then read its stored attributes (including the
	// PayloadID the server assigned at create time).
	rootHandle, err := mds.GetRootHandle(t.Context(), shareName)
	require.NoErrorf(t, err, "GetRootHandle(%s)", shareName)
	fileHandle, err := mds.GetChild(t.Context(), rootHandle, "test.bin")
	require.NoError(t, err, "GetChild(root, test.bin)")
	file, err := mds.GetFile(t.Context(), fileHandle)
	require.NoError(t, err, "GetFile(test.bin)")
	require.NotNil(t, file, "GetFile(test.bin) returned nil file")

	// The stored PayloadID must round-trip through GetFileByPayloadID, and
	// resolve to the SAME inode we walked to above.
	byPayload, err := mds.GetFileByPayloadID(t.Context(), file.PayloadID)
	require.NoErrorf(t, err, "GetFileByPayloadID(%s)", string(file.PayloadID))
	require.NotNilf(t, byPayload, "GetFileByPayloadID(%s) returned nil file", string(file.PayloadID))
	require.Equalf(t, file.ID, byPayload.ID,
		"GetFileByPayloadID(%s) resolved to a different inode", string(file.PayloadID))

	// ---- REQUIRED RED ASSERT (must-have #1, 13-VERIFICATION.md:257-266) ----
	require.Falsef(t, file.FileAttr.ObjectID.IsZero(),
		"FileAttr.ObjectID is the all-zero sentinel after a %d-byte "+
			"NFS write + drain. The rollup-completion ObjectIDPersister "+
			"should have computed the Merkle root on rollup commit and "+
			"the coordinator should have persisted it to FileAttr.ObjectID.",
		objectIDPopulationPayloadSize)

	// ---- REQUIRED CORRECTNESS ASSERT (regression catcher) ----
	expected := block.ComputeObjectID(file.FileAttr.Blocks)
	require.Equalf(t, expected, file.FileAttr.ObjectID,
		"ObjectID is non-zero but does NOT equal ComputeObjectID(Blocks) — "+
			"D-01 Merkle-root reproducibility broken. blocks=%d expected=%s got=%s",
		len(file.FileAttr.Blocks), expected.String(), file.FileAttr.ObjectID.String())

	// ---- Telemetry parity: emit at slog INFO for trend tracking ----
	slog.Info("DEDUP-04 objectid populated",
		"payload_id", string(file.PayloadID),
		"object_id", file.FileAttr.ObjectID.String(),
		"blocks", len(file.FileAttr.Blocks),
		"size", objectIDPopulationPayloadSize)
}

// objIDDrainUploads mirrors dedup_cross_share_test.go::dedupDrainUploads and
// dedup_vmfleet_test.go::vmFleetDrainUploads — kept inline
// "keep the diff minimal" guidance. If a fourth drain-uploads test arrives
// we should lift to a shared helper; today's diff stays minimal.
func objIDDrainUploads(t *testing.T, runner *helpers.CLIRunner) {
	t.Helper()
	out, err := runner.Run("system", "drain-uploads")
	require.NoError(t, err, "drain-uploads should succeed: %s", string(out))
}
