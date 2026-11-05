package platform

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
	"golang.org/x/sys/unix"
)

// dirMountID is the mount ID mountTable gives the directory's mount.
const dirMountID = 90

// The offsets of struct linux_dirent64 (include/linux/dirent.h): d_ino at 0,
// d_off at 8, d_reclen at 16, d_type at 18 and d_name from 19. The fake
// reads records with these, written from the kernel's header rather than
// taken from the code under test.
const (
	recReclen = 16
	recType   = 18
	recName   = 19
)

// probeCall is one entry of a fakeProbe's log.
type probeCall struct {
	// op is open, close, getdents, fstatfs, fstat, statx or ioctl.
	op string
	// fd is the descriptor acted on, or open's result; -1 for a failed open.
	fd int
	// at is, for an open, the descriptor of the directory its name was
	// opened through, or AT_FDCWD for an open by path.
	at int
	// path is the path open was given, the directory's path joined with
	// the name for openat, or for every other call the path the descriptor
	// was opened with.
	path string
	// flags is open's flags or statx's.
	flags int
	// name and mask are statx's path and mask.
	name string
	mask int
	// req is the ioctl's request.
	req uint
}

func (c probeCall) String() string {
	switch c.op {
	case "open":
		return fmt.Sprintf("open(%s, %#x) at %d = %d", c.path, c.flags, c.at, c.fd)
	case "statx":
		return fmt.Sprintf("statx(%d, %q, %#x, %#x) on %s", c.fd, c.name, c.flags, c.mask, c.path)
	case "ioctl":
		return fmt.Sprintf("ioctl(%d, %#x) on %s", c.fd, c.req, c.path)
	default:
		return fmt.Sprintf("%s(%d) on %s", c.op, c.fd, c.path)
	}
}

// fakeProbe is a table for the probe. Its open, openat, close and getdents run on
// the real directory the test built, so the entries listed are real and a
// descriptor left open is one the profiler sees. Its fstatfs, fstat, statx
// and ioctl make no call and answer what the test recorded, so no verdict
// depends on the host's mounts. An error set for an op and a path is returned
// once by that call instead of making it; close still closes then, as Linux
// releases the descriptor even when close reports an error.
type fakeProbe struct {
	// dir is the journal directory, whose ioctl answers dirFlags.
	dir string

	mu   sync.Mutex
	log  []probeCall
	fds  map[int]string
	fail map[string]error

	// onePerCall makes getdents return one record per call, whatever the
	// buffer, so a listing takes as many calls as the directory has
	// entries. Otherwise getdents fills the buffer as the kernel does.
	onePerCall bool
	// pending is, per descriptor, the records onePerCall has still to
	// return.
	pending map[int][][]byte
	// types replaces the d_type of the entries it names.
	types map[string]uint8
	// listed is every name getdents returned, "." and ".." aside, in order.
	listed []string

	// fsType is fstatfs's answer, as the int32 a 32-bit kernel's f_type
	// arrives as.
	fsType int32
	// ino is fstat's answer.
	ino uint64
	// stxMask, stxIno and mntID are statx's answer.
	stxMask uint32
	stxIno  uint64
	mntID   uint64
	// dirFlags is the ioctl's answer on dir; fileFlags its answer on a file
	// of dir by name, FS_NOCOW_FL for a name not in it.
	dirFlags  uint32
	fileFlags map[string]uint32
}

// newFake returns a fake whose answers describe dir as a Btrfs subvolume
// root with chattr +C set, on the mount mountTable writes, so a case fails
// only for what it changes.
func newFake(dir string) *fakeProbe {
	return &fakeProbe{
		dir:      dir,
		fds:      map[int]string{},
		pending:  map[int][][]byte{},
		fsType:   asInt32(magicBtrfs),
		ino:      btrfsSubvolumeRootIno,
		stxMask:  unix.STATX_BASIC_STATS | unix.STATX_MNT_ID,
		stxIno:   btrfsSubvolumeRootIno + 1000,
		mntID:    dirMountID,
		dirFlags: fsNoCOW,
	}
}

// asInt32 returns the 32 bits of v as an int32, as a 32-bit kernel's f_type
// arrives in golang.org/x/sys.
func asInt32(v uint32) int32 {
	return int32(v)
}

// setStatfsType stores v in a Statfs_t.Type of whichever type the
// architecture declares, widening it with its sign as Go converts.
func setStatfsType[T int32 | int64 | uint32](p *T, v int32) {
	*p = T(v)
}

func (f *fakeProbe) table() calls {
	return calls{
		open:           f.open,
		openat:         f.openat,
		close:          f.close,
		getdents:       f.getdents,
		fstatfs:        f.fstatfs,
		fstat:          f.fstat,
		statx:          f.statx,
		ioctlGetUint32: f.ioctl,
	}
}

// failOn makes the next call of op on path return err.
func (f *fakeProbe) failOn(op, path string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fail == nil {
		f.fail = map[string]error{}
	}

	f.fail[op+" "+path] = err
}

// failure returns, and forgets, the error set for op on path.
func (f *fakeProbe) failure(op, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := op + " " + path
	err := f.fail[key]
	delete(f.fail, key)

	return err
}

func (f *fakeProbe) add(c probeCall) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.log = append(f.log, c)
}

// pathOf returns the path the descriptor fd was opened with.
func (f *fakeProbe) pathOf(fd int) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.fds[fd]
}

// calls returns the logged calls of op, in order.
func (f *fakeProbe) calls(op string) []probeCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	var got []probeCall
	for _, c := range f.log {
		if c.op == op {
			got = append(got, c)
		}
	}

	return got
}

// callsOn returns how many calls of op were made on path.
func (f *fakeProbe) callsOn(op, path string) int {
	var n int
	for _, c := range f.calls(op) {
		if c.path == path {
			n++
		}
	}

	return n
}

// listedNames returns every name getdents returned, in order.
func (f *fakeProbe) listedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.listed)
}

func (f *fakeProbe) open(path string, mode int, perm uint32) (int, error) {
	return f.openLogged(unix.AT_FDCWD, path, path, mode, perm)
}

