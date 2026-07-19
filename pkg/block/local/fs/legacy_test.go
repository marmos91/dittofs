package fs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeFile creates parent dirs and writes n bytes at path.
func writeFile(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHasLegacyLocalLayout(t *testing.T) {
	tests := []struct {
		name  string
		setup func(dir string)
		want  bool
	}{
		{
			name:  "empty dir is not legacy",
			setup: func(string) {},
			want:  false,
		},
		{
			name: "populated blobs is legacy",
			setup: func(dir string) {
				writeFile(t, filepath.Join(dir, "blobs", "0000000000000000.blob"), 1024)
			},
			want: true,
		},
		{
			name: "populated logs is legacy",
			setup: func(dir string) {
				writeFile(t, filepath.Join(dir, "logs", "share", "f.log"), 512)
			},
			want: true,
		},
		{
			name: "legacy dirs plus only header-only segments still legacy",
			setup: func(dir string) {
				writeFile(t, filepath.Join(dir, "blobs", "0000000000000000.blob"), 1024)
				// A develop binary that started once over legacy data inits empty
				// header-only segments; those are not data, so the guard must fire.
				writeFile(t, filepath.Join(dir, "journal", "0000000000000000.seg"), legacySegHeaderSize)
			},
			want: true,
		},
		{
			name: "journal segment with data supersedes orphan blobs",
			setup: func(dir string) {
				writeFile(t, filepath.Join(dir, "blobs", "0000000000000000.blob"), 1024)
				writeFile(t, filepath.Join(dir, "journal", "0000000000000000.seg"), legacySegHeaderSize+1)
			},
			want: false,
		},
		{
			name: "empty blobs dir is not legacy",
			setup: func(dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(dir)
			got, err := hasLegacyLocalLayout(dir)
			if err != nil {
				t.Fatalf("hasLegacyLocalLayout: %v", err)
			}
			if got != tc.want {
				t.Fatalf("hasLegacyLocalLayout = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNewWithOptionsRefusesLegacy is the guardrail's whole point: opening a
// pre-journal directory must fail with ErrLegacyLocalFormat rather than start an
// empty journal that serves the stored files as zeros.
func TestNewWithOptionsRefusesLegacy(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "blobs", "0000000000000000.blob"), 1024)
	writeFile(t, filepath.Join(dir, "logs", "share", "f.log"), 512)

	_, err := NewWithOptions(dir, 1<<30, nil, FSStoreOptions{})
	if !errors.Is(err, ErrLegacyLocalFormat) {
		t.Fatalf("NewWithOptions over legacy dir: got %v, want ErrLegacyLocalFormat", err)
	}
}

// TestNewWithOptionsOpensCleanDir confirms the guard does not false-positive on a
// fresh directory: a store with no legacy layout opens normally.
func TestNewWithOptionsOpensCleanDir(t *testing.T) {
	s, err := NewWithOptions(t.TempDir(), 1<<30, nil, FSStoreOptions{})
	if err != nil {
		t.Fatalf("NewWithOptions on clean dir: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
}

// TestNewWithOptionsMigratesLegacy covers the remote-backed upgrade path: with
// MigrateLegacyLayout set, a pre-journal dir opens instead of failing, the legacy
// blobs/+logs/ are archived aside (non-destructively) so the journal owns the dir,
// and the store reports MigratedFromLegacy. A second open is a normal clean open —
// the guard no longer fires because the legacy dirs are gone.
func TestNewWithOptionsMigratesLegacy(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "blobs", "0000000000000000.blob"), 1024)
	writeFile(t, filepath.Join(dir, "logs", "share", "f.log"), 512)

	s, err := NewWithOptions(dir, 1<<30, nil, FSStoreOptions{MigrateLegacyLayout: true})
	if err != nil {
		t.Fatalf("NewWithOptions with MigrateLegacyLayout over legacy dir: %v", err)
	}
	if !s.MigratedFromLegacy() {
		t.Fatal("MigratedFromLegacy = false, want true after archiving a legacy dir")
	}

	// Legacy dirs archived aside (non-destructive), originals gone.
	for _, sub := range []string{"blobs", "logs"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy %s/ still present after migration (err=%v)", sub, err)
		}
		if _, err := os.Stat(filepath.Join(dir, sub+legacyBackupSuffix)); err != nil {
			t.Fatalf("archived %s%s missing: %v", sub, legacyBackupSuffix, err)
		}
	}
	_ = s.Close()

	// Second open: dir is now clean (no legacy layout), so a plain open succeeds
	// and reports no migration — the archive is idempotent across restarts.
	s2, err := NewWithOptions(dir, 1<<30, nil, FSStoreOptions{MigrateLegacyLayout: true})
	if err != nil {
		t.Fatalf("second NewWithOptions after migration: %v", err)
	}
	if s2.MigratedFromLegacy() {
		t.Fatal("MigratedFromLegacy = true on a second open; archive is not idempotent")
	}
	_ = s2.Close()
}
