//go:build e2e

package framework

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// ErrLockWouldBlock is returned when a non-blocking lock operation would block.
var ErrLockWouldBlock = errors.New("lock would block")

// LockFile acquires a file lock (blocking) and returns the file handle.
// The caller must close the file to release the lock, or call UnlockFile.
// If exclusive is true, acquires an exclusive (write) lock; otherwise shared (read) lock.
func LockFile(t *testing.T, path string, exclusive bool) *os.File {
	t.Helper()

	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("LockFile: failed to open file %s: %v", path, err)
	}

	var how int
	if exclusive {
		how = syscall.LOCK_EX
	} else {
		how = syscall.LOCK_SH
	}

	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		t.Fatalf("LockFile: failed to acquire lock on %s: %v", path, err)
	}

	lockType := "shared"
	if exclusive {
		lockType = "exclusive"
	}
	t.Logf("LockFile: acquired %s lock on %s", lockType, path)

	return f
}

// TryLockFile attempts to acquire a file lock (non-blocking).
// Returns the file handle if successful, or error if lock would block.
// Use ErrLockWouldBlock to check if the lock failed due to contention.
func TryLockFile(t *testing.T, path string, exclusive bool) (*os.File, error) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("TryLockFile: failed to open file %s: %w", path, err)
	}

	var how int
	if exclusive {
		how = syscall.LOCK_EX | syscall.LOCK_NB
	} else {
		how = syscall.LOCK_SH | syscall.LOCK_NB
	}

	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		// EAGAIN or EWOULDBLOCK indicates lock is held
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLockWouldBlock
		}
		return nil, fmt.Errorf("TryLockFile: failed to acquire lock on %s: %w", path, err)
	}

	lockType := "shared"
	if exclusive {
		lockType = "exclusive"
	}
	t.Logf("TryLockFile: acquired %s lock on %s", lockType, path)

	return f, nil
}

// UnlockFile releases the lock and closes the file handle.
func UnlockFile(t *testing.T, f *os.File) {
	t.Helper()

	if f == nil {
		return
	}

	path := f.Name()

	// Release the lock explicitly before closing
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Logf("UnlockFile: warning: failed to unlock %s: %v", path, err)
	}

	if err := f.Close(); err != nil {
		t.Logf("UnlockFile: warning: failed to close %s: %v", path, err)
	}

	t.Logf("UnlockFile: released lock on %s", path)
}

// LockFileRange acquires a byte-range lock using fcntl (POSIX locks).
// This provides finer-grained locking than flock, allowing locks on file regions.
// The caller must close the file to release the lock, or call UnlockFileRange.
// Note: On NFS, byte-range locks are sent to the NLM server.
func LockFileRange(t *testing.T, path string, offset, length int64, exclusive bool) *os.File {
	t.Helper()

	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("LockFileRange: failed to open file %s: %v", path, err)
	}

	var lockType int16
	if exclusive {
		lockType = syscall.F_WRLCK
	} else {
		lockType = syscall.F_RDLCK
	}

	flock := &syscall.Flock_t{
		Type:   lockType,
		Whence: int16(os.SEEK_SET),
		Start:  offset,
		Len:    length, // 0 means to EOF
	}

	// F_SETLKW blocks until lock is acquired
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLKW, flock); err != nil {
		_ = f.Close()
		t.Fatalf("LockFileRange: failed to acquire lock on %s [%d:%d]: %v", path, offset, length, err)
	}

	lockTypeStr := "shared"
	if exclusive {
		lockTypeStr = "exclusive"
	}
	t.Logf("LockFileRange: acquired %s lock on %s [%d:%d]", lockTypeStr, path, offset, length)

	return f
}