// openat opens name through the directory open on dirfd, as the kernel
// does, and logs and fails it as an open of the joined path, so a case
// reads a file's open by its path whichever call made it.
func (f *fakeProbe) openat(dirfd int, name string, mode int, perm uint32) (int, error) {
	return f.openLogged(dirfd, filepath.Join(f.pathOf(dirfd), name), name, mode, perm)
}

// openLogged opens name through at, logging and failing it as an open of
// path.
func (f *fakeProbe) openLogged(at int, path, name string, mode int, perm uint32) (int, error) {
	if err := f.failure("open", path); err != nil {
		f.add(probeCall{
			op:    "open",
			fd:    -1,
			at:    at,
			path:  path,
			flags: mode,
		})

		return -1, err
	}

	fd, err := unix.Openat(at, name, mode, perm)
	if err != nil {
		fd = -1
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if err == nil {
		f.fds[fd] = path
	}

	f.log = append(f.log, probeCall{
		op:    "open",
		fd:    fd,
		at:    at,
		path:  path,
		flags: mode,
	})

	return fd, err
}

func (f *fakeProbe) close(fd int) error {
	path := f.pathOf(fd)
	injected := f.failure("close", path)

	// The descriptor is forgotten before the real close releases it, so a
	// concurrent open that is given the same number is not forgotten too.
	f.mu.Lock()
	delete(f.fds, fd)
	delete(f.pending, fd)
	f.log = append(f.log, probeCall{
		op:   "close",
		fd:   fd,
		path: path,
	})
	f.mu.Unlock()

	err := unix.Close(fd)

	if injected != nil {
		return injected
	}

	return err
}

func (f *fakeProbe) getdents(fd int, buf []byte) (int, error) {
	path := f.pathOf(fd)
	f.add(probeCall{
		op:   "getdents",
		fd:   fd,
		path: path,
	})

	if err := f.failure("getdents", path); err != nil {
		return 0, err
	}

	if !f.onePerCall {
		n, err := unix.Getdents(fd, buf)
		if err != nil {
			return n, err
		}

		f.retype(buf[:n])

		return n, nil
	}

	rec, err := f.nextRecord(fd)
	if err != nil {
		return 0, err
	}

	// getdents64 fails with EINVAL when the buffer cannot hold the next
	// record (getdents(2)).
	if len(rec) > len(buf) {
		return 0, unix.EINVAL
	}

	n := copy(buf, rec)
	f.retype(buf[:n])

	return n, nil
}

// nextRecord returns the next record of the directory open on fd, or none
// at its end. The first call reads the whole directory.
func (f *fakeProbe) nextRecord(fd int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	recs, ok := f.pending[fd]
	if !ok {
		buf := make([]byte, 1<<16)
		for {
			n, err := unix.Getdents(fd, buf)
			if err != nil {
				return nil, err
			}

			if n == 0 {
				break
			}

			for off := 0; off < n; {
				reclen := int(binary.NativeEndian.Uint16(buf[off+recReclen:]))
				recs = append(recs, slices.Clone(buf[off:off+reclen]))
				off += reclen
			}
		}
	}

	if len(recs) == 0 {
		f.pending[fd] = nil

		return nil, nil
	}

	f.pending[fd] = recs[1:]

	return recs[0], nil
}

// retype replaces the d_type of the records in b that types names, and
// notes every name in listed.
func (f *fakeProbe) retype(b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for off := 0; off < len(b); {
		reclen := int(binary.NativeEndian.Uint16(b[off+recReclen:]))
		name := b[off+recName : off+reclen]

		if i := slices.Index(name, 0); i >= 0 {
			name = name[:i]
		}

		if t, ok := f.types[string(name)]; ok {
			b[off+recType] = t
		}

		if string(name) != "." && string(name) != ".." {
			f.listed = append(f.listed, string(name))
		}

		off += reclen
	}
}

func (f *fakeProbe) fstatfs(fd int, buf *unix.Statfs_t) error {
	path := f.pathOf(fd)
	f.add(probeCall{
		op:   "fstatfs",
		fd:   fd,
		path: path,
	})

	if err := f.failure("fstatfs", path); err != nil {
		return err
	}

	*buf = unix.Statfs_t{}
	setStatfsType(&buf.Type, f.fsType)

	return nil
}

func (f *fakeProbe) fstat(fd int, st *unix.Stat_t) error {
	path := f.pathOf(fd)
	f.add(probeCall{
		op:   "fstat",
		fd:   fd,
		path: path,
	})

	if err := f.failure("fstat", path); err != nil {
		return err
	}

	*st = unix.Stat_t{}
	st.Ino = f.ino

	return nil
}

func (f *fakeProbe) statx(dirfd int, name string, flags int, mask int, stx *unix.Statx_t) error {
	path := f.pathOf(dirfd)
	f.add(probeCall{
		op:    "statx",
		fd:    dirfd,
		path:  path,
		flags: flags,
		name:  name,
		mask:  mask,
	})

	if err := f.failure("statx", path); err != nil {
		return err
	}

	*stx = unix.Statx_t{
		Mask:   f.stxMask,
		Ino:    f.stxIno,
		Mnt_id: f.mntID,
	}

	return nil
}

func (f *fakeProbe) ioctl(fd int, req uint) (uint32, error) {
	path := f.pathOf(fd)
	f.add(probeCall{
		op:   "ioctl",
		fd:   fd,
		path: path,
		req:  req,
	})

	if err := f.failure("ioctl", path); err != nil {
		return 0, err
	}

	if path == f.dir {
		return f.dirFlags, nil
	}

	flags, ok := f.fileFlags[filepath.Base(path)]
	if !ok {
		return fsNoCOW, nil
	}

	return flags, nil
}

// realDir returns a new empty directory, its path with its links resolved
// as the probe reports it.
func realDir(t *testing.T) string {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	return dir
}

// mountTable writes a mount table holding an ext4 root and a mount of
// fsType at dir with ID dirMountID, in the layout of show_mountinfo
// (fs/proc_namespace.c), and returns its path. It is written outside dir,
// which the probe lists.
func mountTable(t *testing.T, dir, fsType string) string {
	t.Helper()

	return mountTableWith(t, dir, fsType, "rw")
}

// mountTableWith is mountTable with the superblock options options on the
// directory's mount.
func mountTableWith(t *testing.T, dir, fsType, options string) string {
	t.Helper()

	if strings.ContainsAny(dir, " \t\n\\") {
		t.Fatalf("%q would need escaping in a mount table", dir)
	}

	text := "1 1 259:3 / / rw,noatime - ext4 /dev/nvme0n1p3 rw,errors=remount-ro\n" +
		fmt.Sprintf("%d 1 0:45 / %s rw,relatime - %s /dev/sdb1 %s\n", dirMountID, dir, fsType, options)
	path := filepath.Join(t.TempDir(), "mountinfo")

	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// writeFiles creates empty files with names in dir.
func writeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()

	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// wantAllClosed fails t unless every descriptor f's open returned was closed
// once, after it was opened, and nothing else was closed.
func wantAllClosed(t *testing.T, f *fakeProbe) {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	open := map[int]string{}
	for _, c := range f.log {
		switch {
		case c.op == "open" && c.fd >= 0:
			open[c.fd] = c.path
		case c.op == "close":
			if _, ok := open[c.fd]; !ok {
				t.Errorf("%v: the descriptor was not open", c)
			}

			delete(open, c.fd)
		}
	}

	for fd, path := range open {
		t.Errorf("descriptor %d of %s left open", fd, path)
	}
}

// wantIOError fails t unless err is an I/O error of op on path caused by
// cause, and no refusal.
func wantIOError(t *testing.T, err error, op, path string, cause error) {
	t.Helper()

	var r *refusal

	if errors.As(err, &r) {
		t.Fatalf("got refusal %v, want an I/O error of %s on %s", err, op, path)
	}

	var pe *fs.PathError

	if !errors.As(err, &pe) {
		t.Fatalf("got %v, want an I/O error of %s on %s", err, op, path)
	}

	if pe.Op != op || pe.Path != path {
		t.Fatalf("got %s on %s (%v), want %s on %s", pe.Op, pe.Path, err, op, path)
	}

	if !errors.Is(err, cause) {
		t.Fatalf("got %v, want it caused by %v", err, cause)
	}
}

// wantFailure fails t unless err is a failure of the check caused by cause,
// neither nil nor a refusal.
func wantFailure(t *testing.T, err, cause error) {
	t.Helper()

	var r *refusal

	if errors.As(err, &r) {
		t.Fatalf("got refusal %v, want a failure caused by %v", err, cause)
	}

	if !errors.Is(err, cause) {
		t.Fatalf("got %v, want a failure caused by %v", err, cause)
	}
}

// TestCheckProbeReadsItsCalls fails a probe that fills a field from the
// wrong call or asks a call the wrong question: the type from anything but
// fstatfs, the inode from statx rather than fstat, the attribute from another
// file's ioctl, the mount ID from statx asked for STATX_MNT_ID_UNIQUE, whose
// IDs never match the mount table (vfs_statx_path in fs/stat.c), or a
// directory opened twice, which could be two directories. The directory is
// opened read only as a directory, without following a link a rename put
// there after the path was resolved; each regular file is opened read only,
// without following a link, and without blocking, in case a FIFO took its
// name after the listing, since opening a FIFO for reading waits for a
// writer (fifo(7)). The ioctl's answer is the unsigned int ioctl_getflags
// writes (fs/file_attr.c).
func TestCheckProbeReadsItsCalls(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := realDir(t)
	writeFiles(t, dir, "format", "00000000000000000001.seg")
	mountinfo := mountTable(t, dir, "btrfs")

	f := newFake(dir)
	f.dirFlags = fsNoCOW | 0x10
	f.fileFlags = map[string]uint32{
		"format":                   fsNoCOW | 0x20,
		"00000000000000000001.seg": fsNoCOW | 0x4000,
	}

	p, err := probeDir(f.table(), mountinfo, dir)
	if err != nil {
		t.Fatal(err)
	}

	text, err := os.ReadFile(mountinfo)
	if err != nil {
		t.Fatal(err)
	}

	if p.dir != dir || p.mountinfo != string(text) {
		t.Errorf("got dir %q and mount table %q, want %q and %q", p.dir, p.mountinfo, dir, text)
	}

	if p.fsType != magicBtrfs || p.dirIno != btrfsSubvolumeRootIno || p.dirFlags != fsNoCOW|0x10 || p.mountID != dirMountID {
		t.Errorf("got type %#x, inode %d, flags %#x, mount ID %d; want %#x, %d, %#x, %d",
			p.fsType, p.dirIno, p.dirFlags, p.mountID, magicBtrfs, btrfsSubvolumeRootIno, fsNoCOW|0x10, dirMountID)
	}

	files := slices.Clone(p.files)
	slices.SortFunc(files, func(a, b probedFile) int {
		return strings.Compare(a.name, b.name)
	})

	want := []probedFile{
		{
			name:  "00000000000000000001.seg",
			flags: fsNoCOW | 0x4000,
		},
		{
			name:  "format",
			flags: fsNoCOW | 0x20,
		},
	}

	if !slices.Equal(files, want) {
		t.Errorf("got files %v, want %v", files, want)
	}

	opens := f.calls("open")
	var dirOpens []probeCall
	for _, c := range opens {
		if c.path == dir {
			dirOpens = append(dirOpens, c)
		}
	}

	if len(dirOpens) != 1 {
		t.Fatalf("got opens %v, want the directory opened once", opens)
	}

	dirFD := dirOpens[0].fd

	if want := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW; dirOpens[0].flags != want {
		t.Errorf("%v: want flags %#x", dirOpens[0], want)
	}

	for _, op := range []string{"fstatfs", "fstat", "statx"} {
		got := f.calls(op)

		if len(got) != 1 || got[0].fd != dirFD {
			t.Errorf("got %v, want one %s on the directory's descriptor %d", got, op, dirFD)
		}
	}

	stx := f.calls("statx")

	if len(stx) == 1 && (stx[0].name != "" || stx[0].flags&unix.AT_EMPTY_PATH == 0 || stx[0].mask != unix.STATX_MNT_ID) {
		t.Errorf("%v: want statx of the descriptor itself, with AT_EMPTY_PATH, asking STATX_MNT_ID alone (%#x)", stx[0], unix.STATX_MNT_ID)
	}

	ioctls := f.calls("ioctl")

	if len(ioctls) != 3 {
		t.Fatalf("got %v, want one ioctl on the directory and one on each file", ioctls)
	}

	for _, c := range ioctls {
		if c.req != unix.FS_IOC_GETFLAGS {
			t.Errorf("%v: want request FS_IOC_GETFLAGS (%#x)", c, uint(unix.FS_IOC_GETFLAGS))
		}
	}

	for _, c := range opens {
		if c.path == dir {
			continue
		}

		if want := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC; c.flags != want {
			t.Errorf("%v: want flags %#x", c, want)
		}

		if c.at != dirFD {
			t.Errorf("%v: want the file opened through the directory's descriptor %d", c, dirFD)
		}
	}

	wantAllClosed(t, f)
}

// TestCheckFilesOpenedThroughTheDirectory fails a probe that opens a file
// of the directory by its full path rather than through the descriptor the
// directory was listed on. The path is walked again, so a parent renamed
// between the listing and the opens makes the probe read another
// directory's files, or none. Here the parent is renamed away once the
// listing ends: opened through the descriptor, each file is still the one
// listed and the directory is accepted; opened by path, it is not found.
func TestCheckFilesOpenedThroughTheDirectory(t *testing.T) {
	prof.Watch(t, prof.Options{})

	base := realDir(t)
	parent := filepath.Join(base, "p")
	dir := filepath.Join(parent, "j")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	writeFiles(t, dir, "format", "00000000000000000001.seg")
	mountinfo := mountTable(t, dir, "btrfs")

	f := newFake(dir)
	table := f.table()
	renamed := false
	table.getdents = func(fd int, buf []byte) (int, error) {
		n, err := f.getdents(fd, buf)
		if n == 0 && err == nil && !renamed {
			renamed = true

			if err := os.Rename(parent, filepath.Join(base, "gone")); err != nil {
				t.Error(err)
			}
		}

		return n, err
	}

	if err := check(table, mountinfo, dir); err != nil {
		t.Errorf("parent renamed after the listing: got %v, want the directory accepted", err)
	}

	if got := f.calls("ioctl"); len(got) != 3 {
		t.Errorf("got %v, want one ioctl on the directory and one on each file", got)
	}

	wantAllClosed(t, f)
}

// TestCheckNoIoctlOffBtrfs fails a probe that asks for the attribute
// anywhere but on a Btrfs subvolume root, the one place the verdict reads it:
// the ioctl fails on some filesystems the verdict passes, and on the empty
// directory a snapshot leaves for a nested subvolume, so asking elsewhere
// would fail a directory the verdict accepts or refuses for another rule.
func TestCheckNoIoctlOffBtrfs(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, c := range []struct {
		name   string
		magic  uint32
		fsType string
		ino    uint64
		rule   refusalRule
	}{
		{
			name:   "ext4",
			magic:  magicExt,
			fsType: "ext4",
			ino:    btrfsSubvolumeRootIno,
		},
		{
			name:   "xfs",
			magic:  magicXFS,
			fsType: "xfs",
			ino:    btrfsSubvolumeRootIno,
		},
		{
			name:   "tmpfs",
			magic:  tmpfsMagic,
			fsType: "tmpfs",
			ino:    btrfsSubvolumeRootIno,
		},
		{
			name:   "btrfs, not a subvolume root",
			magic:  magicBtrfs,
			fsType: "btrfs",
			ino:    btrfsSubvolumeRootIno + 1,
			rule:   ruleSubvolume,
		},
	} {
		dir := realDir(t)
		writeFiles(t, dir, "format")

		f := newFake(dir)
		f.fsType = asInt32(c.magic)
		f.ino = c.ino
		f.dirFlags = 0
		f.fileFlags = map[string]uint32{
			"format": 0,
		}

		err := check(f.table(), mountTable(t, dir, c.fsType), dir)

		if c.rule == 0 {
			if err != nil {
				t.Errorf("%s: got %v, want the directory accepted", c.name, err)
			}
		} else {
			var r *refusal

			if !errors.As(err, &r) || r.rule != c.rule {
				t.Errorf("%s: got %v, want refused by rule %d", c.name, err, c.rule)
			}
		}

		for _, op := range []string{"ioctl", "getdents"} {
			if got := f.calls(op); len(got) != 0 {
				t.Errorf("%s: got %v, want no %s", c.name, got, op)
			}
		}

		if got := f.calls("open"); len(got) != 1 {
			t.Errorf("%s: got %v, want the directory opened alone", c.name, got)
		}

		wantAllClosed(t, f)
	}
}

// TestCheckEveryFileGetsItsIoctl fails a probe that skips a file of the
// directory: the first or the last entry getdents returns, an entry that
// arrives alone in a call, or an entry past the first of a buffer. RFC 28
// Appendix A: chattr +C "does not apply to a file that already holds data",
// so each file is asked on its own. The listing runs once with getdents
// returning one record per call, so it takes several calls whatever the
// probe's buffer, and once filling the buffer as the kernel does.
func TestCheckEveryFileGetsItsIoctl(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := realDir(t)
	mountinfo := mountTable(t, dir, "btrfs")
	names := []string{
		"format",
		"00000000000000000001.seg",
		"00000000000000000002.seg",
		"00000000000000000003.seg",
		"00000000000000000004.spare.tmp",
		// NAME_MAX (include/uapi/linux/limits.h), a record of 280 bytes.
		strings.Repeat("n", 255),
	}
	writeFiles(t, dir, names...)

	for _, onePerCall := range []bool{true, false} {
		f := newFake(dir)
		f.onePerCall = onePerCall

		if err := check(f.table(), mountinfo, dir); err != nil {
			t.Fatalf("one record per call %t: got %v, want the directory accepted", onePerCall, err)
		}

		// The records are the names, "." and "..", and the end is one more
		// call that returns nothing.
		if got := f.calls("getdents"); onePerCall && len(got) != len(names)+3 {
			t.Fatalf("got %d getdents calls, want one for each of the %d records and one for the end", len(got), len(names)+2)
		}

		listed := f.listedNames()

		if len(listed) != len(names) {
			t.Fatalf("one record per call %t: got listing %v, want %d names", onePerCall, listed, len(names))
		}

		for _, name := range names {
			if n := f.callsOn("ioctl", filepath.Join(dir, name)); n != 1 {
				t.Errorf("one record per call %t: file %s asked %d times, want once", onePerCall, name, n)
			}
		}

		wantAllClosed(t, f)

		for _, name := range []string{listed[0], listed[len(listed)-1]} {
			f := newFake(dir)
			f.onePerCall = onePerCall
			f.fileFlags = map[string]uint32{
				name: 0,
			}

			err := check(f.table(), mountinfo, dir)
			wantRefusal(t, err, ruleCopyOnWrite, dir, "", "", filepath.Join(dir, name))
			wantAllClosed(t, f)
		}
	}
}

// TestCheckSkipsEntriesThatHoldNoData fails a probe that opens an entry of
// the directory that is not a regular file. Opening a device runs its
// driver's open, and FS_IOC_GETFLAGS on a FIFO, a socket or a device falls
// through to that file's own ioctl: Btrfs gives those inodes no fileattr_get
// (btrfs_special_inode_operations in fs/btrfs/inode.c), so ioctl_getflags
// answers ENOIOCTLCMD (fs/file_attr.c) and the ioctl system call then passes
// the command to vfs_ioctl (fs/ioctl.c). None of them holds the journal's
// data, and neither does a subdirectory, which the journal never makes. Each
// is told apart by the listing's d_type and skipped. Every entry here has the
// attribute off, so one that is asked is refused as well as seen opened.
func TestCheckSkipsEntriesThatHoldNoData(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := realDir(t)
	mountinfo := mountTable(t, dir, "btrfs")
	writeFiles(t, dir, "char", "block", "socket")

	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}

	skipped := []string{"char", "block", "socket", "fifo", "subdir"}

	for _, onePerCall := range []bool{true, false} {
		f := newFake(dir)
		f.onePerCall = onePerCall
		f.types = map[string]uint8{
			"char":   unix.DT_CHR,
			"block":  unix.DT_BLK,
			"socket": unix.DT_SOCK,
		}
		f.fileFlags = map[string]uint32{}
		for _, name := range skipped {
			f.fileFlags[name] = 0
		}

		if err := check(f.table(), mountinfo, dir); err != nil {
			t.Errorf("one record per call %t: got %v, want the directory accepted", onePerCall, err)
		}

		for _, name := range skipped {
			if n := f.callsOn("open", filepath.Join(dir, name)); n != 0 {
				t.Errorf("one record per call %t: %s opened %d times, want never", onePerCall, name, n)
			}
		}

		wantAllClosed(t, f)
	}
}

// TestCheckFailsOnLinkOrUnknownEntry fails a probe that skips a symbolic
// link or an entry whose type the listing does not give. A link can point at
// data on another filesystem that was never checked, and the journal opens
// its files by name, following links; an entry of unknown type may be a
// regular file. Neither is a refusal: the check fails naming the entry, and
// the entry is never opened.
func TestCheckFailsOnLinkOrUnknownEntry(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, c := range []struct {
		name  string
		cause error
		// unknown gives the entry d_type DT_UNKNOWN, as a filesystem that
		// does not fill it does (getdents(2)).
		unknown bool
	}{
		{
			name:  "symbolic link",
			cause: errEntryLink,
		},
		{
			name:    "unknown type",
			cause:   errEntryTypeUnknown,
			unknown: true,
		},
	} {
		dir := realDir(t)
		elsewhere := realDir(t)
		writeFiles(t, elsewhere, "target")
		writeFiles(t, dir, "format")
		entry := filepath.Join(dir, "00000000000000000001.seg")

		if c.unknown {
			writeFiles(t, dir, filepath.Base(entry))
		} else if err := os.Symlink(filepath.Join(elsewhere, "target"), entry); err != nil {
			t.Fatal(err)
		}

		f := newFake(dir)

		if c.unknown {
			f.types = map[string]uint8{
				filepath.Base(entry): unix.DT_UNKNOWN,
			}
		}

		err := check(f.table(), mountTable(t, dir, "btrfs"), dir)
		wantFailure(t, err, c.cause)

		if err != nil && !strings.Contains(err.Error(), entry) {
			t.Errorf("%s: got %v, want it to name %s", c.name, err, entry)
		}

		if n := f.callsOn("open", entry); n != 0 {
			t.Errorf("%s: %s opened %d times, want never", c.name, entry, n)
		}

		wantAllClosed(t, f)
	}
}

// TestCheckMountIDNotReported fails a probe that reads stx_mnt_id without
// checking stx_mask: a kernel before Linux 5.8 has no STATX_MNT_ID
// (include/uapi/linux/stat.h) and does not return the bit, and the field
// then holds nothing the kernel wrote. The verdict reads an ID of 0 as
// unknown; any other would be checked against the mount table and refused.
// It also fails a probe that reads the ID on Btrfs only.
func TestCheckMountIDNotReported(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := realDir(t)
	mountinfo := mountTable(t, dir, "btrfs")

	f := newFake(dir)
	f.stxMask = unix.STATX_BASIC_STATS
	f.mntID = 4242

	p, err := probeDir(f.table(), mountinfo, dir)
	if err != nil {
		t.Fatal(err)
	}

	if p.mountID != 0 {
		t.Errorf("got mount ID %d, want 0 where stx_mask lacks STATX_MNT_ID", p.mountID)
	}

	if err := check(f.table(), mountinfo, dir); err != nil {
		t.Errorf("got %v, want the directory accepted with its mount ID unknown", err)
	}

	f = newFake(dir)
	f.mntID = 4242

	err = check(f.table(), mountinfo, dir)
	wantError(t, err, errMountUnresolved)
	wantAllClosed(t, f)

	// The ID is read on every filesystem, not only where the attribute is:
	// on ext4 it is what catches a directory whose mount is not the one the
	// table shows at its path. 1 is the table's root.
	f = newFake(dir)
	f.fsType = asInt32(magicExt)
	f.mntID = 1

	err = check(f.table(), mountTable(t, dir, "ext4"), dir)
	wantError(t, err, errMountIDDisagrees)
	wantAllClosed(t, f)
}

// TestCheckStatxMissing fails a probe that fails the check on a kernel
// without statx, or that reads any other statx failure as an unknown mount
// ID. statx(2): "statx() was added to Linux in kernel 4.11", and an older
// kernel answers ENOSYS. The mount ID is then unknown, as on a kernel from
// 4.11 to 5.7 that does not report it; any other error is a failed call.
func TestCheckStatxMissing(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := realDir(t)
	mountinfo := mountTable(t, dir, "btrfs")

	f := newFake(dir)
	f.mntID = 4242
	f.failOn("statx", dir, unix.ENOSYS)

	p, err := probeDir(f.table(), mountinfo, dir)
	if err != nil {
		t.Fatalf("got %v, want statx's ENOSYS read as an unknown mount ID", err)
	}

	if p.mountID != 0 {
		t.Errorf("got mount ID %d, want 0 where statx is missing", p.mountID)
	}

	wantAllClosed(t, f)

	f = newFake(dir)
	f.failOn("statx", dir, unix.EPERM)

	err = check(f.table(), mountinfo, dir)
	wantIOError(t, err, "statx", dir, unix.EPERM)
	wantAllClosed(t, f)
}

// TestCheckNegativeStatfsType fails a probe that widens statfs's type with
// its sign and compares it as more than 32 bits. golang.org/x/sys declares
// Statfs_t.Type as an int32 on 386, arm and mips, where Btrfs's magic arrives
// negative; read so, it names no listed filesystem, and a Btrfs directory
// without chattr +C would not be refused for it.
func TestCheckNegativeStatfsType(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := realDir(t)
	f := newFake(dir)
	f.dirFlags = 0

	if f.fsType >= 0 {
		t.Fatalf("fstatfs answers %d, want Btrfs's magic as a negative int32", f.fsType)
	}

	err := check(f.table(), mountTable(t, dir, "btrfs"), dir)
	wantRefusal(t, err, ruleCopyOnWrite, dir, "", "", dir)
	wantAllClosed(t, f)
}

// TestCheckFailingCall fails a probe that reads a failed system call as a
// refusal, or as an answer, or that leaves a descriptor open on the way out.
// A failed call says nothing about the directory: it is returned as an I/O
// error naming the call and the path, and every descriptor opened before it
// is closed. The profiler watches each case rather than this test: a watch
// over the whole test and one over a case would overlap, which the profiler
// refuses, and this test itself opens nothing.
func TestCheckFailingCall(t *testing.T) {
	file := "format"

	for _, c := range []struct {
		name string
		op   string
		// file is set when the call fails on file rather than on the
		// directory.
		file bool
	}{
		{
			name: "open the directory",
			op:   "open",
		},
		{
			name: "fstatfs",
			op:   "fstatfs",
		},
		{
			name: "fstat",
			op:   "fstat",
		},
		{
			name: "statx",
			op:   "statx",
		},
		{
			name: "ioctl on the directory",
			op:   "ioctl",
		},
		{
			name: "getdents",
			op:   "getdents",
		},
		{
			name: "close the directory",
			op:   "close",
		},
		{
			name: "open a file",
			op:   "open",
			file: true,
		},
		{
			name: "ioctl on a file",
			op:   "ioctl",
			file: true,
		},
		{
			name: "close a file",
			op:   "close",
			file: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			dir := realDir(t)
			writeFiles(t, dir, file, "00000000000000000001.seg")
			path := dir

			if c.file {
				path = filepath.Join(dir, file)
			}

			f := newFake(dir)
			f.failOn(c.op, path, unix.EIO)

			err := check(f.table(), mountTable(t, dir, "btrfs"), dir)
			wantIOError(t, err, c.op, path, unix.EIO)
			wantAllClosed(t, f)
		})
	}

	t.Run("mount table missing", func(t *testing.T) {
		prof.Watch(t, prof.Options{})

		dir := realDir(t)
		mountinfo := filepath.Join(t.TempDir(), "mountinfo")
		f := newFake(dir)

		err := check(f.table(), mountinfo, dir)
		wantIOError(t, err, "open", mountinfo, fs.ErrNotExist)
		wantAllClosed(t, f)
	})

	t.Run("fstatfs and the close both fail", func(t *testing.T) {
		prof.Watch(t, prof.Options{})

		dir := realDir(t)
		f := newFake(dir)
		f.failOn("fstatfs", dir, unix.EIO)
		f.failOn("close", dir, unix.EBADF)

		err := check(f.table(), mountTable(t, dir, "btrfs"), dir)

		var r *refusal

		if errors.As(err, &r) || !errors.Is(err, unix.EIO) || !errors.Is(err, unix.EBADF) {
			t.Errorf("got %v, want both the fstatfs and the close error, and no refusal", err)
		}

		wantAllClosed(t, f)
	})
}

