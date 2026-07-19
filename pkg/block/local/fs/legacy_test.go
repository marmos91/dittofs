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