// TryLockFileRange attempts to acquire a byte-range lock (non-blocking).
// Returns the file handle if successful, or error if lock would block.
func TryLockFileRange(t *testing.T, path string, offset, length int64, exclusive bool) (*os.File, error) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("TryLockFileRange: failed to open file %s: %w", path, err)
	}

	var lockType int16
	if exclusive {
		lockType = syscall.F_WRLCK
	} else {
		lockType = syscall.F_RDLCK
	}

	flock := &syscall.Flock_t{
		Type:   lockType,
		Whence: int16(os.SEEK_SET),
		Start:  offset,
		Len:    length,
	}

	// F_SETLK is non-blocking
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, flock); err != nil {
		_ = f.Close()
		// EAGAIN or EACCES indicates lock is held
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES) {
			return nil, ErrLockWouldBlock
		}
		return nil, fmt.Errorf("TryLockFileRange: failed to acquire lock on %s [%d:%d]: %w", path, offset, length, err)
	}

	lockTypeStr := "shared"
	if exclusive {
		lockTypeStr = "exclusive"
	}
	t.Logf("TryLockFileRange: acquired %s lock on %s [%d:%d]", lockTypeStr, path, offset, length)

	return f, nil
}

// UnlockFileRange releases a byte-range lock and closes the file handle.
func UnlockFileRange(t *testing.T, f *os.File) {
	t.Helper()

	if f == nil {
		return
	}

	path := f.Name()

	// Release the lock explicitly
	flock := &syscall.Flock_t{
		Type:   syscall.F_UNLCK,
		Whence: int16(os.SEEK_SET),
		Start:  0,
		Len:    0, // 0 means entire file
	}

	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, flock); err != nil {
		t.Logf("UnlockFileRange: warning: failed to unlock %s: %v", path, err)
	}

	if err := f.Close(); err != nil {
		t.Logf("UnlockFileRange: warning: failed to close %s: %v", path, err)
	}

	t.Logf("UnlockFileRange: released lock on %s", path)
}

// WaitForLockRelease polls until a file lock can be acquired, or timeout.
// This is useful for waiting on cross-protocol lock releases.
// Returns true if lock was acquired (and immediately released), false on timeout.
func WaitForLockRelease(t *testing.T, path string, exclusive bool, timeout time.Duration) bool {
	t.Helper()

	deadline := time.Now().Add(timeout)
	pollInterval := 100 * time.Millisecond

	t.Logf("WaitForLockRelease: waiting for lock release on %s (timeout: %v)", path, timeout)

	for time.Now().Before(deadline) {
		f, err := TryLockFile(t, path, exclusive)
		if err == nil {
			// Lock acquired - release it immediately
			UnlockFile(t, f)
			t.Logf("WaitForLockRelease: lock released on %s", path)
			return true
		}

		if !errors.Is(err, ErrLockWouldBlock) {
			// Unexpected error
			t.Logf("WaitForLockRelease: unexpected error: %v", err)
			return false
		}

		time.Sleep(pollInterval)
	}

	t.Logf("WaitForLockRelease: timeout waiting for lock release on %s", path)
	return false
}

// WaitForRangeLockRelease polls until a byte-range lock can be acquired, or timeout.
// Returns true if lock was acquired (and immediately released), false on timeout.
func WaitForRangeLockRelease(t *testing.T, path string, offset, length int64, exclusive bool, timeout time.Duration) bool {
	t.Helper()

	deadline := time.Now().Add(timeout)
	pollInterval := 100 * time.Millisecond

	t.Logf("WaitForRangeLockRelease: waiting for lock release on %s [%d:%d] (timeout: %v)", path, offset, length, timeout)

	for time.Now().Before(deadline) {
		f, err := TryLockFileRange(t, path, offset, length, exclusive)
		if err == nil {
			// Lock acquired - release it immediately
			UnlockFileRange(t, f)
			t.Logf("WaitForRangeLockRelease: lock released on %s [%d:%d]", path, offset, length)
			return true
		}

		if !errors.Is(err, ErrLockWouldBlock) {
			// Unexpected error
			t.Logf("WaitForRangeLockRelease: unexpected error: %v", err)
			return false
		}

		time.Sleep(pollInterval)
	}

	t.Logf("WaitForRangeLockRelease: timeout waiting for lock release on %s [%d:%d]", path, offset, length)
	return false
}

