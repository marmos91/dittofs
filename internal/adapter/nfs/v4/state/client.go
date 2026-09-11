// Package state implements NFSv4 state management for client identity,
// open state, lock state, and lease tracking per RFC 7530 Section 9.
package state

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/logger"
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
// MinorVersion discriminates the two registration flows, and fields that
// apply to only one minor version are marked as such and are left at their
// zero value for the other. Anything version-independent — lease timing,
// principal, callback-path liveness — is shared, so a policy decision that
// reads it does not have to know which registration flow created the record.
type ClientRecord struct {
	// ClientID is the server-assigned 64-bit client identifier.
	// Generated using boot epoch (high 32) + sequence counter (low 32).
	ClientID uint64

	// MinorVersion is the NFSv4 minor version that minted this record: 0 for
	// the SETCLIENTID flow, 1 for the EXCHANGE_ID flow. generateClientID draws
	// both flows from one sequence, so client IDs never collide across the
	// shared numeric index; this field is what keeps a version-sensitive
	// operation from reaching a record the other flow created.
	MinorVersion uint32

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

	// Superseded is the confirmed record this unconfirmed one replaces once it
	// is itself confirmed. v4.1 only: a restarted client's new incarnation is
	// registered alongside the old one, which keeps its client ID, sessions and
	// locking state until the replacement's CREATE_SESSION collapses the two.
	// Nil on every other record.
	Superseded *ClientRecord

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

	// ReclaimComplete records that this client has completed its reclaim:
	// RECLAIM_COMPLETE sent (v4.1), or the first successful CLAIM_PREVIOUS
	// served (v4.0 has no RECLAIM_COMPLETE operation). A second v4.1
	// RECLAIM_COMPLETE draws NFS4ERR_COMPLETE_ALREADY. It holds whether or not
	// a grace window was ever opened, and a client instance that re-registers
	// gets a fresh record with it clear. A pending reclaim-complete persist
	// retry also re-validates against this flag before writing durably.
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

func (sm *StateManager) generateClientID() uint64 {
	seq := atomic.AddUint32(&sm.nextClientSeq, 1)
	return (uint64(sm.bootEpoch) << 32) | uint64(seq)
}

// generateConfirmVerifier creates an unpredictable 8-byte confirm verifier
// using crypto/rand. This prevents malicious or stale clients from guessing
// the verifier and confirming someone else's SETCLIENTID.
//
// Timestamps are not an acceptable source here: they are predictable, which is
// the whole property the verifier must not have.

func (sm *StateManager) generateConfirmVerifier() [8]byte {
	var v [8]byte
	_, _ = rand.Read(v[:])
	return v
}

// SetClientID implements the five-case SETCLIENTID algorithm per RFC 7530 Section 9.1.1.
//
// The algorithm determines the action based on whether the server has
// a confirmed and/or unconfirmed record for the client's id string:
//
//   - Case 1: No confirmed, no unconfirmed -- create new unconfirmed record
//   - Case 2: Confirmed exists + unconfirmed exists -- replace unconfirmed
//   - Case 3: Confirmed exists, different verifier -- client reboot, create new unconfirmed
//   - Case 4: No confirmed, unconfirmed exists -- replace unconfirmed
//   - Case 5: Confirmed exists, same verifier -- re-SETCLIENTID (callback update)
//
// Returns the client ID and confirm verifier on success, or an error.
// The trailing principal is variadic so existing callers/tests that do not
// thread an auth principal keep compiling; production passes ctx.Principal().

func (sm *StateManager) SetClientID(clientIDStr string, verifier [8]byte, callback CallbackInfo, clientAddr string, principal ...string) (*SetClientIDResult, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	princ := firstOrEmpty(principal)
	confirmed := sm.clientsByName[clientIDStr]
	unconfirmed := sm.unconfirmedByName[clientIDStr]

	switch {
	case confirmed == nil && unconfirmed == nil:
		// Case 1: Completely new client
		return sm.createNewClient(clientIDStr, verifier, callback, clientAddr, princ)

	case confirmed != nil && confirmed.VerifierMatches(verifier):
		// Case 5: Same client, same verifier (re-SETCLIENTID, maybe callback update)
		// This handles the case where the client sends SETCLIENTID again with
		// the same verifier. We update the callback and return a new unconfirmed
		// record that, when confirmed, will replace the existing confirmed record.
		return sm.reuseConfirmedClient(confirmed, clientIDStr, verifier, callback, clientAddr, princ)

	case confirmed != nil && !confirmed.VerifierMatches(verifier):
		// Case 3: Same client ID string, different verifier (client reboot)
		// The client has restarted. Create a new unconfirmed record.
		// The old confirmed record is NOT removed yet -- it gets replaced
		// when the new record is confirmed.
		return sm.handleClientReboot(clientIDStr, verifier, callback, clientAddr, princ)

	case confirmed == nil && unconfirmed != nil:
		// Case 4: No confirmed record, unconfirmed exists -- replace unconfirmed
		return sm.replaceUnconfirmed(unconfirmed, clientIDStr, verifier, callback, clientAddr, princ)

	default:
		// Case 2: Confirmed exists AND unconfirmed exists -- replace unconfirmed
		// This is a retransmit or new SETCLIENTID while another is pending.
		return sm.replaceUnconfirmed(unconfirmed, clientIDStr, verifier, callback, clientAddr, princ)
	}
}

// firstOrEmpty returns the first element of ss, or "" if ss is empty. Helper
// for the variadic principal parameter on SetClientID / ExchangeID.

func firstOrEmpty(ss []string) string {
	if len(ss) > 0 {
		return ss[0]
	}
	return ""
}

// clientHasLiveStateLocked reports whether clientID still holds leased state
// that another principal's SETCLIENTID would cancel -- an open, a byte-range
// lock or a delegation -- under a lease that has not lapsed.
//
// Locks are counted separately from opens rather than through them: a lock
// hangs off an open state, but LOCK takes the lock-owner's client ID from the
// wire and does not require it to match the client that owns that open, so a
// client can hold live lock state without owning an open here.
//
// This is what makes the principal check in RFC 7530 Section 16.33.5
// conditional. Section 9.1.2 spells the condition out: when a SETCLIENTID
// arrives "for a client ID that currently has no state, or it has state but
// the lease has expired, rather than returning NFS4ERR_CLID_INUSE, the server
// MUST allow the SETCLIENTID". The security rule the check exists for is the
// MUST NOT in Section 9.1.1 against cancelling leased state established by a
// different principal, and a record holding none has nothing to cancel.
//
// Refusing unconditionally makes a client id string permanently unusable by
// every other principal once one has touched it. Clients derive that string
// from the hostname rather than from their credential, so a second user on the
// same host would never get a client id at all.
//
// Caller must hold sm.mu.

func (sm *StateManager) clientHasLiveStateLocked(clientID uint64) bool {
	if sm.clientLeaseLapsedLocked(clientID) {
		return false
	}
	for _, owner := range sm.openOwners {
		if owner.ClientID == clientID && len(owner.OpenStates) > 0 {
			return true
		}
	}
	for _, lockState := range sm.lockStateByOther {
		if lockState.LockOwner != nil && lockState.LockOwner.ClientID == clientID {
			return true
		}
	}
	for _, deleg := range sm.delegByOther {
		if deleg.ClientID == clientID {
			return true
		}
	}
	return false
}

// unknownClientIDError answers a client ID that no record matches. There are
// two ways that happens and they call for different errors. RFC 7530 Section
// 9.6.3.2 covers the first: once a lease is cancelled, "the use of the
// associated clientid will result in NFS4ERR_EXPIRED being returned", telling
// the client its state is gone and a new client id is what it needs. An id no
// boot of this server could have issued means something else entirely -- the
// server restarted -- and stays NFS4ERR_STALE_CLIENTID. generateClientID puts
// the boot epoch in the high 32 bits, which is what keeps the two apart.
//
// Answering STALE_CLIENTID for both makes a client that merely fell behind on
// RENEW conclude the server rebooted.

func (sm *StateManager) unknownClientIDError(clientID uint64) error {
	if uint32(clientID>>32) == sm.bootEpoch {
		return ErrExpired
	}
	return ErrStaleClientID
}

// createNewClient handles Case 1: completely new client.
// Creates a new unconfirmed record with a fresh client ID and confirm verifier.
// Caller must hold sm.mu.

func (sm *StateManager) createNewClient(clientIDStr string, verifier [8]byte, callback CallbackInfo, clientAddr, principal string) (*SetClientIDResult, error) {
	clientID := sm.generateClientID()
	confirmVerf := sm.generateConfirmVerifier()

	record := &ClientRecord{
		ClientID:        clientID,
		ClientIDString:  clientIDStr,
		Verifier:        verifier,
		ConfirmVerifier: confirmVerf,
		Confirmed:       false,
		Callback:        callback,
		ClientAddr:      clientAddr,
		Principal:       principal,
		CreatedAt:       time.Now(),
		OpenOwners:      make(map[string]*OpenOwner),
	}

	// Store as unconfirmed
	sm.unconfirmedByName[clientIDStr] = record
	sm.clientsByID[clientID] = record

	logger.Info("SETCLIENTID: new client registered (unconfirmed)",
		"client_id_str", clientIDStr,
		"client_id", clientID,
		"client_addr", clientAddr)

	return &SetClientIDResult{
		ClientID:        clientID,
		ConfirmVerifier: confirmVerf,
	}, nil
}

// reuseConfirmedClient handles Case 5: re-SETCLIENTID with same verifier.
// The confirmed record exists with a matching verifier. Create a new
// unconfirmed record that will replace the confirmed one when confirmed.
// Caller must hold sm.mu.

func (sm *StateManager) reuseConfirmedClient(confirmed *ClientRecord, clientIDStr string, verifier [8]byte, callback CallbackInfo, clientAddr, principal string) (*SetClientIDResult, error) {
	// Reject a re-SETCLIENTID by anyone but the principal that established the
	// confirmed record: it would hijack the client's lease and state. See
	// principalHijacks and clientHasLiveStateLocked.
	if principalHijacks(confirmed.Principal, principal) && sm.clientHasLiveStateLocked(confirmed.ClientID) {
		return nil, ErrClientIDInUse
	}

	// Remove any existing unconfirmed record for this name
	if old := sm.unconfirmedByName[clientIDStr]; old != nil {
		// Only delete from clientsByID if it's a different ID than the confirmed client.
		// The unconfirmed record reuses confirmed.ClientID, so we must not delete
		// the confirmed client's entry from clientsByID.
		if old.ClientID != confirmed.ClientID {
			delete(sm.clientsByID, old.ClientID)
		}
		delete(sm.unconfirmedByName, clientIDStr)
	}

	// Create new unconfirmed record with the SAME client ID
	// (re-SETCLIENTID reuses the confirmed client ID)
	confirmVerf := sm.generateConfirmVerifier()

	record := &ClientRecord{
		ClientID:        confirmed.ClientID,
		ClientIDString:  clientIDStr,
		Verifier:        verifier,
		ConfirmVerifier: confirmVerf,
		Confirmed:       false,
		Callback:        callback,
		ClientAddr:      clientAddr,
		Principal:       principal,
		CreatedAt:       time.Now(),
		OpenOwners:      make(map[string]*OpenOwner),
	}

	sm.unconfirmedByName[clientIDStr] = record
	// Note: don't overwrite clientsByID[confirmed.ClientID] here --
	// the confirmed record still holds it until SETCLIENTID_CONFIRM.

	logger.Debug("SETCLIENTID: re-SETCLIENTID for confirmed client",
		"client_id_str", clientIDStr,
		"client_id", confirmed.ClientID,
		"client_addr", clientAddr)

	return &SetClientIDResult{
		ClientID:        confirmed.ClientID,
		ConfirmVerifier: confirmVerf,
	}, nil
}

// handleClientReboot handles Case 3: client reboot (different verifier).
// Creates a new unconfirmed record. The old confirmed record stays until
// the new one is confirmed in SETCLIENTID_CONFIRM.
// Caller must hold sm.mu.

func (sm *StateManager) handleClientReboot(clientIDStr string, verifier [8]byte, callback CallbackInfo, clientAddr, principal string) (*SetClientIDResult, error) {
	// A reboot (different verifier) claimed by anyone but the principal that
	// established the confirmed record is a hijack attempt, not a real reboot.
	// See principalHijacks and clientHasLiveStateLocked.
	if confirmed := sm.clientsByName[clientIDStr]; confirmed != nil {
		if principalHijacks(confirmed.Principal, principal) && sm.clientHasLiveStateLocked(confirmed.ClientID) {
			return nil, ErrClientIDInUse
		}
	}

	// Remove any existing unconfirmed record for this name
	if old := sm.unconfirmedByName[clientIDStr]; old != nil {
		delete(sm.clientsByID, old.ClientID)
		delete(sm.unconfirmedByName, clientIDStr)
	}

	// Create a brand new client ID (reboot = new identity)
	clientID := sm.generateClientID()
	confirmVerf := sm.generateConfirmVerifier()

	record := &ClientRecord{
		ClientID:        clientID,
		ClientIDString:  clientIDStr,
		Verifier:        verifier,
		ConfirmVerifier: confirmVerf,
		Confirmed:       false,
		Callback:        callback,
		ClientAddr:      clientAddr,
		Principal:       principal,
		CreatedAt:       time.Now(),
		OpenOwners:      make(map[string]*OpenOwner),
	}

	sm.unconfirmedByName[clientIDStr] = record
	sm.clientsByID[clientID] = record

	logger.Info("SETCLIENTID: client reboot detected, new unconfirmed record",
		"client_id_str", clientIDStr,
		"new_client_id", clientID,
		"client_addr", clientAddr)

	return &SetClientIDResult{
		ClientID:        clientID,
		ConfirmVerifier: confirmVerf,
	}, nil
}

// replaceUnconfirmed handles Case 2 and Case 4: replace existing unconfirmed.
// Removes the old unconfirmed record and creates a new one.
// Caller must hold sm.mu.

func (sm *StateManager) replaceUnconfirmed(old *ClientRecord, clientIDStr string, verifier [8]byte, callback CallbackInfo, clientAddr, principal string) (*SetClientIDResult, error) {
	// Remove old unconfirmed record
	delete(sm.clientsByID, old.ClientID)
	delete(sm.unconfirmedByName, clientIDStr)

	// Create new unconfirmed record
	clientID := sm.generateClientID()
	confirmVerf := sm.generateConfirmVerifier()

	record := &ClientRecord{
		ClientID:        clientID,
		ClientIDString:  clientIDStr,
		Verifier:        verifier,
		ConfirmVerifier: confirmVerf,
		Confirmed:       false,
		Callback:        callback,
		ClientAddr:      clientAddr,
		Principal:       principal,
		CreatedAt:       time.Now(),
		OpenOwners:      make(map[string]*OpenOwner),
	}

	sm.unconfirmedByName[clientIDStr] = record
	sm.clientsByID[clientID] = record

	logger.Debug("SETCLIENTID: replaced unconfirmed record",
		"client_id_str", clientIDStr,
		"new_client_id", clientID,
		"client_addr", clientAddr)

	return &SetClientIDResult{
		ClientID:        clientID,
		ConfirmVerifier: confirmVerf,
	}, nil
}

// ConfirmClientID implements SETCLIENTID_CONFIRM per RFC 7530 Section 9.1.1.
//
// It validates the confirm verifier and promotes the unconfirmed record to
// confirmed status. If a different confirmed record existed for the same
// client ID string, it is replaced.
//
// After confirmation, a lease timer is created for the client.
//
// Returns nil on success, or ErrStaleClientID if the client ID is unknown,
// or ErrStaleClientID if the confirm verifier doesn't match.

func (sm *StateManager) ConfirmClientID(clientID uint64, confirmVerifier [8]byte) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Look up the record by client ID. SETCLIENTID_CONFIRM is a v4.0-only
	// operation, so a v4.1 record under this ID must not be reachable here:
	// confirming it would arm the CBPathUp probe and the lease timer the
	// EXCHANGE_ID flow manages through CREATE_SESSION instead.
	record := sm.v40ClientLocked(clientID)
	if record == nil {
		return fmt.Errorf("%w: client ID %d not found", ErrStaleClientID, clientID)
	}

	// If already confirmed, check for a pending re-SETCLIENTID (Case 5)
	// where an unconfirmed record exists with the same client ID.
	if record.Confirmed {
		if unconfirmed := sm.unconfirmedByName[record.ClientIDString]; unconfirmed != nil && unconfirmed.ClientID == clientID {
			// The re-SETCLIENTID reuses the client ID, so confirm the record
			// that already owns it and fold in what the new one carried.
			// Swapping the unconfirmed record in instead would leave two
			// records under one ID, and the one clientsByID does not point at
			// keeps a lease timer that RENEW never refreshes yet that still
			// fires and reaps the client.
			//
			// Validated before the fold, not by the shared check below: the
			// fold writes through to the live client, so a confirm carrying the
			// wrong verifier must be refused while the record it names is still
			// untouched. A stale retransmit of the previous confirm reaches
			// here whenever a re-SETCLIENTID is pending.
			if unconfirmed.ConfirmVerifier != confirmVerifier {
				return fmt.Errorf("%w: confirm verifier mismatch for client %d", ErrStaleClientID, clientID)
			}
			record.Verifier = unconfirmed.Verifier
			record.ConfirmVerifier = unconfirmed.ConfirmVerifier
			record.Callback = unconfirmed.Callback
			record.ClientAddr = unconfirmed.ClientAddr
			record.Principal = unconfirmed.Principal
			// The callback address just changed, so the path to it is unproven
			// again until the CB_NULL below says otherwise. Carrying the old
			// generation's verdict forward would let delegations be granted in
			// the window before that probe answers, and recalled to an address
			// this client never confirmed it listens on.
			record.CBPathUp = false
		} else {
			// True retransmit - validate verifier matches the confirmed record
			if record.ConfirmVerifier != confirmVerifier {
				return fmt.Errorf("%w: confirm verifier mismatch for confirmed client %d", ErrStaleClientID, clientID)
			}
			logger.Debug("SETCLIENTID_CONFIRM: retransmit for already-confirmed client",
				"client_id", clientID)
			return nil
		}
	}

	// Validate confirm verifier
	if record.ConfirmVerifier != confirmVerifier {
		return fmt.Errorf("%w: confirm verifier mismatch for client %d", ErrStaleClientID, clientID)
	}

	// Promote to confirmed -- remove from unconfirmed
	delete(sm.unconfirmedByName, record.ClientIDString)

	// If there's an existing confirmed record for the same name, remove it
	// (this happens on client reboot: Case 3 followed by CONFIRM)
	if oldConfirmed := sm.clientsByName[record.ClientIDString]; oldConfirmed != nil && oldConfirmed.ClientID != record.ClientID {
		// Stop old client's lease timer before removing
		if oldConfirmed.Lease != nil {
			oldConfirmed.Lease.Stop()
		}
		// The client rebooted, and RFC 7530 Section 16.34.5 requires more of
		// this confirm than forgetting the record: where a confirmed record
		// for the same id string already exists, "the server MUST remove
		// client x's relevant leased client state". Leaving it behind keeps
		// every stateid the client held before the reboot working, so the
		// files stay share-reserved and byte-range locked on behalf of an
		// incarnation that no longer exists and will never close them.
		sm.releaseClientStateLocked(oldConfirmed.ClientID)
		delete(sm.clientsByID, oldConfirmed.ClientID)
		logger.Info("SETCLIENTID_CONFIRM: replaced old confirmed client",
			"old_client_id", oldConfirmed.ClientID,
			"new_client_id", clientID)
	}

	// Mark as confirmed and store in confirmed map
	record.Confirmed = true
	sm.clientsByName[record.ClientIDString] = record

	// Create the lease timer for the newly confirmed client, replacing any
	// timer the record already carries: an orphaned timer still fires
	// onLeaseExpired for this client ID on its original schedule, reaping the
	// client however often RENEW refreshes the lease that replaced it.
	if record.Lease != nil {
		record.Lease.Stop()
	}
	record.Lease = NewLeaseState(clientID, sm.leaseDuration, sm.onLeaseExpired)
	record.LastRenewal = time.Now()

	logger.Info("SETCLIENTID_CONFIRM: client confirmed",
		"client_id", clientID,
		"client_id_str", record.ClientIDString,
		"client_addr", record.ClientAddr)

	// Persist a durable client-recovery record so this client can reclaim its
	// state after an ungraceful server restart. Best-effort under
	// sm.mu: a persist failure logs a durability alarm but the confirm STILL
	// succeeds (the in-memory record is authoritative for this process). No-op
	// when no recovery store is wired.
	sm.persistClientRecoveryLocked(record.ClientID, record.ClientIDString, record.Verifier, record.Principal)

	// Verify callback path asynchronously via CB_NULL.
	// This runs in a goroutine so SETCLIENTID_CONFIRM returns immediately.
	if record.Callback.Addr != "" {
		cbInfo := record.Callback
		recordPtr := record // capture this record generation's pointer identity
		go func() {
			err := sm.cbNullFunc(context.Background(), cbInfo)
			sm.mu.Lock()
			defer sm.mu.Unlock()
			rec := sm.v40ClientLocked(clientID)
			if rec == nil || rec != recordPtr || rec.Callback != cbInfo {
				// Client was removed, this client ID now points at a different
				// record generation (reboot) while CB_NULL was in flight, or the
				// record kept its identity but moved to another callback address
				// (re-SETCLIENTID). Do not report this probe's verdict about an
				// address the record no longer uses.
				return
			}
			rec.CBPathUp = (err == nil)
			if err != nil {
				logger.Warn("CB_NULL failed, delegations disabled for client",
					"client_id", clientID, "error", err)
			} else {
				logger.Debug("CB_NULL succeeded, delegations enabled for client",
					"client_id", clientID)
			}
		}()
	}

	return nil
}

