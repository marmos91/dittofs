package models

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshot_PathHelpers(t *testing.T) {
	tests := []struct {
		name         string
		shareDataDir string
		id           string
		wantDir      string
		wantManifest string
		wantDump     string
	}{
		{
			name:         "absolute share dir",
			shareDataDir: "/var/dittofs/shares/data",
			id:           "01HX0000000000000000000abc",
			wantDir:      filepath.Join("/var/dittofs/shares/data", "snapshots", "01HX0000000000000000000abc"),
			wantManifest: filepath.Join("/var/dittofs/shares/data", "snapshots", "01HX0000000000000000000abc", "manifest.hashes"),
			wantDump:     filepath.Join("/var/dittofs/shares/data", "snapshots", "01HX0000000000000000000abc", "metadata.dump"),
		},
		{
			name:         "trailing slash normalized by filepath.Join",
			shareDataDir: "/tmp/share/",
			id:           "snap-1",
			wantDir:      filepath.Join("/tmp/share/", "snapshots", "snap-1"),
			wantManifest: filepath.Join("/tmp/share/", "snapshots", "snap-1", "manifest.hashes"),
			wantDump:     filepath.Join("/tmp/share/", "snapshots", "snap-1", "metadata.dump"),
		},
		{
			name:         "empty share dir documents filepath.Join behavior",
			shareDataDir: "",
			id:           "snap-2",
			wantDir:      filepath.Join("", "snapshots", "snap-2"),
			wantManifest: filepath.Join("", "snapshots", "snap-2", "manifest.hashes"),
			wantDump:     filepath.Join("", "snapshots", "snap-2", "metadata.dump"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Snapshot{ID: tt.id, ShareName: "data"}

			if got := s.SnapshotDir(tt.shareDataDir); got != tt.wantDir {
				t.Errorf("SnapshotDir(%q) = %q, want %q", tt.shareDataDir, got, tt.wantDir)
			}
			if got := s.ManifestPath(tt.shareDataDir); got != tt.wantManifest {
				t.Errorf("ManifestPath(%q) = %q, want %q", tt.shareDataDir, got, tt.wantManifest)
			}
			if got := s.MetadataDumpPath(tt.shareDataDir); got != tt.wantDump {
				t.Errorf("MetadataDumpPath(%q) = %q, want %q", tt.shareDataDir, got, tt.wantDump)
			}

			// Trailing-slash + double-slash sanity check on the normalized variant.
			if strings.Contains(strings.TrimPrefix(s.SnapshotDir(tt.shareDataDir), "//"), "//") {
				t.Errorf("SnapshotDir produced double slashes: %q", s.SnapshotDir(tt.shareDataDir))
			}
		})
	}
}

func TestSnapshot_StateConstantValues(t *testing.T) {
	if StateCreating != "creating" {
		t.Errorf("StateCreating = %q, want %q", StateCreating, "creating")
	}
	if StateReady != "ready" {
		t.Errorf("StateReady = %q, want %q", StateReady, "ready")
	}
	if StateFailed != "failed" {
		t.Errorf("StateFailed = %q, want %q", StateFailed, "failed")
	}
}

func TestSnapshot_TableName(t *testing.T) {
	if got := (Snapshot{}).TableName(); got != "snapshots" {
		t.Errorf("TableName() = %q, want %q", got, "snapshots")
	}
}

// TestSnapshot_FieldSet is a regression guard: it reads the source of
// snapshot.go and asserts every required field name appears verbatim. Any
// rename, omission, or accidental swap in a future refactor fails this test
// loudly with a deterministic message, in the spirit of similar source-level
// guards elsewhere in the codebase.
func TestSnapshot_FieldSet(t *testing.T) {
	body, err := os.ReadFile("snapshot.go")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	src := string(body)

	required := []string{
		"ID",
		"ShareName",
		"State",
		"MetadataEngine",
		"ManifestCount",
		"RemoteDurable",
		"CreatedAt",
		"UpdatedAt",
	}

	for _, name := range required {
		// Field declarations are followed by whitespace then the Go type.
		needle := name + " "
		if !strings.Contains(src, needle) {
			t.Errorf("snapshot.go missing required field %q", name)
		}
	}
}
