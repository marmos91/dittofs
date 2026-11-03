package platform

import (
	"errors"
	"io"
	"io/fs"
	"math"

	"golang.org/x/sys/unix"
)

// zeroChunk is the most one zero write covers.
const zeroChunk = 1 << 20

// zeros is the one buffer every zero write reads from. Nothing writes to it,
// so concurrent Allocate calls share it, and a write allocates nothing.
var zeros [zeroChunk]byte

// Allocate is fallocate in its default mode over the range, then zeros
// written over every byte of it (RFC 28 Appendix A: "on Linux, `fallocate`
// then writing zeros over the whole segment"). Where the filesystem has no
// fallocate for the file, the zero writes alone allocate the range. It does
// not sync: the caller's Sync makes the allocation durable, as it does a
// WriteAt. Zeros are written over the whole range, so bytes a write had put
// there read as zeros afterwards. A range no file can hold fails with EINVAL,
// and an empty range does nothing, both before any system call. The
// descriptor is reached through the file's own reference, so a concurrent
// Close cannot release it during the calls. On a closed file a valid,
// non-empty range fails with fs.ErrClosed before any system call.
//
// decision: zero-filled on Linux because fallocate alone leaves unwritten
// extents on ext4 and XFS (RFC 28 Appendix A): the first write into each
// converts it in a filesystem-journal transaction, so every fdatasync after
// new appends would also force a filesystem-journal commit, and RFC 1 §5.2's
// one flush per Sync would not hold. Written zeros make an append an
// overwrite of allocated, written blocks. Withdraw the fill if J1's
// Sync-per-write row, run with and without it, shows unwritten-extent
// conversions cost less than its writes, as the RFC says.
func (f *file) Allocate(off, n int64) error {
	if off < 0 || n < 0 || off > math.MaxInt64-n {
		return &fs.PathError{
			Op:   "fallocate",
			Path: f.f.Name(),
			Err:  unix.EINVAL,
		}
	}

	if n == 0 {
		return nil
	}

	rc, err := f.f.SyscallConn()
	if err != nil {
		return &fs.PathError{
			Op:   "fallocate",
			Path: f.f.Name(),
			Err:  err,
		}
	}

	// As in Sync, Control fails only on a closed file.
	var op string
	var aerr error

	if err := rc.Control(func(fd uintptr) {
		op, aerr = f.allocate(int(fd), off, n)
	}); err != nil {
		return &fs.PathError{
			Op:   "fallocate",
			Path: f.f.Name(),
			Err:  fs.ErrClosed,
		}
	}

	if aerr != nil {
		return &fs.PathError{
			Op:   op,
			Path: f.f.Name(),
			Err:  aerr,
		}
	}

	return nil
}

// allocate makes Allocate's calls on fd, and returns the name of the call
// that failed and its error, if one did. A call interrupted by a signal is
// made again, as the os package does for its own writes: both calls give the
// same result when repeated over the same range, so a repeat hides no
// failure. A fallocate refused with EOPNOTSUPP is followed by the zero writes
// alone. Every other error is returned after one call, and no call follows
// it.
func (f *file) allocate(fd int, off, n int64) (string, error) {
	err := f.calls.fallocate(fd, unix.FALLOC_FL_ALLOCATE_RANGE, off, n)
	for errors.Is(err, unix.EINTR) {
		err = f.calls.fallocate(fd, unix.FALLOC_FL_ALLOCATE_RANGE, off, n)
	}

	// decision: a filesystem with no fallocate for the file (the ext2
	// driver, or the ext4 driver on a file without extents, as an ext3
	// volume makes) is allocated by the zero writes alone, which RFC 28 §2.2
	// allows ("with the platform's allocation call or by writing zeros").
	// Both share ext4's magic, and an ext3 volume can be mounted as ext4, so
	// the directory check cannot refuse them. There the written zeros
	// allocate every block. That is all the fallback guarantees: the
	// directory check passes a filesystem RFC 28 Appendix A does not list,
	// and on ZFS with compression on, its default (ZIO_COMPRESS_DEFAULT in
	// include/sys/zio.h, zfs 2.3.0), neither call allocates. OpenZFS's
	// fallocate without flags only checks the free space and sets the size,
	// reserving no blocks, or returns EOPNOTSUPP when
	// zfs_fallocate_reserve_percent is 0 (zpl_fallocate_common in
	// module/os/linux/zfs/zpl_file.c, zfs 2.3.0), and with compression on a
	// block of zeros is written as a hole (zio_write_compress in
	// module/zfs/zio.c, zfs 2.3.0). A write into a segment allocated there
	// can fail with ENOSPC, and the headroom of RFC 1 §6 does not hold; no
	// data is lost, since the error is returned. On XFS, fallocate is what
	// rules out speculative allocation past the end (RFC 28 Appendix A), so
	// withdraw this fallback if XFS is ever seen returning EOPNOTSUPP.
	if err != nil && !errors.Is(err, unix.EOPNOTSUPP) {
		return "fallocate", err
	}

	end := off + n

	for off < end {
		w, err := f.calls.pwrite(fd, zeros[:min(end-off, zeroChunk)], off)
		if errors.Is(err, unix.EINTR) {
			continue
		}

		if err != nil {
			return "pwrite", err
		}

		// A write that writes nothing and reports no error would repeat for
		// ever; the os package reports it the same way.
		if w <= 0 {
			return "pwrite", io.ErrUnexpectedEOF
		}

		off += int64(w)
	}

	return "", nil
}