// GetClient returns the client record for the given client ID, or nil
// if no record exists. Used by RENEW and other operations that need
// to look up client state.
//
// v4.0 only: the shared index holds both minor versions, so a v4.1 client ID
// is filtered out here and comes back nil. Use clientRecordLocked for a
// lookup that should not care which flow minted the ID.

func (sm *StateManager) GetClient(clientID uint64) *ClientRecord {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.v40ClientLocked(clientID)
}

// clientRecordLocked returns the record for clientID from the shared client
// index, or nil when no client owns that ID.
//
// Callers hold a client ID and have no reason to know which minor version
// minted it, which is why version-independent policy reads the record through
// here rather than naming a version.
//
// Caller must hold sm.mu.

func (sm *StateManager) clientRecordLocked(clientID uint64) *ClientRecord {
	return sm.clientsByID[clientID]
}

// v40ClientLocked returns the v4.0 record for clientID, or nil when the ID is
// unknown or names a v4.1 record. Every SETCLIENTID-flow operation reads the
// index through here: a v4.1 client must be invisible to RENEW, to
// SETCLIENTID_CONFIRM, and to the v4.0 expiry path, all of which would
// otherwise act on state the EXCHANGE_ID flow owns.
//
// Caller must hold sm.mu.

func (sm *StateManager) v40ClientLocked(clientID uint64) *ClientRecord {
	record := sm.clientsByID[clientID]
	if record == nil || record.MinorVersion != 0 {
		return nil
	}
	return record
}

