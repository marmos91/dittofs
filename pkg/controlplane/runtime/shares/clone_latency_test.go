package shares

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

type cloneUploadGate struct {
	*remotememory.Store
	entered chan struct{}
	resume  chan struct{}
	puts    atomic.Int64
	once    sync.Once
}

func (r *cloneUploadGate) PutBlock(ctx context.Context, id string, data io.Reader) error {
	if r.puts.Add(1) == 1 {
		close(r.entered)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.resume:
		}
	}
	return r.Store.PutBlock(ctx, id, data)
}

func (r *cloneUploadGate) release() { r.once.Do(func() { close(r.resume) }) }

func pauseCloneUpload(t *testing.T, f *cloneConcurrencyFixture) *cloneUploadGate {
	t.Helper()
	r := &cloneUploadGate{Store: f.remote, entered: make(chan struct{}), resume: make(chan struct{})}
	f.syncer.SetRemoteBlockStore(r)
	t.Cleanup(r.release)
	return r
}

func waitCloneUpload(t *testing.T, r *cloneUploadGate) {
	t.Helper()
	select {
	case <-r.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("upload did not start")
	}
}

func cloneForegroundOperation(t *testing.T, fn func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("foreground operation waited for the paused upload")
	}
}

func cloneForegroundRead(t *testing.T, f *cloneConcurrencyFixture, id string, want []byte) {
	t.Helper()
	got := make([]byte, len(want))
	cloneForegroundOperation(t, func() error {
		_, err := f.bs.ReadAt(context.Background(), id, got, 0)
		return err
	})
	require.Equal(t, want, got)
}

func TestCloneSourceUploadAllowsIOAndRetriesConcurrentOverwrite(t *testing.T) {
	f := newCloneConcurrencyFixture(t, true)
	ctx := context.Background()
	src, dst := f.file("src"), f.file("dst")
	original := bytes.Repeat([]byte{0x11}, 4096)
	updated := bytes.Repeat([]byte{0x22}, 4096)
	destination := bytes.Repeat([]byte{0x33}, 4096)
	require.NoError(t, f.write(ctx, src, "src", original, 0))
	require.NoError(t, f.write(ctx, dst, "dst", destination, 0))
	gate := pauseCloneUpload(t, f)
	cloned := make(chan error, 1)
	go func() { cloned <- common.CloneWholeFile(ctx, f.bs, f.ms, nil, src, dst, "dst", 0, f.svc) }()
	waitCloneUpload(t, gate)
	cloneForegroundRead(t, f, "src", original)
	cloneForegroundRead(t, f, "dst", destination)
	cloneForegroundOperation(t, func() error { return f.write(ctx, src, "src", updated, 0) })
	cloneForegroundOperation(t, func() error { return f.write(ctx, dst, "dst", destination, 0) })
	gate.release()
	require.NoError(t, receiveCloneResult(t, cloned))
	f.read("dst", updated)
	require.NoError(t, f.bs.DiscardLocalContent(ctx, "dst"))
	require.NoError(t, f.bs.Close())
	f.open()
	f.read("dst", updated)
}

