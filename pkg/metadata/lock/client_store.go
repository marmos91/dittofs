package lock

import (
	"context"
	"time"
)

// ============================================================================
// Client Registration Persistence for NSM (Phase 3)
// ============================================================================

// PersistedClientRegistration is the storage representation of a client registration.
// Used to persist NSM client registrations across server restarts.
// This enables the server to send SM_NOTIFY callbacks to previously registered
// clients when the server restarts.
type PersistedClientRegistration struct {
	// ClientID is the unique identifier (e.g., hostname or IP).
	ClientID string

	// MonName is the monitored hostname (from SM_MON mon_id.mon_name).
	// This identifies what host the client is monitoring.
	MonName string

	// Priv is the 16-byte private data for callbacks.
	// Returned unchanged in SM_NOTIFY to help client identify recovery context.
	Priv [16]byte

	// CallbackHost is the callback target hostname (from my_id.my_name).
	// This is where SM_NOTIFY RPCs will be sent.
	CallbackHost string

	// CallbackProg is the RPC program number for callbacks.
	// Typically NLM program number (100021).
	CallbackProg uint32

	// CallbackVers is the program version for callbacks.
	CallbackVers uint32

	// CallbackProc is the procedure number for callbacks.
	// NLM uses NLM_FREE_ALL (procedure 23) to release locks.
	CallbackProc uint32

	// RegisteredAt is when the registration was created.
	RegisteredAt time.Time

	// ServerEpoch is the server epoch at registration time.
	// Used to detect stale registrations from previous server instances.
	ServerEpoch uint64
}

// ClientRegistrationStore provides persistence for NSM client registrations.
// Implementations exist in memory, badger, and postgres stores.
//
// This interface enables crash recovery:
// 1. On server startup, load all registrations from previous run
// 2. Increment server epoch (state counter)
// 3. Send SM_NOTIFY to all registered callbacks with new state
// 4. Clients can then reclaim locks during grace period
type ClientRegistrationStore interface {
	// PutClientRegistration stores or updates a client registration.
	// If a registration with the same ClientID exists, it is replaced.
	PutClientRegistration(ctx context.Context, reg *PersistedClientRegistration) error

	// GetClientRegistration retrieves a registration by client ID.
	// Returns nil, nil if the registration does not exist.
	GetClientRegistration(ctx context.Context, clientID string) (*PersistedClientRegistration, error)

	// DeleteClientRegistration removes a registration.
	// Returns nil if the registration does not exist.
	DeleteClientRegistration(ctx context.Context, clientID string) error

	// ListClientRegistrations returns all stored registrations.
	// Used on server startup to send SM_NOTIFY callbacks.
	ListClientRegistrations(ctx context.Context) ([]*PersistedClientRegistration, error)

	// DeleteAllClientRegistrations removes all registrations.
	// Used for SM_UNMON_ALL to clear all monitoring for a client.
	// Returns the count of deleted registrations.
	DeleteAllClientRegistrations(ctx context.Context) (int, error)

	// DeleteClientRegistrationsByMonName removes all registrations monitoring a specific host.
	// Used when a monitored host is known to have crashed.
	// Returns the count of deleted registrations.
	DeleteClientRegistrationsByMonName(ctx context.Context, monName string) (int, error)
}

// ToPersistedClientRegistration converts a ClientRegistration to its persisted form.
// This is used when saving registrations to the store.
func ToPersistedClientRegistration(reg *ClientRegistration, serverEpoch uint64) *PersistedClientRegistration {
	if reg == nil {
		return nil
	}

	persisted := &PersistedClientRegistration{
		ClientID:     reg.ClientID,
		MonName:      reg.MonName,
		Priv:         reg.Priv,
		RegisteredAt: reg.RegisteredAt,
		ServerEpoch:  serverEpoch,
	}

	if reg.CallbackInfo != nil {
		persisted.CallbackHost = reg.CallbackInfo.Hostname
		persisted.CallbackProg = reg.CallbackInfo.Program
		persisted.CallbackVers = reg.CallbackInfo.Version
		persisted.CallbackProc = reg.CallbackInfo.Proc
	}

	return persisted
}

// FromPersistedClientRegistration converts a persisted registration back to ClientRegistration.
// This is used when loading registrations from the store.
func FromPersistedClientRegistration(persisted *PersistedClientRegistration) *ClientRegistration {
	if persisted == nil {
		return nil
	}

	reg := &ClientRegistration{
		ClientID:     persisted.ClientID,
		MonName:      persisted.MonName,
		Priv:         persisted.Priv,
		RegisteredAt: persisted.RegisteredAt,
	}

	// Reconstruct callback info if present
	if persisted.CallbackHost != "" {
		reg.CallbackInfo = &NSMCallback{
			Hostname: persisted.CallbackHost,
			Program:  persisted.CallbackProg,
			Version:  persisted.CallbackVers,
			Proc:     persisted.CallbackProc,
		}
	}

	return reg
}
