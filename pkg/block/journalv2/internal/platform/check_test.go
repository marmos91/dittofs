package platform

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// hostLines are lines of /proc/self/mountinfo copied from a Linux 6.12.10
// host. The kernel writes this file (show_mountinfo in fs/proc_namespace.c),
// so its layout does not depend on the distribution. The root filesystem is
// ext4 with barriers on. The /run/snapd/ns line carries no optional field,
// so the separator is the seventh field rather than the eighth.
const hostLines = "" +
	"26 33 0:24 / /sys rw,nosuid,nodev,noexec,relatime shared:7 - sysfs sysfs rw\n" +
	"27 33 0:25 / /proc rw,nosuid,nodev,noexec,relatime shared:13 - proc proc rw\n" +
	"30 33 0:27 / /run rw,nosuid,nodev,noexec,relatime shared:5 - tmpfs tmpfs rw,size=3256364k,mode=755,inode64\n" +
	"33 1 259:3 / / rw,noatime shared:1 - ext4 /dev/nvme0n1p3 rw,errors=remount-ro\n" +
	"1851 30 0:55 / /run/user/1000 rw,nosuid,nodev,relatime shared:624 - tmpfs tmpfs rw,size=3256360k,nr_inodes=814090,mode=700,uid=1000,gid=1000,inode64\n" +
	"2171 30 0:27 /snapd/ns /run/snapd/ns rw,nosuid,nodev,noexec,relatime - tmpfs tmpfs rw,size=3256364k,mode=755,inode64\n"

// hostRootNoBarrier is hostLines with the root filesystem's superblock
// options turned to nobarrier, as Linux 6.12's ext4 writes them.
var hostRootNoBarrier = strings.Replace(hostLines,
	"ext4 /dev/nvme0n1p3 rw,errors=remount-ro", "ext4 /dev/nvme0n1p3 rw,nobarrier,errors=remount-ro", 1)

// The nested mounts below are written in the layout the kernel's
// show_mountinfo produces (fs/proc_namespace.c), with the superblock options
// each filesystem's show_options writes:
//   - ext4 since Linux 3.4 writes "nobarrier" when the mount turned barriers
//     off against the superblock's default, and nothing when they are on
//     (fs/ext4/super.c, _ext4_show_options). An option equal to the default
//     is not written, so a default set in the superblock with
//     tune2fs -o nobarrier does not appear in the table;
//   - ext4 up to Linux 3.3 always writes "barrier=0" or "barrier=1";
//   - XFS up to Linux 4.18 writes "nobarrier" when barriers are off
//     (fs/xfs/xfs_super.c, xfs_showargs); Linux 4.19 removed the option;
//   - Btrfs writes "nobarrier" when barriers are off (fs/btrfs/super.c,
//     btrfs_show_options).
const (
	ext4Nested          = "90 33 259:5 / /srv/journal rw,relatime shared:40 - ext4 /dev/nvme1n1p1 rw\n"
	ext4NestedNoBarrier = "90 33 259:5 / /srv/journal rw,relatime shared:40 - ext4 /dev/nvme1n1p1 rw,nobarrier\n"
	ext4OldBarrierOff   = "90 33 8:17 / /srv/journal rw,relatime - ext4 /dev/sdb1 rw,barrier=0,data=ordered\n"
	ext4OldBarrierOn    = "90 33 8:17 / /srv/journal rw,relatime - ext4 /dev/sdb1 rw,barrier=1,data=ordered\n"
	xfsNested           = "90 33 8:17 / /srv/journal rw,relatime shared:40 - xfs /dev/sdb1 rw,attr2,inode64,noquota\n"
	xfsNestedNoBarrier  = "90 33 8:17 / /srv/journal rw,relatime shared:40 - xfs /dev/sdb1 rw,attr2,nobarrier,inode64,noquota\n"
	btrfsNested         = "90 33 0:45 /journal /srv/journal rw,relatime shared:40 - btrfs /dev/sdb1 rw,ssd,space_cache=v2,subvolid=256,subvol=/journal\n"
	btrfsNestedNoBarr   = "90 33 0:45 /journal /srv/journal rw,relatime shared:40 - btrfs /dev/sdb1 rw,nobarrier,ssd,space_cache=v2,subvolid=256,subvol=/journal\n"
)

// Magics of filesystems RFC 28 Appendix A does not list (TMPFS_MAGIC and
// OVERLAYFS_SUPER_MAGIC in the kernel's include/uapi/linux/magic.h).
const (
	tmpfsMagic   = 0x01021994
	overlayMagic = 0x794c7630
)

