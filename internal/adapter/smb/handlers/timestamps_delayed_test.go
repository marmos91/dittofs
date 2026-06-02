package handlers

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// Drives the helpers directly because the SMB WRITE handler depends on
// session/tree/blockstore plumbing that is heavyweight to fake. The
// helpers encode the entire delayed-write state machine, so covering
// them gives the same protection as a full handler test would.

func mustGet(t *testing.T, h *Handler, fh metadata.FileHandle, authCtx *metadata.AuthContext) *metadata.File {
	t.Helper()
	file, err := h.Registry.GetMetadataService().GetFile(authCtx.Context, fh)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	return file
}

func TestSmbDelayedWrite_VisibleMtimeUntilWindowExpires(t *testing.T) {
	h, authCtx, fh, openFile := setupTimestampTest(t)
	metaSvc := h.Registry.GetMetadataService()

	preWriteMtime := mustGet(t, h, fh, authCtx).Mtime

	// Simulate a WRITE: PrepareWrite -> CommitWrite -> arm window.
	op, err := metaSvc.PrepareWrite(authCtx, fh, 1)
	if err != nil {
		t.Fatalf("PrepareWrite: %v", err)
	}
	if _, err := metaSvc.CommitWrite(authCtx, op); err != nil {
		t.Fatalf("CommitWrite: %v", err)
	}
	if _, err := metaSvc.FlushPendingWriteForFile(authCtx, fh); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	armSmbDelayedWrite(openFile, preWriteMtime, op.NewMtime)

	// Inside the window: QUERY_INFO surfaces the pre-write Mtime.
	file := mustGet(t, h, fh, authCtx)
	applySmbDelayedWriteOverride(openFile, file)
	if !file.Mtime.Equal(preWriteMtime) {
		t.Fatalf("inside window: got %v want %v", file.Mtime, preWriteMtime)
	}

	// Force the window past expiry without sleeping 2 seconds.
	openFile.SmbWriteFlushAt = time.Now().Add(-time.Second)
	file = mustGet(t, h, fh, authCtx)
	applySmbDelayedWriteOverride(openFile, file)
	if !file.Mtime.Equal(op.NewMtime) {
		t.Errorf("after window: got %v want %v", file.Mtime, op.NewMtime)
	}
}

func TestSmbDelayedWrite_SecondWriteDoesNotAdvanceVisibleMtime(t *testing.T) {
	h, authCtx, fh, openFile := setupTimestampTest(t)
	metaSvc := h.Registry.GetMetadataService()

	pre := mustGet(t, h, fh, authCtx).Mtime

	op1, _ := metaSvc.PrepareWrite(authCtx, fh, 1)
	_, _ = metaSvc.CommitWrite(authCtx, op1)
	_, _ = metaSvc.FlushPendingWriteForFile(authCtx, fh)
	armSmbDelayedWrite(openFile, pre, op1.NewMtime)
	openFile.SmbWriteFlushAt = time.Now().Add(-time.Second) // simulate post-window

	time.Sleep(20 * time.Millisecond)
	op2, _ := metaSvc.PrepareWrite(authCtx, fh, 1)
	_, _ = metaSvc.CommitWrite(authCtx, op2)
	_, _ = metaSvc.FlushPendingWriteForFile(authCtx, fh)
	armSmbDelayedWrite(openFile, mustGet(t, h, fh, authCtx).Mtime, op2.NewMtime)

	file := mustGet(t, h, fh, authCtx)
	applySmbDelayedWriteOverride(openFile, file)
	if !file.Mtime.Equal(op1.NewMtime) {
		t.Errorf("second write must not advance visible mtime: got %v want %v",
			file.Mtime, op1.NewMtime)
	}
}

func TestSmbDelayedWrite_FlushCollapsesWindow(t *testing.T) {
	h, authCtx, fh, openFile := setupTimestampTest(t)
	metaSvc := h.Registry.GetMetadataService()

	pre := mustGet(t, h, fh, authCtx).Mtime
	op, _ := metaSvc.PrepareWrite(authCtx, fh, 1)
	_, _ = metaSvc.CommitWrite(authCtx, op)
	_, _ = metaSvc.FlushPendingWriteForFile(authCtx, fh)
	armSmbDelayedWrite(openFile, pre, op.NewMtime)

	// Without flush, override returns pre.
	file := mustGet(t, h, fh, authCtx)
	applySmbDelayedWriteOverride(openFile, file)
	if !file.Mtime.Equal(pre) {
		t.Fatalf("before flush: got %v want %v", file.Mtime, pre)
	}

	flushSmbDelayedWrite(openFile)

	file = mustGet(t, h, fh, authCtx)
	applySmbDelayedWriteOverride(openFile, file)
	if !file.Mtime.Equal(op.NewMtime) {
		t.Errorf("after flush: got %v want %v", file.Mtime, op.NewMtime)
	}
}

func TestSmbDelayedWrite_StickyWinsOverDelayedWindow(t *testing.T) {
	h, authCtx, fh, openFile := setupTimestampTest(t)
	metaSvc := h.Registry.GetMetadataService()

	pre := mustGet(t, h, fh, authCtx).Mtime
	op, _ := metaSvc.PrepareWrite(authCtx, fh, 1)
	_, _ = metaSvc.CommitWrite(authCtx, op)
	_, _ = metaSvc.FlushPendingWriteForFile(authCtx, fh)
	armSmbDelayedWrite(openFile, pre, op.NewMtime)

	future := time.Now().Add(24 * time.Hour).UTC()
	setSmbStickyWriteTime(openFile, future)

	file := mustGet(t, h, fh, authCtx)
	applySmbDelayedWriteOverride(openFile, file)
	if !file.Mtime.Equal(future) {
		t.Errorf("sticky must win: got %v want %v", file.Mtime, future)
	}
}

func TestSmbDelayedWrite_NoOpWhenNothingTriggered(t *testing.T) {
	h, authCtx, fh, openFile := setupTimestampTest(t)
	original := mustGet(t, h, fh, authCtx).Mtime

	file := mustGet(t, h, fh, authCtx)
	applySmbDelayedWriteOverride(openFile, file)
	if !file.Mtime.Equal(original) {
		t.Errorf("override mutated mtime with no state: got %v want %v",
			file.Mtime, original)
	}
}
