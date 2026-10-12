//go:build e2e

// Package helpers — error-conformance triggers.
//
// Each TriggerErr* helper fires one scenario over the given mount root and
// returns the syscall.Errno that the kernel surfaces from the operation. The
// e2e conformance test then asserts that errno matches what the per-adapter
// types.StatusForErr tables say this protocol should return for the
// corresponding metadata.ErrorCode.
//
// Triggers are intentionally minimal: one operation per code, no setup
// teardown beyond what is needed to reproduce the error. Fixture reuse is the
// caller's job (TestCrossProtocol_ErrorConformance mounts each share once and
// reuses the mounts across all subtests).
//
// Protocol-agnostic by design: the same trigger is called against both the
// NFS mount root and the SMB mount root; both kernels (Linux nfs client,
// Linux cifs client) translate the on-wire protocol code into a syscall.Errno
// that the Go test observes. The cross-protocol assertion is "same
// metadata.ErrorCode surfaces via the same client-observable errno on both
// mounts".
//
// Every trigger takes a second root, peerRoot: a mount of the same share made
// by another protocol, and so by another identity, or "" when the share has
// none. Most triggers ignore it; one that needs a file owned by someone else
// seeds it there.
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
	"time"
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
// Triggerable metadata.ErrorCodes (e2e tier).
// ----------------------------------------------------------------------------

// TriggerErrNotFound fires a stat() on a path that does not exist. Produces
// ENOENT on both NFS (NFS3ERR_NOENT / NFS4ERR_NOENT) and SMB
// (STATUS_OBJECT_NAME_NOT_FOUND).
func TriggerErrNotFound(t *testing.T, mountRoot, _ string) TriggerResult {
	t.Helper()
	path := filepath.Join(mountRoot, UniqueTestName("missing"))
	_, err := os.Stat(path)
	return newResult(err)
}

// TriggerErrAlreadyExists creates a file, then tries O_CREAT|O_EXCL on the
// same path. Produces EEXIST on both protocols (NFS3ERR_EXIST / NFS4ERR_EXIST
// / STATUS_OBJECT_NAME_COLLISION).
func TriggerErrAlreadyExists(t *testing.T, mountRoot, _ string) TriggerResult {
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
func TriggerErrNotEmpty(t *testing.T, mountRoot, _ string) TriggerResult {
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
func TriggerErrIsDirectory(t *testing.T, mountRoot, _ string) TriggerResult {
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
func TriggerErrNotDirectory(t *testing.T, mountRoot, _ string) TriggerResult {
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
func TriggerErrNameTooLong(t *testing.T, mountRoot, _ string) TriggerResult {
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
func TriggerErrInvalidArgument(t *testing.T, mountRoot, _ string) TriggerResult {
	t.Helper()
	file := filepath.Join(mountRoot, UniqueTestName("einval")+".txt")
	if err := os.WriteFile(file, []byte("x"), 0644); err != nil {
		t.Fatalf("TriggerErrInvalidArgument: seed create failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(file) })

	_, err := os.Readlink(file)
	return newResult(err)
}

// TriggerErrNoSpace writes past the share's byte quota. mountRoot must be a
// mount of a share whose quota is smaller than the 4 MiB written here. Both
// kernel clients may buffer the write, so the error is taken from whichever
// of write, fsync and close reports it first. Produces ENOSPC
// (NFS3ERR_NOSPC / STATUS_DISK_FULL).
func TriggerErrNoSpace(t *testing.T, mountRoot, _ string) TriggerResult {
	t.Helper()
	path := filepath.Join(mountRoot, UniqueTestName("nospace")+".bin")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("TriggerErrNoSpace: create failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	_, err = f.Write(make([]byte, 4<<20))
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return newResult(err)
}

// TriggerErrReadOnly attempts to create a file on a read-only mount. Requires
// a dedicated read-only share fixture, which TestCrossProtocol_ErrorConformance
// sets up via the "/archive" share; the trigger takes a read-only mount
// root as argument.
func TriggerErrReadOnly(t *testing.T, readOnlyMountRoot, _ string) TriggerResult {
	t.Helper()
	path := filepath.Join(readOnlyMountRoot, UniqueTestName("rofs")+".txt")
	err := os.WriteFile(path, []byte("x"), 0644)
	if err == nil {
		_ = os.Remove(path)
	}
	return newResult(err)
}

// TriggerErrAccessDenied creates a file at the root of a share the mounting
// identity may only read. The server refuses it — the share grant allows only
// reads, and the share root's mode bits give that identity no write either —
// with NFS3ERR_ACCES / STATUS_ACCESS_DENIED → EACCES. The caller runs as
// root, which passes both kernel clients' local permission checks, so the
// denial observed is the server's; which of the two server checks fires
// first is not pinned here.
func TriggerErrAccessDenied(t *testing.T, readOnlyGrantRoot, _ string) TriggerResult {
	t.Helper()
	path := filepath.Join(readOnlyGrantRoot, UniqueTestName("eacces")+".txt")
	err := os.WriteFile(path, []byte("x"), 0644)
	if err == nil {
		_ = os.Remove(path)
	}
	return newResult(err)
}

// TriggerErrPermissionDenied seeds a file through peerRoot — a mount of the
// same share by another identity — then sets its timestamps to an explicit
// value through mountRoot. Over NFS only the owner may set explicit times,
// so the server refuses it: NFS3ERR_PERM → EPERM. Over SMB the same call is
// authorized by the open handle instead, so this trigger is NFS-only.
func TriggerErrPermissionDenied(t *testing.T, mountRoot, peerRoot string) TriggerResult {
	t.Helper()
	if peerRoot == "" {
		t.Fatal("TriggerErrPermissionDenied: needs a peer mount to seed a file owned by another identity")
	}
	name := UniqueTestName("eperm") + ".txt"
	seed := filepath.Join(peerRoot, name)
	if err := os.WriteFile(seed, []byte("x"), 0644); err != nil {
		t.Fatalf("TriggerErrPermissionDenied: seed create through peer mount failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(seed) })

	past := time.Unix(1_000_000_000, 0)
	err := os.Chtimes(filepath.Join(mountRoot, name), past, past)
	return newResult(err)
}

// TriggerErrLocked acquires a byte-range lock from one descriptor and attempts
// a conflicting lock from another. Produces EAGAIN/EWOULDBLOCK when the
// second lock is non-blocking; surfaces as STATUS_FILE_LOCK_CONFLICT on SMB /
// NFS3ERR_JUKEBOX on NFSv3, which translate to the same errno family.
//
// NFS locking depends on the NLM daemon, which macOS does not provide for a
// userspace NFS server, so the trigger skips there.
func TriggerErrLocked(t *testing.T, mountRoot, _ string) TriggerResult {
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