// TestCheckOpenInterrupted fails a probe that returns an open interrupted
// by a signal as a failure: the open is made again, as SyncDir and
// os.OpenFile make it, since repeating an open hides no failure. Two signals
// come in a row, as SyncDir's test sends, so a single retry fails too.
func TestCheckOpenInterrupted(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := realDir(t)
	writeFiles(t, dir, "format")
	mountinfo := mountTable(t, dir, "btrfs")

	for _, path := range []string{dir, filepath.Join(dir, "format")} {
		f := newFake(dir)
		table := f.table()
		interrupted := 0
		table.open = func(p string, mode int, perm uint32) (int, error) {
			if p == path && interrupted < 2 {
				interrupted++
				return -1, unix.EINTR
			}

			return f.open(p, mode, perm)
		}
		table.openat = func(dirfd int, name string, mode int, perm uint32) (int, error) {
			if filepath.Join(f.pathOf(dirfd), name) == path && interrupted < 2 {
				interrupted++
				return -1, unix.EINTR
			}

			return f.openat(dirfd, name, mode, perm)
		}

		if err := check(table, mountinfo, dir); err != nil {
			t.Errorf("open of %s interrupted twice: got %v, want the directory accepted", path, err)
		}

		if n := f.callsOn("open", path); interrupted != 2 || n != 1 {
			t.Errorf("open of %s: %d interrupted, then %d made, want 2 and 1", path, interrupted, n)
		}

		wantAllClosed(t, f)
	}
}

