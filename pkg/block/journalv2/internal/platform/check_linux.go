package platform

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
)

// mountinfoPath is the calling process's mount table (proc(5),
// /proc/[pid]/mountinfo). It is a variable only so a test can give Check a
// recorded table, as it gives the probe one.
var mountinfoPath = "/proc/self/mountinfo"

// checkDirFlags opens the journal directory for the probe: read only,
// refused if the path is not a directory, not inherited by a child process,
// and refused if its last component is a symbolic link, which a rename could
// have put there after the path was resolved.
const checkDirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW

// checkFileFlags opens a regular file of the directory for its ioctl: read
// only, which FS_IOC_GETFLAGS needs no more than, without following a link,
// and without blocking, in case a FIFO took the file's name after the
// listing, since opening a FIFO for reading waits for a writer (fifo(7)).
const checkFileFlags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC

// direntBufSize is the buffer each getdents call fills.
const direntBufSize = 8192

// The offsets of a getdents64 record's fields, struct linux_dirent64 in the
// kernel's include/linux/dirent.h, as golang.org/x/sys lays out unix.Dirent.
const (
	direntReclen = int(unsafe.Offsetof(unix.Dirent{}.Reclen))
	direntType   = int(unsafe.Offsetof(unix.Dirent{}.Type))
	direntName   = int(unsafe.Offsetof(unix.Dirent{}.Name))
)

// The errors the probe returns about an entry of the directory. Neither is a
// refusal: each fails the check, naming the entry.
var (
	// errEntryLink: the directory holds a symbolic link. The journal opens
	// its files by name, following links, so a link can lead it to data on
	// another filesystem that the probe never checked.
	errEntryLink = errors.New("journal: the directory holds a symbolic link")
	// errEntryTypeUnknown: the listing gives an entry no type the probe
	// knows, so it cannot tell a regular file from a device without opening
	// it.
	errEntryTypeUnknown = errors.New("journal: the directory listing gives an entry no known type")
	// errDirentMalformed: a record getdents returned does not fit in the
	// bytes it returned, or its name is empty or has no NUL.
	errDirentMalformed = errors.New("journal: a directory record does not parse")
)

// Check decides whether dir is suitable for a journal under RFC 28 Appendix
// A on Linux. It returns a refusal naming the rule that fails, nil for a
// suitable directory, and any other error when the directory could not be
// probed, which says nothing about whether it is suitable.
func Check(dir string) error {
	return check(realTable, mountinfoPath, dir)
}

// check is Check through the table c, reading the mount table at mountinfo.
func check(c calls, mountinfo, dir string) error {
	p, err := probeDir(c, mountinfo, dir)
	if err != nil {
		return err
	}

	return checkLinuxProbe(p)
}

// probeDir gathers the verdict's inputs for dir through c, reading the mount
// table at mountinfo. dir is made absolute and its links are resolved first,
// as the verdict requires. The directory is opened once, and every call runs
// on that descriptor, as btrfs-progs runs fstatfs and fstat on one
// (btrfs_util_is_subvolume_fd in libbtrfsutil/subvolume.c), so every answer
// is about one directory. A failed call is returned as an error naming it,
// never as a refusal, and the descriptor is closed whatever happened.
func probeDir(c calls, mountinfo, dir string) (linuxProbe, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return linuxProbe{}, err
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return linuxProbe{}, err
	}

	table, err := os.ReadFile(mountinfo)
	if err != nil {
		return linuxProbe{}, err
	}

	fd, err := openRetrying(c, resolved, checkDirFlags)
	if err != nil {
		return linuxProbe{}, &fs.PathError{
			Op:   "open",
			Path: resolved,
			Err:  err,
		}
	}

	p := linuxProbe{
		dir:       resolved,
		mountinfo: string(table),
	}

	// The close is never repeated, as in SyncDir: Linux releases the
	// descriptor even when close reports EINTR.
	if err := joinClose(probeOpenDir(c, fd, &p), c.close(fd), resolved); err != nil {
		return linuxProbe{}, err
	}

	return p, nil
}

