package platform

import (
	"testing"

	"golang.org/x/sys/unix"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// TestCheckConstantsAreLinux pins the statfs types the portable verdict
// declares to golang.org/x/sys. The other two constants have no x/sys pair to
// compare against: fsNoCOW is FS_NOCOW_FL in the kernel's
// include/uapi/linux/fs.h, and btrfsSubvolumeRootIno is
// BTRFS_FIRST_FREE_OBJECTID, 256, in include/uapi/linux/btrfs_tree.h.
func TestCheckConstantsAreLinux(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, c := range []struct {
		name      string
		got, want int64
	}{
		{"ext2", magicExt, unix.EXT2_SUPER_MAGIC},
		{"ext4", magicExt, unix.EXT4_SUPER_MAGIC},
		{"xfs", magicXFS, unix.XFS_SUPER_MAGIC},
		{"btrfs", magicBtrfs, unix.BTRFS_SUPER_MAGIC},
	} {
		if c.got != c.want {
			t.Errorf("%s magic: got %#x, want %#x", c.name, c.got, c.want)
		}
	}
}
