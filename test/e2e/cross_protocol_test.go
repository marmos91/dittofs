//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	nfs3types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
	smbtypes "github.com/marmos91/dittofs/internal/adapter/smb/types"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCrossProtocolInterop validates cross-protocol interoperability between NFS and SMB.
// This covers requirements XPR-01 through XPR-06:
//   - XPR-01: File created via NFS is readable via SMB
//   - XPR-02: File created via SMB is readable via NFS
//   - XPR-03: File created via NFS is deletable via SMB
//   - XPR-04: File created via SMB is deletable via NFS
//   - XPR-05: Directory created via NFS is listable via SMB
//   - XPR-06: Directory created via SMB is listable via NFS
//
// This test proves the shared metadata/content store architecture works correctly
// by validating that changes made via one protocol are visible via the other.
func TestCrossProtocolInterop(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cross-protocol interop tests in short mode")
	}

	// Start server process
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Login as admin to configure the server
	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	// Create shared metadata and block stores
	// Both NFS and SMB will use the same stores to enable cross-protocol access
	metaStoreName := helpers.UniqueTestName("xpmeta")
	blockStoreName := helpers.UniqueTestName("xppayload")
	shareName := "/export"

	_, err := cli.CreateInMemoryMetadataStore(metaStoreName)
	require.NoError(t, err, "Should create metadata store")

	_, err = cli.CreateBlockStore(blockStoreName, "memory")
	require.NoError(t, err, "Should create block store")

	// Create share with read-write default permission
	_, err = cli.CreateShare(shareName, metaStoreName, blockStoreName,
		helpers.WithShareDefaultPermission("read-write"))
	require.NoError(t, err, "Should create share")

	// Create SMB test user with authentication credentials
	// SMB requires authenticated user (unlike NFS which uses AUTH_UNIX)
	smbUsername := helpers.UniqueTestName("xpuser")
	smbPassword := "testpass123" // Must be 8+ chars for SMB

	_, err = cli.CreateUser(smbUsername, smbPassword)
	require.NoError(t, err, "Should create SMB test user")

	// Grant SMB user read-write permission on the share
	err = cli.GrantUserPermission(shareName, smbUsername, "read-write")
	require.NoError(t, err, "Should grant SMB user permission")

	// Enable NFS adapter on a dynamic port
	nfsPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err, "Should enable NFS adapter")

	// Enable SMB adapter on a dynamic port
	smbPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("smb", helpers.WithAdapterPort(smbPort))
	require.NoError(t, err, "Should enable SMB adapter")

	// Wait for both adapters to be ready
	err = helpers.WaitForAdapterStatus(t, cli, "nfs", true, 5*time.Second)
	require.NoError(t, err, "NFS adapter should become enabled")

	err = helpers.WaitForAdapterStatus(t, cli, "smb", true, 5*time.Second)
	require.NoError(t, err, "SMB adapter should become enabled")

	// Wait for both servers to be listening
	framework.WaitForServer(t, nfsPort, 10*time.Second)
	framework.WaitForServer(t, smbPort, 10*time.Second)

	// Mount NFS share
	nfsMount := framework.MountNFS(t, nfsPort)
	t.Cleanup(nfsMount.Cleanup)

	// Mount SMB share with credentials
	smbCreds := framework.SMBCredentials{
		Username: smbUsername,
		Password: smbPassword,
	}
	smbMount := framework.MountSMB(t, smbPort, smbCreds)
	t.Cleanup(smbMount.Cleanup)

	// Run cross-protocol interoperability subtests
	// Note: These tests run sequentially (not parallel) as they share the same mounts
	t.Run("XPR-01 File created via NFS readable via SMB", func(t *testing.T) {
		testFileNFSToSMB(t, nfsMount, smbMount)
	})

	t.Run("XPR-02 File created via SMB readable via NFS", func(t *testing.T) {
		testFileSMBToNFS(t, nfsMount, smbMount)
	})

	t.Run("XPR-03 File created via NFS deletable via SMB", func(t *testing.T) {
		testDeleteNFSViaSMB(t, nfsMount, smbMount)
	})

	t.Run("XPR-04 File created via SMB deletable via NFS", func(t *testing.T) {
		testDeleteSMBViaNFS(t, nfsMount, smbMount)
	})

	t.Run("XPR-05 Directory created via NFS listable via SMB", func(t *testing.T) {
		testDirNFSToSMB(t, nfsMount, smbMount)
	})

	t.Run("XPR-06 Directory created via SMB listable via NFS", func(t *testing.T) {
		testDirSMBToNFS(t, nfsMount, smbMount)
	})
}

