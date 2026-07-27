package fs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// stampPath is where the journal keeps its on-disk format marker, relative to a
// store directory.
func stampPath(dir string) string { return filepath.Join(dir, "journal", "format") }

// readStampVersion returns the version recorded in dir's stamp, failing the test
// if the stamp is missing or unreadable.
func readStampVersion(t *testing.T, dir string) int {
	t.Helper()
	raw, err := os.ReadFile(stampPath(dir))
	if err != nil {
		t.Fatalf("read format stamp: %v", err)
	}
	var st struct {
		Version   int    `json:"version"`
		WrittenBy string `json:"written_by"`
		WrittenAt string `json:"written_at"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("parse format stamp %q: %v", raw, err)
	}
	if st.WrittenBy == "" || st.WrittenAt == "" {
		t.Errorf("stamp %q missing operator fields", raw)
	}
	return st.Version
}

// writeStamp plants a stamp at version v, standing in for a directory a
// different release left behind.
func writeStamp(t *testing.T, dir string, v int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(stampPath(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"version":%d,"written_by":"test","written_at":"2026-01-01T00:00:00Z"}`, v)
	if err := os.WriteFile(stampPath(dir), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestOpenRefusesFutureFormat is the regression for a downgrade serving zeros: a
// directory a newer release wrote must not open at all, because this binary
// would read every range the newer format holds elsewhere as a hole.
func TestOpenRefusesFutureFormat(t *testing.T) {
	dir := t.TempDir()
	writeStamp(t, dir, 2)

	s, err := New(dir, 1<<30, nil)
	if err == nil {
		_ = s.Close()
		t.Fatal("New on a future-format directory succeeded, want refusal")
	}
	if !errors.Is(err, block.ErrFutureFormat) {
		t.Fatalf("New error = %v, want it to wrap block.ErrFutureFormat", err)
	}
}

// TestOpenAcceptsCurrentFormat keeps the guard from firing on state this build
// wrote itself.
func TestOpenAcceptsCurrentFormat(t *testing.T) {
	dir := t.TempDir()
	writeStamp(t, dir, 1)

	s, err := New(dir, 1<<30, nil)
	if err != nil {
		t.Fatalf("New on a current-format directory: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := readStampVersion(t, dir); got != 1 {
		t.Fatalf("stamp version after open = %d, want 1", got)
	}
}

// TestOpenStampsUnstampedDir covers the directories in the field today: they
// predate stamping, so they must open, and they must come out stamped so the
// next downgrade is caught.
func TestOpenStampsUnstampedDir(t *testing.T) {
	dir := t.TempDir()

	s, err := New(dir, 1<<30, nil)
	if err != nil {
		t.Fatalf("New on an unstamped directory: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := readStampVersion(t, dir); got != 1 {
		t.Fatalf("stamp version after open = %d, want 1", got)
	}
}
