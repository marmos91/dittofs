package state

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// ============================================================================
// SeqID Validation
// ============================================================================

// SeqIDValidation is the result of validating a seqid against an OpenOwner.
type SeqIDValidation int

const (
	// SeqIDOK indicates the seqid is the expected next value.
	SeqIDOK SeqIDValidation = iota

	// SeqIDReplay indicates the seqid matches the last processed seqid
	// (retransmit -- return cached result).
	SeqIDReplay

	// SeqIDBad indicates the seqid is neither expected nor a replay.
	SeqIDBad
)

// ============================================================================
// Owner Sequence
// ============================================================================

// ownerSeq is the seqid sequence an open- or lock-owner carries: the last seqid
// processed and the reply cached for replaying it. Both owner kinds embed it, so
// the sequencing rules of RFC 7530 Section 9.1.7 are written once — as Linux
// nfsd writes them once on the struct nfs4_stateowner both its owner kinds
// embed.
type ownerSeq struct {
	// LastSeqID is the last seqid processed for this owner. Failed operations
	// advance it too; see consumeSeqidOnError.
	LastSeqID uint32

	// LastResult is the cached result of the last operation on this owner.
	// Used for replay detection (same seqid returns cached result).
	LastResult *CachedResult
}

// ValidateSeqID checks whether a seqid is valid for this owner.
//
// Per RFC 7530 Section 9.1.7:
//   - Expected = LastSeqID + 1 (with wrap: 0xFFFFFFFF -> 1, not 0)
//   - seqid == expected -> SeqIDOK
//   - seqid == LastSeqID -> SeqIDReplay
//   - else -> SeqIDBad
func (seq *ownerSeq) ValidateSeqID(seqid uint32) SeqIDValidation {
	expected := nextSeqID(seq.LastSeqID)

	if seqid == expected {
		return SeqIDOK
	}
	if seqid == seq.LastSeqID {
		return SeqIDReplay
	}
	return SeqIDBad
}

// consumeSeqidOnError advances this owner's sequence to seqid and caches the
// failed reply, for an operation that failed after reaching seqid checking. It
// is a no-op on success — the success paths advance the seqid themselves, and
// the handler caches the encoded reply — and on the errors failedSeqidReply
// exempts.
//
// Operations defer it as soon as they resolve the owner, so it covers every
// outcome below that point including ones added later. That is safe before the
// seqid check itself because its two verdicts, NFS4ERR_BAD_SEQID and a replay,
// are both exempt; nfsd relies on the same property to call nfsd4_bump_seqid
// unconditionally from a single exit point per operation.
func (seq *ownerSeq) consumeSeqidOnError(seqid uint32, err error) {
	if status, consumed := failedSeqidReply(err); consumed {
		seq.LastSeqID = seqid
		seq.LastResult = &CachedResult{Status: status, Data: encodeStatusReply(status)}
	}
}

// failedSeqidReply reports the status to record for an operation that failed
// after reaching seqid checking, and whether that failure consumes the seqid at
// all.
//
// Per RFC 7530 Section 9.1.7 an owner's sequence advances on every operation
// that reaches seqid checking, whether or not it then succeeds. Only the errors
// listed below leave it untouched, because they mean the request was never
// attributable to the owner and the client does not advance its own sequence
// either. Everything else — NFS4ERR_LOCKS_HELD, NFS4ERR_SHARE_DENIED,
// NFS4ERR_OPENMODE, NFS4ERR_INVAL, … — consumes the seqid: a server that keeps
// it falls permanently one behind the client, and every later operation for that
// owner is answered NFS4ERR_BAD_SEQID, which the client can only escape by
// tearing the owner down. The list matches nfsd's seqid_mutating_err().
func failedSeqidReply(err error) (status uint32, consumed bool) {
	if err == nil {
		return 0, false
	}

	// A replay returns the previous operation's reply; it neither re-runs that
	// operation nor moves the sequence on.
	var replayErr *ReplayError
	if errors.As(err, &replayErr) {
		return replayErr.Status, false
	}

	var stateErr *NFS4StateError
	if !errors.As(err, &stateErr) {
		// Unclassified failure: the client sees NFS4ERR_SERVERFAULT, which is not
		// exempt, so the seqid is consumed like any other failure.
		return types.NFS4ERR_SERVERFAULT, true
	}

	switch stateErr.Status {
	case types.NFS4ERR_STALE_CLIENTID, types.NFS4ERR_STALE_STATEID,
		types.NFS4ERR_BAD_STATEID, types.NFS4ERR_BAD_SEQID,
		types.NFS4ERR_BADXDR, types.NFS4ERR_RESOURCE,
		types.NFS4ERR_NOFILEHANDLE, types.NFS4ERR_MOVED:
		return stateErr.Status, false
	}
	return stateErr.Status, true
}

