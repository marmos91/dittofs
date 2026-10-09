package metadata_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/stretchr/testify/require"
)

func lifecycleHasCode(err error, code metadata.ErrorCode) bool {
	var e *metadata.StoreError
	return errors.As(err, &e) && e.Code == code
}

type lifecyclePauseKey struct{}
type lifecycleFaultKey struct{}

// The barrier returns the real advisory result only after the competing
// operation commits. Transactional reads are never paused by this barrier.
type lifecyclePause struct {
	site, handle, name string
	reached, resume    chan struct{}
	once, releaseOnce  sync.Once
}

func (p *lifecyclePause) release() { p.releaseOnce.Do(func() { close(p.resume) }) }

func lifecycleWait(ctx context.Context, site string, handle metadata.FileHandle, name string) {
	p, _ := ctx.Value(lifecyclePauseKey{}).(*lifecyclePause)
	if p != nil && p.site == site && p.handle == string(handle) && p.name == name {
		p.once.Do(func() { close(p.reached); <-p.resume })
	}
}

type lifecycleStore struct {
	metadata.Store
	inner         *badger.BadgerMetadataStore
	beforeDurable func(context.Context)
	afterDurable  func(context.Context, error)
	beforeRelaxed func(context.Context)
}

func (s *lifecycleStore) WithTransaction(ctx context.Context, fn func(metadata.Transaction) error) error {
	if s.beforeDurable != nil {
		s.beforeDurable(ctx)
	}
	err := s.Store.WithTransaction(ctx, fn)
	if s.afterDurable != nil {
		s.afterDurable(ctx, err)
	}
	return err
}

func (s *lifecycleStore) GetChild(ctx context.Context, parent metadata.FileHandle, name string) (metadata.FileHandle, error) {
	h, err := s.Store.GetChild(ctx, parent, name)
	lifecycleWait(ctx, "child", parent, name)
	return h, err
}

func (s *lifecycleStore) ListChildren(ctx context.Context, parent metadata.FileHandle, cursor string, limit int, attrs metadata.ChildAttrs) ([]metadata.DirEntry, string, error) {
	entries, next, err := s.Store.ListChildren(ctx, parent, cursor, limit, attrs)
	lifecycleWait(ctx, "list", parent, "")
	return entries, next, err
}

func (s *lifecycleStore) WithTransactionRelaxed(ctx context.Context, fn func(metadata.Transaction) error) error {
	if s.beforeRelaxed != nil {
		s.beforeRelaxed(ctx)
	}
	return s.inner.WithTransactionRelaxed(ctx, func(tx metadata.Transaction) error {
		return fn(lifecycleTransaction{Transaction: tx})
	})
}

type lifecycleFault struct {
	site, handle string
	err          error
	nilFile      bool
}

type lifecycleTransaction struct{ metadata.Transaction }

func (tx lifecycleTransaction) GetChild(ctx context.Context, h metadata.FileHandle, name string) (metadata.FileHandle, error) {
	if f, _ := ctx.Value(lifecycleFaultKey{}).(*lifecycleFault); f != nil && f.site == "child" && f.handle == string(h) {
		return nil, f.err
	}
	return tx.Transaction.GetChild(ctx, h, name)
}

func (tx lifecycleTransaction) GetFile(ctx context.Context, h metadata.FileHandle) (*metadata.File, error) {
	if f, _ := ctx.Value(lifecycleFaultKey{}).(*lifecycleFault); f != nil && f.site == "file" && f.handle == string(h) {
		if f.nilFile {
			return nil, nil
		}
		return nil, f.err
	}
	return tx.Transaction.GetFile(ctx, h)
}

func (tx lifecycleTransaction) ListChildren(ctx context.Context, h metadata.FileHandle, cursor string, limit int, attrs metadata.ChildAttrs) ([]metadata.DirEntry, string, error) {
	if f, _ := ctx.Value(lifecycleFaultKey{}).(*lifecycleFault); f != nil && f.site == "list" && f.handle == string(h) {
		return nil, "", f.err
	}
	return tx.Transaction.ListChildren(ctx, h, cursor, limit, attrs)
}