// testFileNFSToSMB tests XPR-01: File created via NFS is readable via SMB.
func testFileNFSToSMB(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	testContent := []byte("Written via NFS, should be readable via SMB")
	fileName := helpers.UniqueTestName("nfs_to_smb") + ".txt"

	nfsPath := nfsMount.FilePath(fileName)
	smbPath := smbMount.FilePath(fileName)

	// Write file via NFS
	framework.WriteFile(t, nfsPath, testContent)
	t.Cleanup(func() {
		_ = os.Remove(nfsPath)
	})

	// Wait for content to sync across protocols
	framework.WaitForContent(t, smbPath, testContent, 5*time.Second)

	// Read file via SMB and verify content
	readContent := framework.ReadFile(t, smbPath)
	assert.True(t, bytes.Equal(testContent, readContent),
		"Content written via NFS should be readable via SMB")

	// Verify file metadata matches
	nfsInfo := framework.GetFileInfo(t, nfsPath)
	smbInfo := framework.GetFileInfo(t, smbPath)
	assert.Equal(t, nfsInfo.Size, smbInfo.Size, "File size should match across protocols")

	t.Log("XPR-01: File created via NFS readable via SMB - PASSED")
}

// testFileSMBToNFS tests XPR-02: File created via SMB is readable via NFS.
func testFileSMBToNFS(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	testContent := []byte("Written via SMB, should be readable via NFS")
	fileName := helpers.UniqueTestName("smb_to_nfs") + ".txt"

	nfsPath := nfsMount.FilePath(fileName)
	smbPath := smbMount.FilePath(fileName)

	// Write file via SMB
	framework.WriteFile(t, smbPath, testContent)
	t.Cleanup(func() {
		_ = os.Remove(smbPath)
	})

	// Wait for content to sync across protocols
	framework.WaitForContent(t, nfsPath, testContent, 5*time.Second)

	// Read file via NFS and verify content
	readContent := framework.ReadFile(t, nfsPath)
	assert.True(t, bytes.Equal(testContent, readContent),
		"Content written via SMB should be readable via NFS")

	// Verify file metadata matches
	nfsInfo := framework.GetFileInfo(t, nfsPath)
	smbInfo := framework.GetFileInfo(t, smbPath)
	assert.Equal(t, nfsInfo.Size, smbInfo.Size, "File size should match across protocols")

	t.Log("XPR-02: File created via SMB readable via NFS - PASSED")
}

// testDeleteNFSViaSMB tests XPR-03: File created via NFS is deletable via SMB.
func testDeleteNFSViaSMB(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	testContent := []byte("Created via NFS, will be deleted via SMB")
	fileName := helpers.UniqueTestName("del_nfs_smb") + ".txt"

	nfsPath := nfsMount.FilePath(fileName)
	smbPath := smbMount.FilePath(fileName)

	// Create file via NFS
	framework.WriteFile(t, nfsPath, testContent)

	// Wait for the file to become visible via SMB
	framework.WaitForFile(t, smbPath, 5*time.Second)
	require.True(t, framework.FileExists(smbPath), "File should exist via SMB before deletion")

	// Delete file via SMB
	err := os.Remove(smbPath)
	require.NoError(t, err, "Should delete file via SMB")

	// Wait for cross-protocol cache invalidation
	framework.WaitForNoFile(t, nfsPath, 5*time.Second)

	// Verify file is deleted via NFS
	assert.False(t, framework.FileExists(nfsPath),
		"File deleted via SMB should not exist via NFS")

	t.Log("XPR-03: File created via NFS deletable via SMB - PASSED")
}