// probe returns a probe of dir under table whose attribute and inode answers
// pass every Btrfs rule, so a case refuses only for what it changes. Its
// mount ID is unknown.
func probe(table, dir string, fsType uint32) linuxProbe {
	return linuxProbe{
		dir:       dir,
		mountinfo: table,
		fsType:    fsType,
		dirFlags:  fsNoCOW,
		dirIno:    btrfsSubvolumeRootIno,
	}
}

// wantRefusal fails t unless err is a refusal by rule of dir naming mount,
// option and path.
func wantRefusal(t *testing.T, err error, rule refusalRule, dir, mount, option, path string) {
	t.Helper()
	var r *refusal

	if !errors.As(err, &r) {
		t.Fatalf("got %v, want a refusal", err)
	}

	if r.rule != rule || r.dir != dir || r.mount != mount || r.option != option || r.path != path {
		t.Fatalf("got refusal {rule %d, dir %q, mount %q, option %q, path %q}, want {rule %d, dir %q, mount %q, option %q, path %q}",
			r.rule, r.dir, r.mount, r.option, r.path, rule, dir, mount, option, path)
	}
}

// wantBarrier fails t unless err refuses dir for option on the mount at mount.
func wantBarrier(t *testing.T, err error, dir, mount, option string) {
	t.Helper()
	wantRefusal(t, err, ruleBarrier, dir, mount, option, "")
}

func wantPass(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("got %v, want the directory accepted", err)
	}
}

// wantError fails t unless err is sentinel and no refusal.
func wantError(t *testing.T, err, sentinel error) {
	t.Helper()
	var r *refusal

	if errors.As(err, &r) {
		t.Fatalf("got refusal %v, want %v", err, sentinel)
	}

	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want %v", err, sentinel)
	}
}

