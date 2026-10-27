//go:build unix

package journalv2test

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAllocatedCountsBlocksNotSize(t *testing.T) {
	dir := t.TempDir()

	full := make([]byte, 64<<10)
	for i := range full {
		full[i] = 1
	}
	if err := os.WriteFile(filepath.Join(dir, "full"), full, 0o644); err != nil {
		t.Fatal(err)
	}
	sparse, err := os.Create(filepath.Join(dir, "sparse"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sparse.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	if err := sparse.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := allocated(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got < int64(len(full)) {
		t.Fatalf("allocated = %d with %d bytes written, want at least that", got, len(full))
	}
	if got >= 64<<20 {
		t.Fatalf("allocated = %d with a 64 MiB file of no written bytes: it counted size, not blocks", got)
	}
}

func TestAllocatedLeavesTheFormatFileOut(t *testing.T) {
	dir := t.TempDir()
	block := make([]byte, 64<<10)
	for i := range block {
		block[i] = 1
	}
	for _, name := range []string{"1.seg", "format", "format.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), block, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	seg, err := allocated(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "1.seg")); err != nil {
		t.Fatal(err)
	}
	rest, err := allocated(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rest != 0 || seg < int64(len(block)) {
		t.Fatalf("allocated = %d with a segment and the format files, %d with the format files alone: want the segment counted and the format files not", seg, rest)
	}
}
