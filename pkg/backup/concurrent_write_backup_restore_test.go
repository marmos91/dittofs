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

func TestConcurrentWriteBackupRestore(t *testing.T) {
	ctx := context.Background()

	src := memory.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = src.Close() })

	shareName := "/concurrent"
	require.NoError(t, src.CreateShare(ctx, &metadata.Share{Name: shareName}),
		"CreateShare")
	// CreateRootDirectory is required before Backup: without a root node the
	// walker treats the share as empty and collects no PayloadIDs.
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

	// 100ms matches ConcurrentWriterDuration in pkg/metadata/storetest/backup_conformance.go.
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

	var buf bytes.Buffer
	ids, err := src.Backup(ctx, &buf)
	require.NoError(t, err, "Backup during concurrent writes")
	cancel()
	wg.Wait()
	require.Zero(t, writerErrs.Load(),
		"writer goroutine must not encounter intermediate errors during concurrent backup")

	dest := memory.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = dest.Close() })
	require.NoError(t, dest.Restore(ctx, bytes.NewReader(buf.Bytes())),
		"Restore into fresh engine")

	restoredIDs := metadata.NewPayloadIDSet()
	shares, err := dest.ListShares(ctx)
	require.NoError(t, err, "dest.ListShares")
	require.Contains(t, shares, shareName, "restored store must expose /concurrent")
	restoredRoot, err := dest.GetRootHandle(ctx, shareName)
	require.NoError(t, err, "dest.GetRootHandle")
	entries, _, err := dest.ListChildren(ctx, restoredRoot, "", 1000)
	require.NoError(t, err, "dest.ListChildren(root)")
	for _, entry := range entries {
		f, err := dest.GetFile(ctx, entry.Handle)
		if err != nil {
			continue
		}
		if f.PayloadID != "" {
			restoredIDs.Add(string(f.PayloadID))
		}
	}

	for pid := range ids {
		require.True(t, restoredIDs.Contains(pid),
			"Backup reported PayloadID %q but restored store has no file with it", pid)
	}
	for pid := range restoredIDs {
		require.True(t, ids.Contains(pid),
			"restored PayloadID %q is not in the Backup's returned set", pid)
	}

	// Re-backup: gob map iteration is non-deterministic so byte equality is best-effort.
	var buf2 bytes.Buffer
	_, err = dest.Backup(ctx, &buf2)
	require.NoError(t, err, "re-Backup from dest")
	if bytes.Equal(buf.Bytes(), buf2.Bytes()) {
		t.Logf("byte-compare PASSED: backup stream is deterministic (%d bytes)", buf.Len())
	} else {
		t.Logf("byte-compare streams differ (%d vs %d bytes) — engine encoding is non-deterministic; PayloadIDSet invariants cover correctness",
			buf.Len(), buf2.Len())
	}
}
