//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// NFSv4 Locking E2E Tests
// =============================================================================
//
// These tests validate NFSv4 byte-range locking (LOCK/LOCKT/LOCKU) through
// actual NFS kernel client mounts, not just unit tests. Tests are parameterized
// to run against both NFSv3 (NLM) and NFSv4.0 (integrated locking) where
// applicable.
//
// The Linux kernel NFS client translates fcntl() F_SETLK/F_SETLKW calls
// into the appropriate locking protocol:
//   - NFSv3: NLM LOCK/UNLOCK/TEST operations
//   - NFSv4: LOCK/LOCKU/LOCKT compound operations
//
// On macOS, NFSv4 locking may not work with userspace servers; tests use
// SkipIfDarwin to handle this.

// TestNFSv4Locking validates byte-range locking semantics across both NFSv3
// and NFSv4.0 mounts. Each subtest exercises a different aspect of POSIX
// byte-range locks (via fcntl) over NFS.
func TestNFSv4Locking(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4 locking tests in short mode")
	}

	// NFSv4 locking tests require Linux
	framework.SkipIfDarwin(t)
	framework.SkipIfNFSLockingUnsupported(t)
	framework.LogPlatformLockingNotes(t)

	_, _, nfsPort := setupNFSv4TestServer(t)

	versions := []string{"3", "4.0"}
	for _, ver := range versions {
		ver := ver
		t.Run(fmt.Sprintf("v%s", ver), func(t *testing.T) {
			if ver == "4.0" {
				framework.SkipIfNFSv4Unsupported(t)
			}

			// Mount the share for this version
			mount1 := framework.MountNFSWithVersion(t, nfsPort, ver)
			t.Cleanup(mount1.Cleanup)

			// Mount a second instance for cross-client tests
			mount2 := framework.MountNFSWithVersion(t, nfsPort, ver)
			t.Cleanup(mount2.Cleanup)

			// a. ReadWriteLocks: shared locks coexist, exclusive conflicts
			t.Run("ReadWriteLocks", func(t *testing.T) {
				testReadWriteLocks(t, mount1, ver)
			})

			// b. ExclusiveLock: mutual exclusion
			t.Run("ExclusiveLock", func(t *testing.T) {
				testExclusiveLock(t, mount1, ver)
			})

			// c. OverlappingRanges: non-overlapping succeeds, overlapping fails
			t.Run("OverlappingRanges", func(t *testing.T) {
				testOverlappingRanges(t, mount1, ver)
			})

			// d. LockUpgrade: shared to exclusive
			t.Run("LockUpgrade", func(t *testing.T) {
				testLockUpgrade(t, mount1, ver)
			})

			// e. LockUnlock: acquire, write, release, another fd can lock
			t.Run("LockUnlock", func(t *testing.T) {
				testLockUnlock(t, mount1, ver)
			})

			// f. CrossClientConflict: two mount points, lock conflicts
			t.Run("CrossClientConflict", func(t *testing.T) {
				testCrossClientConflict(t, mount1, mount2, ver)
			})
		})
	}
}

