package shares

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The journal a share opens must be usable the moment it is returned: rooted
// under the share's own directory, and accepting writes that read back.
func TestOpenShareJournal_WritesReadBack(t *testing.T) {
	root := t.TempDir()
	store, err := OpenShareJournal("/test-share", &LocalStoreDefaults{JournalRoot: root})
	if err != nil {
		t.Fatalf("OpenShareJournal: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	want := []byte("hello journal")
	if err := store.WriteAt(ctx, "test-payload", 0, want); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	got := make([]byte, len(want))
	if _, _, err := store.ReadAt(ctx, "test-payload", 0, got); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("read back %q, want %q", got, want)
	}

	if _, err := os.Stat(filepath.Join(root, "shares", "test-share")); err != nil {
		t.Errorf("share dir not created under the root: %v", err)
	}
}

// A sub-second commit interval barriers the disk faster than it retires them,
// so a typo is held at the floor rather than honoured.
func TestClampDirtyExpire(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"sub-second is clamped", time.Millisecond, minDirtyExpire},
		{"at the floor passes", minDirtyExpire, minDirtyExpire},
		{"above the floor passes", 30 * time.Second, 30 * time.Second},
		{"unset defers to the journal", 0, 0},
		{"negative disables deliberately", -1, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampDirtyExpire(tc.in); got != tc.want {
				t.Errorf("clampDirtyExpire(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
