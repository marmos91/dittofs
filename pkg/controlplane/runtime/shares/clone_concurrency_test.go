package shares

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/journal"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/stretchr/testify/require"
)

// A real journal, Badger metadata and the runtime coordinator exercise the
// entire write -> carve -> manifest -> clone -> cold-read chain.
type cloneConcurrencyFixture struct {
	t      *testing.T
	ms     *metadatabadger.BadgerMetadataStore
	svc    *metadata.Service
	bs     *engine.Store
	syncer *engine.RemoteSync
	remote *remotememory.Store
	path   string
	auth   *metadata.AuthContext
}

func newCloneConcurrencyFixture(t *testing.T, deferred bool) *cloneConcurrencyFixture {
	t.Helper()
	ctx := context.Background()
	ms, err := metadatabadger.NewBadgerMetadataStoreWithDefaultsAndCaches(ctx, t.TempDir(), 8, 8, false)
	require.NoError(t, err)
	f := &cloneConcurrencyFixture{t: t, ms: ms, svc: metadata.New(), remote: remotememory.New(), path: t.TempDir()}
	require.NoError(t, f.svc.RegisterStoreForShare("/clone", ms))
	f.svc.SetDeferredCommit(deferred)
	f.auth = &metadata.AuthContext{Context: ctx, Identity: &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)}, AuthMethod: "unix"}
	f.open()
	f.svc.SetDurableExtentResolver(func(_ string, id metadata.PayloadID) (int64, bool) { return f.bs.DurableExtent(ctx, id) })
	t.Cleanup(func() { _ = f.bs.Close(); _ = ms.Close(); _ = f.remote.Close() })
	return f
}

func (f *cloneConcurrencyFixture) open() {
	f.t.Helper()
	j, err := journal.Open(f.path, journal.Config{MaxLocalBytes: 100 << 20})
	require.NoError(f.t, err)
	cfg := engine.DefaultConfig()
	cfg.ManualSync = true
	wrapped := &nonClosingRemote{f.remote}
	rs := engine.NewRemoteSync(j, wrapped, f.ms, cfg)
	rs.SetRemoteBlockStore(f.remote)
	rs.SetSyncedHashStore(f.ms)
	f.syncer = rs
	f.bs, err = engine.New(engine.BlockStoreConfig{Local: j, Remote: wrapped, RemoteSync: rs, FileChunkStore: f.ms, SyncedHashStore: f.ms, Coordinator: newMetadataCoordinator(f.ms)})
	require.NoError(f.t, err)
	require.NoError(f.t, f.bs.Start(context.Background()))
}

func (f *cloneConcurrencyFixture) file(id string) metadata.FileHandle {
	f.t.Helper()
	now := time.Now()
	file := &metadata.File{ID: uuid.New(), ShareName: "/clone", Path: "/" + id, FileAttr: metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0644, Nlink: 1, PayloadID: metadata.PayloadID(id), Mtime: now, Ctime: now}}
	require.NoError(f.t, f.ms.UpdateAttrs(context.Background(), file))
	handle, err := metadata.EncodeShareHandle(file.ShareName, file.ID)
	require.NoError(f.t, err)
	return handle
}

func (f *cloneConcurrencyFixture) write(ctx context.Context, handle metadata.FileHandle, id string, data []byte, off uint64) error {
	return f.bs.WithPayloadScope(ctx, []string{id}, false, func(ctx context.Context) error {
		auth := *f.auth
		auth.Context = ctx
		intent, err := f.svc.PrepareWrite(&auth, handle, off+uint64(len(data)))
		if err != nil {
			return err
		}
		if err := common.WriteToBlockStore(ctx, f.bs, metadata.PayloadID(id), data, off); err != nil {
			return err
		}
		_, err = f.svc.CommitWrite(&auth, intent)
		return err
	})
}

func (f *cloneConcurrencyFixture) drain(handle metadata.FileHandle, id string) {
	f.t.Helper()
	require.NoError(f.t, f.bs.DrainPayload(context.Background(), id))
	_, err := f.svc.FlushPendingWriteForFile(f.auth, handle, true)
	require.NoError(f.t, err)
}

func (f *cloneConcurrencyFixture) read(id string, want []byte) {
	f.t.Helper()
	got := make([]byte, len(want))
	n, err := f.bs.ReadAt(context.Background(), id, got, 0)
	require.NoError(f.t, err)
	require.Equal(f.t, len(want), n)
	require.Equal(f.t, want, got)
}

type cloneCommitBarrier struct {
	metadata.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *cloneCommitBarrier) WithTransaction(ctx context.Context, fn func(metadata.Transaction) error) error {
	err := s.Store.WithTransaction(ctx, fn)
	if err == nil {
		s.once.Do(func() { close(s.entered); <-s.release })
	}
	return err
}

func receiveCloneResult(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("operation did not finish")
		return nil
	}
}

