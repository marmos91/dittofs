// Package nsm provides Network Status Monitor (NSM) protocol implementation.
package nsm

import (
	"context"
	"sync"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nsm/callback"
	"github.com/marmos91/dittofs/internal/protocol/nsm/handlers"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// NotifyResult contains the result of notifying a single client.
type NotifyResult struct {
	// ClientID is the unique identifier of the client that was notified.
	ClientID string

	// Error is nil on success, or the error that occurred during notification.
	Error error
}

// OnClientCrashFunc is called when a client is detected as crashed.
//
// Implementations should:
//  1. Release all locks held by this client across all shares
//  2. Process NLM blocking queue waiters for affected files
//
// The clientID parameter is the NSM hostname (mon_name) from SM_MON.
// NLM locks have owner IDs formatted as nlm:{caller_name}:{svid}:{oh_hex},
// so implementations should release all locks where OwnerID starts with
// "nlm:{clientID}:".
type OnClientCrashFunc func(ctx context.Context, clientID string) error

// Notifier orchestrates SM_NOTIFY callbacks to registered clients.
//
// Notifier is responsible for:
//   - Sending SM_NOTIFY to all registered clients on server restart
//   - Detecting client crashes when callbacks fail
//   - Triggering lock cleanup for crashed clients
//   - Loading persisted registrations on startup
//
// Per CONTEXT.md decisions:
//   - Parallel notification for fastest recovery
//   - Failed notification = client crashed, cleanup locks immediately
//   - Best effort cleanup - log errors but continue
//
// Thread Safety:
// Notifier is safe for concurrent use by multiple goroutines.
type Notifier struct {
	// handler is the NSM handler (provides tracker and state)
	handler *handlers.Handler

	// client is the callback client for sending SM_NOTIFY
	client *callback.Client

	// serverName is this server's hostname (sent in callbacks)
	serverName string

	// onClientCrash is called when a client crash is detected
	onClientCrash OnClientCrashFunc

	// metrics for observability (may be nil)
	metrics *Metrics
}

// NotifierConfig configures the notifier.
type NotifierConfig struct {
	// Handler is the NSM handler (required).
	// Provides access to ConnectionTracker and server state.
	Handler *handlers.Handler

	// ServerName is this server's hostname (required).
	// Sent in SM_NOTIFY callbacks as the mon_name field.
	ServerName string

	// OnClientCrash is called when a client crash is detected (optional).
	// If nil, crash detection will log but not cleanup locks.
	OnClientCrash OnClientCrashFunc

	// Metrics for observability (optional).
	// If nil, no metrics are recorded.
	Metrics *Metrics
}

// NewNotifier creates a new SM_NOTIFY notifier.
//
// Parameters:
//   - config: Notifier configuration
//
// Returns a configured Notifier ready to send notifications.
//
// Panics if config.Handler is nil.
func NewNotifier(config NotifierConfig) *Notifier {
	if config.Handler == nil {
		panic("NSM notifier requires a handler")
	}

	return &Notifier{
		handler:       config.Handler,
		client:        callback.NewClient(0), // Use default 5s timeout
		serverName:    config.ServerName,
		onClientCrash: config.OnClientCrash,
		metrics:       config.Metrics,
	}
}

// NotifyAllClients sends SM_NOTIFY to all registered clients in parallel.
//
// This method is called on server startup after loading registrations from
// the persistent store. It sends SM_NOTIFY callbacks to all clients that
// registered for monitoring via SM_MON.
//
// Per CONTEXT.md decisions:
//   - Parallel notification for fastest recovery
//   - Failed notification = client crashed, cleanup locks immediately
//   - Process NLM blocking queue waiters when crashed client's locks released
//
// Parameters:
//   - ctx: Context for cancellation
//
// Returns:
//   - Slice of NotifyResult, one per client, indicating success or failure
func (n *Notifier) NotifyAllClients(ctx context.Context) []NotifyResult {
	// Get all clients with NSM callback info
	clients := n.handler.GetTracker().GetNSMClients()

	if len(clients) == 0 {
		logger.Info("NSM: no registered clients to notify")
		return nil
	}

	logger.Info("NSM: notifying all registered clients",
		"count", len(clients),
		"state", n.handler.GetServerState())

	// Record notification attempt
	if n.metrics != nil {
		n.metrics.NotificationsTotal.WithLabelValues("started").Add(float64(len(clients)))
	}

	// Send notifications in parallel
	var wg sync.WaitGroup
	results := make(chan NotifyResult, len(clients))

	for _, client := range clients {
		wg.Add(1)
		go func(c *lock.ClientRegistration) {
			defer wg.Done()

			err := callback.SendNotify(
				ctx,
				n.client,
				c,
				n.serverName,
				n.handler.GetServerState(),
			)

			results <- NotifyResult{
				ClientID: c.ClientID,
				Error:    err,
			}
		}(client)
	}

	// Wait for all notifications to complete, then close channel
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results and handle failures
	var allResults []NotifyResult
	for result := range results {
		allResults = append(allResults, result)

		if result.Error != nil {
			// Per CONTEXT.md: Failed notification = client crashed, cleanup locks
			logger.Warn("NSM: SM_NOTIFY failed, treating client as crashed",
				"client", result.ClientID,
				"error", result.Error)

			if n.metrics != nil {
				n.metrics.NotificationsTotal.WithLabelValues("failed").Inc()
				n.metrics.CrashesDetected.Inc()
			}

			// Trigger crash handling (lock cleanup)
			n.handleClientCrash(ctx, result.ClientID)
		} else {
			logger.Debug("NSM: SM_NOTIFY succeeded", "client", result.ClientID)

			if n.metrics != nil {
				n.metrics.NotificationsTotal.WithLabelValues("success").Inc()
			}
		}
	}

	return allResults
}

// handleClientCrash processes a crashed client - releases locks and unregisters.
//
// Per CONTEXT.md decisions:
//   - Immediate cleanup when crash detected (no delay/grace window)
//   - Process NLM blocking queue waiters when locks released
//   - Best effort cleanup - log errors but continue
func (n *Notifier) handleClientCrash(ctx context.Context, clientID string) {
	logger.Info("NSM: handling client crash", "client", clientID)

	// Unregister from tracker
	n.handler.GetTracker().UnregisterClient(clientID)

	// Call crash handler to release locks (FREE_ALL)
	if n.onClientCrash != nil {
		if err := n.onClientCrash(ctx, clientID); err != nil {
			// Per CONTEXT.md: Best effort cleanup - log error but continue
			logger.Error("NSM: lock cleanup failed for crashed client",
				"client", clientID,
				"error", err)
		}
	}

	// Record cleanup
	if n.metrics != nil {
		n.metrics.CrashCleanups.Inc()
	}
}

// DetectCrash handles notification of a client crash from an external source.
//
// This method can be called when:
//   - SM_NOTIFY is received indicating a host state change
//   - A callback to a client fails during normal operation
//   - Any other crash detection mechanism triggers
//
// Parameters:
//   - ctx: Context for cancellation
//   - clientID: The client identifier (hostname from NSM registration)
func (n *Notifier) DetectCrash(ctx context.Context, clientID string) {
	logger.Info("NSM: client crash detected", "client", clientID)

	if n.metrics != nil {
		n.metrics.CrashesDetected.Inc()
	}

	n.handleClientCrash(ctx, clientID)
}

// LoadRegistrationsFromStore loads persisted registrations into the tracker.
//
// This method is called on server startup to restore NSM state from the
// persistent store. After loading, the server can send SM_NOTIFY to all
// clients to inform them of the restart.
//
// Parameters:
//   - ctx: Context for cancellation
//   - store: The client registration store to load from (may be nil)
//
// Returns:
//   - nil on success
//   - error if loading fails
func (n *Notifier) LoadRegistrationsFromStore(ctx context.Context, store lock.ClientRegistrationStore) error {
	if store == nil {
		logger.Debug("NSM: no client store available, skipping registration load")
		return nil
	}

	registrations, err := store.ListClientRegistrations(ctx)
	if err != nil {
		return err
	}

	logger.Info("NSM: loading persisted registrations", "count", len(registrations))

	tracker := n.handler.GetTracker()
	for _, reg := range registrations {
		// Register in tracker
		if err := tracker.RegisterClient(reg.ClientID, "nsm", "", 0); err != nil {
			logger.Warn("NSM: failed to restore registration",
				"client", reg.ClientID,
				"error", err)
			continue
		}

		// Update NSM-specific fields from persisted registration
		callbackInfo := &lock.NSMCallback{
			Hostname: reg.CallbackHost,
			Program:  reg.CallbackProg,
			Version:  reg.CallbackVers,
			Proc:     reg.CallbackProc,
		}
		tracker.UpdateNSMInfo(reg.ClientID, reg.MonName, reg.Priv, callbackInfo)
	}

	if n.metrics != nil {
		n.metrics.ClientsRegistered.Set(float64(len(registrations)))
	}

	return nil
}

// GetHandler returns the NSM handler for external access.
//
// This allows the adapter to access handler state when needed.
func (n *Notifier) GetHandler() *handlers.Handler {
	return n.handler
}
