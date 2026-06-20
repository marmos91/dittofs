package lifecycle

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// ---- fakes -----------------------------------------------------------------

type fakeSettings struct {
	loadErr                  error
	loaded, started, stopped bool
}

func (f *fakeSettings) LoadInitial(ctx context.Context) error { f.loaded = true; return f.loadErr }
func (f *fakeSettings) Start(ctx context.Context)             { f.started = true }
func (f *fakeSettings) Stop()                                 { f.stopped = true }

type fakeAdapters struct {
	loadErr         error
	loaded, stopped bool
	stopErr         error
}

func (f *fakeAdapters) LoadAdaptersFromStore(ctx context.Context) error {
	f.loaded = true
	return f.loadErr
}

func (f *fakeAdapters) StopAllAdapters() error { f.stopped = true; return f.stopErr }

type fakeFlusher struct {
	called bool
	n      int
	err    error
}

func (f *fakeFlusher) FlushAllPendingWritesForShutdown(timeout time.Duration) (int, error) {
	f.called = true
	return f.n, f.err
}

type fakeStoreCloser struct{ closed bool }

func (f *fakeStoreCloser) CloseMetadataStores() { f.closed = true }

type fakeSIDStore struct {
	mu     sync.Mutex
	vals   map[string]string
	getErr error
	setHit bool
}

func (f *fakeSIDStore) GetSetting(ctx context.Context, key string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vals[key], nil
}

func (f *fakeSIDStore) SetSetting(ctx context.Context, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setHit = true
	if f.vals == nil {
		f.vals = map[string]string{}
	}
	f.vals[key] = value
	return nil
}

type fakeDrainer struct{ drained bool }

func (f *fakeDrainer) ShutdownSnapshots(ctx context.Context) { f.drained = true }

type fakeAPIServer struct {
	port     int
	stopped  bool
	startErr error
}

func (f *fakeAPIServer) Start(ctx context.Context) error {
	if f.startErr != nil {
		return f.startErr
	}
	// Block until shutdown, mirroring a real HTTP server's Start.
	<-ctx.Done()
	return nil
}

func (f *fakeAPIServer) Stop(ctx context.Context) error { f.stopped = true; return nil }
func (f *fakeAPIServer) Port() int                      { return f.port }

// ---- tests -----------------------------------------------------------------

func TestNewDefaultsTimeout(t *testing.T) {
	if got := New(0).shutdownTimeout; got != DefaultShutdownTimeout {
		t.Errorf("zero timeout = %v, want default %v", got, DefaultShutdownTimeout)
	}
	if got := New(5 * time.Second).shutdownTimeout; got != 5*time.Second {
		t.Errorf("timeout = %v, want 5s", got)
	}
}

func TestSetShutdownTimeout(t *testing.T) {
	s := New(time.Second)
	s.SetShutdownTimeout(2 * time.Second)
	if s.shutdownTimeout != 2*time.Second {
		t.Errorf("got %v, want 2s", s.shutdownTimeout)
	}
	s.SetShutdownTimeout(0)
	if s.shutdownTimeout != DefaultShutdownTimeout {
		t.Errorf("zero should reset to default, got %v", s.shutdownTimeout)
	}
}

func TestSIDMapperNilBeforeServe(t *testing.T) {
	if New(0).SIDMapper() != nil {
		t.Error("SIDMapper should be nil before Serve")
	}
}