// encodeStatusReply encodes the reply body of a failed operation: the status
// alone, which is all an NFSv4 operation carries when it fails. It must stay
// byte-identical to what the handlers return for an error (encodeStatusOnly), so
// a replay of the failed request reproduces the original response exactly.
func encodeStatusReply(status uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, status)
	return b
}

// ============================================================================
// OpenOwner
// ============================================================================

// OpenOwner represents an NFSv4 open-owner for state tracking.
// An open-owner is identified by the combination of clientid + owner opaque data.
// Each open-owner has an independent seqid sequence for replay detection.
//
// Per RFC 7530 Section 9.1.7, the server tracks:
//   - The last seqid processed for this owner
//   - The cached result of the last operation (for replay)
//   - Whether the owner has been confirmed via OPEN_CONFIRM
type OpenOwner struct {
	// ClientID is the server-assigned client identifier.
	ClientID uint64

	// OwnerData is the opaque owner identifier from the client.
	OwnerData []byte

	// Principal is the authenticated identity that opened under this owner, as
	// CompoundContext.Principal() renders it, or empty when the OPEN carried no
	// identity.
	//
	// It is not an admission check on OPEN: a client ID spans every principal
	// on the client, and no operation that carries a filehandle binds to one
	// (RFC 8881 Section 18.35.3 warns clients off enforcing on such operations
	// for exactly that reason). It exists for RENEW, whose second permitted
	// principal is "any principal that currently has an OPEN file on the
	// server" (RFC 7530 Section 16.28.5).
	Principal string

	// ownerSeq carries this owner's seqid sequence and replay cache.
	ownerSeq

	// Confirmed indicates whether OPEN_CONFIRM has been called for this owner.
	// New owners created by OPEN must be confirmed before the open state is usable.
	Confirmed bool

	// OpenStates tracks all open files for this owner.
	OpenStates []*OpenState

	// ClientRecord is a back-reference to the owning client record.
	ClientRecord *ClientRecord

	// key is the precomputed composite map key (clientID + hex(ownerData)).
	// It is cached at owner creation so hot-path lookups and comparisons reuse
	// it instead of re-running makeOwnerKey (fmt.Sprintf + hex.EncodeToString)
	// on every stateful op. OwnerData/ClientID are immutable after creation,
	// so the cached key never goes stale.
	key openOwnerKey
}

// Key returns the cached composite map key for this open-owner. Owners
// constructed via the StateManager always carry a cached key; the fallback
// covers literal-built instances (e.g. in tests).
func (oo *OpenOwner) Key() openOwnerKey {
	if oo.key == "" {
		return makeOwnerKey(oo.ClientID, oo.OwnerData)
	}
	return oo.key
}

// ============================================================================
// CachedResult
// ============================================================================

// CachedResult holds the result of the last operation on an open- or
// lock-owner. When a replay is detected (same seqid), this cached result is
// returned instead of re-executing the operation.
//
// Per RFC 7530 Section 9.1.7 the cache must hold the result of the *last*
// owner-seqid-advancing operation (OPEN/CLOSE/OPEN_DOWNGRADE/OPEN_CONFIRM for
// open-owners; LOCK/LOCKU for lock-owners), not just OPEN. This mirrors the
// Linux nfsd per-stateowner `so_replay` buffer.
type CachedResult struct {
	// Status is the NFS4 status code of the cached operation.
	Status uint32

	// Data is the XDR-encoded operation-specific result data.
	Data []byte
}