func TestCloneConcurrentTailSurvivesCommitDiscardSeam(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%v", deferred), func(t *testing.T) {
			f := newCloneConcurrencyFixture(t, deferred)
			ctx := context.Background()
			src, dst, other := f.file("src"), f.file("dst"), f.file("other")
			source := bytes.Repeat([]byte{0x31}, 4096)
			old := bytes.Repeat([]byte{0x62}, 4096)
			tail := bytes.Repeat([]byte{0x73}, 4096)
			require.NoError(t, f.write(ctx, src, "src", source, 0))
			require.NoError(t, f.write(ctx, dst, "dst", old, 0))
			// Leave the source write deferred: clone itself must publish its size and
			// materialize a genuine manifest before selecting the source rows.
			barrier := &cloneCommitBarrier{Store: f.ms, entered: make(chan struct{}), release: make(chan struct{})}
			cloneDone := make(chan error, 1)
			go func() { cloneDone <- common.CloneWholeFile(ctx, f.bs, barrier, nil, src, dst, "dst", 0, f.svc) }()
			select {
			case <-barrier.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("clone never reached post-commit seam")
			}
			writeDone := make(chan error, 1)
			started := make(chan struct{})
			go func() { close(started); writeDone <- f.write(ctx, dst, "dst", tail, 4096) }()
			<-started
			// Independent files must remain writable while the clone owns both IDs.
			unrelated := make(chan error, 1)
			go func() { unrelated <- f.write(ctx, other, "other", []byte("unrelated"), 0) }()
			require.NoError(t, receiveCloneResult(t, unrelated))
			writeFinished := false
			select {
			case err := <-writeDone:
				writeFinished = true
				require.NoError(t, err)
			case <-time.After(30 * time.Millisecond):
			}
			close(barrier.release)
			require.NoError(t, receiveCloneResult(t, cloneDone))
			if !writeFinished {
				require.NoError(t, receiveCloneResult(t, writeDone))
			}
			want := append(bytes.Clone(source), tail...)
			f.read("dst", want)
			f.drain(dst, "dst")
			file, err := f.ms.GetFile(ctx, dst)
			require.NoError(t, err)
			require.Equal(t, uint64(len(want)), file.Size)
			rows, err := f.ms.ListFileChunks(ctx, "dst")
			require.NoError(t, err)
			require.NotEmpty(t, rows)
			sourceRows, err := f.ms.ListFileChunks(ctx, "src")
			require.NoError(t, err)
			require.NotEmpty(t, sourceRows)
			require.Equal(t, sourceRows[0].Hash, rows[0].Hash, "clone must use the source's real carved content")
			// Drop every destination byte locally, reopen the actual journal, and read
			// solely through the committed manifest and packed remote blocks.
			require.NoError(t, f.bs.DiscardLocalContent(ctx, "dst"))
			require.NoError(t, f.bs.Close())
			f.open()
			f.read("dst", want)
		})
	}
}

func TestCloneSeesPendingDestinationTail(t *testing.T) {
	f := newCloneConcurrencyFixture(t, true)
	ctx := context.Background()
	src, dst := f.file("src"), f.file("dst")
	source := bytes.Repeat([]byte{0x18}, 4096)
	old := bytes.Repeat([]byte{0x29}, 8192)
	require.NoError(t, f.write(ctx, src, "src", source, 0))
	require.NoError(t, f.write(ctx, dst, "dst", old, 0))
	persisted, err := f.ms.GetFile(ctx, dst)
	require.NoError(t, err)
	require.Less(t, persisted.Size, uint64(len(old)))
	err = common.CloneWholeFile(ctx, f.bs, f.ms, nil, src, dst, "dst", 0, f.svc)
	var se *metadata.StoreError
	require.True(t, errors.As(err, &se))
	require.Equal(t, metadata.ErrNotSupported, se.Code)
	f.read("dst", old)
	after, err := f.ms.GetFile(ctx, dst)
	require.NoError(t, err)
	require.Equal(t, uint64(len(old)), after.Size)
}

func TestCloneReverseAndSelf(t *testing.T) {
	f := newCloneConcurrencyFixture(t, true)
	ctx := context.Background()
	a, b := f.file("a"), f.file("b")
	data := bytes.Repeat([]byte{0x32}, 4096)
	require.NoError(t, f.write(ctx, a, "a", data, 0))
	require.NoError(t, f.write(ctx, b, "b", data, 0))
	done := make(chan error, 2)
	go func() { done <- common.CloneWholeFile(ctx, f.bs, f.ms, nil, a, b, "b", 0, f.svc) }()
	go func() { done <- common.CloneWholeFile(ctx, f.bs, f.ms, nil, b, a, "a", 0, f.svc) }()
	require.NoError(t, receiveCloneResult(t, done))
	require.NoError(t, receiveCloneResult(t, done))
	require.NoError(t, common.CloneWholeFile(ctx, f.bs, f.ms, nil, a, a, "a", 0, f.svc))
	f.read("a", data)
	f.read("b", data)
}

// The gate is before the journal flush, but held until its final metadata
// callback returns. Pausing PutBlock freezes a real old-destination carve.
type clonePausedRemote struct {
	*remotememory.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *clonePausedRemote) PutBlock(ctx context.Context, id string, data io.Reader) error {
	r.once.Do(func() { close(r.entered); <-r.release })
	return r.Store.PutBlock(ctx, id, data)
}