// testDeleteSMBViaNFS tests XPR-04: File created via SMB is deletable via NFS.
func testDeleteSMBViaNFS(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	testContent := []byte("Created via SMB, will be deleted via NFS")
	fileName := helpers.UniqueTestName("del_smb_nfs") + ".txt"

	nfsPath := nfsMount.FilePath(fileName)
	smbPath := smbMount.FilePath(fileName)

	// Create file via SMB
	framework.WriteFile(t, smbPath, testContent)

	// Wait for the file to become visible via NFS
	framework.WaitForFile(t, nfsPath, 5*time.Second)
	require.True(t, framework.FileExists(nfsPath), "File should exist via NFS before deletion")

	// Delete file via NFS
	err := os.Remove(nfsPath)
	require.NoError(t, err, "Should delete file via NFS")

	// Wait for the deletion to be observable via NFS
	framework.WaitForNoFile(t, nfsPath, 5*time.Second)

	// Verify file is deleted via SMB
	// Note: SMB client caching may cause this to appear present briefly
	// We verify the file was actually deleted by checking it doesn't exist via NFS
	require.False(t, framework.FileExists(nfsPath),
		"File deleted via NFS should not exist via NFS")

	t.Log("XPR-04: File created via SMB deletable via NFS - PASSED")
}

// testDirNFSToSMB tests XPR-05: Directory created via NFS is listable via SMB.
func testDirNFSToSMB(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	dirName := helpers.UniqueTestName("dir_nfs_smb")
	nfsDirPath := nfsMount.FilePath(dirName)
	smbDirPath := smbMount.FilePath(dirName)

	// Create directory via NFS
	framework.CreateDir(t, nfsDirPath)
	t.Cleanup(func() {
		_ = os.RemoveAll(nfsDirPath)
	})

	// Create files inside the directory via NFS
	fileNames := []string{"file1.txt", "file2.txt", "file3.txt"}
	for _, name := range fileNames {
		filePath := filepath.Join(nfsDirPath, name)
		framework.WriteFile(t, filePath, []byte("content for "+name))
	}

	// Create a subdirectory via NFS
	subDir := filepath.Join(nfsDirPath, "subdir")
	framework.CreateDir(t, subDir)

	// Wait for the directory to become visible via SMB
	framework.WaitForDir(t, smbDirPath, 5*time.Second)
	require.True(t, framework.DirExists(smbDirPath),
		"Directory created via NFS should exist via SMB")

	// List directory via SMB and verify entries
	entries := framework.ListDir(t, smbDirPath)

	expectedEntries := append(fileNames, "subdir")
	assert.Len(t, entries, len(expectedEntries), "Should have correct number of entries")

	for _, expected := range expectedEntries {
		found := false
		for _, entry := range entries {
			if entry == expected {
				found = true
				break
			}
		}
		assert.True(t, found, "Directory listing via SMB should contain %s", expected)
	}

	// Verify counts
	assert.Equal(t, len(fileNames), framework.CountFiles(t, smbDirPath),
		"File count should match via SMB")
	assert.Equal(t, 1, framework.CountDirs(t, smbDirPath),
		"Subdirectory count should match via SMB")

	t.Log("XPR-05: Directory created via NFS listable via SMB - PASSED")
}

