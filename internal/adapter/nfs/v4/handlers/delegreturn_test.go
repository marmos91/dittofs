package handlers

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// ============================================================================
// DELEGRETURN Handler Tests
// ============================================================================

func TestHandleDelegReturn_NoCurrentFH(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  nil,
	}

	// Encode DELEGRETURN args: stateid4 (16 bytes)
	var args bytes.Buffer
	args.Write(make([]byte, 16)) // dummy stateid

	result := h.handleDelegReturn(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("DELEGRETURN without FH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
	if result.OpCode != types.OP_DELEGRETURN {
		t.Errorf("DELEGRETURN opCode = %d, want OP_DELEGRETURN (%d)",
			result.OpCode, types.OP_DELEGRETURN)
	}
}

func TestHandleDelegReturn_BadXDR(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  []byte("real-file-handle"),
	}

	// Empty reader: cannot decode stateid
	result := h.handleDelegReturn(ctx, bytes.NewReader([]byte{}))

	if result.Status != types.NFS4ERR_BADXDR {
		t.Errorf("DELEGRETURN with bad XDR status = %d, want NFS4ERR_BADXDR (%d)",
			result.Status, types.NFS4ERR_BADXDR)
	}
}

func TestHandleDelegReturn_ValidDelegation(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	sm := state.NewStateManager(90 * time.Second)
	h := NewHandler(nil, pfs, sm)

	fh := []byte("real-file-handle-deleg")

	// Grant a delegation
	deleg := sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_READ)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  fh,
	}

	// Encode DELEGRETURN args with the delegation's stateid
	var args bytes.Buffer
	types.EncodeStateid4(&args, &deleg.Stateid)

	result := h.handleDelegReturn(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4_OK {
		t.Errorf("DELEGRETURN status = %d, want NFS4_OK (%d)",
			result.Status, types.NFS4_OK)
	}

	// Verify delegation is removed
	delegs := sm.GetDelegationsForFile(fh)
	if len(delegs) != 0 {
		t.Errorf("expected 0 delegations after DELEGRETURN, got %d", len(delegs))
	}
}

func TestHandleDelegReturn_StaleStateid(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	sm := state.NewStateManager(90 * time.Second)
	h := NewHandler(nil, pfs, sm)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  []byte("real-file-handle"),
	}

	// Create a stale stateid (wrong boot epoch)
	var staleStateid types.Stateid4
	staleStateid.Seqid = 1
	staleStateid.Other[0] = 0x03 // StateTypeDeleg
	staleStateid.Other[1] = 0xFF // Wrong epoch
	staleStateid.Other[2] = 0xFF
	staleStateid.Other[3] = 0xFF

	var args bytes.Buffer
	types.EncodeStateid4(&args, &staleStateid)

	result := h.handleDelegReturn(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4ERR_STALE_STATEID {
		t.Errorf("DELEGRETURN with stale stateid status = %d, want NFS4ERR_STALE_STATEID (%d)",
			result.Status, types.NFS4ERR_STALE_STATEID)
	}
}

func TestHandleDelegReturn_AlreadyReturned_Idempotent(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	sm := state.NewStateManager(90 * time.Second)
	h := NewHandler(nil, pfs, sm)

	fh := []byte("real-file-handle-idem")

	// Grant and return a delegation
	deleg := sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_READ)
	err := sm.ReturnDelegation(&deleg.Stateid, 0)
	if err != nil {
		t.Fatalf("ReturnDelegation: %v", err)
	}

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  fh,
	}

	// DELEGRETURN on already-returned delegation: should be idempotent (NFS4_OK)
	var args bytes.Buffer
	types.EncodeStateid4(&args, &deleg.Stateid)

	result := h.handleDelegReturn(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4_OK {
		t.Errorf("DELEGRETURN on already-returned delegation status = %d, want NFS4_OK (%d)",
			result.Status, types.NFS4_OK)
	}
}

func TestHandleDelegReturn_Registered(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	// Verify DELEGRETURN is registered in the dispatch table
	if _, exists := h.v40DispatchTable[types.OP_DELEGRETURN]; !exists {
		t.Error("OP_DELEGRETURN should be registered in dispatch table")
	}
}

// TestHandleDelegReturn_CrossClient covers the ownership check on DELEGRETURN.
// A delegation names state one client holds; another client returning it would
// stop the holder's recall timer and invalidate its cache authority without the
// holder ever knowing.
func TestHandleDelegReturn_CrossClient(t *testing.T) {
	const holder uint64 = 0xAAAA

	tests := []struct {
		name       string
		callerID   uint64
		wantStatus uint32
		wantDelegs int
	}{
		{"another client is refused", 0xBBBB, types.NFS4ERR_BAD_STATEID, 1},
		{"the holding client succeeds", holder, types.NFS4_OK, 0},
		// NFSv4.0 sends no clientid4 on DELEGRETURN and has no session to
		// derive one from, so there is nothing to compare and the delegation
		// stays a bearer token.
		{"no client identity to check", 0, types.NFS4_OK, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pfs := pseudofs.New()
			pfs.Rebuild([]string{"/export"})
			sm := state.NewStateManager(90 * time.Second)
			h := NewHandler(nil, pfs, sm)

			fh := []byte("fh-delegreturn-cross-client")
			deleg := sm.GrantDelegation(holder, fh, types.OPEN_DELEGATE_READ)

			var args bytes.Buffer
			types.EncodeStateid4(&args, &deleg.Stateid)

			result := h.handleDelegReturn(&types.CompoundContext{
				Context:         context.Background(),
				ClientAddr:      "127.0.0.1:9999",
				CurrentFH:       fh,
				SessionClientID: tt.callerID,
			}, bytes.NewReader(args.Bytes()))

			if result.Status != tt.wantStatus {
				t.Errorf("status = %d, want %d", result.Status, tt.wantStatus)
			}
			// A refused return must leave the delegation in place, or the guard
			// has merely changed the status code on a revocation that still
			// happened.
			if got := len(sm.GetDelegationsForFile(fh)); got != tt.wantDelegs {
				t.Errorf("delegations after DELEGRETURN = %d, want %d", got, tt.wantDelegs)
			}
		})
	}
}
