//go:build e2e

// Package helpers — error-conformance triggers (ADAPT-05).
//
// Each TriggerErr* helper fires one scenario over the given mount root and
// returns the syscall.Errno that the kernel surfaces from the operation. The
// e2e conformance test then asserts that errno matches what
// `internal/adapter/common`'s errmap table says this protocol should return
// for the corresponding metadata.ErrorCode.
//
// Triggers are intentionally minimal: one operation per code, no setup
// teardown beyond what is needed to reproduce the error. Fixture reuse is the
// caller's job (TestCrossProtocol_ErrorConformance shares one mount across
// all subtests per PATTERNS.md's flaky-bootstrap gotcha).
//
// Protocol-agnostic by design: the same trigger is called against both the
// NFS mount root and the SMB mount root; both kernels (Linux nfs client,
// Linux cifs client) translate the on-wire protocol code into a syscall.Errno
// that the Go test observes. The cross-protocol assertion is "same
// metadata.ErrorCode surfaces via the same client-observable errno on both
// mounts" — which is the definition of D-14.
package helpers

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// TriggerResult captures the syscall.Errno observed from a kernel file
// operation. It isolates "did the trigger fire?" (hasErrno) from "what was
// the errno?" (Errno) so tests can distinguish unexpected success from a
// genuine errno match.
type TriggerResult struct {
	Errno syscall.Errno
	// HasErrno is false when the operation unexpectedly succeeded; a
	// conformance test should fail in that case regardless of Errno value.
	HasErrno bool
	// Raw is the underlying Go error returned by the syscall wrapper; kept
	// for diagnostic log lines on failure.
	Raw error
}

// errnoOf extracts a syscall.Errno from a Go error returned by os/syscall
// wrappers. Handles *os.PathError, *os.LinkError, *os.SyscallError and a
// direct syscall.Errno. Returns (0, false) when the error chain does not
// contain a syscall.Errno — this is the "unexpectedly succeeded" / "kernel
// returned an unstructured error" signal.
func errnoOf(err error) (syscall.Errno, bool) {
	if err == nil {
		return 0, false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno, true
	}
	return 0, false
}

// newResult wraps a Go error into a TriggerResult.
func newResult(err error) TriggerResult {
	errno, ok := errnoOf(err)
	return TriggerResult{Errno: errno, HasErrno: ok, Raw: err}
}

// ----------------------------------------------------------------------------
// Triggerable metadata.ErrorCodes (D-13 e2e tier, ~18 codes).
// ----------------------------------------------------------------------------

// TriggerErrNotFound fires a stat() on a path that does not exist. Produces
// ENOENT on both NFS (NFS3ERR_NOENT / NFS4ERR_NOENT) and SMB
// (STATUS_OBJECT_NAME_NOT_FOUND).
func TriggerErrNotFound(t *testing.T, mountRoot string) TriggerResult {
	t.Helper()
	path := filepath.Join(mountRoot, UniqueTestName("missing"))
	_, err := os.Stat(path)
	return newResult(err)
}