// testDirSMBToNFS tests XPR-06: Directory created via SMB is listable via NFS.
func testDirSMBToNFS(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	dirName := helpers.UniqueTestName("dir_smb_nfs")
	nfsDirPath := nfsMount.FilePath(dirName)
	smbDirPath := smbMount.FilePath(dirName)

	// Create directory via SMB
	framework.CreateDir(t, smbDirPath)
	t.Cleanup(func() {
		_ = os.RemoveAll(smbDirPath)
	})

	// Create files inside the directory via SMB
	fileNames := []string{"alpha.txt", "beta.txt", "gamma.txt"}
	for _, name := range fileNames {
		filePath := filepath.Join(smbDirPath, name)
		framework.WriteFile(t, filePath, []byte("content for "+name))
	}

	// Create a subdirectory via SMB
	subDir := filepath.Join(smbDirPath, "nested")
	framework.CreateDir(t, subDir)

	// Wait for the directory to become visible via NFS
	framework.WaitForDir(t, nfsDirPath, 5*time.Second)
	require.True(t, framework.DirExists(nfsDirPath),
		"Directory created via SMB should exist via NFS")

	// List directory via NFS and verify entries
	entries := framework.ListDir(t, nfsDirPath)

	expectedEntries := append(fileNames, "nested")
	assert.Len(t, entries, len(expectedEntries), "Should have correct number of entries")

	for _, expected := range expectedEntries {
		found := false
		for _, entry := range entries {
			if entry == expected {
				found = true
				break
			}
		}
		assert.True(t, found, "Directory listing via NFS should contain %s", expected)
	}

	// Verify counts
	assert.Equal(t, len(fileNames), framework.CountFiles(t, nfsDirPath),
		"File count should match via NFS")
	assert.Equal(t, 1, framework.CountDirs(t, nfsDirPath),
		"Subdirectory count should match via NFS")

	t.Log("XPR-06: Directory created via SMB listable via NFS - PASSED")
}

