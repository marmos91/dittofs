package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

func TestCreateOverwriteWaitsForPayloadReplacement(t *testing.T) {
	h, auth, handle, _ := setupDOSAttrModeTest(t, metadata.FileTypeRegular, 0o600)
	metaSvc := h.Registry.GetMetadataService()
	size := uint64(64)
	_, err := metaSvc.SetFileAttributes(auth, handle, &metadata.SetAttrs{Size: &size})
	require.NoError(t, err)
	before, err := metaSvc.GetFile(auth.Context, handle)
	require.NoError(t, err)
	blockStore, err := common.ResolveForWrite(auth.Context, h.Registry, handle)
	require.NoError(t, err)
	req := &CreateRequest{CreateDisposition: types.FileOverwrite, FileAttributes: types.FileAttributeHidden}

	// A clone owns the exclusive scope. The independent open must wait for it;
	// its deadline bounds that wait without scheduling a competing goroutine.
	err = blockStore.WithPayloadScope(auth.Context, []string{string(before.PayloadID)}, true, func(context.Context) error {
		waiting := *auth
		ctx, cancel := context.WithTimeout(auth.Context, 100*time.Millisecond)
		defer cancel()
		waiting.Context = ctx
		file, returnedHandle, overwriteErr := h.overwriteFile(&waiting, before, req)
		require.ErrorIs(t, overwriteErr, context.DeadlineExceeded, "overwrite must join payload admission before changing Size")
		require.Nil(t, file)
		require.Nil(t, returnedHandle)
		current, readErr := metaSvc.GetFile(auth.Context, handle)
		require.NoError(t, readErr)
		require.Equal(t, size, current.Size, "a blocked overwrite must not truncate metadata")
		require.Equal(t, before.Mode, current.Mode)
		require.Equal(t, before.Hidden, current.Hidden)
		return nil
	})
	require.NoError(t, err)

	updated, returnedHandle, err := h.overwriteFile(auth, before, req)
	require.NoError(t, err)
	require.Equal(t, handle, returnedHandle)
	require.Zero(t, updated.Size)
	require.True(t, updated.Hidden)
	require.NotZero(t, updated.Mode&modeDOSArchive)
	require.Equal(t, uint32(0o600), updated.Mode&0o7777)
}

func TestCreateOverwriteReusesOwnedPayloadScope(t *testing.T) {
	h, auth, handle, _ := setupDOSAttrModeTest(t, metadata.FileTypeRegular, 0o600)
	metaSvc := h.Registry.GetMetadataService()
	before, err := metaSvc.GetFile(auth.Context, handle)
	require.NoError(t, err)
	blockStore, err := common.ResolveForWrite(auth.Context, h.Registry, handle)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(auth.Context, 5*time.Second)
	defer cancel()
	err = blockStore.WithPayloadScope(ctx, []string{string(before.PayloadID)}, true, func(scoped context.Context) error {
		scopedAuth := *auth
		scopedAuth.Context = scoped
		mode := uint32(0o640)
		storageBits := modeDOSCompressed | modeDOSSparse
		_, err := metaSvc.SetFileAttributes(&scopedAuth, handle, &metadata.SetAttrs{Mode: &mode, ModeOrMask: &storageBits})
		if err != nil {
			return err
		}
		updated, returnedHandle, err := h.overwriteFile(&scopedAuth, before, &CreateRequest{CreateDisposition: types.FileSupersede})
		if err != nil {
			return err
		}
		require.Equal(t, handle, returnedHandle)
		require.Equal(t, mode|storageBits, updated.Mode&(0o7777|modeDOSCompressed|modeDOSSparse), "overwrite must preserve current attributes, not the pre-scope snapshot")
		require.NotZero(t, updated.Mode&modeDOSArchive)
		return nil
	})
	require.NoError(t, err)
}