// probeOpenDir fills p from the directory open on fd. The attribute and the
// files are read only on a Btrfs subvolume root, the one place the verdict
// reads them.
//
// An open interrupted by a signal is made again, as SyncDir and os.OpenFile
// make it. Every other call is made once: an interrupted fstatfs, fstat,
// statx, ioctl or getdents fails the probe as an error the caller may retry,
// and no answer is read from it.
func probeOpenDir(c calls, fd int, p *linuxProbe) error {
	var sfs unix.Statfs_t

	if err := c.fstatfs(fd, &sfs); err != nil {
		return &fs.PathError{
			Op:   "fstatfs",
			Path: p.dir,
			Err:  err,
		}
	}

	p.fsType = statfsType(sfs.Type)

	var st unix.Stat_t

	if err := c.fstat(fd, &st); err != nil {
		return &fs.PathError{
			Op:   "fstat",
			Path: p.dir,
			Err:  err,
		}
	}

	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return &fs.PathError{
			Op:   "fstat",
			Path: p.dir,
			Err:  unix.ENOTDIR,
		}
	}

	p.dirIno = st.Ino

	mountID, err := statxMountID(c, fd)
	if err != nil {
		return &fs.PathError{
			Op:   "statx",
			Path: p.dir,
			Err:  err,
		}
	}

	p.mountID = mountID

	if p.fsType != magicBtrfs || p.dirIno != btrfsSubvolumeRootIno {
		return nil
	}

	flags, err := c.ioctlGetUint32(fd, unix.FS_IOC_GETFLAGS)
	if err != nil {
		return &fs.PathError{
			Op:   "ioctl",
			Path: p.dir,
			Err:  err,
		}
	}

	p.dirFlags = flags

	names, err := listRegular(c, fd, p.dir)
	if err != nil {
		return err
	}

	for _, name := range names {
		flags, err := fileFlags(c, joinPath(p.dir, name))
		if err != nil {
			return err
		}

		p.files = append(p.files, probedFile{
			name:  name,
			flags: flags,
		})
	}

	return nil
}

// statxMountID returns the ID of the mount holding the directory open on fd,
// asking statx for STATX_MNT_ID alone (linuxProbe's mountID says why never
// STATX_MNT_ID_UNIQUE). It returns 0, which the verdict reads as unknown,
// where the kernel does not report the ID: a kernel before Linux 5.8 has no
// STATX_MNT_ID (include/uapi/linux/stat.h) and leaves the bit out of
// stx_mask, and a kernel before 4.11 has no statx at all and answers ENOSYS
// (statx(2): "statx() was added to Linux in kernel 4.11"). Any other error
// is returned.
func statxMountID(c calls, fd int) (uint64, error) {
	var stx unix.Statx_t
	err := c.statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &stx)
	if errors.Is(err, unix.ENOSYS) {
		return 0, nil
	}

	if err != nil {
		return 0, err
	}

	if stx.Mask&unix.STATX_MNT_ID == 0 {
		return 0, nil
	}

	return stx.Mnt_id, nil
}

// listRegular returns the names of the regular files in dir, listed with
// getdents on fd, the descriptor dir is open on.
func listRegular(c calls, fd int, dir string) ([]string, error) {
	buf := make([]byte, direntBufSize)
	var names []string

	for {
		n, err := c.getdents(fd, buf)
		if err != nil {
			return nil, &fs.PathError{
				Op:   "getdents",
				Path: dir,
				Err:  err,
			}
		}

		if n == 0 {
			return names, nil
		}

		names, err = appendRegular(names, buf[:n], dir)
		if err != nil {
			return nil, err
		}
	}
}

