package adapters

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/health"
)

// DefaultShutdownTimeout is the default timeout for graceful adapter shutdown.
const DefaultShutdownTimeout = 30 * time.Second

// defaultStartTimeout bounds how long a start waits for the adapter to bind.
// Binding is a syscall, so the wait is normally sub-millisecond; the bound
// exists only so setup that runs before the bind and wedges cannot hang the
// caller forever.
const defaultStartTimeout = 15 * time.Second

// ProtocolAdapter is the interface for protocol adapters (NFS, SMB).
//
// It mirrors a strict subset of [adapter.Adapter]: the methods this
// service actually calls during lifecycle management. The Healthcheck
// method is included so the upcoming /status API routes can call it
// directly on a stored ProtocolAdapter without a runtime type
// assertion to [adapter.Adapter] (which would risk a panic if a test
// fake forgets to implement it).
type ProtocolAdapter interface {
	Serve(ctx context.Context) error
	Stop(ctx context.Context) error
	Protocol() string
	Port() int
	Healthcheck(ctx context.Context) health.Report

	// ListenerReady is closed once Serve has bound the listening socket, and
	// stays open when the bind fails. Racing it against Serve returning is what
	// lets a start report the outcome of the bind rather than the intent.
	ListenerReady() <-chan struct{}
}

// RuntimeSetter is implemented by adapters that need runtime access.
type RuntimeSetter interface {
	SetRuntime(rt any)
}

// AdapterFactory creates a ProtocolAdapter from configuration.
type AdapterFactory func(cfg *models.AdapterConfig) (ProtocolAdapter, error)

type adapterEntry struct {
	adapter ProtocolAdapter
	config  *models.AdapterConfig
	cancel  context.CancelFunc

	// served is closed once the serve goroutine has returned, with serveErr
	// holding its result. A start and a teardown can both be waiting on the
	// same adapter to exit, so this is a broadcast rather than a single
	// delivered value: a value sent once would wake only whichever of them
	// happened to receive it, leaving the other to sit out its own timeout and
	// then report that instead of the real outcome. serveErr is written before
	// the close and read only after it.
	served   chan struct{}
	serveErr error

	// stopping marks a teardown in progress: the entry is still in the map,
	// but the adapter is on its way out. Guarded by Service.mu.
	stopping bool

	// cancelled records that the entry's context was already cancelled by a
	// teardown that then timed out waiting for the serve goroutine. The entry
	// stays in the map to keep holding the type against a competing start, but
	// the adapter behind it is no longer serving, so it must never be treated as
	// a live listener that a reload can reuse. Guarded by Service.mu.
	cancelled bool
}

// serving reports whether the entry describes an adapter that confirmed its
// listener and has not since been abandoned. A cancelled entry has not: it
// holds the type so nothing else claims it, but either a start gave up waiting
// for the bind or a teardown gave up waiting for the exit, so nothing about it
// is known to be listening. Caller must hold mu.
func (e *adapterEntry) serving() bool { return !e.cancelled }

// Service manages protocol adapter lifecycle.
type Service struct {
	mu      sync.RWMutex
	entries map[string]*adapterEntry // keyed by adapter type (nfs, smb)
	factory AdapterFactory

	store           store.AdapterStore
	shutdownTimeout time.Duration
	startTimeout    time.Duration
	runtime         any // injected into adapters implementing RuntimeSetter
}

// New creates a new adapter management service.
func New(adapterStore store.AdapterStore, shutdownTimeout time.Duration) *Service {
	if shutdownTimeout == 0 {
		shutdownTimeout = DefaultShutdownTimeout
	}
	return &Service{
		entries:         make(map[string]*adapterEntry),
		store:           adapterStore,
		shutdownTimeout: shutdownTimeout,
		startTimeout:    defaultStartTimeout,
	}
}

func (s *Service) SetRuntime(rt any) { s.runtime = rt }

// SetAdapterFactory must be called before CreateAdapter.
func (s *Service) SetAdapterFactory(factory AdapterFactory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.factory = factory
}

func (s *Service) SetShutdownTimeout(d time.Duration) {
	if d == 0 {
		d = DefaultShutdownTimeout
	}
	s.shutdownTimeout = d
}

