package lock

import (
	"context"
	"time"
)

// ============================================================================
// NFSv4 Client Recovery Persistence (reboot/grace recovery)
// ============================================================================

// V4ClientRecoveryRecord is the durable representation of a confirmed NFSv4
// client identity. It is the DittoFS analog of the Linux nfsd client-record:
// intentionally tiny — id + boot verifier + principal — with NO opens, NO
// locks, and NO stateids. After an ungraceful restart the server reads the set
// of recovery records to know which clients may reclaim their prior state
// during the grace period (RFC 7530 §9.1.4 / RFC 8881 §8.4.2).
//
// Records are server-GLOBAL. The NFSv4 clientid namespace is not owned by any
// share, so the record is keyed by ClientIDString (the stable client identity
// from nfs_client_id4.id), not by share.
type V4ClientRecoveryRecord struct {
	// ClientID is the server-assigned clientid (epoch<<32 | seq) minted at
	// confirm time. Stored for diagnostics and stale-record correlation.
	ClientID uint64

	// ClientIDString is nfs_client_id4.id, the stable client identity. This
	// is the primary key for the record across all backends.
	ClientIDString string

	// BootVerifier is the client boot verifier captured at confirm time.
	// A reclaiming client whose verifier changed rebooted and must NOT
	// reclaim old state (SETCLIENTID Case-3 path). Round-trips byte-exact
	// through every backend.
	BootVerifier [8]byte

	// Principal is the RPCSEC_GSS / AUTH_SYS principal that confirmed the
	// client. Used as a lease-stealing guard at reclaim time.
	Principal string

	// ConfirmedAt is when the client was confirmed.
	ConfirmedAt time.Time

	// ServerEpoch is the epoch under which the client confirmed. Used to GC
	// stale records from previous server instances.
	ServerEpoch uint64

	// ReclaimComplete records that this client finished reclaim (v4.1
	// RECLAIM_COMPLETE, or first successful CLAIM_PREVIOUS for v4.0). It is
	// the minimal representation of the "reclaim done" marker: persisting it
	// means a SECOND restart inside one grace window does not wait on a
	// client that already reclaimed. Set via RecordReclaimComplete and
	// reported back through ListClientRecovery.
	ReclaimComplete bool
}

// ClientRecoveryStore provides server-global persistence for confirmed NFSv4
// client identities, enabling grace/reclaim after a server restart.
// Implementations exist in memory, badger, and postgres stores.
//
// Recovery flow:
//  1. On SETCLIENTID_CONFIRM (v4.0) / EXCHANGE_ID-confirm (v4.1):
//     PutClientRecovery records the confirmed client.
//  2. On lease expiry / DESTROY_CLIENTID: DeleteClientRecovery removes it.
//  3. On RECLAIM_COMPLETE (or first CLAIM_PREVIOUS for v4.0):
//     RecordReclaimComplete marks reclaim done.
//  4. On boot: ListClientRecovery returns the expected reclaim set.
//
// This slice adds ONLY the store; the v4 state manager wiring lands separately.
type ClientRecoveryStore interface {
	// PutClientRecovery stores or replaces the recovery record for a
	// confirmed client. Keyed by ClientIDString: a second Put with the same
	// ClientIDString replaces the prior record (latest wins, one row).
	PutClientRecovery(ctx context.Context, rec *V4ClientRecoveryRecord) error

	// DeleteClientRecovery removes the recovery record for a client.
	// Returns nil if no record exists.
	DeleteClientRecovery(ctx context.Context, clientIDString string) error

	// ListClientRecovery returns all stored recovery records. An empty store
	// returns an empty slice (not an error). Used on boot to compute the
	// expected reclaim set.
	ListClientRecovery(ctx context.Context) ([]*V4ClientRecoveryRecord, error)

	// RecordReclaimComplete persists that the client finished reclaim by
	// setting ReclaimComplete on its record. Returns nil if no record
	// exists for the client (nothing to mark).
	RecordReclaimComplete(ctx context.Context, clientIDString string) error
}