// v41ClientLocked returns the v4.1 record for clientID, or nil when the ID is
// unknown or names a v4.0 record. Every EXCHANGE_ID-flow operation reads the
// index through here, mirroring v40ClientLocked.
//
// Caller must hold sm.mu.

func (sm *StateManager) v41ClientLocked(clientID uint64) *ClientRecord {
	record := sm.clientsByID[clientID]
	if record == nil || record.MinorVersion != 1 {
		return nil
	}
	return record
}

// renewConfirmedClient admits a confirmed client whose lease is still live and
// stamps the renewal. Shared by RENEW and by the implicit renewal every
// operation carrying a valid clientid gets, so the two admission paths cannot
// disagree about what a renewal updates.
//
// Caller must hold sm.mu.

func renewConfirmedClient(record *ClientRecord) error {
	if !record.Confirmed {
		return ErrStaleClientID
	}
	if record.Lease != nil {
		if record.Lease.IsExpired() {
			return ErrExpired
		}
		record.Lease.Renew()
	}
	record.LastRenewal = time.Now()
	return nil
}

// ValidateAndRenewClient admits clientID for an operation and renews its lease
// in one critical section. Unknown or unconfirmed gives ErrStaleClientID (RFC
// 7530 Sections 9.1.1 and 13.1.10.2); a lapsed lease not yet reaped gives
// ErrExpired (Section 9.6.3.2).
//
// Renewal is what Section 9.6.2 grants any operation carrying a valid clientid.
// Without it a v4.0 client that stops sending RENEW once it holds no open state
// has an active mount mistaken for an idle one.
//
// Callers admit before doing any work: OPEN creates or truncates its target
// before it establishes state, so a clientid rejected afterwards strands a file
// the client was told it had not created.

