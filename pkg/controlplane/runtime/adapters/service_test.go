package adapters

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/health"
)

// fakeAdapterStore is a minimal in-memory store.AdapterStore. Only the CRUD
// methods the Service touches are implemented; the rest are never called and
// would nil-panic via the embedded interface, which is the intent.
type fakeAdapterStore struct {
	store.AdapterStore
	mu     sync.Mutex
	byType map[string]*models.AdapterConfig
}

func newFakeAdapterStore() *fakeAdapterStore {
	return &fakeAdapterStore{byType: make(map[string]*models.AdapterConfig)}
}

func (f *fakeAdapterStore) CreateAdapter(_ context.Context, a *models.AdapterConfig) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *a
	f.byType[a.Type] = &cp
	return a.Type, nil
}

func (f *fakeAdapterStore) UpdateAdapter(_ context.Context, a *models.AdapterConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *a
	f.byType[a.Type] = &cp
	return nil
}

func (f *fakeAdapterStore) DeleteAdapter(_ context.Context, t string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.byType, t)
	return nil
}

// fakeListenerAdapter binds a real loopback TCP listener in Serve so a test can
// assert the socket (and its FD) survives a reload untouched.
type fakeListenerAdapter struct {
	protocol string
	port     int

	mu        sync.Mutex
	ln        net.Listener
	ready     chan struct{}
	stopCount atomic.Int32
}

func newFakeListenerAdapter(protocol string, port int) *fakeListenerAdapter {
	return &fakeListenerAdapter{protocol: protocol, port: port, ready: make(chan struct{})}
}

func (a *fakeListenerAdapter) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.ln = ln
	a.mu.Unlock()
	close(a.ready)
	<-ctx.Done()
	return ctx.Err()
}

func (a *fakeListenerAdapter) Stop(context.Context) error {
	a.stopCount.Add(1)
	a.mu.Lock()
	ln := a.ln
	a.mu.Unlock()
	if ln != nil {
		return ln.Close()
	}
	return nil
}

func (a *fakeListenerAdapter) Protocol() string                          { return a.protocol }
func (a *fakeListenerAdapter) Port() int                                 { return a.port }
func (a *fakeListenerAdapter) Healthcheck(context.Context) health.Report { return health.Report{} }

func (a *fakeListenerAdapter) listener() net.Listener {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ln
}

func listenerFD(t *testing.T, ln net.Listener) uintptr {
	t.Helper()
	raw, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	var fd uintptr
	if err := raw.Control(func(f uintptr) { fd = f }); err != nil {
		t.Fatalf("Control: %v", err)
	}
	return fd
}

func waitReady(t *testing.T, a *fakeListenerAdapter) {
	t.Helper()
	select {
	case <-a.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("adapter did not start listening in time")
	}
}

// TestUpdateAdapter_PreservesListenerWhenBindUnchanged proves that reloading an
// adapter with an unchanged listen address keeps the exact same listener
// socket (same FD) — the running adapter is never stopped — and that a real
// listen-address change still rebinds.
func TestUpdateAdapter_PreservesListenerWhenBindUnchanged(t *testing.T) {
	const port = 14445

	var created []*fakeListenerAdapter
	var mu sync.Mutex

	svc := New(newFakeAdapterStore(), time.Second)
	svc.SetAdapterFactory(func(cfg *models.AdapterConfig) (ProtocolAdapter, error) {
		a := newFakeListenerAdapter(cfg.Type, cfg.Port)
		mu.Lock()
		created = append(created, a)
		mu.Unlock()
		return a, nil
	})

	ctx := context.Background()
	cfg := &models.AdapterConfig{Type: "smb", Enabled: true, Port: port}
	if err := svc.CreateAdapter(ctx, cfg); err != nil {
		t.Fatalf("CreateAdapter: %v", err)
	}

	mu.Lock()
	first := created[0]
	mu.Unlock()
	waitReady(t, first)

	fdBefore := listenerFD(t, first.listener())

	// Reload with the same listen address: the listener must survive.
	if err := svc.UpdateAdapter(ctx, &models.AdapterConfig{Type: "smb", Enabled: true, Port: port}); err != nil {
		t.Fatalf("UpdateAdapter (unchanged): %v", err)
	}

	mu.Lock()
	nCreated := len(created)
	mu.Unlock()
	if nCreated != 1 {
		t.Fatalf("factory called again on unchanged reload: created %d adapters, want 1", nCreated)
	}
	if got := first.stopCount.Load(); got != 0 {
		t.Fatalf("running adapter was stopped on unchanged reload: stopCount=%d, want 0", got)
	}
	if svc.GetAdapter("smb") != first {
		t.Fatal("running adapter instance was swapped on unchanged reload")
	}
	if fdAfter := listenerFD(t, first.listener()); fdAfter != fdBefore {
		t.Fatalf("listener FD changed across reload: before=%d after=%d", fdBefore, fdAfter)
	}
	// The socket is still live and accepting.
	c, err := net.DialTimeout("tcp", first.listener().Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("listener not accepting after reload: %v", err)
	}
	_ = c.Close()

	// A genuine listen-address change must rebind: stop the old, start a new.
	if err := svc.UpdateAdapter(ctx, &models.AdapterConfig{Type: "smb", Enabled: true, Port: port + 1}); err != nil {
		t.Fatalf("UpdateAdapter (port change): %v", err)
	}
	if got := first.stopCount.Load(); got != 1 {
		t.Fatalf("old adapter not stopped on port change: stopCount=%d, want 1", got)
	}
	mu.Lock()
	nCreated = len(created)
	second := created[len(created)-1]
	mu.Unlock()
	if nCreated != 2 {
		t.Fatalf("port change did not rebind: created %d adapters, want 2", nCreated)
	}
	waitReady(t, second)
	if svc.GetAdapter("smb") != second {
		t.Fatal("port change did not swap to the new adapter instance")
	}

	if err := svc.StopAllAdapters(); err != nil {
		t.Fatalf("StopAllAdapters: %v", err)
	}
}