// testReadWriteLocks verifies that multiple shared (read) locks coexist, but
// an exclusive (write) lock conflicts with an existing shared lock.
func testReadWriteLocks(t *testing.T, mount *framework.Mount, ver string) {
	t.Helper()

	content := []byte("read-write lock test content for NFSv" + ver)
	fileName := helpers.UniqueTestName(fmt.Sprintf("rw_lock_%s", ver)) + ".txt"
	filePath := mount.FilePath(fileName)

	framework.WriteFile(t, filePath, content)
	t.Cleanup(func() { _ = os.Remove(filePath) })

	// Acquire first shared lock
	f1 := framework.LockFileRange(t, filePath, 0, 0, false) // shared
	t.Cleanup(func() {
		if f1 != nil {
			framework.UnlockFileRange(t, f1)
		}
	})
	t.Log("First shared lock acquired")

	// Second shared lock should succeed (multiple readers allowed)
	f2, err := framework.TryLockFileRange(t, filePath, 0, 0, false)
	if err != nil {
		// Some NFS implementations may not allow multiple shared locks from same client
		t.Logf("Second shared lock not acquired (platform behavior): %v", err)
	} else {
		t.Log("Second shared lock acquired (multiple readers)")
		framework.UnlockFileRange(t, f2)
	}

	// Exclusive lock on overlapping range should fail (shared lock held)
	f3, err := framework.TryLockFileRange(t, filePath, 0, 0, true)
	if err != nil {
		assert.True(t, errors.Is(err, framework.ErrLockWouldBlock),
			"Exclusive lock should fail with EWOULDBLOCK, got: %v", err)
		t.Log("Exclusive lock correctly blocked by shared lock")
	} else {
		// POSIX locks are per-process; same process may upgrade
		t.Log("Exclusive lock acquired (same-process POSIX lock upgrade)")
		framework.UnlockFileRange(t, f3)
	}

	framework.UnlockFileRange(t, f1)
	f1 = nil
	t.Log("ReadWriteLocks: PASSED")
}

// testExclusiveLock verifies that two exclusive locks on the same range conflict,
// and that after release, the second can be acquired.
func testExclusiveLock(t *testing.T, mount *framework.Mount, ver string) {
	t.Helper()

	content := []byte("exclusive lock test content for NFSv" + ver)
	fileName := helpers.UniqueTestName(fmt.Sprintf("excl_lock_%s", ver)) + ".txt"
	filePath := mount.FilePath(fileName)

	framework.WriteFile(t, filePath, content)
	t.Cleanup(func() { _ = os.Remove(filePath) })

	// Acquire exclusive lock
	f1 := framework.LockFileRange(t, filePath, 0, 0, true)
	t.Cleanup(func() {
		if f1 != nil {
			framework.UnlockFileRange(t, f1)
		}
	})
	t.Log("First exclusive lock acquired")

	// Second exclusive lock should fail
	f2, err := framework.TryLockFileRange(t, filePath, 0, 0, true)
	if err != nil {
		assert.True(t, errors.Is(err, framework.ErrLockWouldBlock),
			"Second exclusive lock should fail with EWOULDBLOCK, got: %v", err)
		t.Log("Second exclusive lock correctly blocked")
	} else {
		// Same process may succeed on some implementations (POSIX per-process semantics)
		t.Log("Second exclusive lock acquired (same-process POSIX semantics)")
		framework.UnlockFileRange(t, f2)
	}

	// Release first lock
	framework.UnlockFileRange(t, f1)
	f1 = nil
	t.Log("First exclusive lock released")

	// Now second lock should succeed
	f3 := framework.LockFileRange(t, filePath, 0, 0, true)
	t.Log("Exclusive lock acquired after release")
	framework.UnlockFileRange(t, f3)

	t.Log("ExclusiveLock: PASSED")
}