// TestCheckMountOptions covers RFC 28 Appendix A: "Open MUST refuse a
// copy-on-write filesystem mounted without cache flushes (nobarrier on
// Btrfs)" and "Open MUST refuse a filesystem mounted without cache flushes
// where the mount options say so (nobarrier or barrier=0 on ext4, nobarrier
// on older XFS)". Each row refuses on a mount nested under the ext4 root, and
// the same filesystem without the option passes.
func TestCheckMountOptions(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const dir = "/srv/journal/j"
	refused := []struct {
		name   string
		line   string
		fsType uint32
		option string
	}{
		{"ext4 nobarrier", ext4NestedNoBarrier, magicExt, "nobarrier"},
		{"ext4 barrier=0", ext4OldBarrierOff, magicExt, "barrier=0"},
		{"xfs nobarrier", xfsNestedNoBarrier, magicXFS, "nobarrier"},
		{"btrfs nobarrier", btrfsNestedNoBarr, magicBtrfs, "nobarrier"},
	}
	for _, tc := range refused {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			err := checkLinuxProbe(probe(hostLines+tc.line, dir, tc.fsType))
			wantBarrier(t, err, dir, "/srv/journal", tc.option)
		})
	}

	passed := []struct {
		name   string
		line   string
		fsType uint32
	}{
		{"ext4", ext4Nested, magicExt},
		{"ext4 barrier=1", ext4OldBarrierOn, magicExt},
		{"xfs", xfsNested, magicXFS},
		{"btrfs", btrfsNested, magicBtrfs},
		// Synthetic: no XFS kernel writes barrier=0. The rows are per
		// filesystem, and barrier=0 is ext4's row only.
		{"xfs barrier=0 (synthetic)", "90 33 8:17 / /srv/journal rw,relatime shared:40 - xfs /dev/sdb1 rw,attr2,barrier=0,inode64,noquota\n", magicXFS},
	}
	for _, tc := range passed {
		t.Run("passes "+tc.name, func(t *testing.T) {
			wantPass(t, checkLinuxProbe(probe(hostLines+tc.line, dir, tc.fsType)))
		})
	}

	// The mount point itself is held by its own mount, not by its parent.
	t.Run("refuses the mount point itself", func(t *testing.T) {
		err := checkLinuxProbe(probe(hostLines+ext4NestedNoBarrier, "/srv/journal", magicExt))
		wantBarrier(t, err, "/srv/journal", "/srv/journal", "nobarrier")
	})

	// The option on the parent does not reach a nested mount that holds the
	// directory without it.
	t.Run("passes nobarrier on the parent only", func(t *testing.T) {
		wantPass(t, checkLinuxProbe(probe(hostRootNoBarrier+ext4Nested, dir, magicExt)))
	})

	// And the parent's option is read where the parent does hold the
	// directory.
	t.Run("refuses nobarrier on the root holding the directory", func(t *testing.T) {
		err := checkLinuxProbe(probe(hostRootNoBarrier+ext4Nested, "/var/journal", magicExt))
		wantBarrier(t, err, "/var/journal", "/", "nobarrier")
	})

	// /data is not a prefix of /database on a path boundary.
	t.Run("passes a sibling sharing a name prefix", func(t *testing.T) {
		table := hostLines + "91 33 259:6 / /data rw,relatime shared:41 - ext4 /dev/nvme1n1p2 rw,nobarrier\n"
		wantPass(t, checkLinuxProbe(probe(table, "/database/j", magicExt)))
	})

	// The kernel writes a space, a tab, a newline and a backslash in a mount
	// point as three octal digits (mangle_path in fs/seq_file.c, called by
	// show_mountinfo with " \t\n\\").
	for _, tc := range []struct {
		name, escaped, decoded string
	}{
		{"space", `/mnt/journal\040disk`, "/mnt/journal disk"},
		{"tab", `/mnt/journal\011disk`, "/mnt/journal\tdisk"},
		{"newline", `/mnt/journal\012disk`, "/mnt/journal\ndisk"},
		{"backslash", `/mnt/journal\134disk`, `/mnt/journal\disk`},
	} {
		t.Run("refuses a mount point holding an escaped "+tc.name, func(t *testing.T) {
			table := hostLines + "92 33 259:7 / " + tc.escaped + " rw,relatime shared:42 - ext4 /dev/nvme1n1p3 rw,nobarrier\n"
			err := checkLinuxProbe(probe(table, tc.decoded+"/j", magicExt))
			wantBarrier(t, err, tc.decoded+"/j", tc.decoded, "nobarrier")
		})
	}

	// Options are whole comma-separated words: one that only contains the
	// word does not refuse.
	t.Run("passes an option that only contains the word", func(t *testing.T) {
		line := "90 33 259:5 / /srv/journal rw,relatime shared:40 - ext4 /dev/nvme1n1p1 rw,nobarrier_x,barrier=01\n"
		wantPass(t, checkLinuxProbe(probe(hostLines+line, dir, magicExt)))
	})

	// The barrier options are superblock options, the last field. The
	// per-mount options field never carries them (show_vfsmnt_opts writes
	// only the VFS flags); this line puts one there to prove it is not read.
	t.Run("passes nobarrier in the per-mount field only", func(t *testing.T) {
		line := "90 33 259:5 / /srv/journal rw,relatime,nobarrier shared:40 - ext4 /dev/nvme1n1p1 rw\n"
		wantPass(t, checkLinuxProbe(probe(hostLines+line, dir, magicExt)))
	})

	// The separator follows zero or more optional fields (proc(5)).
	t.Run("refuses a mount with no optional field", func(t *testing.T) {
		line := "93 33 259:5 / /srv/journal rw,relatime - ext4 /dev/nvme1n1p1 rw,nobarrier\n"
		wantBarrier(t, checkLinuxProbe(probe(hostLines+line, dir, magicExt)), dir, "/srv/journal", "nobarrier")
	})
	t.Run("refuses a mount with two optional fields", func(t *testing.T) {
		line := "93 33 259:5 / /srv/journal rw,relatime shared:40 master:12 - ext4 /dev/nvme1n1p1 rw,nobarrier\n"
		wantBarrier(t, checkLinuxProbe(probe(hostLines+line, dir, magicExt)), dir, "/srv/journal", "nobarrier")
	})

	// Btrfs writes subvol= last (btrfs_show_options in fs/btrfs/super.c, every
	// release from Linux 4.5 to 6.17). Up to 6.14 it escapes the value with
	// " \t\n\\", so a comma stays as it is; from 6.15 it uses seq_show_option,
	// which writes a comma as \054 (include/linux/seq_file.h). Either way a
	// subvolume named "a,nobarrier" is not an option.
	t.Run("passes a btrfs subvolume named like the option", func(t *testing.T) {
		line := "90 33 0:45 /a,nobarrier /srv/journal rw,relatime shared:40 - btrfs /dev/sdb1 rw,ssd,space_cache=v2,subvolid=257,subvol=/a,nobarrier\n"
		wantPass(t, checkLinuxProbe(probe(hostLines+line, dir, magicBtrfs)))
	})
	t.Run("passes a btrfs subvolume named like the option, as Linux 6.15 writes it", func(t *testing.T) {
		line := `90 33 0:45 /a,nobarrier /srv/journal rw,relatime shared:40 - btrfs /dev/sdb1 rw,ssd,space_cache=v2,subvolid=257,subvol=/a\054nobarrier` + "\n"
		wantPass(t, checkLinuxProbe(probe(hostLines+line, dir, magicBtrfs)))
	})

	// 9p writes uname= and aname= as given, without escaping
	// (v9fs_show_options in fs/9p/v9fs.c), so a space in a value splits the
	// superblock options. This line has the layout show_mountinfo writes for
	// a 9p mount of a Windows drive; it belongs to another mount and must not
	// stop the journal's check.
	ninep := `97 33 0:61 / /mnt/d rw,noatime shared:45 - 9p drvfs rw,dirsync,aname=drvfs;path=D:\My Data;uid=1000;gid=1000,mmap,access=client,msize=65536,trans=fd,rfd=5,wfd=5` + "\n"
	t.Run("passes beside a 9p mount with a space in its options", func(t *testing.T) {
		wantPass(t, checkLinuxProbe(probe(hostLines+ninep+ext4Nested, dir, magicExt)))
	})

	// Synthetic: an ext4 line of the same shape, holding the directory. The
	// options after the space are still read.
	t.Run("refuses nobarrier after a space in the held mount's options (synthetic)", func(t *testing.T) {
		line := `90 33 259:5 / /srv/journal rw,relatime shared:40 - ext4 /dev/nvme1n1p1 rw,x=D:\My Data,nobarrier` + "\n"
		wantBarrier(t, checkLinuxProbe(probe(hostLines+ninep+line, dir, magicExt)), dir, "/srv/journal", "nobarrier")
	})

	// The filesystem is the one statfs names: ext3 shares ext4's magic and
	// the ext4 row.
	t.Run("refuses ext3 by its statfs type", func(t *testing.T) {
		line := "90 33 8:17 / /srv/journal rw,relatime - ext3 /dev/sdb1 rw,nobarrier,data=ordered\n"
		wantBarrier(t, checkLinuxProbe(probe(hostLines+line, dir, magicExt)), dir, "/srv/journal", "nobarrier")
	})

	// A filesystem RFC 28 Appendix A does not list is not refused here.
	t.Run("passes a tmpfs directory", func(t *testing.T) {
		wantPass(t, checkLinuxProbe(probe(hostLines, "/run/user/1000/j", tmpfsMagic)))
	})
}

