//go:build e2e

// Blocks-only storage E2E: the live write path now packs new writes into
// remote "blocks/<id>" objects rather than per-chunk "cas/<hash>" objects.
// These tests exercise the flip end-to-end through the real protocol stack
// (NFS and SMB) against a real Localstack S3 remote:
//
//  1. Fresh-share write → read → GC over blocks. Write a file, drain, and
//     assert the remote holds blocks/ objects and NO new cas/ objects. Read
//     it back byte-identical. Unlink it, run GC, and assert the block is
//     freed both remotely (blocks/ empty) and locally (per-share local
//     occupancy returns to zero).
//  2. Read-after-evict. Evict every local tier so the next read misses both
//     the read buffer and local disk, then read back byte-identical — proving
//     the block-locator fetch → blockcodec decode → BLAKE3 verify path serves
//     correct bytes from the remote packed block.
//  3. Corruption. Tamper a remote block's bytes directly in S3, force a cold
//     read, and assert the read FAILS CLOSED (BLAKE3 verification rejects the
//     mismatch) rather than returning silently-wrong data.
//
// The NFS variant uses a real kernel mount (sudo). The SMB variant uses the
// pure-Go go-smb2 client (no kernel mount). Both target the same s3-backed
// share type, so both drive the same engine carver + syncer + read path.
//
// Run command (requires sudo + Docker for Localstack):
//
//	cd test/e2e && sudo ./run-e2e.sh --s3 --test TestBlocksFlip
package e2e

import (
	"os"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/require"
)

// blocksIO abstracts the write/read/remove surface so the same lifecycle can
// run over an NFS kernel mount and over a pure-Go SMB client.
type blocksIO struct {
	proto  string
	write  func(name string, data []byte) error
	read   func(name string) ([]byte, error)
	remove func(name string) error
}

// setupBlocksShare provisions a memory-metadata + fs-local + s3-remote share
// for a blocks-flip test and returns the CLI runner, the s3 bucket, and the
// share name. It mirrors the setup used by the canonical CAS immutability
// test so both exercise the same store topology.
func setupBlocksShare(t *testing.T, shareName string) (*helpers.CLIRunner, *framework.LocalstackHelper, string) {
	t.Helper()

	lsHelper := framework.NewLocalstackHelper(t)
	require.NotNil(t, lsHelper, "Localstack helper must be available")

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	setup := helpers.SetupStoreMatrix(t, cli, shareName, helpers.MatrixSetupConfig{
		MetadataType: "memory",
		LocalType:    "fs",
		RemoteType:   "s3",
	}, nil, lsHelper)
	require.NotNil(t, setup, "store-matrix setup")

	require.NotEmpty(t, lsHelper.Buckets, "Localstack helper should track at least one bucket")
	bucket := lsHelper.Buckets[len(lsHelper.Buckets)-1]
	t.Logf("blocks bucket = %q", bucket)

	return cli, lsHelper, bucket
}

func TestBlocksFlipLifecycle_NFS(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping blocks-flip NFS E2E in short mode")
	}
	if !framework.CheckLocalstackAvailable(t) {
		t.Skip("Skipping: Localstack (S3) not available — run via run-e2e.sh --s3")
	}

	shareName := "/export-blocks-nfs"
	cli, lsHelper, bucket := setupBlocksShare(t, shareName)

	nfsPort := helpers.FindFreePort(t)
	_, err := cli.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err, "Should enable NFS adapter")
	t.Cleanup(func() { _, _ = cli.DisableAdapter("nfs") })

	require.NoError(t, helpers.WaitForAdapterStatus(t, cli, "nfs", true, 5*time.Second),
		"NFS adapter should become enabled")
	framework.WaitForServer(t, nfsPort, 10*time.Second)

	mount := mountNFSExport(t, nfsPort, shareName)
	t.Cleanup(mount.Cleanup)

	io := blocksIO{
		proto: "nfs",
		write: func(name string, data []byte) error {
			return os.WriteFile(mount.FilePath(name), data, 0o644)
		},
		read:   func(name string) ([]byte, error) { return os.ReadFile(mount.FilePath(name)) },
		remove: func(name string) error { return os.Remove(mount.FilePath(name)) },
	}

	runBlocksFlipSuite(t, cli, lsHelper, bucket, shareName, io)
}