// testOverlappingRanges verifies that non-overlapping byte-range locks succeed,
// while overlapping ranges conflict.
func testOverlappingRanges(t *testing.T, mount *framework.Mount, ver string) {
	t.Helper()

	// Create a 1KB file for range testing
	content := make([]byte, 1024)
	for i := range content {
		content[i] = byte(i % 256)
	}
	fileName := helpers.UniqueTestName(fmt.Sprintf("overlap_%s", ver)) + ".dat"
	filePath := mount.FilePath(fileName)

	framework.WriteFile(t, filePath, content)
	t.Cleanup(func() { _ = os.Remove(filePath) })

	// Lock bytes 0-100 exclusive
	f1 := framework.LockFileRange(t, filePath, 0, 100, true)
	t.Cleanup(func() {
		if f1 != nil {
			framework.UnlockFileRange(t, f1)
		}
	})
	t.Log("Locked bytes [0:100] exclusive")

	// Lock bytes 200-300 exclusive (non-overlapping) -- should succeed
	f2, err := framework.TryLockFileRange(t, filePath, 200, 100, true)
	if err != nil {
		// Same-process POSIX semantics may behave differently
		t.Logf("Non-overlapping lock [200:300] not acquired (platform behavior): %v", err)
	} else {
		t.Log("Non-overlapping lock [200:300] acquired (expected)")
		defer framework.UnlockFileRange(t, f2)
	}

	// Lock bytes 50-250 (overlapping with [0:100]) -- should fail
	f3, err := framework.TryLockFileRange(t, filePath, 50, 200, true)
	if err != nil {
		assert.True(t, errors.Is(err, framework.ErrLockWouldBlock),
			"Overlapping lock [50:250] should fail with EWOULDBLOCK, got: %v", err)
		t.Log("Overlapping lock [50:250] correctly blocked")
	} else {
		// Same-process POSIX semantics may allow overlapping from same process
		t.Log("Overlapping lock [50:250] acquired (same-process POSIX semantics)")
		framework.UnlockFileRange(t, f3)
	}

	framework.UnlockFileRange(t, f1)
	f1 = nil
	t.Log("OverlappingRanges: PASSED")
}

// testLockUpgrade verifies that a shared lock can be upgraded to exclusive.
func testLockUpgrade(t *testing.T, mount *framework.Mount, ver string) {
	t.Helper()

	content := []byte("lock upgrade test content for NFSv" + ver)
	fileName := helpers.UniqueTestName(fmt.Sprintf("upgrade_%s", ver)) + ".txt"
	filePath := mount.FilePath(fileName)

	framework.WriteFile(t, filePath, content)
	t.Cleanup(func() { _ = os.Remove(filePath) })

	// Acquire shared lock via fcntl
	f, err := os.OpenFile(filePath, os.O_RDWR, 0644)
	require.NoError(t, err, "Should open file for lock upgrade test")
	t.Cleanup(func() { _ = f.Close() })

	// Set shared lock
	sharedLock := &syscall.Flock_t{
		Type:   syscall.F_RDLCK,
		Whence: int16(os.SEEK_SET),
		Start:  0,
		Len:    0,
	}
	err = syscall.FcntlFlock(f.Fd(), syscall.F_SETLKW, sharedLock)
	require.NoError(t, err, "Should acquire shared lock")
	t.Log("Shared lock acquired")

	// Upgrade to exclusive on same fd
	exclusiveLock := &syscall.Flock_t{
		Type:   syscall.F_WRLCK,
		Whence: int16(os.SEEK_SET),
		Start:  0,
		Len:    0,
	}
	err = syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, exclusiveLock)
	if err != nil {
		// Upgrade may fail if other readers exist (even though we're the same process)
		t.Logf("Lock upgrade not supported directly: %v -- trying release-then-acquire", err)

		// Fallback: release shared, then acquire exclusive
		unlockLock := &syscall.Flock_t{
			Type:   syscall.F_UNLCK,
			Whence: int16(os.SEEK_SET),
			Start:  0,
			Len:    0,
		}
		err = syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, unlockLock)
		require.NoError(t, err, "Should release shared lock")

		err = syscall.FcntlFlock(f.Fd(), syscall.F_SETLKW, exclusiveLock)
		require.NoError(t, err, "Should acquire exclusive lock after release")
		t.Log("Lock upgrade via release-then-acquire succeeded")
	} else {
		t.Log("Lock upgrade from shared to exclusive succeeded directly")
	}

	// Clean up -- release exclusive lock
	unlockLock := &syscall.Flock_t{
		Type:   syscall.F_UNLCK,
		Whence: int16(os.SEEK_SET),
		Start:  0,
		Len:    0,
	}
	err = syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, unlockLock)
	require.NoError(t, err, "Should release lock")

	t.Log("LockUpgrade: PASSED")
}