// ============================================================================
// TestCrossProtocol_ErrorConformance.
// ============================================================================
//
// Table-driven verification that the same metadata.ErrorCode produces
// consistent client-observable errnos on both the NFS and SMB mounts, using
// the per-adapter types tables as the single source of truth for expected
// per-protocol codes. One shared server and one set of mounts are reused
// across all subtests, because mounting per subtest is slow and flaky.
//
// The assertion is: types.StatusForErr(storeErr) (nfs/types) AND
// smb/types.StatusForErr(storeErr) from
// the per-adapter types switches match what the kernel NFS/SMB client actually delivers.
// Since the kernel translates protocol codes into errnos for userspace, the
// test compares observed syscall.Errno to the errno that the protocol code
// maps to (via kernel-stable translations documented in this file's
// nfs3StatusToErrno and smbStatusToErrno tables). This preserves the
// one-edit contract: adding a new ErrorCode only requires adding a row in
// the per-adapter types tables plus (when e2e-triggerable) one trigger helper in
// test/e2e/helpers/error_triggers.go and one table row here — the expected
// errnos are derived at runtime from the per-adapter types packages' StatusForErr.
//
// Test tier split:
//   - E2E tier (this function): codes a Linux kernel client can provoke
//     through file-I/O syscalls against a server it does not control.
//   - Unit tier: codes nothing on that path produces — malformed handles,
//     backend I/O faults, NFSv4-only operations, lock-state errors that the
//     kernel clients never send, and codes with no producer at all. See
//     internal/adapter/nfs/types/statusfor_test.go (per-package full-enum tables).
func TestCrossProtocol_ErrorConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cross-protocol error conformance tests in short mode")
	}

	fixture := setupErrorConformanceFixture(t)

	// The conformance table: one row per e2e-triggerable metadata.ErrorCode.
	// Each row names the code, provides the protocol-agnostic trigger, and
	// names the share whose mounts the trigger runs against.
	//
	// Expected errnos are derived at runtime from the per-adapter types packages via
	// nfs3StatusToErrno(nfs3types.StatusForErr(sentinelErr)) and
	// smbStatusToErrno(smbtypes.StatusForErr(sentinelErr)). This keeps a single
	// source of truth (the per-adapter types tables) — adding a new ErrorCode requires
	// one row here, not a second hand-transcribed errno column.
	cases := []errorConformanceCase{
		{name: "ErrNotFound", code: merrs.ErrNotFound, trigger: helpers.TriggerErrNotFound},
		{name: "ErrAlreadyExists", code: merrs.ErrAlreadyExists, trigger: helpers.TriggerErrAlreadyExists},
		{name: "ErrNotEmpty", code: merrs.ErrNotEmpty, trigger: helpers.TriggerErrNotEmpty},
		{name: "ErrIsDirectory", code: merrs.ErrIsDirectory, trigger: helpers.TriggerErrIsDirectory},
		{name: "ErrNotDirectory", code: merrs.ErrNotDirectory, trigger: helpers.TriggerErrNotDirectory},
		{name: "ErrNameTooLong", code: merrs.ErrNameTooLong, trigger: helpers.TriggerErrNameTooLong},
		{name: "ErrInvalidArgument", code: merrs.ErrInvalidArgument, trigger: helpers.TriggerErrInvalidArgument},
		// decision: this row pins the errno a server-side write denial
		// reaches the client as, not which check denies it. A read-only
		// default permission also gives the share root a root-owned 0755
		// mode, so the mode bits refuse the write too, and the row passes
		// even with the share-grant check broken. Pin the grant separately,
		// on a share whose root mode bits would allow the write, if the
		// grant check needs its own e2e coverage.
		{name: "ErrAccessDenied", code: merrs.ErrAccessDenied, trigger: helpers.TriggerErrAccessDenied, share: shareRestricted},
		// decision: only the NFS leg runs. Nothing a cifs client sends
		// without POSIX extensions reaches the ownership check: it discards
		// the server's error on a chmod, never sends a chown, and an explicit
		// timestamp is authorized by FILE_WRITE_ATTRIBUTES on the open handle
		// rather than by ownership. Run the SMB leg if the fixture ever mounts
		// with an option that sends mode or owner changes (cifsacl,
		// modefromsid).
		{name: "ErrPermissionDenied", code: merrs.ErrPermissionDenied, trigger: helpers.TriggerErrPermissionDenied,
			skipSMBReason: "no cifs operation reaches the server's ownership check"},
		{name: "ErrReadOnly", code: merrs.ErrReadOnly, trigger: helpers.TriggerErrReadOnly, share: shareReadOnly},
		{name: "ErrNoSpace", code: merrs.ErrNoSpace, trigger: helpers.TriggerErrNoSpace, share: shareQuota},
		{name: "ErrLocked", code: merrs.ErrLocked, trigger: helpers.TriggerErrLocked},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Derive expected errno-per-protocol from the per-adapter types tables.
			// Single-source-of-truth pivot: adding a new ErrorCode row
			// in the per-adapter types tables propagates here automatically.
			sentinel := &merrs.StoreError{Code: c.code, Message: c.name}
			wantNFSErrno := nfs3StatusToErrno(nfs3types.StatusForErr(sentinel))
			wantSMBErrno := smbStatusToErrno(smbtypes.StatusForErr(sentinel))

			m, ok := fixture.mounts[c.share]
			require.True(t, ok, "fixture has no mounts for share %d", c.share)

			// NFS side.
			t.Run("nfs", func(t *testing.T) {
				got := c.trigger(t, m.nfs, m.nfsPeer)
				assertErrnoMatches(t, "NFS "+c.name, got, wantNFSErrno)
			})

			// SMB side.
			t.Run("smb", func(t *testing.T) {
				if c.skipSMBReason != "" {
					t.Skipf("SMB subtest skipped: %s", c.skipSMBReason)
				}
				got := c.trigger(t, m.smb, m.smbPeer)
				assertErrnoMatches(t, "SMB "+c.name, got, wantSMBErrno)
			})
		})
	}
}

// conformanceShare names which of the fixture's shares a conformance row runs
// against. The zero value is the primary read-write share.
type conformanceShare int

