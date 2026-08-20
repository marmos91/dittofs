package adapters

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
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

func (f *fakeAdapterStore) GetAdapter(_ context.Context, t string) (*models.AdapterConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.byType[t]
	if !ok {
		return nil, errors.New("adapter not found")
	}
	cp := *a
	return &cp, nil
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
func (a *fakeListenerAdapter) ListenerReady() <-chan struct{}            { return a.ready }

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

// TestUpdateAdapter_ZeroPortRebindsFromNonDefault proves that updating an
// adapter bound to a non-default port with an explicit port 0 ("use the
// default") rebinds to the default port instead of silently preserving the old
// listener — which would leave the running port and the persisted (port 0)
// config out of sync.
func TestUpdateAdapter_ZeroPortRebindsFromNonDefault(t *testing.T) {
	const nonDefaultPort = 14445

	var created []*fakeListenerAdapter
	var mu sync.Mutex

	svc := New(newFakeAdapterStore(), time.Second)
	// Mirror the real factory: a zero port resolves to the type's default.
	svc.SetAdapterFactory(func(cfg *models.AdapterConfig) (ProtocolAdapter, error) {
		a := newFakeListenerAdapter(cfg.Type, resolvePort(cfg.Type, cfg.Port))
		mu.Lock()
		created = append(created, a)
		mu.Unlock()
		return a, nil
	})

	ctx := context.Background()
	if err := svc.CreateAdapter(ctx, &models.AdapterConfig{Type: "smb", Enabled: true, Port: nonDefaultPort}); err != nil {
		t.Fatalf("CreateAdapter: %v", err)
	}

	mu.Lock()
	first := created[0]
	mu.Unlock()
	waitReady(t, first)

	// Update with an explicit port 0: resolves to the SMB default, so the
	// listen address changes and the adapter must rebind.
	if err := svc.UpdateAdapter(ctx, &models.AdapterConfig{Type: "smb", Enabled: true, Port: 0}); err != nil {
		t.Fatalf("UpdateAdapter (port 0): %v", err)
	}

	if got := first.stopCount.Load(); got != 1 {
		t.Fatalf("old adapter not stopped on rebind to default: stopCount=%d, want 1", got)
	}
	mu.Lock()
	nCreated := len(created)
	second := created[len(created)-1]
	mu.Unlock()
	if nCreated != 2 {
		t.Fatalf("port 0 did not rebind: created %d adapters, want 2", nCreated)
	}
	waitReady(t, second)
	if svc.GetAdapter("smb") != second {
		t.Fatal("rebind did not swap to the new adapter instance")
	}
	if got := second.Port(); got != models.DefaultSMBPort {
		t.Fatalf("rebound adapter bound to wrong port: got %d, want default %d", got, models.DefaultSMBPort)
	}

	if err := svc.StopAllAdapters(); err != nil {
		t.Fatalf("StopAllAdapters: %v", err)
	}
}

// slowStopAdapter keeps its serve goroutine alive after Stop returns, so a test
// can observe the window between "stop requested" and "adapter actually gone".
type slowStopAdapter struct {
	protocol string
	port     int
	// stopOnce guards stopCalled: a teardown that times out leaves the entry in
	// place, so a later attempt calls Stop on the same adapter again.
	stopOnce   sync.Once
	stopCalled chan struct{}
	release    chan struct{}
	ready      chan struct{}
}

func newSlowStopAdapter(protocol string, port int) *slowStopAdapter {
	return &slowStopAdapter{
		protocol:   protocol,
		port:       port,
		stopCalled: make(chan struct{}),
		release:    make(chan struct{}),
		ready:      make(chan struct{}),
	}
}

func (a *slowStopAdapter) Serve(ctx context.Context) error {
	close(a.ready)
	<-ctx.Done()
	<-a.release
	return ctx.Err()
}

func (a *slowStopAdapter) Stop(context.Context) error {
	a.stopOnce.Do(func() { close(a.stopCalled) })
	return nil
}

func (a *slowStopAdapter) Protocol() string                          { return a.protocol }
func (a *slowStopAdapter) Port() int                                 { return a.port }
func (a *slowStopAdapter) Healthcheck(context.Context) health.Report { return health.Report{} }
func (a *slowStopAdapter) ListenerReady() <-chan struct{}            { return a.ready }

// TestStopAdapter_HoldsEntryUntilAdapterConfirmsStopped proves the registry
// keeps describing an adapter that is still alive: a start of the same type
// issued mid-teardown is refused rather than racing the outgoing adapter for
// its listening socket, and the entry disappears only once the serve goroutine
// has exited.
func TestStopAdapter_HoldsEntryUntilAdapterConfirmsStopped(t *testing.T) {
	svc := New(newFakeAdapterStore(), 5*time.Second)

	outgoing := newSlowStopAdapter("nfs", 12049)
	if err := svc.AddAdapter(outgoing); err != nil {
		t.Fatalf("AddAdapter: %v", err)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- svc.stopAdapter("nfs") }()

	select {
	case <-outgoing.stopCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("stopAdapter never reached the adapter's Stop")
	}

	// Teardown is in flight and the serve goroutine still holds the socket.
	if !svc.IsAdapterRunning("nfs") {
		t.Fatal("adapter reported as not running while its serve goroutine is still alive")
	}
	replacement := newSlowStopAdapter("nfs", 12049)
	if err := svc.AddAdapter(replacement); err == nil {
		t.Fatal("AddAdapter admitted a second nfs adapter while the previous one was still stopping")
	}

	close(outgoing.release)
	if err := <-stopped; err != nil {
		t.Fatalf("stopAdapter: %v", err)
	}

	if svc.IsAdapterRunning("nfs") {
		t.Fatal("entry not removed after the adapter confirmed it stopped")
	}
	if err := svc.AddAdapter(replacement); err != nil {
		t.Fatalf("AddAdapter after confirmed stop: %v", err)
	}

	close(replacement.release)
	if err := svc.StopAllAdapters(); err != nil {
		t.Fatalf("StopAllAdapters: %v", err)
	}
}

// TestUpdateAdapter_ReturnsRestartFailure proves a failed restart is reported to
// the caller instead of leaving it with a success response for an adapter that
// is down, and that the requested config stays persisted so the next start
// retries it.
func TestUpdateAdapter_ReturnsRestartFailure(t *testing.T) {
	st := newFakeAdapterStore()
	svc := New(st, time.Second)

	var factoryCalls atomic.Int32
	svc.SetAdapterFactory(func(cfg *models.AdapterConfig) (ProtocolAdapter, error) {
		if factoryCalls.Add(1) > 1 {
			return nil, errors.New("listen: address already in use")
		}
		return newFakeListenerAdapter(cfg.Type, cfg.Port), nil
	})

	ctx := context.Background()
	if err := svc.CreateAdapter(ctx, &models.AdapterConfig{Type: "smb", Enabled: true, Port: 14445}); err != nil {
		t.Fatalf("CreateAdapter: %v", err)
	}

	err := svc.UpdateAdapter(ctx, &models.AdapterConfig{Type: "smb", Enabled: true, Port: 14446})
	if err == nil {
		t.Fatal("UpdateAdapter reported success although the adapter failed to restart")
	}
	if svc.IsAdapterRunning("smb") {
		t.Fatal("adapter reported as running after a failed restart")
	}

	st.mu.Lock()
	persisted := st.byType["smb"]
	st.mu.Unlock()
	if persisted == nil || persisted.Port != 14446 {
		t.Fatalf("requested config not persisted after failed restart: %+v", persisted)
	}
}

// TestUpdateAdapter_NoPreservedListenerAfterStopTimeout proves that a reload
// following a teardown that timed out does not report success by claiming to
// preserve a listener. The timed-out stop has already cancelled the entry's
// context, so the adapter behind it is on its way out and its socket is not
// reusable; treating the retained entry as a live listener would hand the caller
// a success for an adapter that is going away.
func TestUpdateAdapter_NoPreservedListenerAfterStopTimeout(t *testing.T) {
	svc := New(newFakeAdapterStore(), 50*time.Millisecond)

	stuck := newSlowStopAdapter("nfs", 12049)
	if err := svc.AddAdapter(stuck); err != nil {
		t.Fatalf("AddAdapter: %v", err)
	}

	// The serve goroutine never returns, so the stop cannot confirm and times out.
	if err := svc.stopAdapter("nfs"); err == nil {
		t.Fatal("stopAdapter reported success while the serve goroutine was still running")
	}

	// The entry is deliberately still held, so a competing start stays refused.
	if err := svc.AddAdapter(newSlowStopAdapter("nfs", 12049)); err == nil {
		t.Fatal("AddAdapter admitted a second nfs adapter over a cancelled entry")
	}

	err := svc.UpdateAdapter(context.Background(), &models.AdapterConfig{
		Type: "nfs", Port: 12049, Enabled: true,
	})
	if err == nil {
		t.Fatal("UpdateAdapter returned success for an adapter whose context was already cancelled")
	}

	close(stuck.release)
}

// A failed start must not leave the persisted config claiming the adapter is
// enabled.
func TestEnableAdapter_RollsBackEnabledOnStartFailure(t *testing.T) {
	st := newFakeAdapterStore()
	svc := New(st, time.Second)

	startFails := false
	svc.SetAdapterFactory(func(cfg *models.AdapterConfig) (ProtocolAdapter, error) {
		if startFails {
			return nil, errors.New("boom")
		}
		return newFakeListenerAdapter(cfg.Type, cfg.Port), nil
	})

	ctx := context.Background()
	if err := svc.CreateAdapter(ctx, &models.AdapterConfig{Type: "nfs", Enabled: false, Port: 14449}); err != nil {
		t.Fatalf("CreateAdapter: %v", err)
	}

	startFails = true
	if err := svc.EnableAdapter(ctx, "nfs"); err == nil {
		t.Fatal("expected EnableAdapter to fail when the adapter cannot start")
	}

	stored, err := st.GetAdapter(ctx, "nfs")
	if err != nil {
		t.Fatalf("GetAdapter: %v", err)
	}
	if stored.Enabled {
		t.Fatal("persisted config still claims the adapter is enabled after a failed start")
	}
}

// squattedPortAdapter fails to bind because something else already holds its
// port, the same way a real adapter fails when the kernel SMB listener or
// another process owns the port an operator asked for.
type squattedPortAdapter struct {
	protocol string
	port     int
	ready    chan struct{}
}

func newSquattedPortAdapter(protocol string, port int) *squattedPortAdapter {
	return &squattedPortAdapter{protocol: protocol, port: port, ready: make(chan struct{})}
}

func (a *squattedPortAdapter) Serve(context.Context) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", a.port))
	if err != nil {
		return err
	}
	close(a.ready)
	defer func() { _ = ln.Close() }()
	return nil
}