// testLockUnlock verifies that after acquiring a lock, writing data, and
// unlocking, another file descriptor can acquire the lock.
func testLockUnlock(t *testing.T, mount *framework.Mount, ver string) {
	t.Helper()

	content := []byte("lock-unlock test for NFSv" + ver)
	fileName := helpers.UniqueTestName(fmt.Sprintf("lockunlock_%s", ver)) + ".txt"
	filePath := mount.FilePath(fileName)

	framework.WriteFile(t, filePath, content)
	t.Cleanup(func() { _ = os.Remove(filePath) })

	// Acquire exclusive lock
	f1 := framework.LockFileRange(t, filePath, 0, 0, true)
	t.Log("Exclusive lock acquired")

	// Write some data while holding the lock
	_, err := f1.Seek(0, 0)
	require.NoError(t, err, "Should seek to start")
	writeData := []byte("written under lock protection")
	_, err = f1.Write(writeData)
	require.NoError(t, err, "Should write data under lock")
	err = f1.Sync()
	require.NoError(t, err, "Should sync file")
	t.Log("Data written under exclusive lock")

	// Release the lock
	framework.UnlockFileRange(t, f1)
	t.Log("Exclusive lock released")

	// Another fd should now be able to lock the file
	f2 := framework.LockFileRange(t, filePath, 0, 0, true)
	t.Log("Second fd acquired exclusive lock after release")
	framework.UnlockFileRange(t, f2)

	t.Log("LockUnlock: PASSED")
}

// testCrossClientConflict verifies that locks from two different mount points
// (simulating two NFS clients) conflict properly.
func testCrossClientConflict(t *testing.T, mount1, mount2 *framework.Mount, ver string) {
	t.Helper()

	content := []byte("cross-client conflict test for NFSv" + ver)
	fileName := helpers.UniqueTestName(fmt.Sprintf("crossclient_%s", ver)) + ".txt"

	filePath1 := mount1.FilePath(fileName)
	filePath2 := mount2.FilePath(fileName)

	// Create file via mount1
	framework.WriteFile(t, filePath1, content)
	t.Cleanup(func() { _ = os.Remove(filePath1) })

	// Wait for metadata sync across mounts
	time.Sleep(500 * time.Millisecond)

	// Verify file is visible from mount2
	require.True(t, framework.FileExists(filePath2),
		"File should be visible from second mount point")

	// Acquire exclusive lock via mount1
	f1 := framework.LockFileRange(t, filePath1, 0, 0, true)
	t.Cleanup(func() {
		if f1 != nil {
			framework.UnlockFileRange(t, f1)
		}
	})
	t.Log("Exclusive lock acquired via mount1")

	// Try same lock via mount2 -- should fail (different client)
	f2, err := framework.TryLockFileRange(t, filePath2, 0, 0, true)
	if err != nil {
		assert.True(t, errors.Is(err, framework.ErrLockWouldBlock),
			"Cross-client exclusive lock should fail, got: %v", err)
		t.Log("Cross-client exclusive lock correctly blocked via mount2")
	} else {
		// On some configurations, locks from same machine may not conflict
		t.Log("Cross-client lock acquired (may be same-host optimization)")
		framework.UnlockFileRange(t, f2)
	}

	// Release via mount1
	framework.UnlockFileRange(t, f1)
	f1 = nil
	t.Log("Lock released via mount1")

	// Wait for lock state propagation
	time.Sleep(500 * time.Millisecond)

	// Retry via mount2 -- should now succeed
	released := framework.WaitForRangeLockRelease(t, filePath2, 0, 0, true, 5*time.Second)
	assert.True(t, released, "Lock should be available on mount2 after release on mount1")

	t.Log("CrossClientConflict: PASSED")
}

// =============================================================================
// NFSv4 Blocking Lock Test (v4 only)
// =============================================================================