const (
	// shareReadWrite is "/export": read-write for every identity. The only
	// share with peer mounts.
	shareReadWrite conformanceShare = iota
	// shareReadOnly is "/archive": a read-only share. No peer.
	shareReadOnly
	// shareRestricted is "/restricted": readable by everyone, writable only
	// by an explicit grant. Both legs mount it as an identity without one.
	// No peer.
	shareRestricted
	// shareQuota is "/quota": read-write with a 1 MiB byte quota. No peer.
	shareQuota
)

// conformanceMounts is the set of mount roots a row's trigger runs against.
// nfsPeer and smbPeer are the peer roots handed to the trigger on each leg:
// a mount of the same share by another client or identity, or "" when the
// share has none.
type conformanceMounts struct {
	nfs, smb         string
	nfsPeer, smbPeer string
}

// errorConformanceCase is a single row in TestCrossProtocol_ErrorConformance's
// driving table. The trigger is protocol-agnostic: the same function fires
// once against the NFS mount root and once against the SMB mount root. Both
// invocations return the syscall.Errno observed from the kernel for each
// protocol's translation of the on-wire status.
type errorConformanceCase struct {
	name string
	// code is the sentinel metadata.ErrorCode whose expected NFS/SMB codes
	// are derived via nfs3types.StatusForErr / smbtypes.StatusForErr at test time.
	code merrs.ErrorCode
	// trigger fires ONE scenario against the given mount root and returns
	// the observed TriggerResult. Both protocols share the same trigger.
	trigger func(t *testing.T, mountRoot, peerRoot string) helpers.TriggerResult
	// share selects the fixture share the trigger runs against.
	share conformanceShare
	// skipSMBReason, when set, skips the SMB leg of this subtest. It must
	// say why the code cannot be produced over SMB.
	skipSMBReason string
}

// errorConformanceFixture holds the shared state for
// TestCrossProtocol_ErrorConformance: one server and, for every share a row
// can select, its NFS and SMB mounts.
type errorConformanceFixture struct {
	mounts map[conformanceShare]conformanceMounts
}

// setupErrorConformanceFixture bootstraps the shared server + mounts for
// TestCrossProtocol_ErrorConformance. Runs once per test (not per subtest).
func setupErrorConformanceFixture(t *testing.T) *errorConformanceFixture {
	t.Helper()

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	createShare := func(prefix, share string, opts ...helpers.ShareOption) {
		t.Helper()
		meta := helpers.UniqueTestName(prefix + "meta")
		block := helpers.UniqueTestName(prefix + "payload")
		_, err := cli.CreateInMemoryMetadataStore(meta)
		require.NoError(t, err)
		_, err = cli.CreateBlockStore(block, "memory")
		require.NoError(t, err)
		_, err = cli.CreateShare(share, meta, block, opts...)
		require.NoError(t, err)
	}

	createShare("conf", "/export", helpers.WithShareDefaultPermission("read-write"))
	createShare("confro", "/archive",
		helpers.WithShareReadOnly(true),
		helpers.WithShareDefaultPermission("read-write"))
	// Readable by every identity — including the guest an NFS root is
	// squashed to — and writable only through an explicit grant.
	createShare("confrestricted", "/restricted", helpers.WithShareDefaultPermission("read"))
	createShare("confquota", "/quota",
		helpers.WithShareDefaultPermission("read-write"),
		helpers.WithShareQuotaBytes("1MiB"))

	// SMB users. The writer is granted read-write on every share but
	// "/restricted", where the reader is granted only read.
	smbPass := "testpass123"
	writer := helpers.UniqueTestName("confuser")
	_, err := cli.CreateUser(writer, smbPass)
	require.NoError(t, err)
	for _, share := range []string{"/export", "/archive", "/quota"} {
		require.NoError(t, cli.GrantUserPermission(share, writer, "read-write"))
	}
	reader := helpers.UniqueTestName("confreader")
	_, err = cli.CreateUser(reader, smbPass)
	require.NoError(t, err)
	require.NoError(t, cli.GrantUserPermission("/restricted", reader, "read"))

	// Enable adapters.
	nfsPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err)
	smbPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("smb", helpers.WithAdapterPort(smbPort))
	require.NoError(t, err)

	require.NoError(t, helpers.WaitForAdapterStatus(t, cli, "nfs", true, 5*time.Second))
	require.NoError(t, helpers.WaitForAdapterStatus(t, cli, "smb", true, 5*time.Second))
	framework.WaitForServer(t, nfsPort, 10*time.Second)
	framework.WaitForServer(t, smbPort, 10*time.Second)

	writerCreds := framework.SMBCredentials{Username: writer, Password: smbPass}
	readerCreds := framework.SMBCredentials{Username: reader, Password: smbPass}
	mountNFS := func(share string) string {
		m := framework.MountNFSExportWithVersion(t, nfsPort, share, "3")
		t.Cleanup(m.Cleanup)
		return m.Path
	}
	mountSMB := func(share string, creds framework.SMBCredentials) string {
		m := framework.MountSMBExport(t, smbPort, share, creds)
		t.Cleanup(m.Cleanup)
		return m.Path
	}

	rwNFS := mountNFS("/export")
	rwSMB := mountSMB("/export", writerCreds)

	return &errorConformanceFixture{mounts: map[conformanceShare]conformanceMounts{
		// Each protocol's peer is the other protocol's mount of the share. The
		// two identities differ — NFS root is squashed to the guest, SMB is
		// the writer — so a file one creates is not owned by the other.
		shareReadWrite: {nfs: rwNFS, smb: rwSMB, nfsPeer: rwSMB, smbPeer: rwNFS},
		shareReadOnly:  {nfs: mountNFS("/archive"), smb: mountSMB("/archive", writerCreds)},
		// NFS mounts as root, which the default squash maps to the guest;
		// SMB mounts as the reader. Neither may write.
		shareRestricted: {nfs: mountNFS("/restricted"), smb: mountSMB("/restricted", readerCreds)},
		shareQuota:      {nfs: mountNFS("/quota"), smb: mountSMB("/quota", writerCreds)},
	}}
}

