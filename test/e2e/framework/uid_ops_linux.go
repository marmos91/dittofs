//go:build e2e && linux

package framework

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// uid_ops_linux.go provides filesystem operations executed as a specific
// (non-root) uid/gid against a mounted share. The e2e harness itself runs as
// root (it needs root to mount NFS), but real-world clients act as ordinary
// users. To exercise the actual NFS permission path we drop privileges in a
// child process via SysProcAttr.Credential and perform the I/O there, so the
// kernel NFS client sends AUTH_UNIX credentials for that uid/gid.
//
// Operations are intentionally implemented with coreutils (tee/cat/mkdir/rm/
// chmod/stat/ls) so they mirror exactly what a user typing the same commands
// would experience — the colleague bug that motivated this was a plain `cp`.

// uidCredential builds a SysProcAttr that runs a child process as uid:gid.
// NoSetGroups avoids setgroups(2), which would otherwise require the
// supplementary group list and is unnecessary for these single-identity ops.
func uidCredential(uid, gid uint32) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uid, Gid: gid, NoSetGroups: true},
	}
}

// execAsUID runs name+args as uid:gid. stdin (may be nil) is fed on the child's
// stdin; the child's stdout is written to stdoutW (may be nil to discard). On
// failure the returned error carries the captured stderr.
func execAsUID(uid, gid uint32, stdin []byte, stdoutW io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = uidCredential(uid, gid)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if stdoutW != nil {
		cmd.Stdout = stdoutW
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run %q %v as uid=%d gid=%d: %w: %s",
			name, args, uid, gid, err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// ExecAsUID runs name+args as uid:gid and returns the child's stdout. Useful
// for tests that need the raw output or want to assert on the error directly
// (e.g. permission-denied checks).
func ExecAsUID(t *testing.T, uid, gid uint32, name string, args ...string) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	err := execAsUID(uid, gid, nil, &out, name, args...)
	return out.Bytes(), err
}

// WriteFileAsUID writes data to path as uid:gid (truncating any existing file).
func WriteFileAsUID(t *testing.T, uid, gid uint32, path string, data []byte) error {
	t.Helper()
	// tee opens the file with O_CREAT|O_TRUNC; discard its stdout copy.
	return execAsUID(uid, gid, data, io.Discard, "tee", path)
}

// ReadFileAsUID reads and returns the contents of path as uid:gid.
func ReadFileAsUID(t *testing.T, uid, gid uint32, path string) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	err := execAsUID(uid, gid, nil, &out, "cat", path)
	return out.Bytes(), err
}

// WriteRandomFileAsUID writes size bytes of random data to path as uid:gid and
// returns the sha256 checksum of what was written.
func WriteRandomFileAsUID(t *testing.T, uid, gid uint32, path string, size int64) (string, error) {
	t.Helper()
	data := GenerateRandomData(t, size)
	if err := WriteFileAsUID(t, uid, gid, path, data); err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// VerifyFileChecksumAsUID reads path as uid:gid and checks its sha256 matches.
func VerifyFileChecksumAsUID(t *testing.T, uid, gid uint32, path, want string) error {
	t.Helper()
	data, err := ReadFileAsUID(t, uid, gid, path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("checksum mismatch for %s: want %s got %s", path, want, got)
	}
	return nil
}

// MkdirAsUID creates a single directory as uid:gid (errors if it exists).
func MkdirAsUID(t *testing.T, uid, gid uint32, path string) error {
	t.Helper()
	return execAsUID(uid, gid, nil, nil, "mkdir", path)
}

// MkdirAllAsUID creates a directory and any missing parents as uid:gid.
func MkdirAllAsUID(t *testing.T, uid, gid uint32, path string) error {
	t.Helper()
	return execAsUID(uid, gid, nil, nil, "mkdir", "-p", path)
}

// RemoveAsUID removes a single file as uid:gid (errors if it does not exist).
func RemoveAsUID(t *testing.T, uid, gid uint32, path string) error {
	t.Helper()
	return execAsUID(uid, gid, nil, nil, "rm", path)
}

// RemoveAllAsUID removes a file or directory tree as uid:gid (no error if
// absent), mirroring os.RemoveAll for cleanup.
func RemoveAllAsUID(t *testing.T, uid, gid uint32, path string) error {
	t.Helper()
	return execAsUID(uid, gid, nil, nil, "rm", "-rf", path)
}

// ChmodAsUID changes the permission bits of path as uid:gid.
func ChmodAsUID(t *testing.T, uid, gid uint32, mode os.FileMode, path string) error {
	t.Helper()
	return execAsUID(uid, gid, nil, nil, "chmod", strconv.FormatUint(uint64(mode.Perm()), 8), path)
}

// FileExistsAsUID reports whether path is visible to uid:gid.
func FileExistsAsUID(t *testing.T, uid, gid uint32, path string) bool {
	t.Helper()
	return execAsUID(uid, gid, nil, nil, "test", "-e", path) == nil
}

// ListDirAsUID returns the entry names in path as seen by uid:gid (excluding
// "." and ".."), mirroring framework.ListDir.
func ListDirAsUID(t *testing.T, uid, gid uint32, path string) ([]string, error) {
	t.Helper()
	var out bytes.Buffer
	if err := execAsUID(uid, gid, nil, &out, "ls", "-1A", path); err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out.String(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// StatAsUID returns metadata for path as observed by uid:gid. It fills the same
// FileInfo fields callers use for assertions (Size, Mode permission bits, IsDir).
func StatAsUID(t *testing.T, uid, gid uint32, path string) (FileInfo, error) {
	t.Helper()
	var out bytes.Buffer
	if err := execAsUID(uid, gid, nil, &out, "stat", "-c", "%s|%a|%F", path); err != nil {
		return FileInfo{}, err
	}
	parts := strings.SplitN(strings.TrimSpace(out.String()), "|", 3)
	if len(parts) != 3 {
		return FileInfo{}, fmt.Errorf("unexpected stat output for %s: %q", path, out.String())
	}
	size, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return FileInfo{}, fmt.Errorf("parse stat size %q: %w", parts[0], err)
	}
	perm, err := strconv.ParseUint(parts[1], 8, 32)
	if err != nil {
		return FileInfo{}, fmt.Errorf("parse stat mode %q: %w", parts[1], err)
	}
	isDir := strings.Contains(parts[2], "directory")
	mode := os.FileMode(perm)
	if isDir {
		mode |= os.ModeDir
	}
	return FileInfo{Size: size, Mode: mode, IsDir: isDir}, nil
}
