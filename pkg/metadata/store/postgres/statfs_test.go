package postgres

import (
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestStatfsQuery_ShareScoped asserts the share-scoping rule lives in one place:
// the aggregate always carries a share predicate, so no caller can accidentally
// report the store-wide total for one share. The aggregate counts only regular
// files so the share root directory does not inflate UsedFiles.
func TestStatfsQuery_ShareScoped(t *testing.T) {
	sql, args := statfsQuery("/myshare")
	if !strings.Contains(sql, "share_name = $1") {
		t.Errorf("query missing share predicate: %q", sql)
	}
	if !strings.Contains(sql, "file_type = $2") {
		t.Errorf("query missing regular-file predicate: %q", sql)
	}
	if len(args) != 2 {
		t.Fatalf("query args = %v, want share name + file type", args)
	}
	if got, ok := args[0].(string); !ok || got != "/myshare" {
		t.Errorf("query arg[0] = %v, want \"/myshare\"", args[0])
	}
	if got, ok := args[1].(int); !ok || got != int(metadata.FileTypeRegular) {
		t.Errorf("query arg[1] = %v, want FileTypeRegular", args[1])
	}
}