// TestCheckVisibleMount covers which mount holds the directory: the one
// visible at its path, as RFC 28 Appendix A's barrier rows are about the
// mount the journal's writes go through. proc(5), /proc/[pid]/mountinfo:
// "the top-most mount is the one that is not the parent of any other mount at
// the same location", and "this top-most mount will be accessible only if the
// longest path subprefix of P that is a mount point is not itself hidden by a
// stacked mount."
func TestCheckVisibleMount(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const dir = "/srv/journal/j"

	// Two mounts stacked at one point, whichever line comes first.
	lower := "90 33 259:5 / /srv/journal rw,relatime shared:40 - ext4 /dev/nvme1n1p1 rw,nobarrier\n"
	upper := "95 90 259:8 / /srv/journal rw,relatime shared:43 - ext4 /dev/nvme2n1p1 rw\n"
	t.Run("passes a clean mount stacked over a nobarrier one", func(t *testing.T) {
		wantPass(t, checkLinuxProbe(probe(hostLines+lower+upper, dir, magicExt)))
	})
	t.Run("passes a clean mount stacked over a nobarrier one, listed first", func(t *testing.T) {
		wantPass(t, checkLinuxProbe(probe(hostLines+upper+lower, dir, magicExt)))
	})
	t.Run("refuses a nobarrier mount stacked over a clean one", func(t *testing.T) {
		under := "90 33 259:5 / /srv/journal rw,relatime shared:40 - ext4 /dev/nvme1n1p1 rw\n"
		over := "95 90 259:8 / /srv/journal rw,relatime shared:43 - ext4 /dev/nvme2n1p1 rw,nobarrier\n"
		wantBarrier(t, checkLinuxProbe(probe(hostLines+over+under, dir, magicExt)), dir, "/srv/journal", "nobarrier")
	})

	// The root is the top of the stack at "/", not the first mount there.
	t.Run("refuses a nobarrier mount stacked over a clean root", func(t *testing.T) {
		over := "34 33 259:9 / / rw,relatime shared:2 - ext4 /dev/nvme2n1p2 rw,nobarrier\n"
		wantBarrier(t, checkLinuxProbe(probe(hostLines+over, "/var/journal", magicExt)), "/var/journal", "/", "nobarrier")
	})
	t.Run("passes a clean mount stacked over a nobarrier root", func(t *testing.T) {
		over := "34 33 259:9 / / rw,relatime shared:2 - ext4 /dev/nvme2n1p2 rw\n"
		wantPass(t, checkLinuxProbe(probe(hostRootNoBarrier+over, "/var/journal", magicExt)))
	})

	// Two mounts at one point with one parent leave the visible one
	// undecided, and the check fails closed. Kernels before Linux 4.11 could
	// produce this (attach_shadowed in fs/namespace.c, v4.10); from 4.11 a new
	// mount is tucked under the one already there (mnt_change_mountpoint).
	t.Run("fails on two mounts tied at one point", func(t *testing.T) {
		a := "90 33 259:5 / /srv/journal rw,relatime shared:40 - ext4 /dev/nvme1n1p1 rw\n"
		b := "91 33 259:6 / /srv/journal rw,relatime shared:41 - ext4 /dev/nvme1n1p2 rw,nobarrier\n"
		wantError(t, checkLinuxProbe(probe(hostLines+a+b, dir, magicExt)), errMountUnresolved)
	})

	// Mount IDs "may be reused after umount(2)" (proc(5)), so the top of a
	// stack can carry the lower ID: the highest ID is not the top.
	t.Run("passes a clean top mount with the lower ID", func(t *testing.T) {
		bottom := "95 33 259:5 / /srv/journal rw,relatime shared:40 - ext4 /dev/nvme1n1p1 rw,nobarrier\n"
		top := "90 95 259:8 / /srv/journal rw,relatime shared:43 - ext4 /dev/nvme2n1p1 rw\n"
		wantPass(t, checkLinuxProbe(probe(hostLines+bottom+top, dir, magicExt)))
	})
	t.Run("refuses a nobarrier top mount with the lower ID", func(t *testing.T) {
		bottom := "95 33 259:5 / /srv/journal rw,relatime shared:40 - ext4 /dev/nvme1n1p1 rw\n"
		top := "90 95 259:8 / /srv/journal rw,relatime shared:43 - ext4 /dev/nvme2n1p1 rw,nobarrier\n"
		wantBarrier(t, checkLinuxProbe(probe(hostLines+bottom+top, dir, magicExt)), dir, "/srv/journal", "nobarrier")
	})

	// 95 is stacked on /srv over 90, so 91, mounted on /srv/journal under 90,
	// is hidden: the directory lives on 95. The longest mount point that is a
	// prefix of the directory is 91's, which is the wrong answer.
	hidden := hostLines +
		"90 33 259:5 / /srv rw,relatime shared:40 - ext4 /dev/nvme1n1p1 rw\n" +
		"91 90 259:6 / /srv/journal rw,relatime shared:41 - ext4 /dev/nvme1n1p2 rw\n" +
		"95 90 259:8 / /srv rw,relatime shared:43 - ext4 /dev/nvme2n1p1 rw,nobarrier\n"
	t.Run("refuses through a mount stacked on an ancestor", func(t *testing.T) {
		wantBarrier(t, checkLinuxProbe(probe(hidden, dir, magicExt)), dir, "/srv", "nobarrier")
	})
	t.Run("refuses through a mount stacked on an ancestor, with its mount ID", func(t *testing.T) {
		p := probe(hidden, dir, magicExt)
		p.mountID = 95
		wantBarrier(t, checkLinuxProbe(p), dir, "/srv", "nobarrier")
	})

	// A mount ID is checked against the walk, never trusted in its place.
	t.Run("fails when the mount ID names a hidden mount", func(t *testing.T) {
		p := probe(hidden, dir, magicExt)
		p.mountID = 91
		wantError(t, checkLinuxProbe(p), errMountIDDisagrees)
	})
	t.Run("fails when the mount ID names no mount of the table", func(t *testing.T) {
		p := probe(hidden, dir, magicExt)
		p.mountID = 4242
		wantError(t, checkLinuxProbe(p), errMountUnresolved)
	})
	t.Run("passes when the mount ID agrees", func(t *testing.T) {
		p := probe(hostLines+ext4Nested, dir, magicExt)
		p.mountID = 90
		wantPass(t, checkLinuxProbe(p))
	})
}