func (sm *StateManager) ValidateAndRenewClient(clientID uint64) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	record := sm.clientRecordLocked(clientID)
	if record == nil {
		return sm.unknownClientIDError(clientID)
	}
	return renewConfirmedClient(record)
}

// RemoveClient removes a client record and all associated state.
// Used by lease expiry to clean up expired clients.

func (sm *StateManager) RemoveClient(clientID uint64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	record := sm.v40ClientLocked(clientID)
	if record == nil {
		return
	}

	// Stop lease timer
	if record.Lease != nil {
		record.Lease.Stop()
	}

	// Remove from all maps
	delete(sm.clientsByID, clientID)
	if record.Confirmed {
		if confirmed := sm.clientsByName[record.ClientIDString]; confirmed != nil && confirmed.ClientID == clientID {
			delete(sm.clientsByName, record.ClientIDString)
		}
	} else {
		if unconfirmed := sm.unconfirmedByName[record.ClientIDString]; unconfirmed != nil && unconfirmed.ClientID == clientID {
			delete(sm.unconfirmedByName, record.ClientIDString)
		}
	}

	logger.Info("Client record removed",
		"client_id", clientID,
		"client_id_str", record.ClientIDString)
}

// removeClientOpenStateLocked drops every open-owner belonging to clientID
// together with its open states, its lock states, and the locks those hold in
// the unified lock manager.
//
// It scans sm.openOwners by ClientID rather than walking the record's
// OpenOwners map: that map is only populated on the v4.0 path, and it goes
// stale even there because freeOpenStateidLocked removes owners from
// sm.openOwners without removing them from it.
//
// Caller must hold sm.mu.