func TestBlocksFlipLifecycle_SMB(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping blocks-flip SMB E2E in short mode")
	}
	if !framework.CheckLocalstackAvailable(t) {
		t.Skip("Skipping: Localstack (S3) not available — run via run-e2e.sh --s3")
	}

	shareName := "/export-blocks-smb"
	cli, lsHelper, bucket := setupBlocksShare(t, shareName)

	// SMB authenticates a concrete user; grant it read-write on the share
	// (the store-matrix share carries no default permission).
	const smbUser = "blocksmb"
	const smbPass = "blocksmb-passw0rd"
	_, err := cli.CreateUser(smbUser, smbPass)
	require.NoError(t, err, "Should create SMB user")
	require.NoError(t, cli.GrantUserPermission(shareName, smbUser, "read-write"),
		"Should grant SMB user read-write on share")

	smbPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("smb", helpers.WithAdapterPort(smbPort))
	require.NoError(t, err, "Should enable SMB adapter")
	t.Cleanup(func() { _, _ = cli.DisableAdapter("smb") })

	require.NoError(t, helpers.WaitForAdapterStatus(t, cli, "smb", true, 5*time.Second),
		"SMB adapter should become enabled")
	framework.WaitForServer(t, smbPort, 10*time.Second)

	session := helpers.ConnectSMB3(t, smbPort, smbUser, smbPass)
	share := helpers.MountSMB3Share(t, session, shareName)

	io := blocksIO{
		proto:  "smb",
		write:  func(name string, data []byte) error { return share.WriteFile(name, data, 0o644) },
		read:   func(name string) ([]byte, error) { return share.ReadFile(name) },
		remove: func(name string) error { return share.Remove(name) },
	}

	runBlocksFlipSuite(t, cli, lsHelper, bucket, shareName, io)
}

// runBlocksFlipSuite runs the full blocks-only lifecycle over a protocol IO.
func runBlocksFlipSuite(t *testing.T, cli *helpers.CLIRunner, lsHelper *framework.LocalstackHelper, bucket, shareName string, io blocksIO) {
	t.Helper()

	// A fresh bucket must have no cas/ or blocks/ objects yet.
	require.Empty(t, helpers.ListCASKeys(t, lsHelper, bucket),
		"[%s] fresh bucket should have no cas/ objects", io.proto)
	require.Empty(t, helpers.ListBlockKeys(t, lsHelper, bucket),
		"[%s] fresh bucket should have no blocks/ objects", io.proto)

	// ---- Step 1: fresh write → blocks/ (not cas/) → read-back identical ----
	const fileName = "flip.bin"
	payload := deterministicPayload(t, payloadSize, 11)

	require.NoError(t, io.write(fileName, payload), "[%s] write %s", io.proto, fileName)
	t.Logf("[%s] step 1: wrote %d bytes (sha256=%s)", io.proto, len(payload), shortSha256(payload))

	drainUploads(t, cli)

	blockKeys := helpers.ListBlockKeys(t, lsHelper, bucket)
	require.NotEmpty(t, blockKeys,
		"[%s] expected at least one blocks/ object after write+drain — the live path must pack chunks into blocks", io.proto)
	require.Empty(t, helpers.ListCASKeys(t, lsHelper, bucket),
		"[%s] the flipped write path must NOT create per-chunk cas/ objects — got %v",
		io.proto, helpers.ListCASKeys(t, lsHelper, bucket))
	t.Logf("[%s] step 1: remote holds %d blocks/ objects, 0 cas/ objects", io.proto, len(blockKeys))

	got, err := io.read(fileName)
	require.NoError(t, err, "[%s] read back %s", io.proto, fileName)
	assertSha256Equal(t, payload, got)

	// Local tier should hold the just-written data before eviction.
	statsAfterWrite := helpers.GetBlockStats(t, cli, shareName)
	require.Positive(t, statsAfterWrite.Totals.LocalDiskUsed,
		"[%s] local disk should hold the freshly-written blocks", io.proto)

	// ---- Step 2: read-after-evict (cold read from remote block) ----
	require.NoError(t, helpers.EvictBlocks(t, cli, shareName),
		"[%s] evict local tiers", io.proto)

	statsAfterEvict := helpers.GetBlockStats(t, cli, shareName)
	require.Less(t, statsAfterEvict.Totals.LocalDiskUsed, statsAfterWrite.Totals.LocalDiskUsed,
		"[%s] eviction should reduce local disk usage (was %d, now %d)",
		io.proto, statsAfterWrite.Totals.LocalDiskUsed, statsAfterEvict.Totals.LocalDiskUsed)

	gotCold, err := io.read(fileName)
	require.NoError(t, err, "[%s] cold read %s after evict", io.proto, fileName)
	assertSha256Equal(t, payload, gotCold)
	t.Logf("[%s] step 2: cold read after evict matched (served from remote block)", io.proto)

	// ---- Step 3: unlink → GC frees the block locally AND remotely ----
	require.NoError(t, io.remove(fileName), "[%s] unlink %s", io.proto, fileName)
	drainUploads(t, cli)

	// grace-period 0 reaps just-orphaned blocks immediately (bypasses the 5m floor).
	require.NoError(t, helpers.TriggerBlockGC(t, cli, shareName, "--grace-period", "0"),
		"[%s] block GC run", io.proto)

	waitForRemoteBlocksEmpty(t, lsHelper, bucket, io.proto, 30*time.Second)
	require.Empty(t, helpers.ListCASKeys(t, lsHelper, bucket),
		"[%s] GC must not introduce cas/ objects", io.proto)

	statsAfterGC := helpers.GetBlockStats(t, cli, shareName)
	require.Zero(t, statsAfterGC.Totals.LocalDiskUsed,
		"[%s] GC must free the local block too (local disk used should be 0, got %d)",
		io.proto, statsAfterGC.Totals.LocalDiskUsed)
	t.Logf("[%s] step 3: GC freed the block on both tiers (remote blocks/ empty, local disk 0)", io.proto)

	// ---- Step 4: corruption — tamper a remote block → read fails closed ----
	exerciseBlocksCorruption(t, cli, lsHelper, bucket, shareName, io)
}