// GetLockInfo returns information about who holds a lock on a file range.
// This uses F_GETLK to query lock status without modifying it.
// Returns nil if no conflicting lock exists.
func GetLockInfo(t *testing.T, path string, offset, length int64, exclusive bool) *LockInfo {
	t.Helper()

	f, err := os.OpenFile(path, os.O_RDONLY, 0644)
	if err != nil {
		t.Fatalf("GetLockInfo: failed to open file %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	var lockType int16
	if exclusive {
		lockType = syscall.F_WRLCK
	} else {
		lockType = syscall.F_RDLCK
	}

	flock := &syscall.Flock_t{
		Type:   lockType,
		Whence: int16(os.SEEK_SET),
		Start:  offset,
		Len:    length,
	}

	// F_GETLK queries for conflicting locks
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, flock); err != nil {
		t.Logf("GetLockInfo: F_GETLK failed on %s: %v", path, err)
		return nil
	}

	// If Type is F_UNLCK, no conflicting lock exists
	if flock.Type == syscall.F_UNLCK {
		return nil
	}

	return &LockInfo{
		Type:   lockTypeToString(flock.Type),
		PID:    int(flock.Pid),
		Start:  flock.Start,
		Length: flock.Len,
	}
}

// LockInfo contains information about a file lock.
type LockInfo struct {
	Type   string // "read", "write", or "unknown"
	PID    int    // Process ID holding the lock
	Start  int64  // Lock start offset
	Length int64  // Lock length (0 = to EOF)
}

// lockTypeToString converts a syscall lock type to a human-readable string.
func lockTypeToString(lockType int16) string {
	switch lockType {
	case syscall.F_RDLCK:
		return "read"
	case syscall.F_WRLCK:
		return "write"
	default:
		return "unknown"
	}
}

// SkipIfNFSLockingUnsupported skips the test on platforms where NFS file locking
// (NLM protocol) does not work with userspace NFS servers.
//
// On macOS, NFS byte-range locks (fcntl F_SETLK/F_SETLKW) are forwarded to the
// system lockd daemon, which contacts the portmapper (rpcbind, port 111) to discover
// the NLM service port. Since DittoFS is a userspace server that does not register
// with portmapper, lockd cannot reach the NLM service, causing ENOLCK errors.
// Using the "nolocks" mount option is not a workaround because macOS returns ENOTSUP
// for all locking operations on NFS mounts with nolocks (unlike Linux, where the
// kernel handles fcntl locks locally even with the "nolock" mount option).
//
// On Linux with the "nolock" mount option, fcntl locks are handled locally by the
// kernel without contacting NLM, so locking tests work.
func SkipIfNFSLockingUnsupported(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("Skipping: NFS file locking (NLM) requires portmapper, which is not available with userspace NFS servers on macOS")
	}
}

// LogPlatformLockingNotes logs platform-specific notes about file locking behavior.
// Call this at the start of cross-protocol lock tests for debugging context.
func LogPlatformLockingNotes(t *testing.T) {
	t.Helper()

	t.Logf("Platform: %s/%s", runtime.GOOS, runtime.GOARCH)

	switch runtime.GOOS {
	case "darwin":
		t.Log("macOS locking notes:")
		t.Log("  - flock() uses advisory locks (not enforced by kernel)")
		t.Log("  - fcntl() POSIX locks are per-process (not per-file-descriptor)")
		t.Log("  - NFS v3 uses NLM protocol for byte-range locks")
		t.Log("  - SMB uses oplock/lease mechanism for caching")
	case "linux":
		t.Log("Linux locking notes:")
		t.Log("  - flock() and fcntl() locks are independent systems")
		t.Log("  - fcntl() POSIX locks are per-process (same as macOS)")
		t.Log("  - NFS v3 uses NLM protocol for byte-range locks")
		t.Log("  - CIFS uses oplock mechanism for caching")
		t.Log("  - Use 'nolock' mount option to disable NLM (local only)")
	default:
		t.Logf("No specific locking notes for %s", runtime.GOOS)
	}
}