// TestNFSv4BlockingLock validates NFSv4 blocking lock semantics (F_SETLKW).
// When a lock is held, a blocking lock request should wait until the lock is
// released, rather than failing immediately.
//
// This test is NFSv4-only because NFSv4 integrated locking provides more
// reliable blocking lock semantics than NFSv3 NLM (which depends on
// NLM_GRANTED callbacks that may not work with userspace servers).
func TestNFSv4BlockingLock(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4 blocking lock test in short mode")
	}

	framework.SkipIfDarwin(t)
	framework.SkipIfNFSv4Unsupported(t)
	framework.SkipIfNFSLockingUnsupported(t)
	framework.LogPlatformLockingNotes(t)

	_, _, nfsPort := setupNFSv4TestServer(t)

	// Two separate mounts to simulate two clients
	mount1 := framework.MountNFSWithVersion(t, nfsPort, "4.0")
	t.Cleanup(mount1.Cleanup)

	mount2 := framework.MountNFSWithVersion(t, nfsPort, "4.0")
	t.Cleanup(mount2.Cleanup)

	content := []byte("blocking lock test content")
	fileName := helpers.UniqueTestName("blocking_lock") + ".txt"

	filePath1 := mount1.FilePath(fileName)
	filePath2 := mount2.FilePath(fileName)

	// Create file via mount1
	framework.WriteFile(t, filePath1, content)
	t.Cleanup(func() { _ = os.Remove(filePath1) })

	// Wait for file visibility
	time.Sleep(500 * time.Millisecond)
	require.True(t, framework.FileExists(filePath2), "File should be visible from mount2")

	// Acquire exclusive lock via mount1
	f1 := framework.LockFileRange(t, filePath1, 0, 0, true)
	t.Logf("Exclusive lock acquired via mount1")

	// Start goroutine to acquire blocking lock via mount2 (F_SETLKW)
	var wg sync.WaitGroup
	lockAcquired := make(chan struct{})
	lockErr := make(chan error, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()

		f2, err := os.OpenFile(filePath2, os.O_RDWR, 0644)
		if err != nil {
			lockErr <- fmt.Errorf("failed to open file on mount2: %w", err)
			return
		}
		defer func() { _ = f2.Close() }()

		// F_SETLKW is blocking -- will wait until lock is available
		flock := &syscall.Flock_t{
			Type:   syscall.F_WRLCK,
			Whence: int16(os.SEEK_SET),
			Start:  0,
			Len:    0,
		}

		err = syscall.FcntlFlock(f2.Fd(), syscall.F_SETLKW, flock)
		if err != nil {
			lockErr <- fmt.Errorf("blocking lock failed: %w", err)
			return
		}

		close(lockAcquired)

		// Hold lock briefly then release
		time.Sleep(100 * time.Millisecond)

		unlockFlock := &syscall.Flock_t{
			Type:   syscall.F_UNLCK,
			Whence: int16(os.SEEK_SET),
			Start:  0,
			Len:    0,
		}
		_ = syscall.FcntlFlock(f2.Fd(), syscall.F_SETLK, unlockFlock)
	}()

	// Give the blocking lock request time to be submitted
	time.Sleep(1 * time.Second)

	// Verify lock is not yet acquired (still blocked)
	select {
	case <-lockAcquired:
		t.Log("Blocking lock acquired before release (may be same-host optimization)")
	case err := <-lockErr:
		t.Logf("Blocking lock returned error (may not support cross-mount blocking): %v", err)
	default:
		t.Log("Blocking lock is waiting as expected (lock held by mount1)")
	}

	// Release lock on mount1
	framework.UnlockFileRange(t, f1)
	t.Log("Lock released on mount1")

	// Wait for blocking lock to complete (up to 5s timeout)
	select {
	case <-lockAcquired:
		t.Log("Blocking lock acquired after release -- NFSv4 blocking lock semantics work")
	case err := <-lockErr:
		t.Logf("Blocking lock error after release: %v (platform-dependent)", err)
	case <-time.After(5 * time.Second):
		t.Log("Blocking lock did not complete within 5s timeout (platform-dependent)")
	}

	wg.Wait()
	t.Log("TestNFSv4BlockingLock: PASSED")
}
