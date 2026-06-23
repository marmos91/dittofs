//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// NFSv4.2 sparse-file operations (RFC 7862): SEEK, READ_PLUS, DEALLOCATE,
// ALLOCATE. These mount with vers=4.2 and drive the operations through the
// Linux kernel client:
//   - fallocate -p  -> DEALLOCATE (punch hole)
//   - fallocate     -> ALLOCATE
//   - lseek SEEK_HOLE/SEEK_DATA -> SEEK (the kernel also uses READ_PLUS for
//     buffered reads of sparse files on a v4.2 mount)
// They require a Linux kernel client with NFSv4.2 (>= 4.x); MountNFSWithVersion
// skips on macOS.
// =============================================================================

// Linux lseek whence values for sparse seeks (stable kernel ABI).
const (
	seekData = 3 // SEEK_DATA
	seekHole = 4 // SEEK_HOLE
)

// seekTo opens path and returns the offset of the next data/hole at or after
// `from`, plus any error (ENXIO when no such region exists).
func seekTo(t *testing.T, path string, from int64, whence int) (int64, error) {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	return syscall.Seek(int(f.Fd()), from, whence)
}

// blocks512 returns the number of 512-byte blocks the file occupies on the
// server (st_blocks), used to confirm DEALLOCATE actually frees storage.
func blocks512(t *testing.T, path string) int64 {
	t.Helper()
	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(path, &st))
	return st.Blocks
}

// TestNFSv42Sparse exercises the SEEK / READ_PLUS / DEALLOCATE / ALLOCATE
// cluster over a vers=4.2 mount (SPARSE-01..04).
func TestNFSv42Sparse(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4.2 sparse tests in short mode")
	}

	_, _, nfsPort := setupNFSv4TestServer(t)
	mount := framework.MountNFSWithVersion(t, nfsPort, "4.2")
	t.Cleanup(mount.Cleanup)

	t.Run("SPARSE-04 ALLOCATE extends size and reads back zeros", func(t *testing.T) {
		path := mount.FilePath("alloc.bin")
		framework.WriteFile(t, path, []byte{}) // create empty

		// fallocate offset 0 length 64KiB -> file grows, range reads as zeros.
		out, err := exec.Command("fallocate", "-o", "0", "-l", "65536", path).CombinedOutput()
		require.NoErrorf(t, err, "fallocate: %s", out)

		fi, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, int64(65536), fi.Size(), "ALLOCATE must extend logical size")

		got := framework.ReadFile(t, path)
		assert.Equal(t, 65536, len(got))
		assert.True(t, bytes.Equal(got, make([]byte, 65536)), "allocated range must read back as zeros")
	})

	t.Run("SPARSE-03 DEALLOCATE punches a hole readable as zeros", func(t *testing.T) {
		path := mount.FilePath("dealloc.bin")
		// 1 MiB of 0xAB so the punch is observable in st_blocks.
		full := bytes.Repeat([]byte{0xAB}, 1<<20)
		framework.WriteFile(t, path, full)

		before := blocks512(t, path)

		// Punch a 512 KiB hole at offset 256 KiB.
		out, err := exec.Command("fallocate", "-p", "-o", "262144", "-l", "524288", path).CombinedOutput()
		require.NoErrorf(t, err, "fallocate -p: %s", out)

		got := framework.ReadFile(t, path)
		require.Equal(t, len(full), len(got), "size unchanged by DEALLOCATE")
		// Punched region reads as zeros; surrounding bytes preserved.
		assert.True(t, bytes.Equal(got[:262144], full[:262144]), "head preserved")
		assert.True(t, bytes.Equal(got[262144:262144+524288], make([]byte, 524288)), "punched region zeroed")
		assert.True(t, bytes.Equal(got[262144+524288:], full[262144+524288:]), "tail preserved")

		// Storage usage should drop (best-effort: allow equal on backends that
		// defer reclaim, but never grow).
		after := blocks512(t, path)
		assert.LessOrEqual(t, after, before, "DEALLOCATE should not increase storage")
	})

	t.Run("SPARSE-01 SEEK_HOLE / SEEK_DATA report punched boundaries", func(t *testing.T) {
		path := mount.FilePath("seek.bin")
		full := bytes.Repeat([]byte{0xCD}, 1<<20)
		framework.WriteFile(t, path, full)

		// Punch [256KiB, 768KiB).
		out, err := exec.Command("fallocate", "-p", "-o", "262144", "-l", "524288", path).CombinedOutput()
		require.NoErrorf(t, err, "fallocate -p: %s", out)

		// SEEK_HOLE from 0 -> the start of the punched hole. The exact boundary
		// depends on FastCDC chunk alignment (only whole chunks fully inside the
		// punch become a hole in the block-list hole map), so assert the hole
		// begins within the punched region rather than pinning the byte. It must
		// be a real boundary before EOF, not the file size ("no hole found").
		holeOff, err := seekTo(t, path, 0, seekHole)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, holeOff, int64(262144), "hole should not start before the punch")
		assert.Less(t, holeOff, int64(262144+524288), "hole should start within the punched region")

		// SEEK_DATA from the middle of the punched region -> data resumes by the
		// end of the punch, and never goes backwards.
		dataOff, err := seekTo(t, path, 262144+262144, seekData)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, dataOff, int64(262144+262144), "SEEK_DATA must not go backwards")
		assert.LessOrEqual(t, dataOff, int64(262144+524288), "data should resume by the end of the punched region")
	})

	t.Run("SPARSE-02 READ_PLUS over a sparse file returns correct bytes", func(t *testing.T) {
		// On a v4.2 mount the kernel issues READ_PLUS for buffered reads; the
		// logical content must round-trip exactly regardless of hole encoding.
		path := mount.FilePath("readplus.bin")
		full := bytes.Repeat([]byte{0xEE}, 512<<10)
		framework.WriteFile(t, path, full)

		out, err := exec.Command("fallocate", "-p", "-o", "131072", "-l", "131072", path).CombinedOutput()
		require.NoErrorf(t, err, "fallocate -p: %s", out)

		want := append([]byte(nil), full...)
		for i := 131072; i < 131072+131072; i++ {
			want[i] = 0
		}
		got := framework.ReadFile(t, path)
		assert.True(t, bytes.Equal(got, want), "READ_PLUS reconstructed content must match data+hole layout")
	})
}
