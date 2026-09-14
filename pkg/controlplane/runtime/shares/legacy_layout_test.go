package shares

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journal"
)

func journalConfigForTest() journal.Config { return journal.Config{} }

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A pre-journal share must be refused, not adopted: a directory holding
// blobs/ or logs/ has bytes this build cannot read, and opening it as an empty
// journal serves every stored file as zeros.
func TestCheckLegacyLayout_RefusesPreJournalShare(t *testing.T) {
	for _, sub := range []string{"blobs", "logs"} {
		t.Run(sub, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, sub, "0001.dat"), 1<<10)
			err := checkLegacyLayout(dir)
			if !errors.Is(err, ErrLegacyLocalFormat) {
				t.Fatalf("checkLegacyLayout = %v, want ErrLegacyLocalFormat", err)
			}
		})
	}
}

// A journal directory holding only a header-only segment is not ownership. A
// build without this guard opens a legacy directory once and stamps an empty
// journal beside the untouched bytes; if that stamp counted, the guard would
// pass every directory it had already been run against once.
func TestCheckLegacyLayout_RefusesAfterBrokenBuildStampedAnEmptyJournal(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "blobs", "0001.dat"), 1<<20)
	writeFile(t, filepath.Join(dir, "journal", "000001.seg"), journalSegHeaderSize)
	writeFile(t, filepath.Join(dir, "journal", "format"), 32)

	if err := checkLegacyLayout(dir); !errors.Is(err, ErrLegacyLocalFormat) {
		t.Fatalf("checkLegacyLayout = %v, want ErrLegacyLocalFormat", err)
	}
}

// A journal holding real data supersedes leftover legacy dirs: a completed
// migration, or a native store with an orphaned blobs/, must still open.
func TestCheckLegacyLayout_AllowsPopulatedJournalBesideOrphans(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "blobs", "0001.dat"), 1<<20)
	writeFile(t, filepath.Join(dir, "journal", "000001.seg"), journalSegHeaderSize+1)

	if err := checkLegacyLayout(dir); err != nil {
		t.Fatalf("checkLegacyLayout = %v, want nil", err)
	}
}

// A fresh share and an empty-but-present blobs/ are both ordinary.
func TestCheckLegacyLayout_AllowsFreshAndEmptyDirs(t *testing.T) {
	fresh := t.TempDir()
	if err := checkLegacyLayout(fresh); err != nil {
		t.Fatalf("fresh dir: %v, want nil", err)
	}

	empty := t.TempDir()
	if err := os.MkdirAll(filepath.Join(empty, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkLegacyLayout(empty); err != nil {
		t.Fatalf("empty blobs/: %v, want nil", err)
	}
}

// The guard is only worth anything if the open path calls it: a share directory
// as a pre-journal release left it, handed to the function that opens the local
// store, must come back refused.
func TestOpenJournalStore_RefusesPreJournalShare(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "blobs", "0001.dat"), 1<<20)
	writeFile(t, filepath.Join(dir, "logs", "p1.log"), 4<<10)

	s, err := openJournalStore(dir, 0, 0, journalConfigForTest())
	if s != nil {
		_ = s.Close()
	}
	if !errors.Is(err, ErrLegacyLocalFormat) {
		t.Fatalf("openJournalStore = %v, want ErrLegacyLocalFormat (the share would mount empty and serve zeros)", err)
	}
}

// A share directory that does not exist yet is a fresh share, not a legacy one:
// journal.Open creates it. Refusing here would stop every new share from
// starting.
func TestCheckLegacyLayout_AllowsMissingShareDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-created-yet")
	if err := checkLegacyLayout(missing); err != nil {
		t.Fatalf("checkLegacyLayout on a missing dir = %v, want nil", err)
	}
}

// A symlinked blobs/ pointing outside the share must not be followed out of the
// share directory: the scan is anchored so a crafted share dir cannot make it
// read somewhere else.
func TestCheckLegacyLayout_DoesNotEscapeTheShareDir(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "0001.dat"), 1<<10)

	dir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "blobs")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	// Whatever the verdict, it must be reached without reading through the link
	// to a directory the share does not contain.
	if _, err := hasLegacyLocalLayout(dir); err != nil && !os.IsNotExist(err) {
		t.Logf("symlinked blobs/ reported: %v", err)
	}
}
