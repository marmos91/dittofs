//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
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

// setupNFSv42FSServer starts a server backed by an fs (not memory) local block
// store and returns the CLI runner and NFS port.
//
// Block-list-dependent NFSv4.2 operations (SEEK/READ_PLUS hole map, CLONE
// payload copy) read the file's CAS block list (file.Blocks). Only the fs store
// runs the append-log rollup that chunks written data and persists ChunkRefs to
// metadata; the memory store serves reads directly and never populates
// file.Blocks. With a memory store a freshly written file therefore looks
// block-less — SEEK reports it as all-hole and CLONE copies an empty manifest
// (a zero-filled destination). These tests must run against the fs store and
// wait for the background rollup to populate file.Blocks before the op.
func setupNFSv42FSServer(t *testing.T) (*helpers.CLIRunner, int) {
	t.Helper()

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	runner := helpers.LoginAsAdmin(t, sp.APIURL())

	metaStore := helpers.UniqueTestName("sparse-meta")
	localStore := helpers.UniqueTestName("sparse-local")

	_, err := runner.CreateMetadataStore(metaStore, "memory")
	require.NoError(t, err, "create metadata store")
	t.Cleanup(func() { _ = runner.DeleteMetadataStore(metaStore) })

	_, err = runner.CreateLocalBlockStore(localStore, "fs",
		helpers.WithBlockRawConfig(fmt.Sprintf(`{"path":%q}`, t.TempDir())))
	require.NoError(t, err, "create fs block store")
	t.Cleanup(func() { _ = runner.DeleteLocalBlockStore(localStore) })

	_, err = runner.CreateShare("/export", metaStore, localStore)
	require.NoError(t, err, "create share")
	t.Cleanup(func() { _ = runner.DeleteShare("/export") })

	// Mount as root must not be squashed, so root can write the test files.
	_, err = runner.Run("share", "nfs-config", "set", "/export", "--squash", "root_to_admin")
	require.NoError(t, err, "set share squash policy")

	nfsPort := helpers.FindFreePort(t)
	_, err = runner.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err, "enable NFS adapter")
	t.Cleanup(func() { _, _ = runner.DisableAdapter("nfs") })

	require.NoError(t, helpers.WaitForAdapterStatus(t, runner, "nfs", true, 10*time.Second),
		"NFS adapter should become enabled")
	framework.WaitForServer(t, nfsPort, 10*time.Second)

	return runner, nfsPort
}

// TestNFSv42Sparse exercises the SEEK / READ_PLUS / DEALLOCATE / ALLOCATE
// cluster over a vers=4.2 mount (SPARSE-01..04).
func TestNFSv42Sparse(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4.2 sparse tests in short mode")
	}

	_, nfsPort := setupNFSv42FSServer(t)
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

	t.Run("SPARSE-01 SEEK_HOLE / SEEK_DATA report sparse boundaries", func(t *testing.T) {
		// SEEK derives its data/hole view from the file's CAS block list. Rather
		// than punch a hole (DEALLOCATE granularity is chunk-bounded and rollup
		// re-chunks nondeterministically), build a naturally sparse file: two
		// written data regions separated by an unwritten gap that is never backed
		// by a block. Both regions are small and low-offset so the background
		// rollup ticker chunks them fully; the gap between them is a real,
		// deterministic hole.
		const (
			region   = 2 << 20 // 2 MiB written data regions
			gapStart = region  // first hole begins at the end of region 1 (2 MiB)
			dataAt   = 6 << 20 // second region starts at 6 MiB (gap = [2 MiB, 6 MiB))
		)
		size := int64(dataAt + region)
		path := mount.FilePath("seek.bin")

		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
		require.NoError(t, err)
		blk := bytes.Repeat([]byte{0xCD}, region)
		_, err = f.WriteAt(blk, 0)
		require.NoError(t, err)
		_, err = f.WriteAt(blk, dataAt)
		require.NoError(t, err)
		require.NoError(t, f.Sync(), "flush writes to server (critical for NFSv4)")
		require.NoError(t, f.Close())

		// Wait for the background rollup to chunk both regions into CAS blocks and
		// persist them to metadata. SEEK derives its hole map from file.Blocks, so
		// it only reflects rolled-up data; until rollup runs the file looks
		// block-less. Readiness: from the start of the second region the only
		// remaining hole is the one at EOF, i.e. the whole region is data.
		require.Eventually(t, func() bool {
			h, e := seekTo(t, path, dataAt, seekHole)
			return e == nil && h == size
		}, 20*time.Second, 250*time.Millisecond, "both regions should roll up into CAS blocks")

		// Data is present at the start of the file.
		dataStart, err := seekTo(t, path, 0, seekData)
		require.NoError(t, err)
		assert.Equal(t, int64(0), dataStart, "SEEK_DATA from 0 should find data at offset 0")

		// SEEK_HOLE from 0 -> the gap between the two regions: a real interior
		// hole beginning after the first region and before the second.
		holeOff, err := seekTo(t, path, 0, seekHole)
		require.NoError(t, err)
		assert.Greater(t, holeOff, int64(0), "hole must begin after the first region's data")
		assert.Less(t, holeOff, int64(dataAt), "hole must begin before the second region")

		// SEEK_DATA from inside the gap -> the second region; it skips the gap and
		// never goes backwards.
		fromGap := int64(gapStart + region) // 4 MiB: inside the gap [2 MiB, 6 MiB)
		dataOff, err := seekTo(t, path, fromGap, seekData)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, dataOff, fromGap, "SEEK_DATA must not go backwards")
		assert.Greater(t, dataOff, holeOff, "data must resume after the hole")
		assert.LessOrEqual(t, dataOff, int64(dataAt), "data should resume by the second region")

		// SEEK_HOLE from inside the final region -> EOF (the trailing hole).
		holeAtEOF, err := seekTo(t, path, int64(dataAt)+region/2, seekHole)
		require.NoError(t, err)
		assert.Equal(t, size, holeAtEOF, "SEEK_HOLE in the last region should report the hole at EOF")
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
