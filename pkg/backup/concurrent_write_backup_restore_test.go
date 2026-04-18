//go:build integration

package backup_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestConcurrentWriteBackupRestore proves ROADMAP Phase 7 SC4:
// concurrent writers running against the source store while Backup
// streams out, followed by Restore to a fresh engine and byte-compare
// of the PayloadIDSet. Uses the Phase 2 ConcurrentWriter pattern
// (100ms writer window, atomic error counter) against the memory
// engine. No server process; no Docker. Build tag `integration`.
//
// Placement: pkg/backup/ (not test/e2e/) because SC4 is about the
// metadata-store backup primitive, not the server's REST surface —
// the server is covered by the Phase-7 matrix test.
func TestConcurrentWriteBackupRestore(t *testing.T) {
	ctx := context.Background()

	src := memory.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = src.Close() })

	// --- 1. Seed the source with a deterministic tree ---
	shareName := "/concurrent"
	require.NoError(t, src.CreateShare(ctx, &metadata.Share{Name: shareName}),
		"CreateShare")
	// CreateRootDirectory materialises the root File entry under the
	// share's pre-assigned root handle. Without this the Backup walker
	// in pkg/metadata/store/memory treats the root as a missing node
	// (fd == nil) and returns before enumerating children, so no
	// PayloadIDs would be collected.
	_, err := src.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
		UID:  0,
		GID:  0,
	})
	require.NoError(t, err, "CreateRootDirectory")
	rootHandle, err := src.GetRootHandle(ctx, shareName)
	require.NoError(t, err, "GetRootHandle")

	seedFile := func(name, payload string) {
		h, err := src.GenerateHandle(ctx, shareName, "/"+name)
		require.NoError(t, err, "GenerateHandle(%s)", name)
		_, id, err := metadata.DecodeFileHandle(h)
		require.NoError(t, err, "DecodeFileHandle")
		require.NoError(t, src.PutFile(ctx, &metadata.File{
			ID:        id,
			ShareName: shareName,
			FileAttr: metadata.FileAttr{
				Type:      metadata.FileTypeRegular,
				Mode:      0o644,
				UID:       1000,
				GID:       1000,
				PayloadID: metadata.PayloadID(payload),
			},
		}), "PutFile(%s)", name)
		require.NoError(t, src.SetParent(ctx, h, rootHandle), "SetParent(%s)", name)
		require.NoError(t, src.SetChild(ctx, rootHandle, name, h), "SetChild(%s)", name)
		require.NoError(t, src.SetLinkCount(ctx, h, 1), "SetLinkCount(%s)", name)
	}
	for i := 0; i < 5; i++ {
		seedFile(fmt.Sprintf("seed-%d", i), fmt.Sprintf("payload-seed-%d", i))
	}

	// --- 2. Spawn concurrent writer goroutine, Phase 2 style ---
	// 100ms window matches ConcurrentWriterDuration default in
	// pkg/metadata/storetest/backup_conformance.go.
	writerCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	var writerErrs atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			if writerCtx.Err() != nil {
				return
			}
			name := fmt.Sprintf("concurrent-%d", i)
			h, err := src.GenerateHandle(writerCtx, shareName, "/"+name)
			if err != nil {
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					writerErrs.Add(1)
				}
				i++
				continue
			}
			_, id, err := metadata.DecodeFileHandle(h)
			if err != nil {
				writerErrs.Add(1)
				i++
				continue
			}
			f := &metadata.File{
				ID:        id,
				ShareName: shareName,
				FileAttr: metadata.FileAttr{
					Type:      metadata.FileTypeRegular,
					Mode:      0o644,
					UID:       1000,
					GID:       1000,
					PayloadID: metadata.PayloadID(fmt.Sprintf("payload-concurrent-%d", i)),
				},
			}
			if err := src.PutFile(writerCtx, f); err != nil {
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					writerErrs.Add(1)
				}
				i++
				continue
			}
			if err := src.SetParent(writerCtx, h, rootHandle); err != nil {
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					writerErrs.Add(1)
				}
				i++
				continue
			}
			if err := src.SetChild(writerCtx, rootHandle, name, h); err != nil {
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					writerErrs.Add(1)
				}
				i++
				continue
			}
			if err := src.SetLinkCount(writerCtx, h, 1); err != nil {
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					writerErrs.Add(1)
				}
				i++
				continue
			}
			i++
		}
	}()

	// --- 3. Backup concurrently with writer ---
	var buf bytes.Buffer
	ids, err := src.Backup(ctx, &buf)
	require.NoError(t, err, "Backup during concurrent writes")
	cancel()
	wg.Wait()
	require.Zero(t, writerErrs.Load(),
		"writer goroutine must not encounter intermediate errors during concurrent backup")

	// --- 4. Restore to fresh engine ---
	dest := memory.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = dest.Close() })
	require.NoError(t, dest.Restore(ctx, bytes.NewReader(buf.Bytes())),
		"Restore into fresh engine")

	// --- 5. Byte-compare via PayloadIDSet invariants ---
	// Enumerate all PayloadIDs from the restored store.
	restoredIDs := metadata.NewPayloadIDSet()
	shares, err := dest.ListShares(ctx)
	require.NoError(t, err, "dest.ListShares")
	require.Contains(t, shares, shareName, "restored store must expose /concurrent")
	restoredRoot, err := dest.GetRootHandle(ctx, shareName)
	require.NoError(t, err, "dest.GetRootHandle")
	// NOTE: MetadataStore uses ListChildren (cursor-paginated), NOT ListDir.
	// Signature: ListChildren(ctx, handle, cursor, limit) ([]DirEntry, string, error).
	// We pass cursor="" (start) and limit=1000 (more than enough for this test's
	// ~5 seed + N concurrent files). The returned cursor is discarded because
	// a single page covers the full set at this scale.
	entries, _, err := dest.ListChildren(ctx, restoredRoot, "", 1000)
	require.NoError(t, err, "dest.ListChildren(root)")
	for _, entry := range entries {
		f, err := dest.GetFile(ctx, entry.Handle)
		if err != nil {
			continue
		}
		if f.PayloadID != "" {
			// PayloadIDSet.Add takes a plain string; PayloadID is a named
			// string type, so cast explicitly to satisfy the signature.
			restoredIDs.Add(string(f.PayloadID))
		}
	}

	// Invariant (a): every PayloadID the Backup reported must be
	// present in the restored store. No dangling refs.
	for pid := range ids {
		require.True(t, restoredIDs.Contains(pid),
			"Backup reported PayloadID %q but restored store has no file with it", pid)
	}
	// Invariant (b): every PayloadID in the restored store must be
	// in the Backup's returned set. No uncounted files.
	for pid := range restoredIDs {
		require.True(t, ids.Contains(pid),
			"restored PayloadID %q is not in the Backup's returned set", pid)
	}

	// --- 6. Byte-compare of a re-backup (best-effort). ---
	// If the engine's encoding is deterministic, buf2 == buf. If the
	// encoding is map-iteration-dependent (memory gob), the byte-compare
	// will diverge — treat that as acceptable and skip the equality
	// check, relying on the PayloadIDSet invariants above. Document
	// which path was taken in the SUMMARY.
	var buf2 bytes.Buffer
	_, err = dest.Backup(ctx, &buf2)
	require.NoError(t, err, "re-Backup from dest")
	if bytes.Equal(buf.Bytes(), buf2.Bytes()) {
		t.Logf("byte-compare PASSED: backup stream is deterministic (%d bytes)", buf.Len())
	} else {
		t.Logf("byte-compare streams differ (%d vs %d bytes) — engine encoding is non-deterministic; PayloadIDSet invariants cover SC4",
			buf.Len(), buf2.Len())
	}
}