// TestCheckInterruptedCallsNotRepeated fails a probe that makes a call
// other than open again after a signal interrupted it. SyncDir's rule is
// the one written for this package: an open is made again, and a close never
// is, since "Linux releases the descriptor even when close reports EINTR, so a
// second close could close another file's". Nothing is written for the
// probe's reads of the directory, so each is made once, and an interrupted
// one fails the check as an I/O error, which the caller may retry. The
// profiler watches each case rather than this test, for the reason
// TestCheckFailingCall gives.
func TestCheckInterruptedCallsNotRepeated(t *testing.T) {
	file := "format"

	for _, c := range []struct {
		op string
		// file is set when the call is interrupted on file rather than on
		// the directory.
		file bool
	}{
		{
			op: "fstatfs",
		},
		{
			op: "fstat",
		},
		{
			op: "statx",
		},
		{
			op: "ioctl",
		},
		{
			op: "getdents",
		},
		{
			op: "close",
		},
		{
			op:   "ioctl",
			file: true,
		},
		{
			op:   "close",
			file: true,
		},
	} {
		t.Run(fmt.Sprintf("%s on file %t", c.op, c.file), func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			dir := realDir(t)
			writeFiles(t, dir, file)
			path := dir

			if c.file {
				path = filepath.Join(dir, file)
			}

			f := newFake(dir)
			f.failOn(c.op, path, unix.EINTR)

			err := check(f.table(), mountTable(t, dir, "btrfs"), dir)
			wantIOError(t, err, c.op, path, unix.EINTR)

			if n := f.callsOn(c.op, path); n != 1 {
				t.Errorf("got %d calls of %s on %s, want 1", n, c.op, path)
			}

			wantAllClosed(t, f)
		})
	}
}