// TestCheckMountTable covers inputs that do not describe the directory. The
// options read may then be another mount's, so the check fails, and the
// failure is not a refusal by an RFC rule.
func TestCheckMountTable(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const dir = "/srv/journal/j"
	cases := []struct {
		name   string
		table  string
		fsType uint32
		want   error
	}{
		{"btrfs directory on an ext4 mount", hostLines + ext4Nested, magicBtrfs, errMountTypeMismatch},
		{"ext4 directory on an xfs mount", hostLines + xfsNested, magicExt, errMountTypeMismatch},
		{"a held line with no separator", hostLines + "90 33 259:5 / /srv/journal rw,relatime ext4 /dev/nvme1n1p1 rw,nobarrier\n", magicExt, errMountTableMalformed},
		// The bad line is one the walk never selects.
		{"a line elsewhere with no separator", hostLines + ext4Nested + "96 30 0:60 / /run/other rw,relatime tmpfs tmpfs rw\n", magicExt, errMountTableMalformed},
		// The kernel escapes only " \t\n\\" in a mount point (seq_path_root,
		// called by show_mountinfo in fs/proc_namespace.c), so any other
		// escape is no kernel's.
		{"a mount point with an unknown escape", hostLines + ext4Nested + `96 30 0:60 / /run/journal\qdisk rw,relatime shared:46 - tmpfs tmpfs rw` + "\n", magicExt, errMountTableMalformed},
		{"a mount point with a cut escape", hostLines + ext4Nested + `96 30 0:60 / /run/a\04 rw,relatime shared:46 - tmpfs tmpfs rw` + "\n", magicExt, errMountTableMalformed},
		{"a table with no root", strings.Replace(hostLines, "33 1 259:3 / / ", "33 1 259:3 / /root ", 1), magicExt, errMountUnresolved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, checkLinuxProbe(probe(tc.table, dir, tc.fsType)), tc.want)
		})
	}

	// The reverse: a magic outside the list on a line whose type is listed.
	// statfs and the table read one superblock (vfs_statfs in fs/statfs.c
	// calls its statfs; show_type in fs/proc_namespace.c writes its type),
	// so the two disagree only when the line is not the directory's mount.
	for _, tc := range []struct {
		name string
		line string
	}{
		{"ext4 nobarrier", ext4NestedNoBarrier},
		{"xfs", xfsNested},
		{"btrfs", btrfsNested},
	} {
		t.Run("tmpfs directory on the "+tc.name+" mount", func(t *testing.T) {
			wantError(t, checkLinuxProbe(probe(hostLines+tc.line, dir, tmpfsMagic)), errMountTypeMismatch)
		})
	}

	// A magic outside the list on a line of its own type still passes.
	t.Run("tmpfs directory on a tmpfs mount", func(t *testing.T) {
		line := "90 33 0:60 / /srv/journal rw,nosuid,nodev,relatime shared:40 - tmpfs tmpfs rw,size=1048576k,inode64\n"
		wantPass(t, checkLinuxProbe(probe(hostLines+line, dir, tmpfsMagic)))
	})

	// Copied from the same host: a container's overlay mount.
	t.Run("overlay directory on an overlay mount", func(t *testing.T) {
		const merged = "/var/lib/docker/overlay2/91ac3de8a5642416436c948b912446d85c4f7cc79d9795e27e02cfee8f705348/merged"
		line := "4480 33 0:72 / " + merged + " rw,relatime shared:1469 - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/l/NWAOTQRDFCMJ6YX2WMD4JLIOJP:/var/lib/docker/overlay2/l/AVMRQWDDOLOMATSJST66PCNJQS:/var/lib/docker/overlay2/l/I264ISPSJ3XXBZZQ4HCN6VOJGH,upperdir=/var/lib/docker/overlay2/91ac3de8a5642416436c948b912446d85c4f7cc79d9795e27e02cfee8f705348/diff,workdir=/var/lib/docker/overlay2/91ac3de8a5642416436c948b912446d85c4f7cc79d9795e27e02cfee8f705348/work\n"
		wantPass(t, checkLinuxProbe(probe(hostLines+line, merged+"/j", overlayMagic)))
	})

	// ext2, ext3 and ext4 all report magic 0xef53: none is a mismatch.
	for _, fs := range []string{"ext2", "ext3", "ext4"} {
		t.Run(fs+" on magic 0xef53 is no mismatch", func(t *testing.T) {
			line := "90 33 8:17 / /srv/journal rw,relatime - " + fs + " /dev/sdb1 rw\n"
			wantPass(t, checkLinuxProbe(probe(hostLines+line, dir, magicExt)))
		})
	}
}