func (sm *StateManager) GetConfirmedClientIDs() []uint64 {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	ids := make([]uint64, 0, len(sm.clientsByName))
	for _, record := range sm.clientsByName {
		ids = append(ids, record.ClientID)
	}
	return ids
}

// LoadPreviousClients populates the expected clients list for grace period startup.
// Called by the NFS adapter after reading saved client IDs from disk.
// This is a convenience wrapper around StartGracePeriod.

func (sm *StateManager) LoadPreviousClients(clientIDs []uint64) {
	sm.StartGracePeriod(clientIDs)
}

// SaveClientState returns snapshots of all confirmed clients for serialization.
// The NFS adapter calls this during graceful shutdown to persist client state
// to disk, enabling grace period recovery on the next startup.

func (sm *StateManager) SaveClientState() []ClientSnapshot {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	snapshots := make([]ClientSnapshot, 0, len(sm.clientsByName))
	for _, record := range sm.clientsByName {
		snapshots = append(snapshots, ClientSnapshot{
			ClientID:       record.ClientID,
			ClientIDString: record.ClientIDString,
			Verifier:       record.Verifier,
			ClientAddr:     record.ClientAddr,
		})
	}
	return snapshots
}

// ============================================================================
// Open File Operations
// ============================================================================

// OpenFile implements the state management side of OPEN.
//
// It looks up or creates an OpenOwner for (clientID, ownerData), validates
// the seqid, and either creates a new OpenState or updates an existing one
// (share_access/share_deny accumulation for same file).
//
// Grace period rules:
//   - CLAIM_NULL (new open): blocked with NFS4ERR_GRACE during grace period
//   - CLAIM_PREVIOUS (reclaim): allowed during grace period, blocked with
//     NFS4ERR_NO_GRACE outside grace period
//
// Per RFC 7530 Section 9.1.7:
//   - First OPEN for a new owner creates unconfirmed state + sets OPEN4_RESULT_CONFIRM
//   - Subsequent OPENs from a confirmed owner do not set CONFIRM
//   - Same owner + same file => OR the share_access and share_deny bits
//
// Caller must NOT hold sm.mu.