// CreateAdapter saves the adapter config to store and starts it immediately.
func (s *Service) CreateAdapter(ctx context.Context, cfg *models.AdapterConfig) error {
	if _, err := s.store.CreateAdapter(ctx, cfg); err != nil {
		return fmt.Errorf("failed to save adapter config: %w", err)
	}

	if err := s.startAdapter(cfg); err != nil {
		// Roll back the persisted config so a non-startable adapter does not
		// linger in the store. Use a fresh bounded context, not the caller's:
		// the request may already be canceled (client disconnect/timeout),
		// and that must not abort the cleanup. A rollback failure leaves an
		// orphan row, so surface it rather than swallowing it silently.
		rbCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		if delErr := s.store.DeleteAdapter(rbCtx, cfg.Type); delErr != nil {
			logger.Warn("Failed to roll back adapter config after start failure",
				"type", cfg.Type, "error", delErr)
		}
		return fmt.Errorf("failed to start adapter: %w", err)
	}

	return nil
}

// DeleteAdapter stops the running adapter and removes it from store.
func (s *Service) DeleteAdapter(ctx context.Context, adapterType string) error {
	if err := s.stopAdapter(adapterType); err != nil {
		logger.Warn("Adapter stop failed during delete", "type", adapterType, "error", err)
	}

	if err := s.store.DeleteAdapter(ctx, adapterType); err != nil {
		return fmt.Errorf("failed to delete adapter from store: %w", err)
	}

	return nil
}

// UpdateAdapter updates the persisted config, then reloads the adapter.
//
// The reload preserves the running adapter — and with it the live TCP
// listener and any in-flight connections — when the new config keeps the
// same listen address (bind address + port) and the adapter stays enabled.
// A stop/start is only performed when the listen address actually changes or
// the enabled state flips; other configuration is applied by the live
// settings reload path or on the next rebind. This keeps a config change
// (e.g. re-enabling an already-running adapter) from momentarily dropping the
// accept socket and cutting existing sessions.
//
// A failed restart is returned, not logged: the caller must not see success for
// an adapter that is down. The new config stays persisted — it is the requested
// state, and the next start retries it from the store.
func (s *Service) UpdateAdapter(ctx context.Context, cfg *models.AdapterConfig) error {
	if err := s.store.UpdateAdapter(ctx, cfg); err != nil {
		return fmt.Errorf("failed to update adapter config: %w", err)
	}

	s.mu.RLock()
	entry, ok := s.entries[cfg.Type]
	// A stopping or cancelled entry is on its way out, so its listener is not
	// reusable even though the entry is still in the map.
	serving := ok && !entry.stopping && !entry.cancelled
	s.mu.RUnlock()

	if serving && cfg.Enabled && sameListenAddr(entry, cfg) {
		logger.Info("Adapter listen address unchanged; preserving listener across reload",
			"type", cfg.Type, "port", entry.adapter.Port())
		return nil
	}

	_ = s.stopAdapter(cfg.Type)
	if cfg.Enabled {
		if err := s.startAdapter(cfg); err != nil {
			return fmt.Errorf("failed to restart adapter after update: %w", err)
		}
	}

	return nil
}

// resolvePort returns the port an adapter of the given type binds to, treating
// a zero port as "use the type's default" — the same substitution the factory
// applies when it constructs the adapter. Types without a known default keep
// the raw port unchanged.
func resolvePort(adapterType string, port int) int {
	if port != 0 {
		return port
	}
	if def := models.DefaultPort(adapterType); def != 0 {
		return def
	}
	return port
}

// sameListenAddr reports whether cfg binds to the same address and port as the
// already-running adapter, so a reload need not recreate the TCP listener. A
// zero port is resolved to its default first, so re-pointing a non-default-port
// adapter at the default (port 0) is correctly seen as a change and rebinds,
// keeping the running listener and the persisted config in agreement.
func sameListenAddr(entry *adapterEntry, cfg *models.AdapterConfig) bool {
	if adapterBindAddress(entry.config) != adapterBindAddress(cfg) {
		return false
	}
	return resolvePort(cfg.Type, cfg.Port) == entry.adapter.Port()
}