func TestCloneWaitsForOldDestinationCarve(t *testing.T) {
	f := newCloneConcurrencyFixture(t, true)
	ctx := context.Background()
	src, dst := f.file("src"), f.file("dst")
	source := bytes.Repeat([]byte{0x27}, 4096)
	old := bytes.Repeat([]byte{0x38}, 4096)
	require.NoError(t, f.write(ctx, src, "src", source, 0))
	f.drain(src, "src")
	require.NoError(t, f.write(ctx, dst, "dst", old, 0))
	paused := &clonePausedRemote{Store: f.remote, entered: make(chan struct{}), release: make(chan struct{})}
	f.syncer.SetRemoteBlockStore(paused)
	flushDone := make(chan error, 1)
	go func() { _, err := f.syncer.Flush(ctx, "dst"); flushDone <- err }()
	select {
	case <-paused.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("destination did not start carving")
	}
	cloneDone := make(chan error, 1)
	go func() { cloneDone <- common.CloneWholeFile(ctx, f.bs, f.ms, nil, src, dst, "dst", 0, f.svc) }()
	select {
	case err := <-cloneDone:
		close(paused.release)
		t.Fatalf("clone escaped old destination carve: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(paused.release)
	require.NoError(t, receiveCloneResult(t, flushDone))
	require.NoError(t, receiveCloneResult(t, cloneDone))
	f.read("dst", source)
	require.NoError(t, f.bs.DiscardLocalContent(ctx, "dst"))
	f.read("dst", source)
	srcRows, err := f.ms.ListFileChunks(ctx, "src")
	require.NoError(t, err)
	dstRows, err := f.ms.ListFileChunks(ctx, "dst")
	require.NoError(t, err)
	require.Len(t, dstRows, len(srcRows))
	require.Equal(t, srcRows[0].Hash, dstRows[0].Hash)
}

type clonePendingFlushBarrier struct {
	metadata.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *clonePendingFlushBarrier) WithTransaction(ctx context.Context, fn func(metadata.Transaction) error) error {
	s.once.Do(func() { close(s.entered); <-s.release })
	return s.Store.WithTransaction(ctx, fn)
}

func TestCloneWaitsForPoppedPendingMetadata(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%v", shutdown), func(t *testing.T) {
			f := newCloneConcurrencyFixture(t, true)
			ctx := context.Background()
			src, dst := f.file("src"), f.file("dst")
			source := bytes.Repeat([]byte{0x17}, 4096)
			old := bytes.Repeat([]byte{0x28}, 8192)
			require.NoError(t, f.write(ctx, src, "src", source, 0))
			f.drain(src, "src")
			require.NoError(t, f.write(ctx, dst, "dst", old, 0))
			_, err := f.bs.Flush(ctx, "dst")
			require.NoError(t, err)
			barrier := &clonePendingFlushBarrier{Store: f.ms, entered: make(chan struct{}), release: make(chan struct{})}
			f.svc.RemoveStoreForShare("/clone")
			require.NoError(t, f.svc.RegisterStoreForShare("/clone", barrier))
			flushed := make(chan error, 1)
			go func() {
				var err error
				if shutdown {
					_, err = f.svc.FlushAllPendingWritesForShutdown(10 * time.Second)
				} else {
					_, err = f.svc.FlushPendingWriteForFile(f.auth, dst, true)
				}
				flushed <- err
			}()
			select {
			case <-barrier.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("metadata flush never reached commit")
			}
			cloned := make(chan error, 1)
			go func() { cloned <- common.CloneWholeFile(ctx, f.bs, f.ms, nil, src, dst, "dst", 0, f.svc) }()
			select {
			case err := <-cloned:
				close(barrier.release)
				t.Fatalf("clone missed in-flight pending metadata: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			close(barrier.release)
			require.NoError(t, receiveCloneResult(t, flushed))
			err = receiveCloneResult(t, cloned)
			var se *metadata.StoreError
			require.True(t, errors.As(err, &se))
			require.Equal(t, metadata.ErrNotSupported, se.Code)
			f.read("dst", old)
		})
	}
}

func TestCloneRefreshesWriteAttributes(t *testing.T) {
	f := newCloneConcurrencyFixture(t, true)
	ctx := context.Background()
	src, dst := f.file("src"), f.file("dst")
	require.NoError(t, f.write(ctx, src, "src", bytes.Repeat([]byte{0x19}, 8192), 0))
	require.NoError(t, f.write(ctx, dst, "dst", bytes.Repeat([]byte{0x20}, 4096), 0))
	require.NoError(t, common.CloneWholeFile(ctx, f.bs, f.ms, nil, src, dst, "dst", 0, f.svc))
	require.NoError(t, f.bs.WithPayloadScope(ctx, []string{"dst"}, false, func(ctx context.Context) error {
		auth := *f.auth
		auth.Context = ctx
		intent, err := f.svc.PrepareWrite(&auth, dst, 1)
		if err != nil {
			return err
		}
		require.Equal(t, uint64(8192), intent.PreWriteAttr.Size, "next WRITE must observe cloned size rather than the previous cached destination")
		return nil
	}))
}
