package fs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/health"
)

// newTestStore creates an FSStore rooted at a fresh tempdir suitable
// for healthcheck assertions. The tempdir is cleaned up via t.Cleanup.
func newTestStore(t *testing.T) *FSStore {
	t.Helper()
	dir := t.TempDir()
	bs, err := New(dir, 0, 0, nil)
	if err != nil {
		t.Fatalf("NewWithDefaults: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs
}

func TestFSStore_Healthcheck_HealthyOnWritableTempDir(t *testing.T) {
	bs := newTestStore(t)
	rep := bs.Healthcheck(context.Background())
	if rep.Status != health.StatusHealthy {
		t.Fatalf("fresh store: got %q (%q), want healthy", rep.Status, rep.Message)
	}
}

func TestFSStore_Healthcheck_UnhealthyAfterClose(t *testing.T) {
	bs := newTestStore(t)
	if err := bs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rep := bs.Healthcheck(context.Background())
	if rep.Status != health.StatusUnhealthy {
		t.Fatalf("closed store: got %q, want unhealthy", rep.Status)
	}
	if !strings.Contains(rep.Message, "closed") {
		t.Fatalf("closed store message: got %q, want 'closed' substring", rep.Message)
	}
}

func TestFSStore_Healthcheck_UnhealthyWhenBaseDirRemoved(t *testing.T) {
	dir := t.TempDir()
	bs, err := New(dir, 0, 0, nil)
	if err != nil {
		t.Fatalf("NewWithDefaults: %v", err)
	}
	defer func() { _ = bs.Close() }()

	// Remove the directory out from under the store. The probe must
	// detect the missing path via os.Stat.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	rep := bs.Healthcheck(context.Background())
	if rep.Status != health.StatusUnhealthy {
		t.Fatalf("removed baseDir: got %q, want unhealthy", rep.Status)
	}
}

func TestFSStore_Healthcheck_UnhealthyWhenBaseDirIsFile(t *testing.T) {
	parent := t.TempDir()
	notADir := filepath.Join(parent, "imafile")
	if err := os.WriteFile(notADir, []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Construct an FSStore manually because NewWithDefaults expects a
	// directory; we want to exercise the "not a directory" branch.
	bs := &FSStore{baseDir: notADir}

	rep := bs.Healthcheck(context.Background())
	if rep.Status != health.StatusUnhealthy {
		t.Fatalf("file-as-baseDir: got %q, want unhealthy", rep.Status)
	}
	if !strings.Contains(rep.Message, "not a directory") {
		t.Fatalf("file-as-baseDir message: got %q, want 'not a directory' substring", rep.Message)
	}
}

func TestFSStore_Healthcheck_RespectsCanceledContext(t *testing.T) {
	bs := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep := bs.Healthcheck(ctx)
	if rep.Status != health.StatusUnknown {
		t.Fatalf("canceled ctx: got %q, want unknown", rep.Status)
	}
}
