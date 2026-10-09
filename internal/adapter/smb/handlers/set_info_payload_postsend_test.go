package handlers

import (
	"context"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// The dispatcher consumes PostSend from the original request context after
// encoding its response. Payload admission must preserve that output channel
// when a size change records a parent-directory lease break.
func TestSetInfoPayloadScopePreservesPostSend(t *testing.T) {
	for _, class := range []struct {
		name string
		info types.FileInfoClass
	}{
		{"EOF", types.FileEndOfFileInformation},
		{"Allocation", types.FileAllocationInformation},
	} {
		for _, chained := range []bool{false, true} {
			name := class.name + "/empty"
			if chained {
				name = class.name + "/existing"
			}
			t.Run(name, func(t *testing.T) {
				h, ctx, open, notifier, holder, key := setupSetInfoPostSend(t)
				requestContext := ctx.Context
				priorCalls := 0
				if chained {
					ctx.PostSend = func() {
						priorCalls++
						if got := len(notifier.forSession(holder)); got != 0 {
							t.Errorf("existing hook ran after %d lease notifications", got)
						}
					}
				}

				resp, err := h.SetInfo(ctx, &SetInfoRequest{
					InfoType:      types.SMB2InfoTypeFile,
					FileInfoClass: uint8(class.info),
					FileID:        open.FileID,
					Buffer:        encodeAllocationInfo(4),
				})
				if err != nil || resp == nil || resp.Status != types.StatusSuccess {
					t.Fatalf("SetInfo: response=%v error=%v", resp, err)
				}
				if ctx.Context != requestContext {
					t.Fatal("SetInfo replaced the dispatcher's request context")
				}
				if priorCalls != 0 || len(notifier.forSession(holder)) != 0 {
					t.Fatal("post-send work ran before the response")
				}

				// Consume exactly the context the dispatcher handed to SetInfo.
				if _, err := resp.Encode(); err != nil {
					t.Fatalf("encode response: %v", err)
				}
				if ctx.PostSend == nil {
					t.Fatal("SetInfo did not publish the lease break to the dispatcher")
				}
				ctx.PostSend()
				if chained && priorCalls != 1 {
					t.Fatalf("existing hook ran %d times, want 1", priorCalls)
				}
				breaks := notifier.forSession(holder)
				if len(breaks) != 1 {
					t.Fatalf("dispatcher delivered %d lease breaks, want 1", len(breaks))
				}
				if got := breaks[0]; got.LeaseKey != key || got.CurrentState != lock.LeaseStateRead|lock.LeaseStateHandle || got.NewState != lock.LeaseStateNone {
					t.Fatalf("unexpected parent lease break: %+v", got)
				}
			})
		}
	}
}

func TestSetInfoPayloadScopeErrorPreservesPostSend(t *testing.T) {
	for _, class := range []types.FileInfoClass{types.FileEndOfFileInformation, types.FileAllocationInformation} {
		t.Run(fmt.Sprint(class), func(t *testing.T) {
			h, ctx, open, notifier, holder, _ := setupSetInfoPostSend(t)
			open.GrantedAccess = uint32(types.FileReadAttributes)
			priorCalls := 0
			ctx.PostSend = func() { priorCalls++ }
			resp, err := h.SetInfo(ctx, &SetInfoRequest{
				InfoType:      types.SMB2InfoTypeFile,
				FileInfoClass: uint8(class),
				FileID:        open.FileID,
				Buffer:        encodeAllocationInfo(4),
			})
			if err != nil || resp == nil || resp.Status != types.StatusAccessDenied {
				t.Fatalf("SetInfo: response=%v error=%v, want access denied", resp, err)
			}
			if priorCalls != 0 {
				t.Fatal("existing hook ran before the error response")
			}
			if ctx.PostSend == nil {
				t.Fatal("SetInfo lost the existing hook on its error path")
			}
			ctx.PostSend()
			if priorCalls != 1 || len(notifier.forSession(holder)) != 0 {
				t.Fatalf("error response ran existing hook %d times and sent %d lease breaks", priorCalls, len(notifier.forSession(holder)))
			}
		})
	}
}

func setupSetInfoPostSend(t *testing.T) (*Handler, *SMBHandlerContext, *OpenFile, *capturingNotifier, uint64, [16]byte) {
	t.Helper()
	h, ctx, parent := setupStreamsDisabledShare(t, false)
	fid := dirTestCreate(t, h, ctx, "resize", types.FileOpenIf, types.FileNonDirectoryFile)
	open, ok := h.GetOpenFile(fid)
	if !ok {
		t.Fatal("created file has no open handle")
	}
	open.GrantedAccess |= uint32(types.FileWriteData)
	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		t.Fatalf("BuildAuthContext: %v", err)
	}
	size := uint64(16)
	if _, err := h.Registry.GetMetadataService().SetFileAttributes(authCtx, open.MetadataHandle, &metadata.SetAttrs{Size: &size}); err != nil {
		t.Fatalf("seed size: %v", err)
	}

	mgr := lock.NewManager()
	notifier := &capturingNotifier{}
	h.LeaseManager = lease.NewLeaseManager(&staticLockResolver{mgr: mgr}, notifier)
	mgr.RegisterBreakCallbacks(lease.NewSMBBreakHandler(h.LeaseManager, notifier))
	const holder uint64 = 99
	key := [16]byte{0x17}
	if _, _, err := h.LeaseManager.RequestLease(context.Background(), lock.FileHandle(parent), key, [16]byte{}, holder, [16]byte{},
		"directory-holder", "directory-client", ctx.ShareName, lock.LeaseStateRead|lock.LeaseStateHandle, true); err != nil {
		t.Fatalf("parent directory lease: %v", err)
	}
	return h, ctx, open, notifier, holder, key
}
