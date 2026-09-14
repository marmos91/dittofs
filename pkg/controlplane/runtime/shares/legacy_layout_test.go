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

// A pre-journal share must be refused, not adopted. Before #1802 was deleted
// with pkg/block/local/fs, this was the reported #1801 incident: the directory
// opened as an empty journal and served every stored file as zeros.
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

// The case that actually reaches an operator today. Develop has been stamping
// empty journals over legacy directories, so by the time anyone installs a
// fixed build the share already has a journal/ with a header-only segment. If
// that counted as ownership the guard would protect only sites that had never
// started the broken build — that is, almost nobody.
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

// The guard is only worth anything if the open path calls it. This is the end
// to end shape of the #1801 report: a share directory as a pre-journal release
// left it, handed to the function that opens the local store.
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
