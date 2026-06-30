//go:build e2e

package e2e

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// SMB ↔ SMB byte-range locking (same-protocol)
//
// These tests exercise SMB2 LOCK enforcement between two independent SMB
// sessions against the same share. They mount the share twice (two cifs mounts =
// two SMB sessions) and hold the first lock in a *subprocess* so the two lock
// owners are genuinely distinct: POSIX fcntl locks are per-process, so two locks
// taken from the same process are merged by the kernel and never reach the
// server as a conflict. By re-exec'ing the test binary as a dedicated lock
// holder we force the conflict to be resolved server-side, which is what we want
// to verify.
//
// The companion NFSv4↔NFSv4 coverage lives in nfsv4_locking_test.go
// (TestNFSv4Locking / CrossClientConflict). The NLM(NFSv3)↔NLM axis is not
// covered here because the e2e harness mounts NFSv3 with `nolock`, so v3 locks
// never reach the server (tracked separately).
// =============================================================================

// holderEnvVar marks a re-exec'd lock-holder subprocess (see TestMain, which
// skips all shared setup/teardown when it is set).
const holderEnvVar = "DITTOFS_E2E_LOCK_HOLDER"

// holdSMBByteRangeLock re-execs the test binary as a subprocess that opens path,
// acquires an fcntl byte-range lock [offset, offset+length), and holds it until
// the returned stop func is called. It blocks until the child reports the lock
// is held, so on return the lock is guaranteed to be active. This yields a lock
// owner distinct from the test process.
func holdSMBByteRangeLock(t *testing.T, path string, offset, length int64, exclusive bool) (stop func()) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run", "^TestSMBLockHolderHelper$", "-test.v")
	cmd.Env = append(os.Environ(),
		holderEnvVar+"=1",
		"DITTOFS_HOLDER_PATH="+path,
		"DITTOFS_HOLDER_OFFSET="+strconv.FormatInt(offset, 10),
		"DITTOFS_HOLDER_LENGTH="+strconv.FormatInt(length, 10),
		"DITTOFS_HOLDER_EXCLUSIVE="+map[bool]string{true: "1", false: "0"}[exclusive],
	)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err, "holder: stdout pipe")
	require.NoError(t, cmd.Start(), "holder: start subprocess")

	// Wait for the child to report the lock is held (or failed).
	type result struct{ ok bool }
	done := make(chan result, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			t.Logf("holder: %s", line)
			if strings.Contains(line, "HOLDER_LOCKED") {
				done <- result{ok: true}
				return
			}
			if strings.Contains(line, "HOLDER_ERR") {
				done <- result{ok: false}
				return
			}
		}
		done <- result{ok: false}
	}()

	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	t.Cleanup(stop)

	select {
	case r := <-done:
		require.True(t, r.ok, "holder subprocess failed to acquire the lock")
	case <-time.After(20 * time.Second):
		stop()
		t.Fatal("holder subprocess did not acquire the lock within 20s")
	}
	return stop
}