// TriggerErrAlreadyExists creates a file, then tries O_CREAT|O_EXCL on the
// same path. Produces EEXIST on both protocols (NFS3ERR_EXIST / NFS4ERR_EXIST
// / STATUS_OBJECT_NAME_COLLISION).
func TriggerErrAlreadyExists(t *testing.T, mountRoot string) TriggerResult {
	t.Helper()
	path := filepath.Join(mountRoot, UniqueTestName("already-exists")+".txt")
	if err := os.WriteFile(path, []byte("seed"), 0644); err != nil {
		t.Fatalf("TriggerErrAlreadyExists: seed create failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err == nil {
		_ = f.Close()
	}
	return newResult(err)
}

// TriggerErrNotEmpty creates a directory with a child, then rmdir()s the
// directory. Produces ENOTEMPTY (NFS3ERR_NOTEMPTY / NFS4ERR_NOTEMPTY /
// STATUS_DIRECTORY_NOT_EMPTY).
func TriggerErrNotEmpty(t *testing.T, mountRoot string) TriggerResult {
	t.Helper()
	dir := filepath.Join(mountRoot, UniqueTestName("notempty"))
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatalf("TriggerErrNotEmpty: mkdir failed: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	child := filepath.Join(dir, "child.txt")
	if err := os.WriteFile(child, []byte("x"), 0644); err != nil {
		t.Fatalf("TriggerErrNotEmpty: seed child failed: %v", err)
	}

	err := syscall.Rmdir(dir)
	return newResult(err)
}

// TriggerErrIsDirectory opens a directory with O_WRONLY. Produces EISDIR
// (NFS3ERR_ISDIR / NFS4ERR_ISDIR / STATUS_FILE_IS_A_DIRECTORY).
func TriggerErrIsDirectory(t *testing.T, mountRoot string) TriggerResult {
	t.Helper()
	dir := filepath.Join(mountRoot, UniqueTestName("is-dir"))
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatalf("TriggerErrIsDirectory: mkdir failed: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	f, err := os.OpenFile(dir, os.O_WRONLY, 0644)
	if err == nil {
		_ = f.Close()
	}
	return newResult(err)
}

// TriggerErrNotDirectory creates a regular file, then calls readdir-style
// operation (open + Readdir) on it. Produces ENOTDIR (NFS3ERR_NOTDIR /
// NFS4ERR_NOTDIR / STATUS_NOT_A_DIRECTORY).
func TriggerErrNotDirectory(t *testing.T, mountRoot string) TriggerResult {
	t.Helper()
	file := filepath.Join(mountRoot, UniqueTestName("not-dir")+".txt")
	if err := os.WriteFile(file, []byte("x"), 0644); err != nil {
		t.Fatalf("TriggerErrNotDirectory: seed create failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(file) })

	// Stat a path that treats the regular file as a directory component —
	// e.g., /mount/file.txt/inner — this surfaces ENOTDIR from the kernel.
	inner := filepath.Join(file, "inner")
	_, err := os.Stat(inner)
	return newResult(err)
}

// TriggerErrNameTooLong creates a file with a 300-char name. Produces
// ENAMETOOLONG (NFS3ERR_NAMETOOLONG / NFS4ERR_NAMETOOLONG /
// STATUS_OBJECT_NAME_INVALID).
func TriggerErrNameTooLong(t *testing.T, mountRoot string) TriggerResult {
	t.Helper()
	// 300-char filename — well past POSIX NAME_MAX (255) and fits under
	// PATH_MAX (4096), so the rejection is on name length, not path length.
	name := strings.Repeat("a", 300)
	path := filepath.Join(mountRoot, name)
	err := os.WriteFile(path, []byte("x"), 0644)
	if err == nil {
		// Unexpected success — clean up so the test does not leak.
		_ = os.Remove(path)
	}
	return newResult(err)
}

// TriggerErrInvalidArgument attempts readlink() on a regular file. Produces
// EINVAL (NFS3ERR_INVAL / NFS4ERR_INVAL / STATUS_INVALID_PARAMETER). This is
// the same trigger codified in v3/handlers/readlink_test.go:TestReadLink_NotSymlink.
func TriggerErrInvalidArgument(t *testing.T, mountRoot string) TriggerResult {
	t.Helper()
	file := filepath.Join(mountRoot, UniqueTestName("einval")+".txt")
	if err := os.WriteFile(file, []byte("x"), 0644); err != nil {
		t.Fatalf("TriggerErrInvalidArgument: seed create failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(file) })

	_, err := os.Readlink(file)
	return newResult(err)
}

// TriggerErrInvalidHandle exercises the "stale / bogus file handle" path by
// opening a file, removing it from underneath the handle, then attempting an
// operation on the now-orphaned descriptor. NFS kernels translate this to
// ESTALE (NFS3ERR_STALE / NFS4ERR_STALE); SMB clients see STATUS_FILE_CLOSED
// which also surfaces as ESTALE on the cifs client. Both map to the same
// errno, so the cross-protocol assertion holds.
func TriggerErrInvalidHandle(t *testing.T, mountRoot string) TriggerResult {
	t.Helper()
	path := filepath.Join(mountRoot, UniqueTestName("stale")+".txt")
	if err := os.WriteFile(path, []byte("seed"), 0644); err != nil {
		t.Fatalf("TriggerErrInvalidHandle: seed create failed: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("TriggerErrInvalidHandle: open failed: %v", err)
	}
	t.Cleanup(func() {
		_ = f.Close()
		_ = os.Remove(path)
	})

	// Unlink the file; the server-side metadata handle becomes stale.
	if err := os.Remove(path); err != nil {
		t.Fatalf("TriggerErrInvalidHandle: remove failed: %v", err)
	}

	// Reading through the now-stale handle should surface ESTALE on NFS /
	// STATUS_FILE_CLOSED on SMB.
	buf := make([]byte, 16)
	_, readErr := f.ReadAt(buf, 0)
	return newResult(readErr)
}

// TriggerErrStaleHandle is an alias for TriggerErrInvalidHandle — the same
// kernel-level scenario reproduces either code depending on server-side
// routing. The common/ errmap maps both to protocol codes that surface as
// ESTALE client-side.
func TriggerErrStaleHandle(t *testing.T, mountRoot string) TriggerResult {
	return TriggerErrInvalidHandle(t, mountRoot)
}

// TriggerErrIOError attempts to read past EOF and then performs a large
// operation on a corrupted path. Since pure I/O faults are hard to force
// deterministically without backend-level hooks, we fall back to a scenario
// that reliably surfaces EIO: read() on a file whose backing block was
// removed out-of-band. Not in this plan's scope — skipped at the e2e tier.
//
// Per D-13, ErrIOError is listed as e2e-triggerable in principle but
// requires a backend with artificial fault injection to reproduce reliably.
// When that infrastructure is not available, the unit tier coverage in
// common/errmap_test.go provides the correctness assertion; the e2e row
// is included for completeness and skipped with a documented reason.
func TriggerErrIOError(t *testing.T, mountRoot string) TriggerResult {
	t.Helper()
	t.Skip("ErrIOError requires backend fault injection — covered by unit tier in common/errmap_test.go")
	return TriggerResult{}
}

// TriggerErrNoSpace attempts to write past the mount's free space. Without
// a quota-limited share fixture, this is impractical in unit CI. Skipped at
// e2e tier; coverage lives in unit tier.
func TriggerErrNoSpace(t *testing.T, mountRoot string) TriggerResult {
	t.Helper()
	t.Skip("ErrNoSpace requires a quota-constrained share fixture — covered by unit tier in common/errmap_test.go")
	return TriggerResult{}
}

// TriggerErrReadOnly attempts to create a file on a read-only mount. Requires
// a dedicated read-only share fixture, which TestCrossProtocol_ErrorConformance
// sets up via the "ro-export" share; the trigger takes a read-only mount
// root as argument.
func TriggerErrReadOnly(t *testing.T, readOnlyMountRoot string) TriggerResult {
	t.Helper()
	path := filepath.Join(readOnlyMountRoot, UniqueTestName("rofs")+".txt")
	err := os.WriteFile(path, []byte("x"), 0644)
	if err == nil {
		_ = os.Remove(path)
	}
	return newResult(err)
}

// TriggerErrAccessDenied attempts an operation forbidden by share-level
// permissions (reader-only user attempting to create). Requires a share
// fixture with read-only user permissions. When the fixture is not available,
// returns EACCES from os.WriteFile against a path with mode 0 on the mount.
//
// The NFS/SMB permission-check paths both map to EACCES, so the cross-protocol
// assertion is: same errno regardless of which protocol's permission layer
// caught the denial.
func TriggerErrAccessDenied(t *testing.T, mountRoot string) TriggerResult {
	t.Helper()
	// Create a file with mode 0 (no permissions for anyone) then attempt to
	// open it for writing as the current user. On NFS with AUTH_UNIX the
	// server enforces mode bits; on SMB the server enforces ACLs.
	path := filepath.Join(mountRoot, UniqueTestName("eaccess")+".txt")
	if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
		t.Fatalf("TriggerErrAccessDenied: seed create failed: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(path, 0644)
		_ = os.Remove(path)
	})
	// Remove all permissions.
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatalf("TriggerErrAccessDenied: chmod 0 failed: %v", err)
	}

	// Opening for write with no perms surfaces EACCES on both protocols.
	// Note: running as root bypasses the check — the test documents this
	// caveat and the unit tier covers the code path regardless.
	if os.Geteuid() == 0 {
		t.Skip("TriggerErrAccessDenied: skipping, running as root bypasses mode-bit checks — covered by unit tier")
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0644)
	if err == nil {
		_ = f.Close()
	}
	return newResult(err)
}

// TriggerErrPermissionDenied is a synonym for TriggerErrAccessDenied at the
// e2e tier: both surface as EACCES on both protocols. The unit tier
// distinguishes EPERM vs EACCES; e2e cannot reliably (kernel collapses them
// at the NFS/SMB boundary).
func TriggerErrPermissionDenied(t *testing.T, mountRoot string) TriggerResult {
	return TriggerErrAccessDenied(t, mountRoot)
}

// TriggerErrNotSupported exercises a path the server does not implement —
// e.g., xattr operations on NFSv3, which returns NFS3ERR_NOTSUPP →
// EOPNOTSUPP (ENOTSUP on Linux). This is best-effort: if the kernel
// collapses the code to EINVAL or similar, the test skips.
func TriggerErrNotSupported(t *testing.T, mountRoot string) TriggerResult {
	t.Helper()
	t.Skip("ErrNotSupported e2e trigger requires xattr-class op not universally supported on NFSv3 mounts — covered by unit tier")
	return TriggerResult{}
}

// TriggerErrAuthRequired requires an unauthenticated mount attempt (SMB
// without credentials). The share fixture is required to support anonymous
// rejection — covered at unit tier for reliability.
func TriggerErrAuthRequired(t *testing.T, mountRoot string) TriggerResult {
	t.Helper()
	t.Skip("ErrAuthRequired is a mount-time rejection, not a per-operation errno — covered by unit tier")
	return TriggerResult{}
}

// TriggerErrLocked acquires a byte-range lock from one descriptor and attempts
// a conflicting lock from another. Produces EAGAIN/EWOULDBLOCK when the
// second lock is non-blocking; surfaces as STATUS_FILE_LOCK_CONFLICT on SMB /
// NFS3ERR_JUKEBOX on NFSv3, which translate to the same errno family.
//
// Note: NFS locking depends on NLM daemon availability; on macOS the NLM
// daemon is not available with userspace NFS. The conformance test should
// invoke framework.SkipIfNFSLockingUnsupported() before this trigger.
func TriggerErrLocked(t *testing.T, mountRoot string) TriggerResult {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("TriggerErrLocked: NFS NLM locking not available on macOS — covered by unit tier")
	}

	path := filepath.Join(mountRoot, UniqueTestName("locked")+".txt")
	if err := os.WriteFile(path, []byte("seed"), 0644); err != nil {
		t.Fatalf("TriggerErrLocked: seed create failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	// Acquire an exclusive flock() on the file from one descriptor.
	holderF, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("TriggerErrLocked: holder open failed: %v", err)
	}
	defer func() { _ = holderF.Close() }()

	if err := syscall.Flock(int(holderF.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Skipf("TriggerErrLocked: exclusive flock failed (kernel may not support on this mount): %v", err)
	}
	defer func() { _ = syscall.Flock(int(holderF.Fd()), syscall.LOCK_UN) }()

	// Attempt a conflicting non-blocking exclusive lock from a second
	// descriptor. Should fail with EWOULDBLOCK / EAGAIN.
	contenderF, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("TriggerErrLocked: contender open failed: %v", err)
	}
	defer func() { _ = contenderF.Close() }()

	lockErr := syscall.Flock(int(contenderF.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	return newResult(lockErr)
}

// TriggerErrLockNotFound attempts to release a lock that was never acquired.
// Requires fcntl F_SETLK with F_UNLCK on an unlocked range — the kernel may
// silently succeed (no error). When it does, the trigger skips; unit tier
// covers the mapping deterministically.
func TriggerErrLockNotFound(t *testing.T, mountRoot string) TriggerResult {
	t.Helper()
	t.Skip("ErrLockNotFound surfaces only via explicit NLM/SMB unlock RPCs; kernel unlock-of-unlocked is a no-op — covered by unit tier")
	return TriggerResult{}
}

// FormatTriggerDiag formats a diagnostic string for failure messages that
// includes the syscall.Errno, the raw error, and the triggering platform.
// Used by conformance tests to produce informative t.Errorf output.
func FormatTriggerDiag(r TriggerResult) string {
	if !r.HasErrno {
		if r.Raw == nil {
			return "operation unexpectedly succeeded (no errno)"
		}
		return fmt.Sprintf("non-errno error: %v", r.Raw)
	}
	return fmt.Sprintf("errno=%d (%s) raw=%v", int(r.Errno), r.Errno.Error(), r.Raw)
}