// adapterBindAddress returns the configured bind address, or "" when the
// adapter binds all interfaces (the default).
func adapterBindAddress(cfg *models.AdapterConfig) string {
	if cfg == nil {
		return ""
	}
	parsed, err := cfg.GetConfig()
	if err != nil {
		return ""
	}
	addr, _ := parsed["bind_address"].(string)
	return addr
}

func (s *Service) EnableAdapter(ctx context.Context, adapterType string) error {
	cfg, err := s.store.GetAdapter(ctx, adapterType)
	if err != nil {
		return fmt.Errorf("adapter not found: %w", err)
	}

	cfg.Enabled = true
	if err := s.store.UpdateAdapter(ctx, cfg); err != nil {
		return fmt.Errorf("failed to enable adapter: %w", err)
	}

	if err := s.startAdapter(cfg); err != nil {
		// Roll back the persisted flag so the store does not advertise an
		// adapter that never started. A fresh context keeps an already-canceled
		// request from aborting the cleanup.
		rbCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		cfg.Enabled = false
		if rbErr := s.store.UpdateAdapter(rbCtx, cfg); rbErr != nil {
			logger.Warn("Failed to roll back adapter enabled flag after start failure",
				"type", adapterType, "error", rbErr)
		}
		return fmt.Errorf("failed to start adapter: %w", err)
	}

	return nil
}

func (s *Service) DisableAdapter(ctx context.Context, adapterType string) error {
	cfg, err := s.store.GetAdapter(ctx, adapterType)
	if err != nil {
		return fmt.Errorf("adapter not found: %w", err)
	}

	_ = s.stopAdapter(adapterType)
	cfg.Enabled = false
	if err := s.store.UpdateAdapter(ctx, cfg); err != nil {
		return fmt.Errorf("failed to disable adapter: %w", err)
	}

	return nil
}

// startAdapter creates the adapter, runs it, and returns only once its
// listener is bound. The entry is registered before the wait so a competing
// start of the same type is refused while this one is still binding, and it is
// dropped again if the bind fails.
func (s *Service) startAdapter(cfg *models.AdapterConfig) error {
	// Every start routes through here, which is the only place that covers all
	// of them: the adapter constructors panic on a port they cannot bind, and
	// the boot-time load runs with no recover above it, so a port persisted
	// before it could be refused would otherwise kill the process at startup
	// with no API left to correct it through.
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid adapter config: %w", err)
	}

	entry, err := s.claimAndRunAdapter(cfg)
	if err != nil {
		return err
	}
	return s.awaitListener(cfg.Type, entry)
}

// claimAndRunAdapter builds the adapter and registers its entry, holding mu for
// the whole build so a competing start of the same type is refused while this
// one is still constructing. The unlock is deferred rather than written out on
// each exit: a constructor that panics under this lock would otherwise leave it
// held for the life of the process, blocking every later adapter call including
// the read-only ones.
func (s *Service) claimAndRunAdapter(cfg *models.AdapterConfig) (*adapterEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.typeClaimedLocked(cfg.Type); err != nil {
		return nil, err
	}

	if s.factory == nil {
		return nil, fmt.Errorf("adapter factory not set")
	}

	adp, err := s.factory(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create adapter: %w", err)
	}

	return s.registerAndRunAdapterLocked(adp, cfg), nil
}

// awaitListener blocks until the adapter has bound its listening socket, its
// serve goroutine has returned, or the start deadline passes. Binding happens
// inside Serve, so this is the only point at which a bind failure — an
// occupied port, an unavailable address, insufficient privilege — becomes
// observable; returning before it makes every start report the intent to serve
// rather than the outcome.
func (s *Service) awaitListener(adapterType string, entry *adapterEntry) error {
	select {
	case <-entry.adapter.ListenerReady():
		logger.Info("Adapter started", "type", adapterType, "port", entry.adapter.Port())
		return nil

	case <-entry.served:
		// The serve goroutine returned, so it has released the socket: drop the
		// entry and free the type for a retry. Drop it only while the map still
		// holds this entry — a teardown that overtook this start has already
		// removed it, and whatever registered the type afterwards belongs to a
		// later start that is still serving.
		entry.cancel()
		s.mu.Lock()
		if s.entries[adapterType] == entry {
			delete(s.entries, adapterType)
		}
		s.mu.Unlock()
		if entry.serveErr == nil {
			return fmt.Errorf("adapter %s stopped before its listener was ready", adapterType)
		}
		return entry.serveErr

	case <-time.After(s.startTimeout):
		// The adapter is still running and may yet bind, so the entry stays to
		// keep holding the type, marked the same way a timed-out stop marks it.
		// Its context is cancelled, so it will exit; reap it then, or the type
		// stays claimed by an adapter no caller can reach — a failed start rolls
		// the store row back, and a later stop looks that row up before it would
		// ever reach this entry.
		entry.cancel()
		s.mu.Lock()
		entry.cancelled = true
		s.mu.Unlock()
		go s.reapWhenServed(adapterType, entry)
		return fmt.Errorf("adapter %s listener not ready after %s", adapterType, s.startTimeout)
	}
}