// appendRegular appends to names the regular files among the getdents64
// records in b, read from dir. Each entry's type is the record's d_type, so
// no entry is opened to learn it: opening a device runs its driver's open,
// and FS_IOC_GETFLAGS on a FIFO, a socket or a device falls through to that
// file's own ioctl, since Btrfs gives those inodes no fileattr_get
// (btrfs_special_inode_operations in fs/btrfs/inode.c), ioctl_getflags then
// answers ENOIOCTLCMD (fs/file_attr.c), and the ioctl system call hands the
// command to vfs_ioctl (fs/ioctl.c). Btrfs fills d_type for every entry, in
// btrfs_real_readdir (fs/btrfs/inode.c) and, for entries not yet in the
// tree, btrfs_readdir_delayed_dir_index (fs/btrfs/delayed-inode.c).
//
// A subdirectory, a FIFO, a socket and a device hold none of the journal's
// data and are skipped. A symbolic link fails with errEntryLink, and an
// entry of any other type, DT_UNKNOWN included, with errEntryTypeUnknown.
func appendRegular(names []string, b []byte, dir string) ([]string, error) {
	for len(b) > 0 {
		if len(b) < direntName {
			return nil, &fs.PathError{
				Op:   "getdents",
				Path: dir,
				Err:  errDirentMalformed,
			}
		}

		reclen := int(binary.NativeEndian.Uint16(b[direntReclen:]))

		if reclen < direntName || reclen > len(b) {
			return nil, &fs.PathError{
				Op:   "getdents",
				Path: dir,
				Err:  errDirentMalformed,
			}
		}

		rec := b[:reclen]
		b = b[reclen:]

		// The kernel writes the name and a NUL after it inside the record,
		// and refuses an empty name (filldir64 and verify_dirent_name in
		// fs/readdir.c), so a record with no NUL, or nothing before it, is
		// malformed.
		name := rec[direntName:]
		end := bytes.IndexByte(name, 0)

		if end <= 0 {
			return nil, &fs.PathError{
				Op:   "getdents",
				Path: dir,
				Err:  errDirentMalformed,
			}
		}

		name = name[:end]

		if string(name) == "." || string(name) == ".." {
			continue
		}

		switch rec[direntType] {
		case unix.DT_REG:
			names = append(names, string(name))
		case unix.DT_DIR, unix.DT_FIFO, unix.DT_SOCK, unix.DT_CHR, unix.DT_BLK:
			// decision: a subdirectory, a FIFO, a socket and a device pass
			// without their attribute being read, which is narrower than RFC
			// 28 Appendix A's every file already in the directory. None
			// holds the journal's data, which it keeps in regular files of
			// its own, and opening a device runs its driver's open. Withdraw
			// the skip if the journal ever keeps data in a subdirectory or in
			// a file that is not a regular one.
		case unix.DT_LNK:
			return nil, fmt.Errorf("%w: %s", errEntryLink, joinPath(dir, string(name)))
		default:
			return nil, fmt.Errorf("%w: %s has type %d", errEntryTypeUnknown, joinPath(dir, string(name)), rec[direntType])
		}
	}

	return names, nil
}

// fileFlags returns FS_IOC_GETFLAGS's answer for the regular file at path,
// opened for the call and closed before returning.
func fileFlags(c calls, path string) (uint32, error) {
	fd, err := openRetrying(c, path, checkFileFlags)
	if err != nil {
		return 0, &fs.PathError{
			Op:   "open",
			Path: path,
			Err:  err,
		}
	}

	flags, err := c.ioctlGetUint32(fd, unix.FS_IOC_GETFLAGS)
	if err != nil {
		err = &fs.PathError{
			Op:   "ioctl",
			Path: path,
			Err:  err,
		}
	}

	if err := joinClose(err, c.close(fd), path); err != nil {
		return 0, err
	}

	return flags, nil
}

// openRetrying opens path with flags through c, again for as long as the
// open is interrupted by a signal, as SyncDir does: repeating an open hides
// no failure.
func openRetrying(c calls, path string, flags int) (int, error) {
	fd, err := c.open(path, flags, 0)
	for errors.Is(err, unix.EINTR) {
		fd, err = c.open(path, flags, 0)
	}

	return fd, err
}

// joinClose returns err, an error from a descriptor of path, with closeErr,
// the error of closing that descriptor: the one that happened as itself, and
// both through errors.Join when both did, as SyncDir does.
func joinClose(err, closeErr error, path string) error {
	if closeErr == nil {
		return err
	}

	closeErr = &fs.PathError{
		Op:   "close",
		Path: path,
		Err:  closeErr,
	}

	if err == nil {
		return closeErr
	}

	return errors.Join(err, closeErr)
}