// TestCheckCopyOnWrite covers RFC 28 Appendix A: "The journal's directory
// MUST have copy-on-write disabled before any file is created in it (chattr
// +C on Btrfs, which new files inherit; it does not apply to a file that
// already holds data)", "An implementation MUST check the attribute at Init
// and at open and refuse a directory without it on such a filesystem", and
// "MUST refuse, on Btrfs, a directory that is not a subvolume's root".
func TestCheckCopyOnWrite(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const dir = "/srv/journal"
	btrfs := func() linuxProbe { return probe(hostLines+btrfsNested, dir, magicBtrfs) }

	t.Run("refuses a btrfs directory without the attribute", func(t *testing.T) {
		p := btrfs()
		p.dirFlags = 0
		wantRefusal(t, checkLinuxProbe(p), ruleCopyOnWrite, dir, "", "", dir)
	})

	// A file created before the attribute was set keeps copy-on-write. The
	// bad file is the last in one case and the first in the other, so
	// checking only one end does not find both.
	t.Run("refuses a last file without the attribute in a +C directory", func(t *testing.T) {
		p := btrfs()
		p.files = []probedFile{
			{"00000000000000000001.seg", fsNoCOW},
			{"format", 0},
		}
		wantRefusal(t, checkLinuxProbe(p), ruleCopyOnWrite, dir, "", "", dir+"/format")
	})
	t.Run("refuses a first file without the attribute in a +C directory", func(t *testing.T) {
		p := btrfs()
		p.files = []probedFile{
			{"format", 0},
			{"00000000000000000001.seg", fsNoCOW},
		}
		wantRefusal(t, checkLinuxProbe(p), ruleCopyOnWrite, dir, "", "", dir+"/format")
	})

	// Inode 257 is an ordinary directory; inode 2 is the empty directory a
	// snapshot leaves where a nested subvolume was
	// (BTRFS_EMPTY_SUBVOL_DIR_OBJECTID in include/uapi/linux/btrfs_tree.h).
	for _, tc := range []struct {
		name string
		ino  uint64
	}{
		{"an ordinary directory", btrfsSubvolumeRootIno + 1},
		{"the placeholder of a nested subvolume", 2},
	} {
		t.Run("refuses "+tc.name+" as not a subvolume root", func(t *testing.T) {
			p := btrfs()
			p.dirIno = tc.ino
			wantRefusal(t, checkLinuxProbe(p), ruleSubvolume, dir, "", "", dir)
		})
	}

	// The accepted form: its own subvolume, +C, every file +C. Other flags
	// beside the attribute (FS_NOATIME_FL here) do not hide it.
	t.Run("passes the accepted form", func(t *testing.T) {
		const noatime = 0x00000080
		p := btrfs()
		p.dirFlags = fsNoCOW | noatime
		p.files = []probedFile{
			{"format", fsNoCOW},
			{"00000000000000000001.seg", fsNoCOW | noatime},
		}
		wantPass(t, checkLinuxProbe(p))
	})

	// At Init the directory is empty.
	t.Run("passes an empty accepted directory", func(t *testing.T) {
		wantPass(t, checkLinuxProbe(btrfs()))
	})

	// ext4 and XFS write allocated space in place: neither the attribute
	// nor a subvolume is asked of them.
	for _, tc := range []struct {
		name   string
		line   string
		fsType uint32
	}{
		{"ext4", ext4Nested, magicExt},
		{"xfs", xfsNested, magicXFS},
	} {
		t.Run("passes "+tc.name+" without the attribute", func(t *testing.T) {
			p := probe(hostLines+tc.line, dir, tc.fsType)
			p.dirFlags = 0
			p.dirIno = 2
			p.files = []probedFile{{"format", 0}}
			wantPass(t, checkLinuxProbe(p))
		})
	}
}