// assertErrnoMatches compares an observed TriggerResult to an expected
// syscall.Errno, with a human-friendly diagnostic on failure. An observed
// "no errno" (operation unexpectedly succeeded) is always a failure.
func assertErrnoMatches(t *testing.T, label string, got helpers.TriggerResult, want syscall.Errno) {
	t.Helper()
	if !got.HasErrno {
		t.Errorf("%s: %s; want errno=%d (%s)", label, helpers.FormatTriggerDiag(got), int(want), want.Error())
		return
	}
	if got.Errno != want {
		t.Errorf("%s: got errno=%d (%s), want errno=%d (%s)",
			label, int(got.Errno), got.Errno.Error(), int(want), want.Error())
	}
}

// nfs3StatusToErrno translates a raw NFS3 status code into the syscall.Errno
// the Linux NFS client surfaces to userspace. This mapping is kernel-stable
// (see linux/fs/nfs/nfs3proc.c nfs3_proc_error_to_errno et al). The table
// below covers every code the NFSv3 adapter maps (nfs/types StatusFor);
// unknown codes fall through to EIO (matches kernel behavior for NFS3ERR_IO
// and unrecognized codes).
func nfs3StatusToErrno(code uint32) syscall.Errno {
	switch code {
	case nfs3types.NFS3OK:
		return 0
	case nfs3types.NFS3ErrPerm:
		return syscall.EPERM
	case nfs3types.NFS3ErrNoEnt:
		return syscall.ENOENT
	case nfs3types.NFS3ErrIO:
		return syscall.EIO
	case nfs3types.NFS3ErrAccess:
		return syscall.EACCES
	case nfs3types.NFS3ErrExist:
		return syscall.EEXIST
	case nfs3types.NFS3ErrNotDir:
		return syscall.ENOTDIR
	case nfs3types.NFS3ErrIsDir:
		return syscall.EISDIR
	case nfs3types.NFS3ErrInval:
		return syscall.EINVAL
	case nfs3types.NFS3ErrFBig:
		return syscall.EFBIG
	case nfs3types.NFS3ErrNoSpc:
		return syscall.ENOSPC
	case nfs3types.NFS3ErrRofs:
		return syscall.EROFS
	case nfs3types.NFS3ErrNameTooLong:
		return syscall.ENAMETOOLONG
	case nfs3types.NFS3ErrNotEmpty:
		return syscall.ENOTEMPTY
	case nfs3types.NFS3ErrDquot:
		return syscall.EDQUOT
	case nfs3types.NFS3ErrStale:
		return syscall.ESTALE
	case nfs3types.NFS3ErrBadHandle:
		return syscall.ESTALE
	case nfs3types.NFS3ErrNotSupp:
		return syscall.EOPNOTSUPP
	case nfs3types.NFS3ErrJukebox:
		// JUKEBOX is NFS-specific "try again later" — the Linux client
		// maps it to EAGAIN/EJUKEBOX. Used for lock conflicts and
		// transient resource failures.
		return syscall.EAGAIN
	default:
		return syscall.EIO
	}
}