func TestCloneDestinationUploadDoesNotQueueForegroundIO(t *testing.T) {
	f := newCloneConcurrencyFixture(t, true)
	ctx := context.Background()
	// Sorted acquisition reaches the source first. If a failed try keeps that
	// partial acquisition, the source operations below will also hang.
	src, dst := f.file("a-source"), f.file("z-destination")
	source := bytes.Repeat([]byte{0x44}, 4096)
	old := bytes.Repeat([]byte{0x55}, 4096)
	require.NoError(t, f.write(ctx, src, "a-source", source, 0))
	f.drain(src, "a-source")
	require.NoError(t, f.write(ctx, dst, "z-destination", old, 0))
	gate := pauseCloneUpload(t, f)
	flushed := make(chan error, 1)
	go func() { _, err := f.syncer.Flush(ctx, "z-destination"); flushed <- err }()
	waitCloneUpload(t, gate)
	cloned := make(chan error, 1)
	go func() { cloned <- common.CloneWholeFile(ctx, f.bs, f.ms, nil, src, dst, "z-destination", 0, f.svc) }()
	// Give clone time to attempt admission while the destination pass is held.
	select {
	case err := <-cloned:
		t.Fatalf("clone passed the outstanding destination carve: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	cloneForegroundRead(t, f, "a-source", source)
	cloneForegroundRead(t, f, "z-destination", old)
	cloneForegroundOperation(t, func() error { return f.write(ctx, src, "a-source", source, 0) })
	cloneForegroundOperation(t, func() error { return f.write(ctx, dst, "z-destination", old, 0) })
	gate.release()
	require.NoError(t, receiveCloneResult(t, flushed))
	require.NoError(t, receiveCloneResult(t, cloned))
	require.NoError(t, f.bs.DiscardLocalContent(ctx, "z-destination"))
	f.read("z-destination", source)
}

func TestCloneSourceUploadPreservesConcurrentDestinationTail(t *testing.T) {
	f := newCloneConcurrencyFixture(t, true)
	ctx := context.Background()
	src, dst := f.file("src"), f.file("dst")
	source := bytes.Repeat([]byte{0x66}, 4096)
	old := bytes.Repeat([]byte{0x77}, 4096)
	tail := bytes.Repeat([]byte{0x88}, 4096)
	require.NoError(t, f.write(ctx, src, "src", source, 0))
	require.NoError(t, f.write(ctx, dst, "dst", old, 0))
	gate := pauseCloneUpload(t, f)
	cloned := make(chan error, 1)
	go func() { cloned <- common.CloneWholeFile(ctx, f.bs, f.ms, nil, src, dst, "dst", 0, f.svc) }()
	waitCloneUpload(t, gate)
	cloneForegroundOperation(t, func() error { return f.write(ctx, dst, "dst", tail, 4096) })
	gate.release()
	err := receiveCloneResult(t, cloned)
	var se *metadata.StoreError
	require.ErrorAs(t, err, &se)
	require.Equal(t, metadata.ErrNotSupported, se.Code)
	want := append(bytes.Clone(old), tail...)
	f.read("dst", want)
	f.drain(dst, "dst")
	require.NoError(t, f.bs.DiscardLocalContent(ctx, "dst"))
	f.read("dst", want)
}

func TestCloneStrictDurabilityDoesNotUploadDestinationUnderExclusion(t *testing.T) {
	f := newCloneConcurrencyFixture(t, true)
	ctx := context.Background()
	src, dst := f.file("src"), f.file("dst")
	source := bytes.Repeat([]byte{0x99}, 4096)
	require.NoError(t, f.write(ctx, src, "src", source, 0))
	f.drain(src, "src")
	require.NoError(t, f.write(ctx, dst, "dst", bytes.Repeat([]byte{0xaa}, 4096), 0))
	f.bs.SetRequireDurableCommit(true)
	gate := pauseCloneUpload(t, f)
	cloneForegroundOperation(t, func() error {
		return common.CloneWholeFile(ctx, f.bs, f.ms, nil, src, dst, "dst", 0, f.svc)
	})
	require.Zero(t, gate.puts.Load(), "replaced destination bytes must not be uploaded under exclusive admission")
	f.read("dst", source)
}

func TestCloneDeadlineWhileDestinationUploadStalls(t *testing.T) {
	f := newCloneConcurrencyFixture(t, true)
	ctx := context.Background()
	src, dst := f.file("src"), f.file("dst")
	source := bytes.Repeat([]byte{0xbb}, 4096)
	old := bytes.Repeat([]byte{0xcc}, 4096)
	require.NoError(t, f.write(ctx, src, "src", source, 0))
	f.drain(src, "src")
	require.NoError(t, f.write(ctx, dst, "dst", old, 0))
	gate := pauseCloneUpload(t, f)
	flushed := make(chan error, 1)
	go func() { _, err := f.syncer.Flush(ctx, "dst"); flushed <- err }()
	waitCloneUpload(t, gate)
	cloneCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	cloned := make(chan error, 1)
	go func() { cloned <- common.CloneWholeFile(cloneCtx, f.bs, f.ms, nil, src, dst, "dst", 0, f.svc) }()
	select {
	case err := <-cloned:
		require.True(t, errors.Is(err, context.DeadlineExceeded), "clone deadline: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("clone ignored its deadline")
	}
	cloneForegroundRead(t, f, "dst", old)
	cloneForegroundOperation(t, func() error { return f.write(ctx, dst, "dst", old, 0) })
	gate.release()
	require.NoError(t, receiveCloneResult(t, flushed))
}