// TestCheckRefusalOrder pins which refusal is reported when several rules
// fail at once: the first in the order mount options, subvolume root,
// directory attribute, files. Only that one is reported.
func TestCheckRefusalOrder(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const dir = "/srv/journal"
	all := func(line string) linuxProbe {
		p := probe(hostLines+line, dir, magicBtrfs)
		p.dirFlags = 0
		p.dirIno = btrfsSubvolumeRootIno + 1
		p.files = []probedFile{{"format", 0}}
		return p
	}

	// The error reported when several rules fail is the one a probe failing
	// only the first of them gets: same rule, same message, and nothing of
	// the later rules.
	sameAs := func(got, want error) {
		t.Helper()
		var g, w *refusal

		if !errors.As(got, &g) || !errors.As(want, &w) {
			t.Fatalf("got %v and %v, want two refusals", got, want)
		}

		if g.rule != w.rule || got.Error() != want.Error() {
			t.Fatalf("got rule %d %q, want rule %d %q", g.rule, got.Error(), w.rule, want.Error())
		}
	}

	only := func(line string) linuxProbe { return probe(hostLines+line, dir, magicBtrfs) }

	err := checkLinuxProbe(all(btrfsNestedNoBarr))
	wantBarrier(t, err, dir, dir, "nobarrier")
	sameAs(err, checkLinuxProbe(only(btrfsNestedNoBarr)))

	err = checkLinuxProbe(all(btrfsNested))
	wantRefusal(t, err, ruleSubvolume, dir, "", "", dir)
	notRoot := only(btrfsNested)
	notRoot.dirIno = btrfsSubvolumeRootIno + 1
	sameAs(err, checkLinuxProbe(notRoot))

	p := all(btrfsNested)
	p.dirIno = btrfsSubvolumeRootIno
	err = checkLinuxProbe(p)
	wantRefusal(t, err, ruleCopyOnWrite, dir, "", "", dir)
	noAttr := only(btrfsNested)
	noAttr.dirFlags = 0
	sameAs(err, checkLinuxProbe(noAttr))

	p.dirFlags = fsNoCOW
	wantRefusal(t, checkLinuxProbe(p), ruleCopyOnWrite, dir, "", "", dir+"/format")
}