// TestCheckResolvesLinks fails a probe that hands the verdict the path it
// was given rather than the directory it names: the verdict walks the path as
// text through the mount table, so a link, a relative path or a trailing
// slash would be refused as not clean, or matched against the mount of the
// link rather than of its target.
func TestCheckResolvesLinks(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := realDir(t)
	mountinfo := mountTable(t, dir, "btrfs")
	links := realDir(t)
	link := filepath.Join(links, "journal")

	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}

	t.Chdir(links)

	// A relative path that crosses no link, so only Abs makes it absolute.
	rel, err := filepath.Rel(links, dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, given := range []string{link, link + "/", "journal", "./journal/", rel} {
		f := newFake(dir)
		f.dirFlags = 0

		err := check(f.table(), mountinfo, given)
		wantRefusal(t, err, ruleCopyOnWrite, dir, "", "", dir)
		opens := f.calls("open")

		if len(opens) != 1 || opens[0].path != dir {
			t.Errorf("given %s: got opens %v, want %s opened", given, opens, dir)
		}

		wantAllClosed(t, f)
	}
}

// TestCheckConcurrentCallers runs the probe from several goroutines at once
// on one directory, for the race detector: Init and Open of two journals may
// probe together, and a probe that shared a buffer or a descriptor between
// callers would race or mix their answers.
func TestCheckConcurrentCallers(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := realDir(t)
	writeFiles(t, dir, "format", "00000000000000000001.seg", "00000000000000000002.seg")
	mountinfo := mountTable(t, dir, "btrfs")

	// Each caller has its own fake, so no lock of a shared fake orders the
	// callers for the race detector, and each fake copies one record per
	// getdents call into the probe's buffer, a write the detector sees; it
	// does not see the kernel's.
	fakes := make([]*fakeProbe, 16)
	for i := range fakes {
		fakes[i] = newFake(dir)
		fakes[i].onePerCall = true
	}

	var wg sync.WaitGroup
	errs := make([]error, len(fakes))
	for i, f := range fakes {
		wg.Go(func() {
			errs[i] = check(f.table(), mountinfo, dir)
		})
	}

	wg.Wait()

	for i, f := range fakes {
		if errs[i] != nil {
			t.Errorf("caller %d: got %v, want the directory accepted", i, errs[i])
		}

		if got := f.calls("ioctl"); len(got) != 4 {
			t.Errorf("caller %d: got %d ioctls, want four", i, len(got))
		}

		wantAllClosed(t, f)
	}
}

