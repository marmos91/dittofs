package platform

import (
	"errors"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
)

// OS returns the storage seam over the directory dir on the operating
// system's file interface. It fails with an error matching fs.ErrNotExist if
// dir does not exist, and creates nothing.
//
// Each platform implements the seam with its own durable calls: on Linux,
// fdatasync for a file and fsync of the directory's descriptor; on macOS,
// F_FULLFSYNC for both; on Windows, FlushFileBuffers on the file and on the
// directory opened with backup semantics. Allocation writes every byte of the
// allocated range where the platform's allocation call alone would leave
// extents unwritten.
func OS(dir string) (storage.FS, error) {
	return nil, core.ErrNotImplemented
}

// Report is what Probe found about the filesystem holding a journal's
// directory.
type Report struct {
	// Barriers reports whether a sync reaches the device: false when the
	// filesystem is mounted without cache flushes (nobarrier or barrier=0),
	// or the volume's write-cache buffer flushing is turned off by policy,
	// where the platform reports either.
	Barriers bool

	// CopyOnWrite reports whether the filesystem may allocate anew on a write
	// into bytes already allocated, unless copy-on-write is disabled for the
	// directory, as on Btrfs.
	CopyOnWrite bool

	// NoCoWSet reports whether copy-on-write is disabled for the directory,
	// so the files created in it inherit that. Meaningful only when
	// CopyOnWrite is set.
	NoCoWSet bool

	// BtrfsSubvolumeRoot reports whether the directory is the root of a Btrfs
	// subvolume, which keeps it out of a snapshot of the volume around it.
	// Meaningful only when CopyOnWrite is set.
	BtrfsSubvolumeRoot bool

	// ThinProvisioned reports whether the platform names the device as
	// thin-provisioned, where a write into allocated space can still fail.
	// It is reported as a health condition, never refused.
	ThinProvisioned bool
}

// Probe inspects the filesystem holding dir and reports what it found. A
// property the platform cannot determine is reported as the value Check
// accepts: an undetectable condition is not a refusal.
func Probe(dir string) (Report, error) {
	return Report{}, core.ErrNotImplemented
}

// The refusals Check returns. Init and Open return them unchanged: a journal
// is not created or opened on a filesystem that would void its durability or
// its headroom.
var (
	// ErrNoBarriers is returned for a filesystem on which no sync reaches
	// the device, so a synced record would not survive a power loss.
	ErrNoBarriers = errors.New("journal: filesystem is mounted without cache flushes")

	// ErrCopyOnWrite is returned for a directory on a copy-on-write
	// filesystem that does not have copy-on-write disabled.
	ErrCopyOnWrite = errors.New("journal: copy-on-write is not disabled for the directory")

	// ErrNotSubvolumeRoot is returned for a directory on a copy-on-write
	// filesystem that is not the root of its own subvolume.
	ErrNotSubvolumeRoot = errors.New("journal: directory is not a subvolume root")
)

// Check returns the refusal r calls for, or nil. It returns ErrNoBarriers
// when Barriers is unset; otherwise, when CopyOnWrite is set, ErrCopyOnWrite
// when NoCoWSet is unset and ErrNotSubvolumeRoot when BtrfsSubvolumeRoot is
// unset, checked in that order. ThinProvisioned is never refused.
func Check(r Report) error {
	return core.ErrNotImplemented
}