type lifecycleFixture struct {
	store *badger.BadgerMetadataStore
	svc   *metadata.Service
	root  metadata.FileHandle
	auth  *metadata.AuthContext
}

func newLifecycleFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	ctx := context.Background()
	store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	root, err := store.CreateRootDirectory(ctx, "/lifecycle", &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777})
	require.NoError(t, err)
	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare("/lifecycle", &lifecycleStore{Store: store, inner: store}))
	return &lifecycleFixture{store: store, svc: svc, root: lifecycleHandle(t, root), auth: &metadata.AuthContext{
		Context: ctx, AuthMethod: "unix",
		Identity: &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
	}}
}

func lifecycleHandle(t *testing.T, file *metadata.File) metadata.FileHandle {
	t.Helper()
	h, err := metadata.EncodeFileHandle(file)
	require.NoError(t, err)
	return h
}

func (f *lifecycleFixture) create(t *testing.T, parent metadata.FileHandle, name string, directory bool) metadata.FileHandle {
	t.Helper()
	var file *metadata.File
	var err error
	if directory {
		file, _, err = f.svc.CreateDirectory(f.auth, parent, name, &metadata.FileAttr{Mode: 0o777})
	} else {
		file, _, err = f.svc.CreateFile(f.auth, parent, name, &metadata.FileAttr{Mode: 0o666})
	}
	require.NoError(t, err)
	return lifecycleHandle(t, file)
}

func (f *lifecycleFixture) pause(t *testing.T, site string, parent metadata.FileHandle, name string) (*metadata.AuthContext, *lifecyclePause) {
	t.Helper()
	p := &lifecyclePause{site: site, handle: string(parent), name: name, reached: make(chan struct{}), resume: make(chan struct{})}
	t.Cleanup(p.release)
	auth := *f.auth
	auth.Context = context.WithValue(auth.Context, lifecyclePauseKey{}, p)
	return &auth, p
}

func lifecycleReached(t *testing.T, p *lifecyclePause) {
	t.Helper()
	select {
	case <-p.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("operation never reached its advisory-read barrier")
	}
}

func lifecycleResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("namespace operation did not complete")
		return nil
	}
}

func (f *lifecycleFixture) assertChild(t *testing.T, parent metadata.FileHandle, name string, expected metadata.FileHandle) {
	t.Helper()
	actual, err := f.store.GetChild(f.auth.Context, parent, name)
	require.NoError(t, err)
	require.Equal(t, expected, actual)
	_, err = f.store.GetFile(f.auth.Context, actual)
	require.NoError(t, err, "the surviving name must resolve to a live inode")
}

func TestDirectoryLifecycleRmdirRechecks(t *testing.T) {
	for _, operation := range []string{"create", "move-in", "hard-link", "rename-and-replace"} {
		t.Run(operation, func(t *testing.T) {
			f := newLifecycleFixture(t)
			dir := f.create(t, f.root, "dir", true)
			source := f.create(t, f.root, "source", false)
			auth, pause := f.pause(t, "list", dir, "")
			done := make(chan error, 1)
			go func() { _, err := f.svc.RemoveDirectory(auth, f.root, "dir"); done <- err }()
			lifecycleReached(t, pause)
			var child metadata.FileHandle
			switch operation {
			case "create":
				child = f.create(t, dir, "child", false)
			case "move-in":
				_, _, err := f.svc.Move(f.auth, f.root, "source", dir, "child")
				require.NoError(t, err)
				child = source
			case "hard-link":
				_, err := f.svc.CreateHardLink(f.auth, dir, "child", source)
				require.NoError(t, err)
				child = source
			case "rename-and-replace":
				_, _, err := f.svc.Move(f.auth, f.root, "dir", f.root, "moved")
				require.NoError(t, err)
				child = f.create(t, f.root, "dir", true)
			}
			pause.release()
			err := lifecycleResult(t, done)
			require.Error(t, err, "rmdir must not act on its stale empty-directory result")
			if operation == "rename-and-replace" {
				require.True(t, lifecycleHasCode(err, metadata.ErrConflict))
				f.assertChild(t, f.root, "moved", dir)
				f.assertChild(t, f.root, "dir", child)
			} else {
				require.True(t, lifecycleHasCode(err, metadata.ErrNotEmpty))
				f.assertChild(t, f.root, "dir", dir)
				f.assertChild(t, dir, "child", child)
			}
		})
	}
}