func renewPrincipalAllowed(record *ClientRecord, principal string) bool {
	if record.Principal == "" || principal == record.Principal {
		return true
	}
	for _, owner := range record.OpenOwners {
		if owner.Principal == principal && len(owner.OpenStates) > 0 {
			return true
		}
	}
	return false
}

// RenewLease implements the RENEW operation's state management.
//
// Per RFC 7530 Section 16.28:
//   - Validates the client ID exists and is confirmed
//   - Validates the caller may renew this client's lease (Section 16.28.5)
//   - Resets the lease timer and updates LastRenewal
//   - Returns ErrStaleClientID if client is unknown or unconfirmed
//   - Returns NFS4ERR_EXPIRED if the lease has already expired
//   - Returns NFS4ERR_ACCESS if the caller is not one of the principals
//     Section 16.28.5 permits
//
// The trailing principal is variadic so callers and tests that do not thread an
// auth principal keep compiling; production passes ctx.Principal().
//
// Caller must NOT hold sm.mu.

func (sm *StateManager) RenewLease(clientID uint64, principal ...string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// v4.0 only: RENEW does not exist in v4.1, where SEQUENCE renews the lease.
	// A v4.1 record must be unreachable through this renewal: it would stamp a
	// lease the SEQUENCE handler owns.
	record := sm.v40ClientLocked(clientID)
	if record == nil {
		return sm.unknownClientIDError(clientID)
	}

	// Checked before the renewal below: a refused RENEW must leave the lease
	// exactly where it was, or the rejection still hands the caller the effect
	// it asked for.
	if !renewPrincipalAllowed(record, firstOrEmpty(principal)) {
		return ErrRenewAccess
	}

	if err := renewConfirmedClient(record); err != nil {
		return err
	}

	logger.Debug("RenewLease: lease renewed",
		"client_id", clientID,
		"client_id_str", record.ClientIDString)

	return nil
}

