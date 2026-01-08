package metadata_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// TestFileIDConsistencyAcrossPackages verifies that all functions that extract
// file IDs from handles return the same value.
//
// This is a critical regression test for the "fileid changed" NFS bug where
// different code paths (FSINFO, GETATTR, READDIRPLUS) returned different
// fileids for the same handle, causing the Linux NFS client to reject mounts.
//
// Root cause: Multiple implementations existed:
//   - fsinfo.go: ExtractFileIDFromHandle() used first 8 bytes (WRONG)
//   - memory/store.go: extractFileIDFromHandle() used FNV-1a hash (WRONG)
//   - metadata/handle.go: HandleToINode() used SHA-256 hash (CORRECT)
//   - xdr/filehandle.go: ExtractFileID() delegated to HandleToINode (CORRECT)
//
// All implementations MUST use metadata.HandleToINode() as the canonical source.
func TestFileIDConsistencyAcrossPackages(t *testing.T) {
	// Generate various test handles
	testCases := []struct {
		name      string
		shareName string
		id        uuid.UUID
	}{
		{
			name:      "typical export handle",
			shareName: "/export",
			id:        uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
		},
		{
			name:      "short share name",
			shareName: "/a",
			id:        uuid.MustParse("123e4567-e89b-12d3-a456-426614174000"),
		},
		{
			name:      "long share name",
			shareName: "/very/long/path/to/share",
			id:        uuid.MustParse("7c9e6679-7425-40de-944b-e07fc1f90ae7"),
		},
		{
			name:      "share without leading slash",
			shareName: "data",
			id:        uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		},
		{
			name:      "nil UUID",
			shareName: "/export",
			id:        uuid.Nil,
		},
		{
			name:      "max UUID",
			shareName: "/export",
			id:        uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create handle
			handle, err := metadata.EncodeShareHandle(tc.shareName, tc.id)
			if err != nil {
				t.Fatalf("EncodeShareHandle() error = %v", err)
			}

			// Get fileid from canonical source (metadata.HandleToINode)
			canonicalFileID := metadata.HandleToINode(handle)

			// Verify XDR ExtractFileID returns same value
			xdrFileID := xdr.ExtractFileID(handle)
			if xdrFileID != canonicalFileID {
				t.Errorf("xdr.ExtractFileID() = 0x%x, want 0x%x (canonical)",
					xdrFileID, canonicalFileID)
			}

			// Verify handlers.ExtractFileIDFromHandle returns same value
			handlersFileID, err := handlers.ExtractFileIDFromHandle(handle)
			if err != nil {
				t.Fatalf("handlers.ExtractFileIDFromHandle() error = %v", err)
			}
			if handlersFileID != canonicalFileID {
				t.Errorf("handlers.ExtractFileIDFromHandle() = 0x%x, want 0x%x (canonical)",
					handlersFileID, canonicalFileID)
			}

			// Verify none of them return raw bytes (the original bug)
			// First 8 bytes of "/export:" = 0x2f6578706f72743a
			if tc.shareName == "/export" {
				rawBytesFileID := uint64(0x2f6578706f72743a)
				if canonicalFileID == rawBytesFileID {
					t.Errorf("Canonical fileid equals raw bytes 0x%x - this indicates the hash is broken",
						rawBytesFileID)
				}
			}
		})
	}
}

// TestFileIDStability verifies that fileid values don't change between calls.
// NFS clients cache fileids and will reject mounts if they change.
func TestFileIDStability(t *testing.T) {
	handle, _ := metadata.EncodeShareHandle("/export", uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"))

	// Call each function multiple times and verify consistency
	for i := 0; i < 100; i++ {
		id1 := metadata.HandleToINode(handle)
		id2 := xdr.ExtractFileID(handle)
		id3, _ := handlers.ExtractFileIDFromHandle(handle)

		if id1 != id2 || id2 != id3 {
			t.Fatalf("Iteration %d: fileid mismatch - HandleToINode=0x%x, xdr=0x%x, handlers=0x%x",
				i, id1, id2, id3)
		}
	}
}
