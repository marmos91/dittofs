package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

type smbScopePauseKey struct{}

type smbScopePauseStore struct {
	metadata.Store
	once    sync.Once
	entered chan struct{}
	resume  chan struct{}
	failure error
}

func (s *smbScopePauseStore) WithTransaction(ctx context.Context, fn func(metadata.Transaction) error) error {
	if ctx.Value(smbScopePauseKey{}) != nil {
		s.once.Do(func() {
			close(s.entered)
			select {
			case <-s.resume:
			case <-ctx.Done():
			}
		})
		if s.failure != nil {
			return s.failure
		}
	}
	return s.Store.WithTransaction(ctx, fn)
}

// A WRITE-like request must still hold admission after the journal accepts the
// bytes. The SET_ZERO_DATA case uses deferred commits to pause its final flush,
// after zeroFillRange has returned and released its own nested admission.
func TestSMBPayloadScopeCoversMetadataTransaction(t *testing.T) {
	for _, operation := range []string{"WRITE", "COPYCHUNK", "zeroFillRange", "SET_ZERO_DATA", "SET_INFO_EOF", "SET_INFO_allocation", "WRITE_error", "WRITE_cancel"} {
		t.Run(operation, func(t *testing.T) {
			h, ctx, src, dst := setupCopyChunkFrozenFixture(t)
			installLocalOnlyCopyChunkStore(t, h, ctx)
			metaSvc := h.Registry.GetMetadataService()
			metaSvc.SetDeferredCommit(false)
			original := bytes.Repeat([]byte{0x53}, 8192)
			tail := bytes.Repeat([]byte{0x74}, 4096)
			for _, seed := range []struct {
				open *OpenFile
				data []byte
			}{{src, tail}, {dst, original}} {
				resp, err := h.Write(ctx, &WriteRequest{FileID: seed.open.FileID, Length: uint32(len(seed.data)), Data: seed.data})
				require.NoError(t, err)
				require.Equal(t, types.StatusSuccess, resp.Status)
			}
			var copyInput []byte
			if operation == "COPYCHUNK" {
				resp, err := h.Ioctl(ctx, buildIoctlRequestBody(FsctlSrvRequestResumeKey, src.FileID, nil, 32))
				require.NoError(t, err)
				require.Equal(t, types.StatusSuccess, resp.Status)
				require.GreaterOrEqual(t, len(resp.Data), 48+resumeKeyLen)
				copyInput = make([]byte, 56)
				copy(copyInput, resp.Data[48:48+resumeKeyLen])
				binary.LittleEndian.PutUint32(copyInput[24:28], 1)
				binary.LittleEndian.PutUint64(copyInput[40:48], 8192)
				binary.LittleEndian.PutUint32(copyInput[48:52], 4096)
			}
			file, err := metaSvc.GetFile(t.Context(), dst.MetadataHandle)
			require.NoError(t, err)
			bs, err := h.Registry.GetBlockStoreForShare(ctx.ShareName)
			require.NoError(t, err)
			store, err := h.Registry.(*runtime.Runtime).GetMetadataStoreForShare(ctx.ShareName)
			require.NoError(t, err)
			gate := &smbScopePauseStore{Store: store, entered: make(chan struct{}), resume: make(chan struct{})}
			if operation == "WRITE_error" {
				gate.failure = errors.New("metadata commit failed")
			}
			require.NoError(t, metaSvc.RegisterStoreForShare(ctx.ShareName, gate))
			metaSvc.SetDeferredCommit(operation == "SET_ZERO_DATA")
			var once sync.Once
			resume := func() { once.Do(func() { close(gate.resume) }) }
			t.Cleanup(resume)
			requestCtx, cancelRequest := context.WithCancel(context.WithValue(t.Context(), smbScopePauseKey{}, true))
			defer cancelRequest()
			ctx.Context = requestCtx
			type result struct {
				status types.Status
				err    error
			}
			done := make(chan result, 1)
			go func() {
				switch operation {
				case "WRITE", "WRITE_error", "WRITE_cancel":
					resp, err := h.Write(ctx, &WriteRequest{FileID: dst.FileID, Offset: 8192, Length: uint32(len(tail)), Data: tail})
					done <- result{resp.Status, err}
				case "COPYCHUNK":
					resp, err := h.Ioctl(ctx, buildIoctlRequestBody(FsctlSrvCopyChunk, dst.FileID, copyInput, copyChunkResponseLen))
					done <- result{resp.Status, err}
				case "zeroFillRange":
					auth, err := BuildAuthContext(ctx)
					if err != nil {
						done <- result{err: err}
						return
					}
					committed, err := h.zeroFillRange(auth, dst, 2048, 6144)
					status := types.StatusSuccess
					if !committed {
						status = types.StatusUnsuccessful
					}
					done <- result{status, err}
				case "SET_ZERO_DATA":
					input := make([]byte, 16)
					binary.LittleEndian.PutUint64(input[:8], 2048)
					binary.LittleEndian.PutUint64(input[8:], 6144)
					resp, err := h.Ioctl(ctx, buildIoctlRequestBody(FsctlSetZeroData, dst.FileID, input, 0))
					done <- result{resp.Status, err}
				case "SET_INFO_EOF", "SET_INFO_allocation":
					class := types.FileEndOfFileInformation
					if operation == "SET_INFO_allocation" {
						class = types.FileAllocationInformation
					}
					resp, err := h.SetInfo(ctx, &SetInfoRequest{InfoType: types.SMB2InfoTypeFile, FileInfoClass: uint8(class), FileID: dst.FileID, Buffer: encodeAllocationInfo(4096)})
					done <- result{resp.Status, err}
				}
			}()
			select {
			case <-gate.entered:
			case got := <-done:
				t.Fatalf("operation missed metadata transaction: %+v", got)
			case <-time.After(5 * time.Second):
				t.Fatal("operation did not reach metadata transaction")
			}
			want := append([]byte(nil), original...)
			switch operation {
			case "WRITE", "COPYCHUNK", "WRITE_error", "WRITE_cancel":
				want = append(want, tail...)
			case "zeroFillRange", "SET_ZERO_DATA":
				clear(want[2048:6144])
			case "SET_INFO_EOF", "SET_INFO_allocation":
				want = want[:4096]
			}
			if operation != "SET_INFO_EOF" && operation != "SET_INFO_allocation" {
				got := make([]byte, len(want))
				_, err := bs.ReadAt(t.Context(), string(file.PayloadID), got, 0)
				require.NoError(t, err)
				require.Equal(t, want, got, "engine writes must finish before probing outer admission")
			}
			probe, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			err = bs.WithPayloadScope(probe, []string{string(file.PayloadID)}, true, func(context.Context) error { return nil })
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("replacement entered during %s metadata transaction: %v", operation, err)
			}
			if operation == "WRITE_cancel" {
				cancelRequest()
			}
			resume()
			select {
			case got := <-done:
				require.NoError(t, got.err)
				if operation == "WRITE_error" || operation == "WRITE_cancel" {
					require.NotEqual(t, types.StatusSuccess, got.status)
					want = original
				} else {
					require.Equal(t, types.StatusSuccess, got.status)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("operation did not finish after metadata resumed")
			}
			after, err := metaSvc.GetFile(t.Context(), dst.MetadataHandle)
			require.NoError(t, err)
			require.Equal(t, uint64(len(want)), after.Size)
			got := make([]byte, len(want))
			_, err = bs.ReadAt(t.Context(), string(file.PayloadID), got, 0)
			require.NoError(t, err)
			require.Equal(t, want, got)
			released, releaseCancel := context.WithTimeout(t.Context(), time.Second)
			defer releaseCancel()
			require.NoError(t, bs.WithPayloadScope(released, []string{string(file.PayloadID)}, true, func(context.Context) error { return nil }), "success and error responses must release admission")
		})
	}
}
