package postgres

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestStatfsQuery_ScopedVsStoreWide asserts the share-scoping rule lives in one
// place: a handle that decodes to a share name yields the WHERE-predicated query
// with the share name as the sole argument; an undecodable handle falls back to
// the store-wide aggregate with no args.
func TestStatfsQuery_ScopedVsStoreWide(t *testing.T) {
	handle, err := encodeFileHandle("/myshare", uuid.New().String())
	if err != nil {
		t.Fatalf("encodeFileHandle: %v", err)
	}

	sql, args := statfsQuery(handle)
	if !strings.Contains(sql, "WHERE share_name = $1") {
		t.Errorf("scoped query missing WHERE predicate: %q", sql)
	}
	if len(args) != 1 {
		t.Fatalf("scoped query args = %v, want exactly the share name", args)
	}
	if got, ok := args[0].(string); !ok || got != "/myshare" {
		t.Errorf("scoped query arg[0] = %v, want \"/myshare\"", args[0])
	}
}

// TestStatfsQuery_InvalidHandleFallsBackStoreWide ensures an undecodable handle
// does not error or scope to a garbage share — it falls back to the store-wide
// aggregate (single-share compatible) with no bound args.
func TestStatfsQuery_InvalidHandleFallsBackStoreWide(t *testing.T) {
	sql, args := statfsQuery([]byte{0x01, 0x02, 0x03}) // not a valid encoded handle
	if strings.Contains(sql, "WHERE") {
		t.Errorf("invalid handle produced a scoped query: %q", sql)
	}
	if len(args) != 0 {
		t.Errorf("invalid handle produced args %v, want none", args)
	}
}

// TestBuildFilesystemStatistics verifies the shared assembler computes used /
// available from the scanned aggregate and reports the unlimited ceilings.
func TestBuildFilesystemStatistics(t *testing.T) {
	stats := buildFilesystemStatistics(4096, 3)

	if stats.UsedBytes != 4096 {
		t.Errorf("UsedBytes = %d, want 4096", stats.UsedBytes)
	}
	if stats.UsedFiles != 3 {
		t.Errorf("UsedFiles = %d, want 3", stats.UsedFiles)
	}
	if stats.TotalBytes != statfsTotalBytes {
		t.Errorf("TotalBytes = %d, want %d", stats.TotalBytes, statfsTotalBytes)
	}
	if stats.TotalFiles != statfsTotalFiles {
		t.Errorf("TotalFiles = %d, want %d", stats.TotalFiles, statfsTotalFiles)
	}
	if stats.AvailableBytes != statfsTotalBytes-4096 {
		t.Errorf("AvailableBytes = %d, want %d", stats.AvailableBytes, statfsTotalBytes-4096)
	}
	if stats.AvailableFiles != statfsTotalFiles-3 {
		t.Errorf("AvailableFiles = %d, want %d", stats.AvailableFiles, statfsTotalFiles-3)
	}
}

// TestBuildFilesystemStatistics_ZeroUsage covers the empty-share case: zero
// usage yields the full ceiling as available.
func TestBuildFilesystemStatistics_ZeroUsage(t *testing.T) {
	stats := buildFilesystemStatistics(0, 0)
	if stats.AvailableBytes != statfsTotalBytes {
		t.Errorf("AvailableBytes = %d, want full ceiling %d", stats.AvailableBytes, statfsTotalBytes)
	}
	if stats.AvailableFiles != statfsTotalFiles {
		t.Errorf("AvailableFiles = %d, want full ceiling %d", stats.AvailableFiles, statfsTotalFiles)
	}
}