func TestSetAPIServerPanicsAfterServe(t *testing.T) {
	s := New(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Serve returns immediately on cancelled ctx
	_ = s.Serve(ctx, nil, &fakeAdapters{}, nil, nil, nil, nil)

	defer func() {
		if recover() == nil {
			t.Error("SetAPIServer after Serve should panic")
		}
	}()
	s.SetAPIServer(&fakeAPIServer{})
}

// Serve must run shutdown in order and propagate ctx.Err() when the context is
// cancelled. With a nil SID store, an ephemeral SID is generated.
func TestServeGracefulShutdownOnCancel(t *testing.T) {
	s := New(time.Second)
	settings := &fakeSettings{}
	adapters := &fakeAdapters{}
	flusher := &fakeFlusher{n: 3}
	closer := &fakeStoreCloser{}
	drainer := &fakeDrainer{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.Serve(ctx, settings, adapters, flusher, closer, nil, drainer)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if s.SIDMapper() == nil {
		t.Error("ephemeral SID mapper not generated for nil store")
	}
	if !settings.loaded || !settings.started || !settings.stopped {
		t.Errorf("settings lifecycle incomplete: %+v", settings)
	}
	if !adapters.loaded || !adapters.stopped {
		t.Errorf("adapters lifecycle incomplete: %+v", adapters)
	}
	if !drainer.drained {
		t.Error("snapshot drainer not invoked")
	}
	if !flusher.called {
		t.Error("flusher not invoked")
	}
	if !closer.closed {
		t.Error("metadata stores not closed")
	}
}

// A failure loading adapters aborts Serve before entering the select loop and
// is returned wrapped.
func TestServeAdapterLoadFailure(t *testing.T) {
	s := New(time.Second)
	adapters := &fakeAdapters{loadErr: errors.New("boom")}
	closer := &fakeStoreCloser{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.Serve(ctx, nil, adapters, nil, closer, nil, nil)
	if err == nil {
		t.Fatal("expected error from adapter load failure")
	}
	// shutdown() is NOT reached on early adapter-load return.
	if closer.closed {
		t.Error("metadata stores should not be closed on early adapter-load failure")
	}
}

// When the API server's Start fails, Serve shuts down and returns the wrapped
// API error rather than ctx.Err().
func TestServeAPIServerFailureTriggersShutdown(t *testing.T) {
	s := New(time.Second)
	api := &fakeAPIServer{port: 8080, startErr: errors.New("bind failed")}
	s.SetAPIServer(api)

	closer := &fakeStoreCloser{}
	// Long-lived ctx so the only way out is the API error path.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := s.Serve(ctx, nil, &fakeAdapters{}, nil, closer, nil, nil)
	if err == nil || !errors.Is(err, api.startErr) {
		t.Errorf("err = %v, want wrapped %v", err, api.startErr)
	}
	if !closer.closed {
		t.Error("shutdown should have closed metadata stores")
	}
	if !api.stopped {
		t.Error("API server Stop not called during shutdown")
	}
}

// Serve uses sync.Once: a second call is a no-op and returns nil.
func TestServeOnlyRunsOnce(t *testing.T) {
	s := New(time.Second)
	adapters := &fakeAdapters{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = s.Serve(ctx, nil, adapters, nil, nil, nil, nil)

	second := &fakeAdapters{}
	if err := s.Serve(ctx, nil, second, nil, nil, nil, nil); err != nil {
		t.Errorf("second Serve = %v, want nil", err)
	}
	if second.loaded {
		t.Error("second Serve should not have loaded adapters")
	}
}

// initMachineSID loads a previously persisted SID rather than generating a new
// one when a valid value is stored.
func TestInitMachineSIDLoadsStored(t *testing.T) {
	gen := New(0)
	gen.initMachineSID(context.Background(), nil)
	stored := gen.SIDMapper().MachineSIDString()

	s := New(0)
	store := &fakeSIDStore{vals: map[string]string{"machine_sid": stored}}
	s.initMachineSID(context.Background(), store)
	if s.SIDMapper() == nil {
		t.Fatal("SID mapper nil after load")
	}
	if got := s.SIDMapper().MachineSIDString(); got != stored {
		t.Errorf("loaded SID = %q, want %q", got, stored)
	}
	if store.setHit {
		t.Error("SetSetting should not be called when a valid SID is already stored")
	}
}

// On first boot (empty store) a SID is generated and persisted.
func TestInitMachineSIDFirstBootPersists(t *testing.T) {
	s := New(0)
	store := &fakeSIDStore{vals: map[string]string{}}
	s.initMachineSID(context.Background(), store)
	if s.SIDMapper() == nil {
		t.Fatal("SID mapper nil")
	}
	if !store.setHit {
		t.Error("first boot should persist generated SID")
	}
	if store.vals["machine_sid"] != s.SIDMapper().MachineSIDString() {
		t.Error("persisted SID does not match generated mapper")
	}
}

// A corrupt stored SID is discarded and a fresh one generated + persisted.
func TestInitMachineSIDInvalidStoredRegenerates(t *testing.T) {
	s := New(0)
	store := &fakeSIDStore{vals: map[string]string{"machine_sid": "not-a-valid-sid"}}
	s.initMachineSID(context.Background(), store)
	if s.SIDMapper() == nil {
		t.Fatal("SID mapper nil after invalid stored value")
	}
	if !store.setHit {
		t.Error("invalid stored SID should trigger regenerate + persist")
	}
	if store.vals["machine_sid"] == "not-a-valid-sid" {
		t.Error("invalid SID was not replaced")
	}
}

// A read error from the store is tolerated: a SID is still generated.
func TestInitMachineSIDReadErrorGenerates(t *testing.T) {
	s := New(0)
	store := &fakeSIDStore{getErr: errors.New("db down")}
	s.initMachineSID(context.Background(), store)
	if s.SIDMapper() == nil {
		t.Error("SID mapper should still be generated despite read error")
	}
}

// A pinned machine SID is applied in preference to generation and persisted so
// it is authoritative (AD-3 #1235).
func TestInitMachineSIDPinnedApplied(t *testing.T) {
	const pinned = "S-1-5-21-10-20-30"
	s := New(0)
	s.SetPinnedMachineSID(pinned)
	store := &fakeSIDStore{vals: map[string]string{}}
	s.initMachineSID(context.Background(), store)
	if s.SIDMapper() == nil {
		t.Fatal("SID mapper nil after pin")
	}
	if s.SIDMapper().MachineSIDString() != pinned {
		t.Errorf("mapper SID = %q, want pinned %q", s.SIDMapper().MachineSIDString(), pinned)
	}
	if store.vals["machine_sid"] != pinned {
		t.Errorf("pinned SID not persisted: got %q", store.vals["machine_sid"])
	}
}

// A pinned SID overrides a different stored value (operator intent wins).
func TestInitMachineSIDPinnedOverridesStored(t *testing.T) {
	const pinned = "S-1-5-21-1-1-1"
	s := New(0)
	s.SetPinnedMachineSID(pinned)
	store := &fakeSIDStore{vals: map[string]string{"machine_sid": "S-1-5-21-9-9-9"}}
	s.initMachineSID(context.Background(), store)
	if s.SIDMapper().MachineSIDString() != pinned {
		t.Errorf("pinned SID did not override stored value: got %q", s.SIDMapper().MachineSIDString())
	}
	if store.vals["machine_sid"] != pinned {
		t.Errorf("override not persisted: got %q", store.vals["machine_sid"])
	}
}

// An invalid pinned SID is ignored and the normal generate/load path runs.
func TestInitMachineSIDPinnedInvalidFallsBack(t *testing.T) {
	s := New(0)
	s.SetPinnedMachineSID("garbage")
	store := &fakeSIDStore{vals: map[string]string{}}
	s.initMachineSID(context.Background(), store)
	if s.SIDMapper() == nil {
		t.Fatal("SID mapper nil after invalid pin")
	}
	if s.SIDMapper().MachineSIDString() == "garbage" {
		t.Error("invalid pin was applied verbatim")
	}
	if !store.setHit {
		t.Error("fallback path should generate + persist")
	}
}

// Two services pinned to the same SID produce identical mappers (node parity).
func TestInitMachineSIDPinnedNodeParity(t *testing.T) {
	const pinned = "S-1-5-21-7-7-7"
	a, b := New(0), New(0)
	a.SetPinnedMachineSID(pinned)
	b.SetPinnedMachineSID(pinned)
	a.initMachineSID(context.Background(), nil)
	b.initMachineSID(context.Background(), nil)
	if a.SIDMapper().MachineSIDString() != b.SIDMapper().MachineSIDString() {
		t.Errorf("pinned nodes diverge: %q vs %q",
			a.SIDMapper().MachineSIDString(), b.SIDMapper().MachineSIDString())
	}
}