// TestSMBLockHolderHelper is not a real test: when re-exec'd with holderEnvVar
// set it acts as the lock-holder subprocess for holdSMBByteRangeLock. Otherwise
// it returns immediately.
func TestSMBLockHolderHelper(t *testing.T) {
	if os.Getenv(holderEnvVar) != "1" {
		t.Skip("lock-holder helper (only runs as a re-exec'd subprocess)")
	}

	path := os.Getenv("DITTOFS_HOLDER_PATH")
	offset, offErr := strconv.ParseInt(os.Getenv("DITTOFS_HOLDER_OFFSET"), 10, 64)
	length, lenErr := strconv.ParseInt(os.Getenv("DITTOFS_HOLDER_LENGTH"), 10, 64)
	exclusive := os.Getenv("DITTOFS_HOLDER_EXCLUSIVE") == "1"
	if path == "" || offErr != nil || lenErr != nil {
		// Bad invocation: fail loudly rather than silently locking a wrong range
		// (e.g. Len=0 would mean "to EOF").
		fmt.Printf("HOLDER_ERR bad args: path=%q offset=%v length=%v\n", path, offErr, lenErr)
		return
	}

	// Under full-suite load the parent's create — issued on a *different* SMB
	// session — can lag this fresh session's view, so the open races the
	// server-side commit and transiently returns ENOENT, and the first
	// (uncontended) F_SETLK can momentarily hit a server-busy conflict. Retry
	// both within a bounded deadline (shorter than the parent's 20s wait) so a
	// load-induced race doesn't fail the holder and surface as the misleading
	// "did not acquire the lock within 20s".
	deadline := time.Now().Add(10 * time.Second)

	var f *os.File
	for {
		var openErr error
		f, openErr = os.OpenFile(path, os.O_RDWR, 0o644)
		if openErr == nil {
			break
		}
		if time.Now().After(deadline) {
			fmt.Printf("HOLDER_ERR open %s: %v\n", path, openErr)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer func() { _ = f.Close() }()

	lockType := int16(syscall.F_RDLCK)
	if exclusive {
		lockType = syscall.F_WRLCK
	}
	flock := &syscall.Flock_t{
		Type:   lockType,
		Whence: int16(os.SEEK_SET),
		Start:  offset,
		Len:    length,
	}
	for {
		lockErr := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, flock)
		if lockErr == nil {
			break
		}
		if time.Now().After(deadline) {
			fmt.Printf("HOLDER_ERR lock: %v\n", lockErr)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Println("HOLDER_LOCKED")
	_ = os.Stdout.Sync()

	// Hold the lock until the parent kills us.
	select {}
}

// setupSMBByteRangeLockTest starts a server with one SMB share and returns two
// independent cifs mounts of it plus the SMB port.
func setupSMBByteRangeLockTest(t *testing.T) (mount1, mount2 *framework.Mount) {
	t.Helper()
	framework.SkipIfNoSMBMount(t)
	framework.LogPlatformLockingNotes(t)

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)
	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	metaStoreName := helpers.UniqueTestName("meta")
	localStoreName := helpers.UniqueTestName("local")
	_, err := cli.CreateMetadataStore(metaStoreName, "memory")
	require.NoError(t, err, "Should create metadata store")
	_, err = cli.CreateLocalBlockStore(localStoreName, "memory")
	require.NoError(t, err, "Should create local block store")

	shareName := helpers.UniqueTestName("smblock")
	_, err = cli.CreateShare(shareName, metaStoreName, localStoreName,
		helpers.WithShareDefaultPermission("read-write"))
	require.NoError(t, err, "Should create share")

	smbCreds := framework.SMBCredentials{
		Username: "admin",
		Password: helpers.GetAdminPassword(),
	}

	smbPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("smb", helpers.WithAdapterPort(smbPort))
	require.NoError(t, err, "Should enable SMB adapter")
	require.NoError(t, helpers.WaitForAdapterStatus(t, cli, "smb", true, 5*time.Second),
		"SMB adapter should become enabled")
	framework.WaitForServer(t, smbPort, 10*time.Second)

	// Two cifs mounts of the same share = two independent SMB sessions.
	mount1 = framework.MountSMBExport(t, smbPort, shareName, smbCreds)
	t.Cleanup(mount1.Cleanup)
	mount2 = framework.MountSMBExport(t, smbPort, shareName, smbCreds)
	t.Cleanup(mount2.Cleanup)
	return mount1, mount2
}

// TestSMBByteRangeLocking validates SMB2 byte-range lock enforcement between two
// independent SMB sessions.
func TestSMBByteRangeLocking(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping SMB byte-range locking tests in short mode")
	}

	mount1, mount2 := setupSMBByteRangeLockTest(t)

	t.Run("SMBLK-01 exclusive lock blocks a conflicting exclusive lock", func(t *testing.T) {
		fileName := helpers.UniqueTestName("smblk_excl") + ".txt"
		p1 := mount1.FilePath(fileName)
		p2 := mount2.FilePath(fileName)
		framework.WriteFile(t, p1, []byte("smb exclusive lock conflict"))
		framework.WaitForFile(t, p2, 5*time.Second)

		// Holder (distinct owner) takes an exclusive lock on [0,100) via mount2.
		stop := holdSMBByteRangeLock(t, p2, 0, 100, true)

		// A conflicting exclusive lock via mount1 must be refused server-side.
		f, err := framework.TryLockFileRange(t, p1, 0, 100, true)
		if f != nil {
			framework.UnlockFileRange(t, f)
		}
		require.Error(t, err,
			"SMBLK-01: exclusive lock must be blocked while another session holds an overlapping exclusive lock")
		assert.ErrorIs(t, err, framework.ErrLockWouldBlock,
			"SMBLK-01: conflict should surface as a would-block error")

		// After the holder releases, the lock becomes available.
		stop()
		released := framework.WaitForRangeLockRelease(t, p1, 0, 100, true, 5*time.Second)
		assert.True(t, released, "SMBLK-01: lock should be grantable after the holder releases")
	})

	t.Run("SMBLK-02 non-overlapping ranges do not conflict", func(t *testing.T) {
		fileName := helpers.UniqueTestName("smblk_nonoverlap") + ".txt"
		p1 := mount1.FilePath(fileName)
		p2 := mount2.FilePath(fileName)
		framework.WriteFile(t, p1, make([]byte, 512))
		framework.WaitForFile(t, p2, 5*time.Second)

		stop := holdSMBByteRangeLock(t, p2, 0, 100, true)
		defer stop()

		// A disjoint range must be grantable concurrently.
		f, err := framework.TryLockFileRange(t, p1, 200, 100, true)
		require.NoError(t, err,
			"SMBLK-02: a non-overlapping range must not conflict with another session's lock")
		require.NotNil(t, f)
		framework.UnlockFileRange(t, f)
	})

	t.Run("SMBLK-03 shared locks are compatible", func(t *testing.T) {
		fileName := helpers.UniqueTestName("smblk_shared") + ".txt"
		p1 := mount1.FilePath(fileName)
		p2 := mount2.FilePath(fileName)
		framework.WriteFile(t, p1, []byte("smb shared lock compatibility"))
		framework.WaitForFile(t, p2, 5*time.Second)

		// Holder takes a shared (read) lock on [0,100) via mount2.
		stop := holdSMBByteRangeLock(t, p2, 0, 100, false)
		defer stop()

		// A second shared lock on the same range via mount1 must be granted.
		f, err := framework.TryLockFileRange(t, p1, 0, 100, false)
		require.NoError(t, err,
			"SMBLK-03: a shared lock must be compatible with another session's shared lock")
		require.NotNil(t, f)
		framework.UnlockFileRange(t, f)
	})
}