// reapWhenServed drops the entry once its serve goroutine has returned, freeing
// the type for a later start. It is the cleanup for an entry that was abandoned
// mid-flight: the context is already cancelled, so the wait ends as soon as the
// adapter unwinds. The entry is dropped only while the map still holds it, so a
// teardown that got there first cannot have its replacement removed. An adapter
// that never returns keeps its entry, which is the honest answer — it may still
// hold the socket.
func (s *Service) reapWhenServed(adapterType string, entry *adapterEntry) {
	<-entry.served

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries[adapterType] == entry {
		delete(s.entries, adapterType)
		logger.Info("Abandoned adapter released its socket", "type", adapterType)
	}
}

// stopAdapter tears down the running adapter of the given type. Its entry stays
// in the map for the whole teardown and is dropped only once the serve goroutine
// confirms it exited, so a concurrent start of the same type is refused instead
// of racing the outgoing adapter for its listening socket. A stop that times out
// keeps the entry too — the adapter is still alive — and only clears the
// stopping mark so a later attempt can retry.
func (s *Service) stopAdapter(adapterType string) error {
	s.mu.Lock()
	entry, exists := s.entries[adapterType]
	if !exists {
		s.mu.Unlock()
		return fmt.Errorf("adapter %s not running", adapterType)
	}
	if entry.stopping {
		s.mu.Unlock()
		return fmt.Errorf("adapter %s is already stopping", adapterType)
	}
	entry.stopping = true
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()

	logger.Info("Stopping adapter", "type", adapterType)

	if err := entry.adapter.Stop(ctx); err != nil {
		logger.Warn("Adapter stop error", "type", adapterType, "error", err)
	}

	entry.cancel()
	select {
	case <-entry.served:
		s.mu.Lock()
		delete(s.entries, adapterType)
		s.mu.Unlock()
		logger.Info("Adapter stopped", "type", adapterType)
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		entry.stopping = false
		entry.cancelled = true
		s.mu.Unlock()
		go s.reapWhenServed(adapterType, entry)
		logger.Warn("Adapter stop timed out", "type", adapterType)
		return fmt.Errorf("adapter %s stop timed out", adapterType)
	}
}