// TestCheckEntryPoint fails a Check that does not run the probe on the path
// it is given: one that returns nil, or that reads a path it cannot open as an
// accepted directory. A missing path, a broken link and a regular file each
// fail the check, and none is a refusal.
func TestCheckEntryPoint(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := realDir(t)
	missing := filepath.Join(dir, "missing")
	broken := filepath.Join(dir, "broken")
	file := filepath.Join(dir, "file")
	writeFiles(t, dir, "file")

	if err := os.Symlink(missing, broken); err != nil {
		t.Fatal(err)
	}

	wantFailure(t, Check(missing), fs.ErrNotExist)
	wantFailure(t, Check(broken), fs.ErrNotExist)
	wantIOError(t, Check(file), "open", file, unix.ENOTDIR)
}

// TestCheckReadsTheProcessMountTable fails a Check that reads another mount
// table than the calling process's mountinfo (proc(5), /proc/[pid]/mountinfo).
// /proc/self/mounts, for one, has another layout, so every directory would
// fail with a table that does not parse. On a real temporary directory, Check
// must resolve the directory's mount in the real table: it returns nil or a
// refusal, never one of the errors that say the table does not describe the
// directory, nor a failed call. On an ext4, XFS or tmpfs host the probe stops
// after statx, so the listing and the ioctl, which run only on a Btrfs
// subvolume root, do not run here; it is TestSyscallTableIsReal that holds
// the getdents and ioctl entries to their golang.org/x/sys functions.
func TestCheckReadsTheProcessMountTable(t *testing.T) {
	prof.Watch(t, prof.Options{})

	if mountinfoPath != "/proc/self/mountinfo" {
		t.Errorf("Check reads %s, want /proc/self/mountinfo", mountinfoPath)
	}

	dir := realDir(t)
	writeFiles(t, dir, "format")
	err := Check(dir)

	var r *refusal

	if err != nil && !errors.As(err, &r) {
		t.Errorf("got %v, want the directory accepted or refused by a rule", err)
	}
}

