package models

import (
	"errors"
	"fmt"
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

// TestSnapshotFailureError covers rebuilding a caller-facing error from a
// terminal row: a recorded kind restores the sentinel identity, and a row
// without one (written before the kind was persisted, or classified from a
// cause matching no sentinel) still yields a non-nil error so a failed
// snapshot can never read as a success.
func TestSnapshotFailureError(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		row  Snapshot
		want error // nil = any non-nil error, matching no sentinel
	}{
		{"verify", Snapshot{Error: "boom", FailureKind: FailureKindVerify}, ErrSnapshotVerifyFailed},
		{"backup", Snapshot{Error: "boom", FailureKind: FailureKindBackup}, ErrSnapshotBackupFailed},
		{"drain timeout", Snapshot{Error: "boom", FailureKind: FailureKindDrainTimeout}, ErrSnapshotDrainTimeout},
		{"kind not recorded", Snapshot{Error: "boom"}, nil},
		{"unknown kind", Snapshot{Error: "boom", FailureKind: "martian"}, nil},
		{"no message either", Snapshot{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SnapshotFailureError(&tc.row)
			if got == nil {
				t.Fatal("SnapshotFailureError = nil, want non-nil for a failed row")
			}
			if tc.want != nil && !errors.Is(got, tc.want) {
				t.Fatalf("SnapshotFailureError = %v, want errors.Is(%v)", got, tc.want)
			}
			if tc.want == nil && SnapshotFailureKind(got) != "" {
				t.Fatalf("SnapshotFailureError = %v, want no sentinel", got)
			}
		})
	}
}

// TestSnapshotFailureKind pins that the kind persisted by the orchestration
// round-trips back to the sentinel the caller matches on.
func TestSnapshotFailureKind(t *testing.T) {
	t.Parallel()

	for _, sentinel := range []error{ErrSnapshotVerifyFailed, ErrSnapshotBackupFailed, ErrSnapshotDrainTimeout} {
		cause := fmt.Errorf("snapshot create abc: step failed: %w: %v", sentinel, errors.New("cause"))
		row := Snapshot{Error: cause.Error(), FailureKind: SnapshotFailureKind(cause)}
		if !errors.Is(SnapshotFailureError(&row), sentinel) {
			t.Fatalf("round-trip lost sentinel %v (kind %q)", sentinel, row.FailureKind)
		}
	}
	if got := SnapshotFailureKind(errors.New("unclassified")); got != "" {
		t.Fatalf("SnapshotFailureKind(unclassified) = %q, want \"\"", got)
	}
	if got := SnapshotFailureKind(nil); got != "" {
		t.Fatalf("SnapshotFailureKind(nil) = %q, want \"\"", got)
	}
}