func (a *squattedPortAdapter) Stop(context.Context) error                { return nil }
func (a *squattedPortAdapter) Protocol() string                          { return a.protocol }
func (a *squattedPortAdapter) Port() int                                 { return a.port }
func (a *squattedPortAdapter) Healthcheck(context.Context) health.Report { return health.Report{} }
func (a *squattedPortAdapter) ListenerReady() <-chan struct{}            { return a.ready }

// squatPort binds a loopback port and keeps it for the test, returning the
// port number nothing else can claim while the test runs.
func squatPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("squat listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// TestStartAdapter_ReportsBindFailure proves a start reports the outcome of the
// bind rather than the intent to serve. Binding happens inside Serve, which
// runs in its own goroutine, so a start that returns early hands back success
// for a listener that never came up — the caller then advertises an adapter
// that is not serving, and clients reaching the port find whatever else owns
// it. The seam is shared by every adapter type, so both are checked.
func TestStartAdapter_ReportsBindFailure(t *testing.T) {
	for _, adapterType := range []string{"smb", "nfs"} {
		t.Run(adapterType, func(t *testing.T) {
			port := squatPort(t)

			st := newFakeAdapterStore()
			svc := New(st, time.Second)
			svc.SetAdapterFactory(func(cfg *models.AdapterConfig) (ProtocolAdapter, error) {
				return newSquattedPortAdapter(cfg.Type, cfg.Port), nil
			})

			cfg := &models.AdapterConfig{Type: adapterType, Port: port, Enabled: false}
			if _, err := st.CreateAdapter(context.Background(), cfg); err != nil {
				t.Fatalf("seed adapter: %v", err)
			}

			err := svc.EnableAdapter(context.Background(), adapterType)
			if err == nil {
				t.Fatal("EnableAdapter reported success for an adapter that never bound its port")
			}
			if !strings.Contains(err.Error(), "address already in use") {
				t.Fatalf("error does not name the bind failure: %v", err)
			}

			if svc.IsAdapterRunning(adapterType) {
				t.Error("adapter left registered as running after a failed bind")
			}

			stored, err := st.GetAdapter(context.Background(), adapterType)
			if err != nil {
				t.Fatalf("GetAdapter: %v", err)
			}
			if stored.Enabled {
				t.Error("store still advertises the adapter as enabled after a failed bind")
			}
		})
	}
}

// wedgedAdapter never binds and never returns from Serve, standing in for setup
// that hangs before the listener is created.
type wedgedAdapter struct {
	protocol string
	port     int
	release  chan struct{}
	ready    chan struct{}
}

func newWedgedAdapter(protocol string, port int) *wedgedAdapter {
	return &wedgedAdapter{
		protocol: protocol,
		port:     port,
		release:  make(chan struct{}),
		ready:    make(chan struct{}),
	}
}

func (a *wedgedAdapter) Serve(context.Context) error {
	<-a.release
	return nil
}

func (a *wedgedAdapter) Stop(context.Context) error                { return nil }
func (a *wedgedAdapter) Protocol() string                          { return a.protocol }
func (a *wedgedAdapter) Port() int                                 { return a.port }
func (a *wedgedAdapter) Healthcheck(context.Context) health.Report { return health.Report{} }
func (a *wedgedAdapter) ListenerReady() <-chan struct{}            { return a.ready }

// TestStartAdapter_TimesOutWhenListenerNeverBinds proves the wait is bounded:
// an adapter that wedges before binding fails the start instead of blocking the
// caller forever. Its entry stays behind because the adapter is still alive and
// may yet claim the socket.
func TestStartAdapter_TimesOutWhenListenerNeverBinds(t *testing.T) {
	svc := New(newFakeAdapterStore(), time.Second)
	svc.startTimeout = 50 * time.Millisecond

	wedged := newWedgedAdapter("nfs", 12049)
	svc.SetAdapterFactory(func(cfg *models.AdapterConfig) (ProtocolAdapter, error) {
		return wedged, nil
	})

	err := svc.CreateAdapter(context.Background(), &models.AdapterConfig{
		Type: "nfs", Port: 12049, Enabled: true,
	})
	if err == nil {
		t.Fatal("CreateAdapter reported success for an adapter that never bound")
	}
	if !strings.Contains(err.Error(), "listener not ready") {
		t.Fatalf("error does not name the wait: %v", err)
	}

	close(wedged.release)
}
