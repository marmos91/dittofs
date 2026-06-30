//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNFSv42Clone exercises NFSv4.2 CLONE (RFC 7862 op 71) end-to-end over a
// vers=4.2 mount, driven by `cp --reflink` — the exact request a Linux client
// issues for a reflink (fs/nfs/nfs42proc.c nfs42_proc_clone -> OP_CLONE).
//
// CLONE-01 proves the reflink round-trip: the clone is content-identical to the
// source (the destination references the same content-addressed blocks).
// CLONE-02 proves copy-on-write: writing to one side after the clone diverges
// the two files, leaving the other untouched — intrinsic to the dedup store.
func TestNFSv42Clone(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4.2 CLONE tests in short mode")
	}

	// CLONE copies the source's CAS block list (file.Blocks). The memory store
	// never populates it, so a clone over a memory-backed share copies an empty
	// manifest and reads back zeros. Use the fs store and wait for the source to
	// roll up before cloning.
	_, nfsPort := setupNFSv42FSServer(t)
	mount := framework.MountNFSWithVersion(t, nfsPort, "4.2")
	t.Cleanup(mount.Cleanup)

	t.Run("CLONE-01 cp --reflink produces a content-identical copy", func(t *testing.T) {
		src := mount.FilePath("clone-src.bin")
		dst := mount.FilePath("clone-dst.bin")

		// 4 MiB of pseudo-random-but-deterministic content so the clone is large
		// enough to make any accidental byte-copy observable, yet reproducible.
		full := make([]byte, 4<<20)
		for i := range full {
			full[i] = byte(i*131 + 7)
		}
		framework.WriteFile(t, src, full)
		// No rollup wait needed: CLONE drains the source into CAS itself (#1481).

		// cp --reflink=always FAILS rather than falling back to a plain copy if
		// the server does not support CLONE, so a clean exit proves OP_CLONE ran.
		out, err := exec.Command("cp", "--reflink=always", src, dst).CombinedOutput()
		require.NoErrorf(t, err, "cp --reflink=always: %s", out)

		got := framework.ReadFile(t, dst)
		require.Equal(t, len(full), len(got), "clone size must equal source size")
		assert.True(t, bytes.Equal(full, got), "clone content must equal source content")
	})

	t.Run("CLONE-02 copy-on-write: writing one side does not change the other", func(t *testing.T) {
		src := mount.FilePath("cow-src.bin")
		dst := mount.FilePath("cow-dst.bin")

		original := bytes.Repeat([]byte{0xCC}, 1<<20)
		framework.WriteFile(t, src, original)
		// No rollup wait needed: CLONE drains the source into CAS itself (#1481).

		out, err := exec.Command("cp", "--reflink=always", src, dst).CombinedOutput()
		require.NoErrorf(t, err, "cp --reflink=always: %s", out)

		// Overwrite the first 64 KiB of the destination with a distinct pattern.
		f, err := os.OpenFile(dst, os.O_WRONLY, 0)
		require.NoError(t, err)
		patch := bytes.Repeat([]byte{0x55}, 64<<10)
		_, werr := f.WriteAt(patch, 0)
		require.NoError(t, werr)
		require.NoError(t, f.Close())

		// Source is unchanged (COW: the write produced new CAS blocks).
		srcGot := framework.ReadFile(t, src)
		assert.True(t, bytes.Equal(original, srcGot), "source must be untouched after writing the clone")

		// Destination has the patched head and the original tail.
		dstGot := framework.ReadFile(t, dst)
		require.Equal(t, len(original), len(dstGot))
		assert.True(t, bytes.Equal(dstGot[:len(patch)], patch), "clone head must reflect the new write")
		assert.True(t, bytes.Equal(dstGot[len(patch):], original[len(patch):]), "clone tail must keep cloned content")
	})
}