// TestCheckRefusalNamesWhat checks that the message an operator reads names
// the directory, the subject of the rule and what to change. The words are
// asserted, not the message's final form.
func TestCheckRefusalNamesWhat(t *testing.T) {
	prof.Watch(t, prof.Options{})

	btrfs := func() linuxProbe { return probe(hostLines+btrfsNested, "/srv/journal", magicBtrfs) }
	noAttr := btrfs()
	noAttr.dirFlags = 0
	notRoot := btrfs()
	notRoot.dirIno = btrfsSubvolumeRootIno + 1
	badFile := btrfs()
	badFile.files = []probedFile{{"format", 0}}

	for _, tc := range []struct {
		name string
		p    linuxProbe
		want []string
	}{
		{"barrier", probe(hostLines+ext4NestedNoBarrier, "/srv/journal/j", magicExt),
			[]string{"/srv/journal/j", "mount /srv/journal", "nobarrier", "remount with barriers"}},
		{"subvolume", notRoot, []string{"/srv/journal", "subvolume root"}},
		{"directory attribute", noAttr, []string{"/srv/journal", "chattr +C"}},
		{"file attribute", badFile, []string{"/srv/journal/format", "chattr +C", "copy it into a new file", "recreate the journal"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkLinuxProbe(tc.p)
			if err == nil {
				t.Fatal("got nil, want a refusal")
			}

			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message %q does not name %q", err.Error(), want)
				}
			}
		})
	}
}

// TestCheckConcurrent runs the verdict from several goroutines at once on
// shared probes, for the race detector: Init and Open of two journals may
// probe together, and a verdict that sorted or edited a probe's files in
// place would race.
func TestCheckConcurrent(t *testing.T) {
	prof.Watch(t, prof.Options{})

	files := []probedFile{
		{"format", fsNoCOW},
		{"00000000000000000002.seg", fsNoCOW},
		{"00000000000000000001.seg", fsNoCOW},
	}
	refused := probe(hostLines+btrfsNestedNoBarr, "/srv/journal", magicBtrfs)
	refused.files = files
	passed := probe(hostLines+btrfsNested, "/srv/journal", magicBtrfs)
	passed.files = files

	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Go(func() {
			if i%2 == 0 {
				errs[i] = checkLinuxProbe(refused)
			} else {
				errs[i] = checkLinuxProbe(passed)
			}
		})
	}
	wg.Wait()

	for i, err := range errs {
		var r *refusal
		if got := errors.As(err, &r); got != (i%2 == 0) {
			t.Errorf("caller %d: got %v, want refused %t", i, err, i%2 == 0)
		}
	}
}
