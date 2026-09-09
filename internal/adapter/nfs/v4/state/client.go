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

// ErrRenewAccess indicates a RENEW arrived under a principal that neither
// established the client ID nor holds an open under it (RFC 7530 Section
// 16.28.5). Maps to NFS4ERR_ACCESS (13).
var ErrRenewAccess = errors.New("renew principal not permitted for this client ID")

// principalHijacks reports whether a request carrying incoming may take over a
// client record established by stored.
//
// RFC 7530 Section 19 requires the principal on SETCLIENTID and
// SETCLIENTID_CONFIRM to be "checked against and matched with the previous use
// of these operations", because those are the operations that release a
// client's state; Section 9.1.1 states the rule as a MUST NOT on cancelling
// leased state established by a different principal. RFC 8881 Section 18.35.4
// case 9 is the same rule for EXCHANGE_ID.
//
// A request that carries no identity at all does not match a record that has
// one. Admitting it would leave the guard reachable only by callers that
// volunteer a credential: a client ID established under Kerberos would be
// takeable by an AUTH_NONE caller, and neither SETCLIENTID nor
// SETCLIENTID_CONFIRM carries a filehandle, so an export's sec= policy never
// sees the request.
//
// A record with no stored principal stays open: it was established without an
// identity, and there is nothing to check a later request against.
func principalHijacks(stored, incoming string) bool {
	return stored != "" && stored != incoming
}

// ============================================================================
// Client Record
// ============================================================================

// ClientRecord represents the server-side state for a single NFSv4 client of
// any minor version. It tracks the client's identity, verifier, confirmation
// status, and callback information for delegations.
//
// A v4.0 client is established by SETCLIENTID + SETCLIENTID_CONFIRM and is
// identified per RFC 7530 Section 9.1.1 by:
//   - ClientIDString: opaque string from nfs_client_id4.id (unique per client)
//   - Verifier: 8-byte value that changes on client reboot
//   - ClientID: server-assigned 64-bit identifier
//
// A v4.1 client is established by EXCHANGE_ID + CREATE_SESSION and is
// identified per RFC 8881 Section 18.35 by OwnerID (co_ownerid) and the same
// Verifier (co_verifier) and ClientID. The two identity fields stay distinct
// because they index different lookup maps and different durable
// client-recovery keys; the wire formats they carry are not interchangeable.
//
// Fields that apply to only one minor version are marked as such and are left
// at their zero value for the other. Anything version-independent — lease
// timing, principal, callback-path liveness — is shared, so a policy decision
// that reads it does not have to know which registration flow created the
// record.
type ClientRecord struct {
	// ClientID is the server-assigned 64-bit client identifier.
	// Generated using boot epoch (high 32) + sequence counter (low 32).
	ClientID uint64

	// ClientIDString is the client-provided opaque identifier
	// (nfs_client_id4.id). v4.0 only. This is the stable identity that
	// persists across reboots.
	ClientIDString string

	// OwnerID is co_ownerid from client_owner4. v4.1 only. Stored as bytes
	// for byte-exact comparison.
	OwnerID []byte

	// Verifier is the client-provided 8-byte value that changes on reboot.
	// Used to detect client restarts. co_verifier on v4.1.
	Verifier [8]byte

	// ConfirmVerifier is the server-generated 8-byte verifier returned
	// by SETCLIENTID and validated by SETCLIENTID_CONFIRM. v4.0 only.
	// Generated using crypto/rand for unpredictability.
	ConfirmVerifier [8]byte

	// Confirmed indicates whether the client has completed registration:
	// SETCLIENTID_CONFIRM on v4.0, CREATE_SESSION on v4.1.
	Confirmed bool

	// Callback holds the client's callback information for delegations.
	// v4.0 only: a v4.1 client's callbacks travel over the session
	// backchannel, which carries no separate address.
	Callback CallbackInfo

	// ClientAddr is the network address of the client (for logging/debugging).
	ClientAddr string

	// Principal is the RPCSEC_GSS / AUTH_SYS principal that established this
	// client (best-effort; "uid:N" for AUTH_SYS, the GSS principal otherwise,
	// "" when unknown). Captured at SETCLIENTID or EXCHANGE_ID and persisted
	// into the durable client-recovery record at confirm time as a
	// lease-stealing guard.
	Principal string

	// ImplDomain is the implementation domain from nfs_impl_id4
	// (e.g. "kernel.org"). v4.1 only.
	ImplDomain string

	// ImplName is the implementation name from nfs_impl_id4
	// (e.g. "Linux NFS client"). v4.1 only.
	ImplName string

	// ImplDate is the build date from nfs_impl_id4. v4.1 only.
	ImplDate time.Time

	// SequenceID is the CREATE_SESSION slot sequence ID, initialized to 0.
	// v4.1 only. ExchangeID returns SequenceID+1 as eir_sequenceid so the
	// client sends that value as csa_sequenceid. CreateSession validates
	// csa_sequenceid == SequenceID+1 (the "new request" check per RFC 8881
	// Section 18.36), then advances SequenceID to match.
	SequenceID uint32

	// CachedCreateSessionRes is the XDR-encoded CREATE_SESSION reply held for
	// replay per RFC 8881 Section 18.36. v4.1 only. Set when CREATE_SESSION
	// succeeds.
	CachedCreateSessionRes []byte

	// CreatedAt is when this record was created.
	CreatedAt time.Time

	// LastRenewal is the most recent lease renewal time.
	// Updated by RENEW, SEQUENCE, OPEN, and any implicit lease renewal.
	LastRenewal time.Time

	// Lease is the lease timer for this client.
	// Created when the client is confirmed.
	// Fires onLeaseExpired callback when the lease duration elapses
	// without renewal.
	Lease *LeaseState

	// OpenOwners tracks all open-owners for this client.
	// Keyed by hex-encoded owner data. v4.0 only.
	OpenOwners map[string]*OpenOwner

	// ReclaimComplete records that this client has sent RECLAIM_COMPLETE, so a
	// second one draws NFS4ERR_COMPLETE_ALREADY. v4.1 only: v4.0 has no such
	// operation. It holds whether or not a grace window was ever opened, and a
	// client instance that re-registers gets a fresh record with it clear.
	ReclaimComplete bool

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
// for delegation recall and other server-initiated callbacks.
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

	// Ident is the callback_ident the client supplied in SETCLIENTID. NFSv4.0
	// clients match an incoming CB_COMPOUND to their own mount by this value, so
	// it has to be echoed back in every callback; a callback carrying the wrong
	// one is rejected before any operation in it is looked at. NFSv4.1 replaced
	// it with the session and ignores the field.
	Ident uint32
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
// replay caching, and open state management .
