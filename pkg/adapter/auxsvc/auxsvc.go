// Package auxsvc defines a small abstraction for the auxiliary/companion
// protocol services that run alongside a main protocol adapter.
//
// A protocol adapter (NFS, SMB) blocks in Serve, accepting connections on its
// main port. Around it run a handful of smaller services with their own
// lifecycle: the NFS embedded portmapper, the system-rpcbind registration, the
// UDP lock-manager transport, the NSM startup notifier, and — added for issue
// #1609 — the mDNS and WS-Discovery advertisers. Historically each was started
// and stopped by bespoke code inline in the adapter. This package folds them
// into one uniform Service interface managed by a Group, so:
//
//   - every companion shares the same Start/Stop shape and logging, and
//   - a service can be toggled at runtime (from a live settings change) without
//     restarting the whole adapter.
//
// A Service differs from the adapter itself in that Start is non-blocking: it
// binds and launches background goroutines, then returns, leaving the service
// running until Stop (or the base context) tears it down.
package auxsvc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// ErrAlreadyRunning is returned by Start when a service with the same Name is
// already tracked. Reconcile treats it as benign (a lost start race).
var ErrAlreadyRunning = errors.New("auxsvc: service already running")

// stopTimeout bounds a single-service Stop initiated by StopOne, where the
// caller (e.g. a settings-watcher callback) has no natural context to bound the
// teardown. StopAll uses the context passed by the adapter's Stop instead.
const stopTimeout = 5 * time.Second

// Service is an auxiliary protocol server that runs alongside a main adapter —
// e.g. the NFS embedded portmapper, the UDP lock-manager transport, or the
// mDNS / WS-Discovery advertisers.
//
// Unlike the adapter (whose Serve blocks), a Service starts in the background
// and is independently start/stoppable, so a live settings change can toggle
// one without restarting the whole adapter.
type Service interface {
	// Name is a stable identifier ("portmapper", "mdns", "wsd") used as the
	// Group key and in logs. Names must be unique within a Group.
	Name() string

	// Start binds listeners / launches background goroutines and returns
	// promptly: nil once the service is ready, or an error if it could not
	// start. ctx bounds the service's whole lifetime (the owning adapter's
	// Serve context), not merely the Start call.
	Start(ctx context.Context) error

	// Stop tears the service down. It must be idempotent and block until the
	// service's background goroutines have exited.
	Stop(ctx context.Context) error
}

// Group tracks the auxiliary services running alongside one adapter. The
// adapter holds a Group, seeds it with its Serve context via SetBaseContext,
// starts each enabled service through Start, and tears the whole set down in
// its Stop via StopAll.
//
// Services started later (e.g. from a live settings-change callback) bind to the
// stored base context, not the transient context of the caller, so they live
// until the adapter stops rather than until the callback returns.
type Group struct {
	mu      sync.Mutex
	baseCtx context.Context
	order   []string // registration order; StopAll tears down in reverse
	running map[string]Service
}

// NewGroup returns an empty Group. Call SetBaseContext before starting services.
func NewGroup() *Group {
	return &Group{running: make(map[string]Service)}
}

// SetBaseContext records the adapter's Serve context. Call once at the top of
// Serve, before starting any service. Services subsequently started — including
// from live settings callbacks — inherit this context for their lifetime.
func (g *Group) SetBaseContext(ctx context.Context) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.baseCtx = ctx
}

// Start starts s using the stored base context and tracks it by Name(). It is
// an error to Start before SetBaseContext, or to start a service whose name is
// already running. When s.Start fails, the service is not tracked and the error
// is returned.
func (g *Group) Start(s Service) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.baseCtx == nil {
		return fmt.Errorf("auxsvc: Start(%q) before SetBaseContext", s.Name())
	}
	name := s.Name()
	if _, ok := g.running[name]; ok {
		return fmt.Errorf("%w: %q", ErrAlreadyRunning, name)
	}
	if err := s.Start(g.baseCtx); err != nil {
		return fmt.Errorf("auxsvc: start %q: %w", name, err)
	}
	g.running[name] = s
	g.order = append(g.order, name)
	logger.Debug("auxsvc started", "name", name)
	return nil
}

// Ready reports whether SetBaseContext has been called, i.e. the owning adapter
// has entered Serve. Live reconcilers (settings callbacks) check this so a
// settings-apply that runs before Serve is a no-op — the initial start is done
// by Serve itself.
func (g *Group) Ready() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.baseCtx != nil
}

// Reconcile starts or stops the named service so its running state matches want,
// letting a live settings change toggle it. It is a no-op until Ready (before
// Serve has seeded the base context), so the initial start — performed by Serve
// itself — is never raced. build is invoked only when a start is needed. Returns
// the Start error, if any; a stop error is swallowed (StopAll/StopOne already
// debug-log it).
func (g *Group) Reconcile(name string, want bool, build func() Service) error {
	if !g.Ready() {
		return nil
	}
	switch running := g.IsRunning(name); {
	case want && !running:
		// Two goroutines can observe want && !running concurrently (e.g. the NFS
		// accept-loop apply and the settings-watcher poll); the loser gets
		// ErrAlreadyRunning, which is benign here — the single-instance invariant
		// still holds — so it is not surfaced as a start failure.
		if err := g.Start(build()); err != nil && !errors.Is(err, ErrAlreadyRunning) {
			return err
		}
	case !want && running:
		return g.StopOne(name)
	}
	return nil
}

// IsRunning reports whether a service with the given name is currently tracked
// as running.
func (g *Group) IsRunning(name string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.running[name]
	return ok
}

// StopOne stops and forgets a single running service, for a live disable. It is
// a no-op returning nil when the named service is not running. The teardown is
// bounded by an internal timeout since the caller (a settings callback) has no
// natural context.
func (g *Group) StopOne(name string) error {
	g.mu.Lock()
	s, ok := g.running[name]
	if ok {
		delete(g.running, name)
		g.removeFromOrderLocked(name)
	}
	g.mu.Unlock()

	if !ok {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	err := s.Stop(ctx)
	logger.Debug("auxsvc stopped", "name", name, "error", err)
	return err
}

// StopAll stops every running service in reverse start order and clears the
// group. It is called from the adapter's Stop, before the base listener
// teardown, so ordering that matters (e.g. unregister from the system rpcbind
// before closing the portmapper) is preserved: services registered earlier stop
// later. The first Stop error is returned; every service is still stopped.
func (g *Group) StopAll(ctx context.Context) error {
	g.mu.Lock()
	names := make([]string, len(g.order))
	for i, n := range g.order {
		names[len(g.order)-1-i] = n // reverse
	}
	svcs := make([]Service, len(names))
	for i, n := range names {
		svcs[i] = g.running[n]
	}
	g.running = make(map[string]Service)
	g.order = nil
	// Clear the base context so any live reconcile that races this shutdown
	// no-ops (Ready() becomes false, and Start re-checks baseCtx under the lock)
	// rather than starting a service that StopAll would never tear down.
	g.baseCtx = nil
	g.mu.Unlock()

	var firstErr error
	for i, s := range svcs {
		if err := s.Stop(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
		logger.Debug("auxsvc stopped", "name", names[i])
	}
	return firstErr
}

// removeFromOrderLocked drops name from the order slice. Caller holds g.mu.
func (g *Group) removeFromOrderLocked(name string) {
	for i, n := range g.order {
		if n == name {
			g.order = append(g.order[:i], g.order[i+1:]...)
			return
		}
	}
}
