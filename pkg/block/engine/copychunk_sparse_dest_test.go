package engine

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/local/memory"
)

// TestWriteAt_SparseDest_ReadsLeadingGapAsZeros reproduces the data-plane
// invariant exercised by smbtorture smb2.ioctl.copy_chunk_sparse_dest: a
// write into a 0-byte file at offset 4096 must leave the [0, 4096) region
// readable as zeros (a sparse hole) on a subsequent read, while the written
// region [4096, 8192) returns the written bytes.
//
// SRV_COPYCHUNK (executeCopyChunks) writes the copied extent via the same
// engine.Store.WriteAt path a normal SMB WRITE uses, so driving WriteAt
// directly here exercises the real copychunk write/read code path (the FSCTL
// handler is a thin wrapper over this call).
func TestWriteAt_SparseDest_ReadsLeadingGapAsZeros(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx := context.Background()
	const payloadID = "sparse-dest"

	// Source pattern written at offset 4096 (non-zero so a zero-fill bug is
	// distinguishable from a correct hole).
	pattern := make([]byte, 4096)
	for i := range pattern {
		pattern[i] = byte((i % 251) + 1) // 1..251, never zero
	}

	if _, err := bs.WriteAt(ctx, payloadID, nil, pattern, 4096); err != nil {
		t.Fatalf("WriteAt at offset 4096: %v", err)
	}

	// The Samba test reads exactly [0, 4096) — entirely inside the hole —
	// and asserts all-zeros with STATUS_OK.
	hole := make([]byte, 4096)
	n, err := bs.ReadAt(ctx, payloadID, nil, hole, 0)
	if err != nil {
		t.Fatalf("ReadAt [0,4096): %v", err)
	}
	if n != len(hole) {
		t.Fatalf("ReadAt short read: got %d, want %d", n, len(hole))
	}
	for i := 0; i < 4096; i++ {
		if hole[i] != 0 {
			t.Fatalf("leading gap not zero at byte %d: got 0x%02x", i, hole[i])
		}
	}

	// And the copied region [4096, 8192) must equal the written pattern.
	copied := make([]byte, 4096)
	if _, err := bs.ReadAt(ctx, payloadID, nil, copied, 4096); err != nil {
		t.Fatalf("ReadAt [4096,8192): %v", err)
	}
	if !bytes.Equal(copied, pattern) {
		t.Fatalf("copied region mismatch in [4096,8192)")
	}
}

// TestMemoryStore_ReadPayloadAt_SparseLeadingGap isolates the local-store
// contract: ReadPayloadAt over a window whose leading bytes were never
// written must serve those bytes as zeros, not return ErrFileBlockNotFound.
func TestMemoryStore_ReadPayloadAt_SparseLeadingGap(t *testing.T) {
	store := memory.New()
	ctx := context.Background()
	const payloadID = "sparse-local"

	data := bytes.Repeat([]byte{0xAB}, 4096)
	if err := store.AppendWrite(ctx, payloadID, data, 4096); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	dest := make([]byte, 8192)
	n, err := store.ReadPayloadAt(ctx, payloadID, dest, 0)
	if err != nil {
		t.Fatalf("ReadPayloadAt: %v", err)
	}
	if n != len(dest) {
		t.Fatalf("ReadPayloadAt short read: got %d, want %d", n, len(dest))
	}
	for i := 0; i < 4096; i++ {
		if dest[i] != 0 {
			t.Fatalf("leading gap not zero at byte %d: got 0x%02x", i, dest[i])
		}
	}
	if !bytes.Equal(dest[4096:], data) {
		t.Fatalf("written region mismatch")
	}
}
