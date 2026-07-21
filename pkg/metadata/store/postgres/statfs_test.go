package postgres

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestStatfsQuery_ScopedVsStoreWide asserts the share-scoping rule lives in one
// place: a handle that decodes to a share name yields the share-scoped query
// (share name + regular-file-type predicate). The aggregate counts only regular
// files so the share root directory does not inflate UsedFiles.
func TestStatfsQuery_ScopedVsStoreWide(t *testing.T) {
	handle, err := encodeFileHandle("/myshare", uuid.New().String())
	if err != nil {
		t.Fatalf("encodeFileHandle: %v", err)
	}

	sql, args := statfsQuery(handle)
	if !strings.Contains(sql, "share_name = $1") {
		t.Errorf("scoped query missing share predicate: %q", sql)
	}
	if !strings.Contains(sql, "file_type = $2") {
		t.Errorf("scoped query missing regular-file predicate: %q", sql)
	}
	if len(args) != 2 {
		t.Fatalf("scoped query args = %v, want share name + file type", args)
	}
	if got, ok := args[0].(string); !ok || got != "/myshare" {
		t.Errorf("scoped query arg[0] = %v, want \"/myshare\"", args[0])
	}
	if got, ok := args[1].(int); !ok || got != int(metadata.FileTypeRegular) {
		t.Errorf("scoped query arg[1] = %v, want FileTypeRegular", args[1])
	}
}

// TestStatfsQuery_InvalidHandleFallsBackStoreWide ensures an undecodable handle
// does not error or scope to a garbage share — it falls back to the store-wide
// aggregate (single-share compatible). The aggregate still restricts to regular
// files (matching the store-wide usedBytes counter) so directories are excluded.
func TestStatfsQuery_InvalidHandleFallsBackStoreWide(t *testing.T) {
	sql, args := statfsQuery([]byte{0x01, 0x02, 0x03}) // not a valid encoded handle
	if strings.Contains(sql, "share_name") {
		t.Errorf("invalid handle produced a share-scoped query: %q", sql)
	}
	if !strings.Contains(sql, "file_type = $1") {
		t.Errorf("store-wide query missing regular-file predicate: %q", sql)
	}
	if len(args) != 1 {
		t.Fatalf("store-wide query args = %v, want just the file type", args)
	}
	if got, ok := args[0].(int); !ok || got != int(metadata.FileTypeRegular) {
		t.Errorf("store-wide query arg[0] = %v, want FileTypeRegular", args[0])
	}
}