// smbStatusToErrno translates a raw SMB NT status into the syscall.Errno the
// Linux cifs client surfaces to userspace. This mapping mirrors kernel
// fs/cifs/smb2maperror.c — every code the SMB adapter maps (smb/types StatusFor)
// column is covered explicitly; unknown codes fall through to EIO.
func smbStatusToErrno(code smbtypes.Status) syscall.Errno {
	switch code {
	case smbtypes.StatusSuccess:
		return 0
	case smbtypes.StatusObjectNameNotFound:
		return syscall.ENOENT
	case smbtypes.StatusAccessDenied:
		return syscall.EACCES
	case smbtypes.StatusObjectNameCollision:
		return syscall.EEXIST
	case smbtypes.StatusDirectoryNotEmpty:
		return syscall.ENOTEMPTY
	case smbtypes.StatusFileIsADirectory:
		return syscall.EISDIR
	case smbtypes.StatusNotADirectory:
		return syscall.ENOTDIR
	case smbtypes.StatusInvalidParameter:
		return syscall.EINVAL
	case smbtypes.StatusDiskFull:
		return syscall.ENOSPC
	case smbtypes.StatusNotSupported:
		return syscall.EOPNOTSUPP
	case smbtypes.StatusInvalidHandle:
		// cifs surfaces bad-handle as EBADF.
		return syscall.EBADF
	case smbtypes.StatusFileClosed:
		// Matches cifs client's ESTALE mapping for
		// STATUS_FILE_CLOSED on stale-handle responses.
		return syscall.ESTALE
	case smbtypes.StatusObjectNameInvalid:
		// Linux cifs maps the common "name malformed / too long" codes to
		// ENAMETOOLONG in practice; EINVAL is the alternate surface. We
		// take ENAMETOOLONG since that matches the triggered-case
		// (TriggerErrNameTooLong uses a 300-char name).
		return syscall.ENAMETOOLONG
	case smbtypes.StatusFileLockConflict:
		// Real Linux cifs maps the SMB lock-conflict NT statuses to -EACCES,
		// NOT -EAGAIN (fs/smb/client/smb2maperror.c). A non-blocking flock()
		// conflict over cifs therefore surfaces EACCES to userspace even
		// though POSIX flock() would otherwise report EWOULDBLOCK. (NFS keeps
		// EAGAIN: its NFS3ERR_JUKEBOX → EAGAIN mapping is unaffected.)
		return syscall.EACCES
	case smbtypes.StatusLockNotGranted:
		return syscall.EACCES
	case smbtypes.StatusRangeNotLocked:
		return syscall.EINVAL
	case smbtypes.StatusUnexpectedIOError:
		return syscall.EIO
	case smbtypes.StatusInsufficientResources:
		return syscall.ENOMEM
	case smbtypes.StatusInternalError:
		return syscall.EIO
	default:
		return syscall.EIO
	}
}