// ============================================================================
// Lock Manager Integration
// ============================================================================

// SetLockManager sets the static unified lock manager for byte-range conflict
// detection. Init-only: must be called during construction, before any goroutine
// serves requests, so lock-free reads in lockManagerFor are safe. Primarily used
// by tests; production uses SetLockManagerResolver.

func (sm *StateManager) RenewV41Lease(clientID uint64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	record := sm.v41ClientLocked(clientID)
	if record == nil {
		return
	}

	record.LastRenewal = time.Now()
	if record.Lease != nil {
		record.Lease.Renew()
	}

	logger.Debug("RenewV41Lease: v4.1 lease renewed",
		"client_id", fmt.Sprintf("0x%x", clientID))
}

// GetStatusFlags computes the SEQ4_STATUS_* bitmask for a SEQUENCE response.
//
// Per RFC 8881 Section 18.46, the server reports status flags covering:
//   - Callback path health (CB_PATH_DOWN, BACKCHANNEL_FAULT)
//   - Lease expiry (EXPIRED_ALL_STATE_REVOKED)
//   - Delegation state (RECALLABLE_STATE_REVOKED)
//
// Thread-safe: acquires sm.mu.RLock.

func (sm *StateManager) GetStatusFlags(session *Session) uint32 {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var flags uint32

	// CB_PATH_DOWN: set when no back-bound connection exists for any session
	// of this client. Cleared when at least one back-bound connection is present.
	sm.connMu.RLock()
	hasBack := sm.hasBackBoundConnection(session.ClientID)
	hasFault := sm.backchannelFaults[session.ClientID]
	sm.connMu.RUnlock()

	if session.BackChannelSlots == nil || !hasBack {
		flags |= types.SEQ4_STATUS_CB_PATH_DOWN
	}

	// BACKCHANNEL_FAULT: set when a callback send actually fails.
	if hasFault {
		flags |= types.SEQ4_STATUS_BACKCHANNEL_FAULT
	}

	// Check client lease expiry
	if record := sm.v41ClientLocked(session.ClientID); record != nil && record.Lease != nil && record.Lease.IsExpired() {
		flags |= types.SEQ4_STATUS_EXPIRED_ALL_STATE_REVOKED
	}

	// Check for revoked delegations for this client via the per-client
	// revoked-delegation index (O(1)) instead of scanning every delegation.
	if sm.revokedDelegCount[session.ClientID] > 0 {
		flags |= types.SEQ4_STATUS_RECALLABLE_STATE_REVOKED
	}

	return flags
}

// ============================================================================
// NFSv4.1 Session Management
// ============================================================================

// CreateSession implements the CREATE_SESSION algorithm per RFC 8881 Section 18.36.
//
// The algorithm uses the client's sequence ID to detect replays:
//   - sequenceID == record.SequenceID: replay -- return cached response
//   - sequenceID == record.SequenceID + 1: new request -- create session
//   - otherwise: misordered -- return error
//
// On success, returns the CreateSessionResult and nil cached bytes. The encoded
// XDR response is also cached on the client record under sm.mu in the same
// critical section as the sequence-ID bump, so a concurrent retransmit that
// matches record.SequenceID always observes a populated cache (RFC 8881
// Section 18.36 replay requirement) — there is no window where the seqid has
// advanced but the cache is still nil.
// On replay, returns nil result and the cached XDR response bytes.
// On error, returns an appropriate NFS4StateError.
//
// The first successful CREATE_SESSION confirms the client and starts its lease.
//
// Caller must NOT hold sm.mu.