func TestDirectoryLifecycleRejectsRemovedParent(t *testing.T) {
	for _, operation := range []string{"create", "hard-link", "move-in"} {
		t.Run(operation, func(t *testing.T) {
			f := newLifecycleFixture(t)
			dir := f.create(t, f.root, "dir", true)
			source := f.create(t, f.root, "source", false)
			auth, pause := f.pause(t, "child", dir, "child")
			done := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "create":
					_, _, err = f.svc.CreateFile(auth, dir, "child", &metadata.FileAttr{Mode: 0o666})
				case "hard-link":
					_, err = f.svc.CreateHardLink(auth, dir, "child", source)
				case "move-in":
					_, _, err = f.svc.Move(auth, f.root, "source", dir, "child")
				}
				done <- err
			}()
			lifecycleReached(t, pause)
			_, err := f.svc.RemoveDirectory(f.auth, f.root, "dir")
			require.NoError(t, err)
			pause.release()
			require.Error(t, lifecycleResult(t, done), "no child may be committed below a removed parent")
			_, err = f.store.GetChild(f.auth.Context, dir, "child")
			require.True(t, metadata.IsNotFoundError(err))
			_, err = f.store.GetFile(f.auth.Context, dir)
			require.True(t, metadata.IsNotFoundError(err), "hard-link insertion must not resurrect the parent")
			f.assertChild(t, f.root, "source", source)
			links, err := f.store.GetLinkCount(f.auth.Context, source)
			require.NoError(t, err)
			require.Equal(t, uint32(1), links)
		})
	}
}

func TestDirectoryLifecycleRenameRechecksVictim(t *testing.T) {
	f := newLifecycleFixture(t)
	source := f.create(t, f.root, "source", true)
	victim := f.create(t, f.root, "victim", true)
	auth, pause := f.pause(t, "list", victim, "")
	done := make(chan error, 1)
	go func() { _, _, err := f.svc.Move(auth, f.root, "source", f.root, "victim"); done <- err }()
	lifecycleReached(t, pause)
	child := f.create(t, victim, "child", false)
	pause.release()
	err := lifecycleResult(t, done)
	require.True(t, lifecycleHasCode(err, metadata.ErrNotEmpty), "overwrite must recheck victim emptiness: %v", err)
	f.assertChild(t, f.root, "source", source)
	f.assertChild(t, f.root, "victim", victim)
	f.assertChild(t, victim, "child", child)
}

func TestDirectoryLifecycleUnlinkRechecksBinding(t *testing.T) {
	f := newLifecycleFixture(t)
	f.create(t, f.root, "victim", false)
	replacement := f.create(t, f.root, "replacement", false)
	auth, pause := f.pause(t, "child", f.root, "victim")
	done := make(chan error, 1)
	go func() { _, _, err := f.svc.RemoveFile(auth, f.root, "victim"); done <- err }()
	lifecycleReached(t, pause)
	_, _, err := f.svc.Move(f.auth, f.root, "replacement", f.root, "victim")
	require.NoError(t, err)
	pause.release()
	err = lifecycleResult(t, done)
	require.True(t, lifecycleHasCode(err, metadata.ErrConflict), "unlink must not delete a replacement name: %v", err)
	f.assertChild(t, f.root, "victim", replacement)
	links, err := f.store.GetLinkCount(f.auth.Context, replacement)
	require.NoError(t, err)
	require.Equal(t, uint32(1), links)
}