// exerciseBlocksCorruption writes a fresh file, tampers its remote packed
// block directly in S3, forces a cold read, and asserts the read fails closed
// (BLAKE3 verification catches the mismatch) rather than returning wrong data.
func exerciseBlocksCorruption(t *testing.T, cli *helpers.CLIRunner, lsHelper *framework.LocalstackHelper, bucket, shareName string, io blocksIO) {
	t.Helper()

	const fileName = "corrupt.bin"
	payload := deterministicPayload(t, payloadSize, 22)

	require.NoError(t, io.write(fileName, payload), "[%s] write %s", io.proto, fileName)
	drainUploads(t, cli)

	blockKeys := helpers.ListBlockKeys(t, lsHelper, bucket)
	require.NotEmpty(t, blockKeys, "[%s] corruption test needs at least one blocks/ object", io.proto)
	tamperKey := blockKeys[0]

	// Corrupt a run of bytes in the middle of the block, preserving the object
	// length. This lands inside a packed chunk's wire bytes, so the per-chunk
	// BLAKE3 recompute on the read path must reject it.
	original := helpers.GetBlockObject(t, lsHelper, bucket, tamperKey)
	require.NotEmpty(t, original, "[%s] block object %q should be non-empty", io.proto, tamperKey)
	tampered := make([]byte, len(original))
	copy(tampered, original)
	start := len(tampered) / 2
	end := start + 64
	if end > len(tampered) {
		end = len(tampered)
	}
	for i := start; i < end; i++ {
		tampered[i] ^= 0xFF
	}
	helpers.PutBlockObject(t, lsHelper, bucket, tamperKey, tampered)
	t.Logf("[%s] step 4: tampered %d bytes of remote block %q (length preserved)", io.proto, end-start, tamperKey)

	// Force a genuine cold read so the tampered remote bytes are actually
	// fetched (evict both local disk and read buffer).
	require.NoError(t, helpers.EvictBlocks(t, cli, shareName), "[%s] evict before corrupt read", io.proto)

	data, readErr := io.read(fileName)
	switch {
	case readErr != nil:
		t.Logf("[%s] step 4 PASS: cold read of tampered block failed closed: %v", io.proto, readErr)
	case len(data) == len(payload) && bytesEqual(data, payload):
		// A tier we did not evict served the original verified bytes. This is
		// safe (the protocol never surfaced corrupt data) but does not prove
		// fail-closed on the tampered fetch — flag it loudly instead of a silent pass.
		t.Errorf("[%s] step 4 INCONCLUSIVE: cold read returned the ORIGINAL bytes — "+
			"the tampered remote block was not fetched (a cache tier served it). "+
			"Eviction may not have forced a remote fetch.", io.proto)
	default:
		t.Errorf("[%s] step 4 FAIL-OPEN: cold read of a tampered block returned %d bytes of "+
			"data that is neither an error nor the original payload — BLAKE3 verification "+
			"did not fail closed", io.proto, len(data))
	}

	// Clean up the corrupt file so trailing GC/cleanup is well-defined.
	_ = io.remove(fileName)
}

// waitForRemoteBlocksEmpty polls until the blocks/ prefix is empty or the
// timeout elapses, then asserts emptiness. GC runs to completion synchronously
// via the CLI, but the S3 listing may lag the DELETE slightly on some emulators.
func waitForRemoteBlocksEmpty(t *testing.T, lsHelper *framework.LocalstackHelper, bucket, proto string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []string
	for time.Now().Before(deadline) {
		last = helpers.ListBlockKeys(t, lsHelper, bucket)
		if len(last) == 0 {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.Empty(t, last,
		"[%s] GC must reap every blocks/ object after unlink; %d survived: %v",
		proto, len(last), last)
}

// bytesEqual reports whether two byte slices are identical. Kept local to
// avoid pulling bytes.Equal semantics into the assertion message path.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
