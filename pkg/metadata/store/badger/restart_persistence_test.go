package badger_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// TestRestartPersistence_EAsAndADSStream pins the restart guarantee behind the
// SupportsExtendedAttrs capability flip: a base file's extended attributes
// (SMB FILE_FULL_EA_INFORMATION) and an Alternate Data Stream child record
// (the colon-named "base:stream" sibling the SMB layer creates for named
// streams) must survive closing and reopening the Badger store from the same
// on-disk path — i.e. a real server restart, not just an in-process round-trip.
//
// The memory store cannot exercise this (it does not persist), so this lives in
// the Badger package rather than the shared conformance suite.
func TestRestartPersistence_EAsAndADSStream(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")

	const (
		shareName  = "/restart"
		baseName   = "doc.txt"
		basePath   = "/doc.txt"
		streamName = "doc.txt:user.test" // ADS sibling, colon-named like the SMB layer
		streamPath = "/doc.txt:user.test"
		streamSize = uint64(7)
	)
	wantEAs := map[string][]byte{
		"USERTEST": []byte("hello"),
		"EMPTY":    {}, // zero-length EA must survive too
	}

	// --- Phase 1: write, then close (simulate shutdown) -------------------
	// File handles are stable across restart (architecture invariant #3), so
	// capture the root handle here and reuse it after reopen.
	var baseHandle, streamHandle, root metadata.FileHandle
	{
		store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
		require.NoError(t, err)

		root = mkShare(t, store, shareName)
		baseHandle = mkFile(t, store, shareName, root, baseName, basePath, func(a *metadata.FileAttr) {
			a.EAs = wantEAs
		})
		streamHandle = mkFile(t, store, shareName, root, streamName, streamPath, func(a *metadata.FileAttr) {
			a.Size = streamSize
		})

		require.NoError(t, store.Close())
	}

	// --- Phase 2: reopen from the same path (simulate restart) ------------
	store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	// EAs survive on the base file.
	base, err := store.GetFile(ctx, baseHandle)
	require.NoError(t, err)
	require.Len(t, base.EAs, len(wantEAs), "EA count lost across restart")
	for name, val := range wantEAs {
		got, ok := base.EAs[name]
		require.Truef(t, ok, "EA %q missing after restart", name)
		require.Truef(t, bytes.Equal(got, val), "EA %q = %q, want %q", name, got, val)
	}

	// The ADS stream child record survives with its size.
	stream, err := store.GetFile(ctx, streamHandle)
	require.NoError(t, err)
	require.Equal(t, streamSize, stream.Size, "ADS stream size lost across restart")

	// Directory enumeration still lists both the base and the ADS sibling.
	names := map[string]bool{}
	require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		entries, _, err := tx.ListChildren(ctx, root, "", 100)
		if err != nil {
			return err
		}
		for _, e := range entries {
			names[e.Name] = true
		}
		return nil
	}))
	require.True(t, names[baseName], "base file missing from listing after restart")
	require.True(t, names[streamName], "ADS stream child missing from listing after restart")
}

// mkShare creates a share with a root directory and returns the root handle.
func mkShare(t *testing.T, store metadata.Store, shareName string) metadata.FileHandle {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, store.CreateShare(ctx, &metadata.Share{Name: shareName}))
	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	})
	require.NoError(t, err)
	h, err := metadata.EncodeFileHandle(rootFile)
	require.NoError(t, err)
	return h
}

// mkFile creates a regular file entry at fullPath under dirHandle, applies mutate
// to its FileAttr, persists it, and wires up parent/child links.
func mkFile(t *testing.T, store metadata.Store, shareName string, dirHandle metadata.FileHandle, name, fullPath string, mutate func(*metadata.FileAttr)) metadata.FileHandle {
	t.Helper()
	ctx := context.Background()
	handle, err := store.GenerateHandle(ctx, shareName, fullPath)
	require.NoError(t, err)
	_, id, err := metadata.DecodeFileHandle(handle)
	require.NoError(t, err)

	file := &metadata.File{
		ShareName: shareName,
		Path:      fullPath,
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o600, UID: 1000, GID: 1000,
		},
	}
	file.ID = id
	mutate(&file.FileAttr)

	require.NoError(t, store.PutFile(ctx, file))
	require.NoError(t, store.SetParent(ctx, handle, dirHandle))
	require.NoError(t, store.SetChild(ctx, dirHandle, name, handle))
	return handle
}
