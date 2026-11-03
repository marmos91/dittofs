//go:build linux || darwin

package segment_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/format"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/platform"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/segment"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// osCap is the cap of the real-storage set: large enough that the filesystem
// reports whole blocks for it, small enough that three of them cost nothing.
const osCap = int64(1 << 20)

// allocatedBytes returns the bytes the filesystem holds blocks for in the
// file at path, from the block count stat reports — the standard library's
// view of the same fact the platform's own tests read through x/sys, which
// this package may not import.
func allocatedBytes(t *testing.T, path string) int64 {
	t.Helper()

	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Blocks * 512
}

// checkWholeOnDisk fails t unless the file path is exactly cap bytes, all
// zeros, fully allocated — the whole segment a Make promises on real
// storage, where a page cache cannot hide a file that is not.
func checkWholeOnDisk(t *testing.T, path string) {
	t.Helper()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if int64(len(got)) != osCap {
		t.Fatalf("%s holds %d bytes, want the whole cap %d", path, len(got), osCap)
	}
	for i, b := range got {
		if b != 0 {
			t.Fatalf("%s holds %#x at %d, want the zeros a fresh spare is made of", path, b, i)
		}
	}
	if ab := allocatedBytes(t, path); ab < osCap {
		t.Fatalf("%s holds %d allocated bytes, want at least the whole cap %d", path, ab, osCap)
	}
}

// TestMakeOnRealStorage checks the whole spare the platform's calls make on
// the filesystem this test runs on, through the Maker rather than the
// platform's own tests: after each of three Makes the segment file is at the
// cap, reads as zeros end to end, is fully allocated, and the sum of the
// segment files' allocated bytes never exceeds what the ledger accounts
// (R75: the accounting never trails the filesystem in use).
func TestMakeOnRealStorage(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	o := platform.NewOS(dir)
	set := segment.NewSet(osCap, nil)
	m := segment.NewMaker(o, set, 3*osCap, 0, nil)

	for want := range 3 {
		id, err := m.Make()
		if err != nil {
			t.Fatalf("Make %d: %v", want+1, err)
		}
		if id != core.SegmentID(want+1) {
			t.Fatalf("Make %d returned id %d, want %d", want+1, id, want+1)
		}

		path := filepath.Join(dir, format.SegmentName(id))
		checkWholeOnDisk(t, path)

		c := set.Counters()
		if c.UsedBytes != int64(want+1)*osCap {
			t.Fatalf("after %d Makes the footprint is %d, want %d",
				want+1, c.UsedBytes, int64(want+1)*osCap)
		}

		// The ledger never accounts less than the segment files hold.
		var sum int64
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read the directory: %v", err)
		}
		for _, e := range entries {
			if kind, _ := format.ClassifyName(e.Name()); kind == format.NameSegment {
				sum += allocatedBytes(t, filepath.Join(dir, e.Name()))
			}
		}
		if sum > c.UsedBytes {
			t.Fatalf("the segment files hold %d allocated bytes, more than the %d accounted", sum, c.UsedBytes)
		}
	}
}
