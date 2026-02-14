// Package state implements NFSv4 state management for client identity,
// open state, lock state, and lease tracking per RFC 7530 Section 9.
package state

import (
	"errors"
	"time"
)

// ============================================================================
// Error Types
// ============================================================================

// ErrStaleClientID indicates the client ID is not recognized by the server.
// Maps to NFS4ERR_STALE_CLIENTID (10022).
var ErrStaleClientID = errors.New("stale client ID")

// ErrClientIDInUse indicates the client ID string is already confirmed
// by a different principal/address and the callback check failed.
// Maps to NFS4ERR_CLID_INUSE (10017).
var ErrClientIDInUse = errors.New("client ID in use")

// ============================================================================
// Client Record
// ============================================================================

// ClientRecord represents the server-side state for a single NFSv4 client.
// It tracks the client's identity, verifier, confirmation status, and
// callback information for delegations.
//
// Per RFC 7530 Section 9.1.1, each client is identified by:
//   - ClientIDString: opaque string from nfs_client_id4.id (unique per client)
//   - Verifier: 8-byte value that changes on client reboot
//   - ClientID: server-assigned 64-bit identifier
type ClientRecord struct {
	// ClientID is the server-assigned 64-bit client identifier.
	// Generated using boot epoch (high 32) + sequence counter (low 32).
	ClientID uint64

	// ClientIDString is the client-provided opaque identifier (nfs_client_id4.id).
	// This is the stable identity that persists across reboots.
	ClientIDString string

	// Verifier is the client-provided 8-byte value that changes on reboot.
	// Used to detect client restarts.
	Verifier [8]byte

	// ConfirmVerifier is the server-generated 8-byte verifier returned
	// by SETCLIENTID and validated by SETCLIENTID_CONFIRM.
	// Generated using crypto/rand for unpredictability.
	ConfirmVerifier [8]byte

	// Confirmed indicates whether SETCLIENTID_CONFIRM has been called.
	Confirmed bool

	// Callback holds the client's callback information for delegations.
	Callback CallbackInfo

	// ClientAddr is the network address of the client (for logging/debugging).
	ClientAddr string

	// CreatedAt is when this record was created.
	CreatedAt time.Time

	// LastRenewal is the most recent lease renewal time.
	// Updated by RENEW, OPEN, and any implicit lease renewal.
	LastRenewal time.Time

	// Lease is the lease timer for this client.
	// Created when the client is confirmed via SETCLIENTID_CONFIRM.
	// Fires onLeaseExpired callback when the lease duration elapses
	// without renewal.
	Lease *LeaseState

	// OpenOwners tracks all open-owners for this client.
	// Keyed by hex-encoded owner data. Populated in Plan 09-02.
	OpenOwners map[string]*OpenOwner

	// CBPathUp indicates whether the callback path to this client has been
	// verified via CB_NULL. Defaults to false (not verified).
	// Set to true after a successful CB_NULL on SETCLIENTID_CONFIRM.
	// Set to false on CB_RECALL failure (callback path is down).
	CBPathUp bool
}

// VerifierMatches returns true if the given verifier matches this client's verifier.
func (cr *ClientRecord) VerifierMatches(v [8]byte) bool {
	return cr.Verifier == v
}

// ============================================================================
// Callback Info
// ============================================================================

// CallbackInfo holds the client's callback program information
// for delegation recall (Phase 11) and other server-initiated callbacks.
//
// Per RFC 7530 Section 16.33 (cb_client4):
//
//	struct cb_client4 {
//	    unsigned int cb_program;
//	    netaddr4     cb_location;
//	};
type CallbackInfo struct {
	// Program is the RPC program number for callbacks.
	Program uint32

	// NetID is the transport protocol ("tcp", "tcp6", etc.).
	NetID string

	// Addr is the callback address in universal address format.
	Addr string
}

// ============================================================================
// SetClientID Result
// ============================================================================

// SetClientIDResult is the result returned by StateManager.SetClientID.
// It contains the values needed for the SETCLIENTID response.
type SetClientIDResult struct {
	// ClientID is the server-assigned 64-bit client identifier.
	ClientID uint64

	// ConfirmVerifier is the server-generated verifier for SETCLIENTID_CONFIRM.
	ConfirmVerifier [8]byte
}

// Note: OpenOwner is defined in openowner.go with full seqid validation,
// replay caching, and open state management (Plan 09-02).
