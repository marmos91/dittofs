//go:build e2e && linux

package e2e

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/stretchr/testify/require"
)

// TestNFSSeekDir lists a directory over a kernel NFS mount, keeping each
// entry's position, then seeks back to every position and checks that the
// next entry read is the one that followed it. This is what telldir() and
// seekdir() do: a position is the d_off the kernel reports for an entry, and
// seekdir() hands it to lseek(). Over NFS the position is the server's
// cookie for the entry, which the client will not seek to when it reads as
// negative.
//
// The names lead with the part that differs and end in a shared suffix, so
// the hashes behind their cookies spread over the whole 64-bit range within
// one directory, and some have bit 63 set. Names that differ only in their
// last byte hash alike in the top bits, so a directory of them would have
// no such name about half the time.
func TestNFSSeekDir(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFS seekdir tests in short mode")
	}

	_, _, nfsPort := setupNFSv4TestServer(t)

	const files = 64
	for _, ver := range []string{"3", "4.0", "4.1"} {
		t.Run("v"+ver, func(t *testing.T) {
			framework.SkipIfNFSVersionUnsupported(t, ver)

			mount := framework.MountNFSWithVersion(t, nfsPort, ver)
			t.Cleanup(mount.Cleanup)

			dir := mount.FilePath("seekdir_v" + ver)
			framework.CreateDir(t, dir)
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			for i := range files {
				framework.WriteFile(t, filepath.Join(dir, fmt.Sprintf("%02d-seekdir", i)), nil)
			}

			fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
			require.NoError(t, err, "open the directory")
			t.Cleanup(func() { _ = syscall.Close(fd) })

			entries := readDirents(t, fd)
			require.GreaterOrEqual(t, len(entries), files, "the listing should hold every file")

			var failed []string
			for i, e := range entries[:len(entries)-1] {
				want := entries[i+1].name
				if _, err := syscall.Seek(fd, e.off, io.SeekStart); err != nil {
					failed = append(failed, fmt.Sprintf("seek to %d, after %q: %v", e.off, e.name, err))
					continue
				}
				switch rest := readDirents(t, fd); {
				case len(rest) == 0:
					failed = append(failed, fmt.Sprintf("seek to %d, after %q: no entry, want %q", e.off, e.name, want))
				case rest[0].name != want:
					failed = append(failed, fmt.Sprintf("seek to %d, after %q: read %q, want %q", e.off, e.name, rest[0].name, want))
				}
			}
			require.Empty(t, failed, "%d of %d positions did not seek back", len(failed), len(entries)-1)
		})
	}
}

// dirent is one entry of a getdents64 listing: its name and its d_off, the
// position after it.
type dirent struct {
	name string
	off  int64
}

// readDirents reads the directory open on fd from its current position to
// its end, decoding each getdents64 record: d_ino (8 bytes), d_off (8),
// d_reclen (2), d_type (1), then the NUL-terminated name.
func readDirents(t *testing.T, fd int) []dirent {
	t.Helper()
	var out []dirent
	buf := make([]byte, 64<<10)
	for {
		n, err := syscall.Getdents(fd, buf)
		require.NoError(t, err, "getdents")
		if n == 0 {
			return out
		}
		for rec := buf[:n]; len(rec) > 0; {
			reclen := binary.NativeEndian.Uint16(rec[16:18])
			name := rec[19:reclen]
			out = append(out, dirent{
				name: string(name[:bytes.IndexByte(name, 0)]),
				off:  int64(binary.NativeEndian.Uint64(rec[8:16])),
			})
			rec = rec[reclen:]
		}
	}
}