// TestCheckEntryPointRunsTheVerdict fails a Check that probes the directory
// and returns without the verdict: it would accept a mount without cache
// flushes, which RFC 28 Appendix A says Open MUST refuse. Check is given a
// recorded nobarrier ext4 mount and recorded statfs answers through the same
// table and mount table path it reads in production, swapped for the test.
func TestCheckEntryPointRunsTheVerdict(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := realDir(t)
	f := newFake(dir)
	f.fsType = asInt32(magicExt)

	table := realTable
	path := mountinfoPath
	realTable = f.table()
	mountinfoPath = mountTableWith(t, dir, "ext4", "rw,nobarrier")
	t.Cleanup(func() {
		realTable = table
		mountinfoPath = path
	})

	wantBarrier(t, Check(dir), dir, dir, "nobarrier")
	wantAllClosed(t, f)
}

// TestAppendRegularMalformed fails a getdents record parser that trusts a
// record's length or name: a d_reclen of zero would never move past the
// record, one past the buffer or inside the header would read outside the
// record, and a name with no NUL in its record or an empty one is no name the
// kernel writes (filldir64 and verify_dirent_name in fs/readdir.c). Each such
// buffer fails as a getdents error, never a panic, and the parser returns:
// the test fails a parser that has not returned in a bound far above what
// any of these buffers, at most a few records, takes.
func TestAppendRegularMalformed(t *testing.T) {
	prof.Watch(t, prof.Options{})

	valid := direntRecord(24, unix.DT_REG, "a")
	cut := append(slices.Clone(valid), direntRecord(24, unix.DT_REG, "b")[:20]...)

	for _, c := range []struct {
		name string
		buf  []byte
	}{
		{
			name: "reclen zero",
			buf:  direntRecord(0, unix.DT_REG, "a")[:24],
		},
		{
			name: "reclen past the buffer",
			buf:  direntRecord(4096, unix.DT_REG, "a")[:24],
		},
		{
			name: "reclen inside the header",
			buf:  direntRecord(18, unix.DT_REG, "a")[:24],
		},
		{
			name: "buffer cut in the header",
			buf:  valid[:10],
		},
		{
			name: "buffer cut in a record",
			buf:  cut,
		},
		{
			name: "name without NUL in its record",
			buf:  direntRecord(24, unix.DT_REG, "abcde"),
		},
		{
			name: "empty name",
			buf:  direntRecord(24, unix.DT_REG, ""),
		},
	} {
		names, err := appendRegularBounded(t, c.buf)

		var pe *fs.PathError

		if !errors.As(err, &pe) || pe.Op != "getdents" || pe.Path != "/j" || !errors.Is(err, errDirentMalformed) {
			t.Errorf("%s: got names %q and %v, want a getdents error on /j for a malformed record", c.name, names, err)
		}
	}

	names, err := appendRegularBounded(t, valid)
	if err != nil || !slices.Equal(names, []string{"a"}) {
		t.Errorf("a valid record: got names %q and %v, want [a]", names, err)
	}

	// filldir64 (fs/readdir.c) writes the name and one NUL, and leaves the
	// record's padding as the buffer held it, so bytes after the NUL are
	// not the name's.
	names, err = appendRegularBounded(t, direntRecord(32, unix.DT_REG, "a\x00\xff\xff\xff"))
	if err != nil || !slices.Equal(names, []string{"a"}) {
		t.Errorf("a record with bytes after the NUL: got names %q and %v, want [a]", names, err)
	}
}