func (s *Service) StopAllAdapters() error {
	s.mu.RLock()
	types := make([]string, 0, len(s.entries))
	for t := range s.entries {
		types = append(types, t)
	}
	s.mu.RUnlock()

	var lastErr error
	for _, t := range types {
		if err := s.stopAdapter(t); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// LoadAdaptersFromStore loads enabled adapters from store and starts them.
//
// An adapter that cannot start is logged and skipped rather than failing the
// load. A persisted port can become unusable between one boot and the next —
// another process claimed it, or the host lost the address — and taking the
// whole server down with it would also take down the control-plane API, which
// is the only way left to correct the port without console access. The adapter
// stays enabled in the store: that is the requested state, retried on the next
// start or when the operator re-enables it.
func (s *Service) LoadAdaptersFromStore(ctx context.Context) error {
	adapters, err := s.store.ListAdapters(ctx)
	if err != nil {
		return fmt.Errorf("failed to list adapters: %w", err)
	}

	for _, cfg := range adapters {
		if !cfg.Enabled {
			logger.Info("Adapter disabled, skipping", "type", cfg.Type)
			continue
		}

		if err := s.startAdapter(cfg); err != nil {
			logger.Error("Adapter failed to start; continuing without it",
				"type", cfg.Type, "port", cfg.Port, "error", err)
		}
	}

	return nil
}

// ListRunningAdapters names the adapters that are serving. It answers the same
// question as [Service.IsAdapterRunning] and must agree with it — readiness
// reports this list, so an abandoned entry counted here would advertise a
// listener that may not exist.
func (s *Service) ListRunningAdapters() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	types := make([]string, 0, len(s.entries))
	for t, e := range s.entries {
		if e.serving() {
			types = append(types, t)
		}
	}
	return types
}

// IsAdapterRunning reports whether an adapter of the given type is serving.
func (s *Service) IsAdapterRunning(adapterType string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, exists := s.entries[adapterType]
	return exists && e.serving()
}

// GetAdapter returns the running adapter for the given type, or nil if
// no adapter of that type is currently running. Used by status probes
// that need to call [ProtocolAdapter.Healthcheck] without going
// through a runtime type assertion to the full [adapter.Adapter]
// interface.
func (s *Service) GetAdapter(adapterType string) ProtocolAdapter {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[adapterType]
	if !ok {
		return nil
	}
	return e.adapter
}

// AddAdapter directly starts a pre-created adapter (for testing, bypasses store).
func (s *Service) AddAdapter(adapter ProtocolAdapter) error {
	adapterType := adapter.Protocol()

	s.mu.Lock()

	if err := s.typeClaimedLocked(adapterType); err != nil {
		s.mu.Unlock()
		return err
	}

	cfg := &models.AdapterConfig{Type: adapterType, Port: adapter.Port(), Enabled: true}
	entry := s.registerAndRunAdapterLocked(adapter, cfg)
	s.mu.Unlock()

	return s.awaitListener(adapterType, entry)
}

// clientDisconnecter allows force-closing a specific client connection.
// Defined locally to avoid import cycles with pkg/adapter.
type clientDisconnecter interface {
	ForceCloseByAddress(addr string) bool
}

// ForceCloseClientConnection closes the TCP connection for a specific client address
// on the adapter handling the given protocol. This triggers the adapter's normal
// connection cleanup chain.
func (s *Service) ForceCloseClientConnection(protocol, addr string) bool {
	s.mu.RLock()
	entry, ok := s.entries[protocol]
	s.mu.RUnlock()
	if !ok || entry.adapter == nil {
		return false
	}
	if dc, ok := entry.adapter.(clientDisconnecter); ok {
		return dc.ForceCloseByAddress(addr)
	}
	return false
}

// typeClaimedLocked returns an error when an entry still holds adapterType, so
// no new adapter of that type may claim it: either one is serving, or a teardown
// has not yet released the listening socket. Returns nil when the type is free.
// Caller must hold mu.
func (s *Service) typeClaimedLocked(adapterType string) error {
	e, exists := s.entries[adapterType]
	switch {
	case !exists:
		return nil
	case e.stopping:
		return fmt.Errorf("adapter %s is still stopping", adapterType)
	case e.cancelled:
		return fmt.Errorf("adapter %s did not confirm shutdown and still holds its socket", adapterType)
	default:
		return fmt.Errorf("adapter %s already running", adapterType)
	}
}

// registerAndRunAdapterLocked runs the adapter in a goroutine and returns its
// entry. The adapter has not bound its listener yet at this point; callers wait
// for that with [Service.awaitListener]. Caller must hold mu.
func (s *Service) registerAndRunAdapterLocked(adp ProtocolAdapter, cfg *models.AdapterConfig) *adapterEntry {
	if setter, ok := adp.(RuntimeSetter); ok && s.runtime != nil {
		setter.SetRuntime(s.runtime)
	}

	ctx, cancel := context.WithCancel(context.Background())

	entry := &adapterEntry{
		adapter: adp,
		config:  cfg,
		cancel:  cancel,
		served:  make(chan struct{}),
	}
	s.entries[cfg.Type] = entry

	go func() {
		logger.Info("Starting adapter", "protocol", adp.Protocol(), "port", adp.Port())
		err := adp.Serve(ctx)
		if err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
			logger.Error("Adapter failed", "protocol", adp.Protocol(), "error", err)
		}
		entry.serveErr = err
		close(entry.served)
	}()

	return entry
}