// ReplayError is returned by owner-seqid-advancing StateManager methods when a
// client retransmits the operation at the last-processed seqid. It carries the
// exact encoded reply bytes (and status) of the original operation so the
// handler can return them verbatim, satisfying the NFSv4.0 exactly-once
// (replay) contract (RFC 7530 Section 9.1.7).
//
// It implements error so it can flow through the existing
// `(*types.Stateid4, error)` / `(*LockResult, error)` return paths without
// changing signatures; handlers type-assert it before mapping to a status.
type ReplayError struct {
	// Status is the NFS4 status code of the cached operation.
	Status uint32

	// Data is the XDR-encoded operation-specific result data to replay.
	Data []byte
}

func (e *ReplayError) Error() string { return "replay of cached owner-seqid result" }

// ============================================================================
// OpenState
// ============================================================================

// OpenState represents the state of a single open file for an open-owner.
// Created by OPEN, removed by CLOSE.
//
// Per RFC 7530 Section 9.1:
//   - Each OpenState has a unique stateid (generated by the server)
//   - share_access/share_deny accumulate across multiple OPENs on same file
//   - CLOSE removes the OpenState and its stateid
type OpenState struct {
	// Stateid is the server-assigned state identifier for this open.
	Stateid types.Stateid4

	// Owner is the open-owner that created this state.
	Owner *OpenOwner

	// FileHandle is the file handle of the opened file.
	FileHandle []byte

	// ShareAccess is the accumulated share access mode (OR'd across OPENs).
	ShareAccess uint32

	// ShareDeny is the accumulated share deny mode (OR'd across OPENs).
	ShareDeny uint32

	// Confirmed indicates whether OPEN_CONFIRM has been called for this state.
	Confirmed bool

	// LockStates tracks lock stateids derived from this open.
	LockStates []*LockState
}

// ============================================================================
// OpenFileResult
// ============================================================================

// OpenFileResult is the result returned by StateManager.OpenFile.
// It contains everything the OPEN handler needs to build its response.
type OpenFileResult struct {
	// Stateid is the stateid to return to the client.
	Stateid types.Stateid4

	// RFlags is the result flags (OPEN4_RESULT_CONFIRM if new owner).
	RFlags uint32

	// IsReplay indicates this is a replay of a previous operation.
	// When true, CachedStatus and CachedData should be returned directly.
	IsReplay bool

	// CachedStatus is the status from the replayed operation (only if IsReplay).
	CachedStatus uint32

	// CachedData is the XDR data from the replayed operation (only if IsReplay).
	CachedData []byte
}

// OpenSeqResult is returned by the open-owner-seqid-advancing StateManager
// methods that yield a single stateid (CLOSE / OPEN_CONFIRM / OPEN_DOWNGRADE).
// It carries the resulting stateid plus the open-owner identity so the handler
// can cache the encoded reply for replay detection (CacheOpenOwnerResult).
type OpenSeqResult struct {
	// Stateid is the stateid to return to the client.
	Stateid types.Stateid4

	// OwnerClientID and OwnerData identify the open-owner whose seqid this op
	// advanced.
	OwnerClientID uint64
	OwnerData     []byte
}

// ============================================================================
// Owner Key
// ============================================================================

// openOwnerKey is a composite key for looking up open-owners in maps.
// It combines the client ID and hex-encoded owner data for uniqueness.
type openOwnerKey string

// makeOwnerKey creates an openOwnerKey from a client ID and owner data.
func makeOwnerKey(clientID uint64, ownerData []byte) openOwnerKey {
	return openOwnerKey(fmt.Sprintf("%d:%s", clientID, hex.EncodeToString(ownerData)))
}

// ============================================================================
// Helpers
// ============================================================================

// nextSeqID computes the next expected seqid with wrap-around.
// Per RFC 7530, seqid 0xFFFFFFFF wraps to 1 (not 0, since 0 is
// reserved for special stateids).
func nextSeqID(current uint32) uint32 {
	if current == 0xFFFFFFFF {
		return 1
	}
	return current + 1
}
