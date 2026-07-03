package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestEnumerateOpenFiles_SMB verifies the handler's open-file table feeds the
// block-GC open-handle hold (#1448): every live open with a metadata handle
// is emitted, entries without a handle are skipped, and removing the open
// (close/session teardown) releases the hold.
func TestEnumerateOpenFiles_SMB(t *testing.T) {
	h := &Handler{}
	h.files.Store("fid-1", &OpenFile{MetadataHandle: metadata.FileHandle("mh-1")})
	h.files.Store("fid-2", &OpenFile{MetadataHandle: metadata.FileHandle("mh-2")})
	h.files.Store("fid-3", &OpenFile{}) // no metadata handle — skipped

	collect := func() map[string]int {
		got := make(map[string]int)
		if err := h.EnumerateOpenFiles(context.Background(), func(fh []byte) error {
			got[string(fh)]++
			return nil
		}); err != nil {
			t.Fatalf("EnumerateOpenFiles: %v", err)
		}
		return got
	}

	got := collect()
	if len(got) != 2 || got["mh-1"] != 1 || got["mh-2"] != 1 {
		t.Fatalf("open files = %v, want mh-1 and mh-2 exactly once", got)
	}

	// Close: entry removed from the table → hold released.
	h.files.Delete("fid-1")
	got = collect()
	if len(got) != 1 || got["mh-2"] != 1 {
		t.Fatalf("after close: got %v, want only mh-2", got)
	}

	// Callback errors abort the enumeration (GC fails closed).
	boom := errors.New("boom")
	if err := h.EnumerateOpenFiles(context.Background(), func([]byte) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}