func TestDirectoryLifecycleReadErrorsRollBack(t *testing.T) {
	for _, operation := range []string{"create", "hard-link", "unlink", "rmdir", "rename"} {
		for _, fault := range []string{"parent-error", "parent-nil", "child-error", "empty-read-error"} {
			if fault == "empty-read-error" && operation != "rmdir" && operation != "rename" {
				continue
			}
			t.Run(operation+"/"+fault, func(t *testing.T) {
				f := newLifecycleFixture(t)
				source := f.create(t, f.root, "source", operation == "rename")
				victim := f.create(t, f.root, "victim", true)
				injected := errors.New("directory read failed")
				failure := &lifecycleFault{site: "file", handle: string(f.root), err: injected, nilFile: fault == "parent-nil"}
				switch fault {
				case "empty-read-error":
					failure.site, failure.handle = "list", string(victim)
				case "child-error":
					failure.site = "child"
				}
				auth := *f.auth
				auth.Context = context.WithValue(auth.Context, lifecycleFaultKey{}, failure)
				var err error
				switch operation {
				case "create":
					_, _, err = f.svc.CreateFile(&auth, f.root, "new", &metadata.FileAttr{Mode: 0o666})
				case "hard-link":
					_, err = f.svc.CreateHardLink(&auth, f.root, "new", source)
				case "unlink":
					_, _, err = f.svc.RemoveFile(&auth, f.root, "source")
				case "rmdir":
					_, err = f.svc.RemoveDirectory(&auth, f.root, "victim")
				case "rename":
					_, _, err = f.svc.Move(&auth, f.root, "source", f.root, "victim")
				}
				if fault == "parent-nil" {
					require.True(t, metadata.IsNotFoundError(err), "nil parent must abort: %v", err)
				} else {
					require.ErrorIs(t, err, injected)
				}
				f.assertChild(t, f.root, "source", source)
				f.assertChild(t, f.root, "victim", victim)
				_, err = f.store.GetChild(f.auth.Context, f.root, "new")
				require.True(t, metadata.IsNotFoundError(err))
				// A failed transaction must also release its lifecycle locks.
				f.create(t, f.root, "after-failure", false)
			})
		}
	}
}

type lifecycleNotifier struct{ callback func() }

func (n lifecycleNotifier) OnDirChange(lock.FileHandle, lock.DirChangeType, string, [16]byte, bool) {
	n.callback()
}

func TestDirectoryLifecycleNotificationRunsAfterUnlock(t *testing.T) {
	f := newLifecycleFixture(t)
	source := f.create(t, f.root, "source", true)
	destination := f.create(t, f.root, "destination", true)
	var called atomic.Bool
	callbackResult := make(chan error, 1)
	f.svc.SetDirChangeNotifier("/lifecycle", lifecycleNotifier{callback: func() {
		if !called.CompareAndSwap(false, true) {
			return
		}
		// The first create needs the moved directory's lifecycle guard; the
		// second needs a parent-link mutex held during the outer rename.
		_, _, err := f.svc.CreateDirectory(f.auth, source, "nested", &metadata.FileAttr{Mode: 0o777})
		if err == nil {
			_, _, err = f.svc.CreateDirectory(f.auth, f.root, "from-notifier", &metadata.FileAttr{Mode: 0o777})
		}
		callbackResult <- err
	}})
	done := make(chan error, 1)
	go func() { _, _, err := f.svc.Move(f.auth, f.root, "source", destination, "moved"); done <- err }()
	require.NoError(t, lifecycleResult(t, done))
	require.NoError(t, lifecycleResult(t, callbackResult))
	f.assertChild(t, destination, "moved", source)
}