// direntRecord returns a getdents64 record of len max(reclen, 24) bytes, laid
// out as struct linux_dirent64 (include/linux/dirent.h), whose d_reclen says
// reclen, with inode 1, type typ and the bytes of name from d_name on, and
// zeros after them.
func direntRecord(reclen uint16, typ uint8, name string) []byte {
	b := make([]byte, max(int(reclen), 24))
	binary.NativeEndian.PutUint64(b, 1)
	binary.NativeEndian.PutUint16(b[recReclen:], reclen)
	b[recType] = typ
	copy(b[recName:], name)

	return b
}

// appendRegularBounded runs appendRegular on buf, from the directory /j, and
// returns its answer. A panic fails t; so does a call that has not returned
// after a bound only a parser that never moves past a record reaches.
func appendRegularBounded(t *testing.T, buf []byte) ([]string, error) {
	t.Helper()

	type answer struct {
		names    []string
		err      error
		panicked any
	}

	done := make(chan answer, 1)

	go func() {
		var a answer

		defer func() {
			a.panicked = recover()
			done <- a
		}()

		a.names, a.err = appendRegular(nil, slices.Clone(buf[:len(buf):len(buf)]), "/j")
	}()

	select {
	case a := <-done:
		if a.panicked != nil {
			t.Errorf("appendRegular panicked on %x: %v", buf, a.panicked)
		}

		return a.names, a.err
	case <-time.After(10 * time.Second):
		t.Fatalf("appendRegular has not returned on %x: it does not move past a record", buf)
	}

	return nil, nil
}
