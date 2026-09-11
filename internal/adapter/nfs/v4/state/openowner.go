package state

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/logger"
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

	// openRefusal is the last OPEN this server refused on its own, if any.
	openRefusal *openRefusal

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

// openRefusal remembers an OPEN this server refused before the state layer saw
// it: the seqid it consumed and the status it answered.
//
// It is deliberately kept apart from OpenOwner.LastResult. That cache is shared
// by every owner-seqid-advancing operation -- CLOSE, OPEN_CONFIRM and
// OPEN_DOWNGRADE all write to it -- so at any moment it may hold a reply of a
// different shape than an OPEN's. Replaying those bytes in an OPEN's place
// answers with the right operation number and the wrong body, and the client
// reads the next operation's number out of the middle of it.
type openRefusal struct {
	seqid  uint32
	status uint32
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

// shareModeBit maps a share_access value to its bit in
// OpenState.openedAccessModes. A value outside 0..3 is not a share mode at all
// and gets no bit, so it can never satisfy an OPEN_DOWNGRADE.
func shareModeBit(mode uint32) uint8 {
	if mode > types.OPEN4_SHARE_ACCESS_BOTH {
		return 0
	}
	return 1 << mode
}

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

	// openedAccessModes records the share_access value every OPEN behind this
	// state asked for, one bit per value (bit 1 READ, bit 2 WRITE, bit 3 BOTH).
	// OPEN_DOWNGRADE may only name a mode that was actually opened, and the
	// accumulated ShareAccess union cannot tell that apart: a single OPEN for
	// BOTH leaves READ and WRITE standing in the union with neither ever
	// opened. Linux nfsd keeps the same bitmap (st_access_bmap) for the same
	// reason.
	openedAccessModes uint8

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

func (sm *StateManager) OpenFile(
	clientID uint64,
	ownerData []byte,
	seqid uint32,
	fileHandle []byte,
	shareAccess, shareDeny uint32,
	claimType uint32,
	principal ...string,
) (result *OpenFileResult, err error) {
	// Grace period checks (before acquiring sm.mu)
	switch claimType {
	case types.CLAIM_NULL:
		// New open: blocked during grace period
		if sm.IsInGrace() {
			return nil, ErrGrace
		}
	case types.CLAIM_PREVIOUS:
		// Reclaim: only allowed during grace period
		if !sm.IsInGrace() {
			return nil, ErrNoGrace
		}
		// Verifier-gated reclaim (RFC 7530 §9.1.4): a reclaiming
		// client whose ClientIDString matches a pre-restart record but whose
		// BootVerifier changed rebooted and must NOT reclaim prior state.
		// validateReclaimVerifier self-gates on the boot-load snapshot, so it is
		// a no-op when no durable prior record exists (reclaim allowed as before).
		sm.mu.RLock()
		gp := sm.gracePeriod
		rec := sm.v40ClientLocked(clientID)
		sm.mu.RUnlock()
		if rec != nil {
			if err := sm.validateReclaimVerifier(rec.ClientIDString, rec.Verifier); err != nil {
				return nil, err
			}
		}
		// Notify the grace period that this client has reclaimed. Mark both by
		// numeric clientID (same-epoch reclaim) and by ClientIDString (the
		// boot-loaded roster is keyed by string since reclaiming clients get a
		// fresh clientID after restart).
		if gp != nil {
			gp.ClientReclaimed(clientID)
			if rec != nil {
				gp.ClientReclaimedByString(rec.ClientIDString)
			}
		}
		// v4.0 has no RECLAIM_COMPLETE; the first successful CLAIM_PREVIOUS is
		// the analog reclaim marker. On that false→true transition, set the
		// in-memory flag (the durable write mirrors it, and a pending retry
		// re-validates against it) and persist it so a second restart inside
		// one grace window does not wait on this client again. Later reclaim
		// OPENs short-circuit: a per-OPEN persist attempt would fire a store
		// write for every reclaimed file even though the durable record is
		// already marked.
		if rec != nil {
			sm.mu.Lock()
			firstReclaim := !rec.ReclaimComplete
			rec.ReclaimComplete = true
			if firstReclaim {
				sm.recordReclaimCompleteLocked(clientID, rec.ClientIDString)
			}
			sm.mu.Unlock()
		}
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Look up or create the open-owner
	ownerKey := makeOwnerKey(clientID, ownerData)
	owner, ownerExists := sm.openOwners[ownerKey]

	if ownerExists {
		// Existing owner: validate seqid
		// seqid=0 is the v4.1 bypass convention: slot table provides replay protection
		if seqid != 0 {
			validation := owner.ValidateSeqID(seqid)
			switch validation {
			case SeqIDReplay:
				// Return cached result
				if owner.LastResult != nil {
					return &OpenFileResult{
						IsReplay:     true,
						CachedStatus: owner.LastResult.Status,
						CachedData:   owner.LastResult.Data,
					}, nil
				}
				// No cached result (shouldn't happen), treat as bad seqid
				return nil, ErrBadSeqid
			case SeqIDBad:
				return nil, ErrBadSeqid
			case SeqIDOK:
				// Continue with normal processing
			}
		}
	} else {
		// New owner: create it
		clientRecord := sm.clientRecordLocked(clientID)
		owner = &OpenOwner{
			ClientID:  clientID,
			OwnerData: make([]byte, len(ownerData)),
			// ponytail: recorded once, at the OPEN that creates the owner,
			// rather than per open state as FreeBSD's ls_uid is. Every client
			// that matters keys its open-owners by credential (Linux
			// nfs4_get_state_owner takes the cred), so one principal per owner
			// holds; track it per OpenState only if a client turns up that
			// shares an owner across users.
			Principal: firstOrEmpty(principal),

			Confirmed:    false,
			OpenStates:   make([]*OpenState, 0),
			ClientRecord: clientRecord,
			key:          ownerKey,
		}
		copy(owner.OwnerData, ownerData)
		sm.openOwners[ownerKey] = owner

		// OpenOwners is populated on the v4.0 path only; it is left nil on a
		// v4.1 record, whose owners are torn down by purgeV41Client instead.
		if clientRecord != nil && clientRecord.OpenOwners != nil {
			clientRecord.OpenOwners[string(ownerData)] = owner
		}
	}

	// The seqid is now this owner's, whatever the OPEN goes on to return: a
	// failure below (share reservation conflict) consumes it just as success
	// does, and the client advances either way.
	defer func() { owner.consumeSeqidOnError(seqid, err) }()

	// Enforce share reservations (RFC 7530 Section 9.9; Linux nfsd
	// nfs4_share_conflict). This runs AFTER the owner seqid/replay gate above:
	// a replayed OPEN must return its cached result and a bad seqid must return
	// NFS4ERR_BAD_SEQID — neither may be turned into NFS4ERR_SHARE_DENIED by a
	// conflict that arose after the original request. Reclaim (CLAIM_PREVIOUS)
	// re-establishes prior state and is exempt. The scan runs under sm.mu so it
	// observes a consistent snapshot of every live open.
	if claimType != types.CLAIM_PREVIOUS {
		sm.expireLapsedHoldersLocked(fileHandle, clientID)

		if conflict := sm.shareConflictLocked(fileHandle, shareAccess, shareDeny); conflict {
			logger.Debug("OpenFile: share reservation conflict",
				"client_id", clientID,
				"owner", string(ownerData),
				"req_access", shareAccess,
				"req_deny", shareDeny)
			return nil, ErrShareDenied
		}
	}

	// Check if this owner already has an open on this file
	var existingState *OpenState
	for _, os := range owner.OpenStates {
		if bytes.Equal(os.FileHandle, fileHandle) {
			existingState = os
			break
		}
	}

	var resultStateid types.Stateid4

	if existingState != nil {
		// Accumulate share_access and share_deny (Pitfall 7)
		existingState.ShareAccess |= shareAccess
		existingState.ShareDeny |= shareDeny
		existingState.openedAccessModes |= shareModeBit(shareAccess)

		// Increment the stateid seqid for this operation
		existingState.Stateid.Seqid = nextSeqID(existingState.Stateid.Seqid)
		resultStateid = existingState.Stateid
	} else {
		// Create new OpenState
		other := sm.generateStateidOther(StateTypeOpen)
		resultStateid = types.Stateid4{
			Seqid: 1,
			Other: other,
		}

		fhCopy := make([]byte, len(fileHandle))
		copy(fhCopy, fileHandle)

		openState := &OpenState{
			Stateid:           resultStateid,
			Owner:             owner,
			FileHandle:        fhCopy,
			ShareAccess:       shareAccess,
			ShareDeny:         shareDeny,
			openedAccessModes: shareModeBit(shareAccess),
			Confirmed:         owner.Confirmed,
		}

		owner.OpenStates = append(owner.OpenStates, openState)
		sm.openStateByOther[other] = openState
		sm.addOpenStateToFileLocked(openState)
	}

	// Determine rflags: OPEN4_RESULT_CONFIRM only if owner is not yet confirmed
	var rflags uint32
	rflags |= types.OPEN4_RESULT_LOCKTYPE_POSIX
	if !owner.Confirmed {
		rflags |= types.OPEN4_RESULT_CONFIRM
	}

	// Update owner seqid and cache
	owner.LastSeqID = seqid

	logger.Debug("OpenFile: state created/updated",
		"client_id", clientID,
		"owner", string(ownerData),
		"seqid", seqid,
		"stateid_seqid", resultStateid.Seqid,
		"share_access", shareAccess,
		"share_deny", shareDeny,
		"confirm_required", !owner.Confirmed)

	return &OpenFileResult{
		Stateid: resultStateid,
		RFlags:  rflags,
	}, nil
}

// shareConflictLocked reports whether granting an OPEN with the requested
// share_access / share_deny on fileHandle would conflict with an open already
// held on it. A conflict exists when the requested access is denied by an
// existing open, or the requested deny would exclude an existing open's access.
//
// RFC 7530 Section 9.9 gives the rule as pseudo-code over the file's
// accumulated state -- "(request.access & file_state.deny) || (request.deny &
// file_state.access)" -- and then says in as many words that "this checking of
// share reservations on OPEN is done with no exception for an existing OPEN for
// the same open-owner". Skipping the requesting owner's own opens, on the theory
// that share bits merely accumulate per owner, let an owner that had denied
// READ to everyone go on to open the same file for reading itself. Linux nfsd
// keeps the deny mask on the file (nfs4_file, fi_share_deny) for the same
// reason.
//
// Caller must hold sm.mu, for reading or for writing.

func (sm *StateManager) shareConflictLocked(
	fileHandle []byte,
	reqAccess, reqDeny uint32,
) bool {
	// Iterate only the opens on this file via the secondary per-file index
	// (openStateByFile), not every open in the server.
	for _, os := range sm.openStateByFile[string(fileHandle)] {
		if reqAccess&os.ShareDeny != 0 || reqDeny&os.ShareAccess != 0 {
			return true
		}
	}
	return false
}

// addOpenStateToFileLocked inserts an OpenState into the per-file open index.
// Caller must hold sm.mu.

func (sm *StateManager) addOpenStateToFileLocked(os *OpenState) {
	fhKey := string(os.FileHandle)
	sm.openStateByFile[fhKey] = append(sm.openStateByFile[fhKey], os)
}

// removeOpenStateFromFileLocked removes an OpenState from the per-file open
// index, dropping the file's slot entirely when it becomes empty so the map
// stays bounded. Caller must hold sm.mu.

func (sm *StateManager) removeOpenStateFromFileLocked(os *OpenState) {
	fhKey := string(os.FileHandle)
	states := sm.openStateByFile[fhKey]
	for i, s := range states {
		if s != os {
			continue
		}
		// Shift the tail down, nil out the now-unused last slot so the backing
		// array does not retain the removed OpenState, then reslice.
		copy(states[i:], states[i+1:])
		states[len(states)-1] = nil
		states = states[:len(states)-1]
		if len(states) == 0 {
			delete(sm.openStateByFile, fhKey)
		} else {
			sm.openStateByFile[fhKey] = states
		}
		return
	}
}

// ReplayOpenSeqid reports the status to replay when seqid retransmits an OPEN
// this server refused on its own, so the caller can answer it without running
// the OPEN a second time (RFC 7530 Section 9.1.7).
//
// Re-executing such a retransmission answers from the state of the world now
// rather than the state it had when the client first asked, so a refusal whose
// cause has since gone away -- the colliding name removed, the permission
// granted -- came back as a success the client had no reply slot for.
//
// It covers only the refusals ConsumeOpenSeqid recorded. An OPEN that reached
// the state layer is replayed by OpenFile from the owner's shared reply cache,
// and that cache must not be consulted here: CLOSE, OPEN_CONFIRM and
// OPEN_DOWNGRADE write to it too, so it may hold a reply of a different shape,
// which an OPEN replaying it would return under its own operation number.

func (sm *StateManager) ReplayOpenSeqid(clientID uint64, ownerData []byte, seqid uint32) (uint32, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	owner, exists := sm.openOwners[makeOwnerKey(clientID, ownerData)]
	if !exists || owner.openRefusal == nil {
		return 0, false
	}
	if owner.openRefusal.seqid != seqid || owner.ValidateSeqID(seqid) != SeqIDReplay {
		return 0, false
	}
	return owner.openRefusal.status, true
}

// ConsumeOpenSeqid records against an open-owner's sequence an OPEN that failed
// before it ever reached OpenFile, and caches the status so a retransmission
// replays it.
//
// RFC 7530 Section 9.1.7 advances an owner's sequence for every OPEN that
// reaches seqid checking, and the client advances its own whether the server
// answered success or a consuming error. An OPEN the handler refuses on its own
// -- a create collision, a target of the wrong object type, a name the server
// rejects -- never reached the state manager, so its seqid went unrecorded and
// the server fell one behind the client. Every later OPEN for that owner was
// then answered NFS4ERR_BAD_SEQID, which a client can only escape by tearing
// the owner down.
//
// It is a no-op for an owner that does not exist yet, because a first OPEN that
// fails leaves no owner behind and the client's retry at seqid 1 is valid
// against a fresh one; for a seqid that is not the owner's expected next one,
// so a replay returns its cached reply rather than consuming a second seqid;
// and for the statuses Section 9.1.7 exempts.

func (sm *StateManager) ConsumeOpenSeqid(clientID uint64, ownerData []byte, seqid, status uint32) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	owner, exists := sm.openOwners[makeOwnerKey(clientID, ownerData)]
	if !exists {
		return
	}
	if owner.ValidateSeqID(seqid) != SeqIDOK {
		return
	}
	before := owner.LastSeqID
	owner.consumeSeqidOnError(seqid, &NFS4StateError{Status: status})
	// consumeSeqidOnError leaves the sequence alone for the statuses
	// Section 9.1.7 exempts; nothing was recorded, so there is nothing to
	// replay either.
	if owner.LastSeqID != before {
		owner.openRefusal = &openRefusal{seqid: seqid, status: status}
	}
}

// CacheOpenOwnerResult stores the encoded reply for an open-owner so that a
// replay (retransmit at the same seqid) returns the exact original response.
//
// It must be called by the handler after encoding the reply of EVERY
// owner-seqid-advancing op (OPEN/CLOSE/OPEN_DOWNGRADE/OPEN_CONFIRM), not just
// OPEN — otherwise a retransmit of a CLOSE/DOWNGRADE/CONFIRM would replay stale
// OPEN bytes (or none), causing NFS4ERR_BAD_SEQID storms (RFC 7530 §9.1.7,
// mirrors Linux nfsd so_replay). data must be a caller-owned copy; it is
// retained until the next op or lease expiry.

func (sm *StateManager) CacheOpenOwnerResult(clientID uint64, ownerData []byte, status uint32, data []byte) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	ownerKey := makeOwnerKey(clientID, ownerData)
	if owner, exists := sm.openOwners[ownerKey]; exists {
		owner.LastResult = &CachedResult{Status: status, Data: data}
		return
	}

	// The owner may have just been removed from the live table by CLOSE (its
	// last open state went away). It is still reachable via closedOwnerByOther
	// so the CLOSE reply can be cached for a retransmitted CLOSE replay.
	for _, owner := range sm.closedOwnerByOther {
		if owner.ClientID == clientID && bytes.Equal(owner.OwnerData, ownerData) {
			owner.LastResult = &CachedResult{Status: status, Data: data}
			return
		}
	}
}

// CacheLockOwnerResult stores the encoded reply for a lock-owner so that a
// replayed LOCK/LOCKU at the same lock-owner seqid returns the exact original
// response instead of NFS4ERR_BAD_SEQID (which the Linux client treats as
// fatal, dropping the lock-owner and silently losing locks). Symmetric to
// CacheOpenOwnerResult; see RFC 7530 §9.1.7 / §8.20.

func (sm *StateManager) CacheLockOwnerResult(clientID uint64, ownerData []byte, status uint32, data []byte) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	loKey := makeLockOwnerKey(clientID, ownerData)
	lockOwner, exists := sm.lockOwners[loKey]
	if !exists {
		return
	}

	lockOwner.LastResult = &CachedResult{
		Status: status,
		Data:   data,
	}
}

// ConfirmOpen implements the OPEN_CONFIRM operation's state management.
//
// Per RFC 7530 Section 16.20:
//   - Validates the stateid
//   - Validates the seqid on the owner
//   - Promotes the open-owner and open-state to confirmed
//   - Increments the stateid seqid
//
// Caller must NOT hold sm.mu.

func (sm *StateManager) ConfirmOpen(stateid *types.Stateid4, seqid uint32, callerClientID uint64) (result *OpenSeqResult, err error) {
	// A special stateid names no state at all, and RFC 7530 Section 9.1.4.3
	// admits one only on READ, WRITE and SETATTR. stateidMissError documents
	// why it must be rejected before a table miss is classified.
	if stateid.IsSpecialStateid() {
		return nil, ErrBadStateid
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Look up the open state
	openState, exists := sm.openStateByOther[stateid.Other]
	if !exists {
		return nil, sm.stateidMissError(stateid.Other)
	}

	// A stateid is not a bearer token: reject one that names another client's
	// state before acting on it; see checkStateidOwner.
	if err := checkStateidOwner(callerClientID, openState.Owner.ClientID); err != nil {
		return nil, err
	}

	owner := openState.Owner

	// The request is attributable to this owner, so any failure below consumes
	// its seqid (RFC 7530 Section 9.1.7).
	defer func() { owner.consumeSeqidOnError(seqid, err) }()

	// Validate seqid on the owner
	// seqid=0 is the v4.1 bypass convention: slot table provides replay protection
	if seqid != 0 {
		validation := owner.ValidateSeqID(seqid)
		switch validation {
		case SeqIDReplay:
			// Replay the exact cached reply of the original op at this seqid.
			if owner.LastResult != nil {
				return nil, &ReplayError{Status: owner.LastResult.Status, Data: owner.LastResult.Data}
			}
			return nil, ErrBadSeqid
		case SeqIDBad:
			return nil, ErrBadSeqid
		case SeqIDOK:
			// Continue
		}
	}

	// The stateid must name this open's current seqid, compared after the owner
	// seqid above so a retransmit still replays; see checkStateidSeqid.
	if err := checkStateidSeqid(stateid.Seqid, openState.Stateid.Seqid); err != nil {
		return nil, err
	}

	// An open confirms once. A second OPEN_CONFIRM finds no unconfirmed state,
	// so the stateid it carries no longer names anything OPEN_CONFIRM can act
	// on (RFC 7530 Section 16.18.5).
	if openState.Confirmed {
		return nil, ErrBadStateid
	}

	// Promote to confirmed
	openState.Confirmed = true
	owner.Confirmed = true

	// Increment stateid seqid
	openState.Stateid.Seqid = nextSeqID(openState.Stateid.Seqid)

	// Update owner seqid and cache
	owner.LastSeqID = seqid

	resultStateid := openState.Stateid

	logger.Debug("ConfirmOpen: owner confirmed",
		"client_id", owner.ClientID,
		"stateid_seqid", resultStateid.Seqid)

	return &OpenSeqResult{
		Stateid:       resultStateid,
		OwnerClientID: owner.ClientID,
		OwnerData:     owner.OwnerData,
	}, nil
}

// ConfirmOpenV41 confirms an open-owner for NFSv4.1 without incrementing
// the stateid seqid. In v4.1, OPEN_CONFIRM doesn't exist; owners are
// implicitly confirmed through the session/slot mechanism. The stateid
// must remain at seqid=1 (the initial value) because the Linux NFS client's
// nfs_set_open_stateid_locked() expects sequential stateids starting from 1.
//
// Caller must NOT hold sm.mu.

func (sm *StateManager) ConfirmOpenV41(stateid *types.Stateid4, callerClientID uint64) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	openState, exists := sm.openStateByOther[stateid.Other]
	if !exists {
		return sm.stateidMissError(stateid.Other)
	}

	// A stateid is not a bearer token: another client's open must not be
	// confirmed through this caller; see checkStateidOwner.
	if err := checkStateidOwner(callerClientID, openState.Owner.ClientID); err != nil {
		return err
	}

	openState.Confirmed = true
	openState.Owner.Confirmed = true
	return nil
}

// CloseFile implements the CLOSE operation's state management.
//
// Per RFC 7530 Section 16.3:
//   - Validates the stateid
//   - Validates the seqid on the owner
//   - Removes the OpenState from all maps
//   - If owner has no more OpenStates, cleans up the owner
//   - Returns a zeroed stateid
//
// Caller must NOT hold sm.mu.

func (sm *StateManager) CloseFile(stateid *types.Stateid4, seqid uint32, callerClientID uint64) (result *OpenSeqResult, err error) {
	// A special stateid names no open state, and RFC 7530 Section 9.1.4.3
	// admits one only on READ, WRITE and SETATTR, so CLOSE has nothing to act
	// on. stateidMissError documents why it must be rejected before a table
	// miss is classified.
	if stateid.IsSpecialStateid() {
		return nil, ErrBadStateid
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Look up the open state
	openState, exists := sm.openStateByOther[stateid.Other]
	if !exists {
		// The state was already removed by a prior CLOSE. If this is a
		// retransmit of that CLOSE (same owner-seqid), replay its cached reply
		// (RFC 7530 §9.1.7) rather than returning NFS4ERR_BAD_STATEID.
		// The cached reply belongs to the owner that sent the original CLOSE, so
		// it is only replayed to that owner's client; see checkStateidOwner.
		if owner, ok := sm.closedOwnerByOther[stateid.Other]; ok && seqid != 0 &&
			checkStateidOwner(callerClientID, owner.ClientID) == nil {
			if owner.ValidateSeqID(seqid) == SeqIDReplay && owner.LastResult != nil {
				return nil, &ReplayError{Status: owner.LastResult.Status, Data: owner.LastResult.Data}
			}
		}
		return nil, sm.stateidMissError(stateid.Other)
	}

	// A stateid is not a bearer token: reject one that names another client's
	// state before acting on it; see checkStateidOwner.
	if err := checkStateidOwner(callerClientID, openState.Owner.ClientID); err != nil {
		return nil, err
	}

	owner := openState.Owner

	// The request is attributable to this owner, so any failure below consumes
	// its seqid (RFC 7530 Section 9.1.7). NFS4ERR_LOCKS_HELD is the case that
	// matters in practice: a database closing a file while byte-range locks are
	// outstanding hits it routinely, and leaving the seqid behind would answer
	// every later CLOSE and LOCK for this owner with NFS4ERR_BAD_SEQID.
	defer func() { owner.consumeSeqidOnError(seqid, err) }()

	// Validate seqid on the owner
	// seqid=0 is the v4.1 bypass convention: slot table provides replay protection
	if seqid != 0 {
		validation := owner.ValidateSeqID(seqid)
		switch validation {
		case SeqIDReplay:
			// Replay the exact cached CLOSE reply rather than re-deriving it.
			if owner.LastResult != nil {
				return nil, &ReplayError{Status: owner.LastResult.Status, Data: owner.LastResult.Data}
			}
			return nil, ErrBadSeqid
		case SeqIDBad:
			return nil, ErrBadSeqid
		case SeqIDOK:
			// Continue
		}
	}

	// The stateid must name this open's current seqid, compared after the owner
	// seqid above so a retransmit still replays; see checkStateidSeqid.
	if err := checkStateidSeqid(stateid.Seqid, openState.Stateid.Seqid); err != nil {
		return nil, err
	}

	// Refuse while ranges are genuinely held -- RFC 7530 Section 16.2.4 permits
	// refusing or freeing, and this server refuses.
	if sm.hasOutstandingLocksLocked(openState) {
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_LOCKS_HELD,
			Message: "cannot close: byte-range locks still held, use LOCKU first",
		}
	}

	// Remove the open state from the "other" map and per-file index.
	delete(sm.openStateByOther, stateid.Other)
	sm.removeOpenStateFromFileLocked(openState)

	// The open's lock stateids die with it: they hold no ranges (checked above)
	// and nothing may use them once the open they derive from is gone.
	for _, lockState := range openState.LockStates {
		delete(sm.lockStateByOther, lockState.Stateid.Other)
	}
	for _, lockState := range openState.LockStates {
		sm.dropLockOwnerIfUnreferencedLocked(lockState.LockOwner)
	}
	openState.LockStates = nil

	// Remember which retained owner this now-closed stateid belonged to so a
	// retransmitted CLOSE can still resolve the owner's cached reply.
	sm.closedOwnerByOther[stateid.Other] = owner

	// Remove from owner's OpenStates list
	for i, os := range owner.OpenStates {
		if os == openState {
			owner.OpenStates = append(owner.OpenStates[:i], owner.OpenStates[i+1:]...)
			break
		}
	}

	// Update owner seqid
	owner.LastSeqID = seqid

	// If the owner has no more open states, drop it from the LIVE owner table
	// (openOwners + client record) so a subsequent OPEN by the same owner name
	// is treated as a brand-new owner — NOT subjected to this now-stale seqid,
	// which would otherwise mis-classify a genuinely new OPEN as a replay (and
	// return the cached CLOSE reply) or as NFS4ERR_BAD_SEQID. This matches the
	// pre-retention behavior. The owner object itself stays reachable via
	// closedOwnerByOther (installed above), so a retransmitted CLOSE can still
	// replay its cached reply (RFC 7530 §9.1.7); that entry is reaped on lease
	// expiry.
	if len(owner.OpenStates) == 0 {
		delete(sm.openOwners, owner.Key())
		if owner.ClientRecord != nil {
			delete(owner.ClientRecord.OpenOwners, string(owner.OwnerData))
		}
		logger.Debug("CloseFile: owner removed from live table (no more open states), retained for CLOSE replay",
			"client_id", owner.ClientID)
	}

	logger.Debug("CloseFile: state removed",
		"client_id", owner.ClientID,
		"seqid", seqid)

	// Return zeroed stateid plus owner identity for reply caching.
	return &OpenSeqResult{
		OwnerClientID: owner.ClientID,
		OwnerData:     owner.OwnerData,
	}, nil
}

// hasOutstandingLocksLocked reports whether any byte-range lock derived from
// openState is still held in the lock manager. Not the same question as whether
// openState has lock stateids: one stays valid after LOCKU frees its last range
// (RFC 7530 Section 9.1.4.4), so that count never drops back to zero.
//
// Caller must hold sm.mu.

func (sm *StateManager) DowngradeOpen(stateid *types.Stateid4, seqid uint32, newShareAccess, newShareDeny uint32, callerClientID uint64) (result *OpenSeqResult, err error) {
	// A special stateid names no state at all, and RFC 7530 Section 9.1.4.3
	// admits one only on READ, WRITE and SETATTR. stateidMissError documents
	// why it must be rejected before a table miss is classified.
	if stateid.IsSpecialStateid() {
		return nil, ErrBadStateid
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Look up the open state
	openState, exists := sm.openStateByOther[stateid.Other]
	if !exists {
		return nil, sm.stateidMissError(stateid.Other)
	}

	// A stateid is not a bearer token: reject one that names another client's
	// state before acting on it; see checkStateidOwner.
	if err := checkStateidOwner(callerClientID, openState.Owner.ClientID); err != nil {
		return nil, err
	}

	owner := openState.Owner

	// The request is attributable to this owner, so any failure below consumes
	// its seqid (RFC 7530 Section 9.1.7), the NFS4ERR_INVAL rejections included.
	defer func() { owner.consumeSeqidOnError(seqid, err) }()

	// Validate seqid on the owner
	// seqid=0 is the v4.1 bypass convention: slot table provides replay protection
	if seqid != 0 {
		validation := owner.ValidateSeqID(seqid)
		switch validation {
		case SeqIDReplay:
			// Replay the exact cached reply (original stateid bytes), not the
			// current stateid which a later DOWNGRADE may have advanced.
			if owner.LastResult != nil {
				return nil, &ReplayError{Status: owner.LastResult.Status, Data: owner.LastResult.Data}
			}
			return nil, ErrBadSeqid
		case SeqIDBad:
			return nil, ErrBadSeqid
		case SeqIDOK:
			// Continue
		}
	}

	// The stateid must name this open's current seqid, compared after the owner
	// seqid above so a retransmit still replays; see checkStateidSeqid.
	if err := checkStateidSeqid(stateid.Seqid, openState.Stateid.Seqid); err != nil {
		return nil, err
	}

	// The new share_access must be a mode some OPEN behind this state actually
	// asked for, which is stricter than being a subset of the accumulated
	// union: one OPEN for BOTH leaves READ and WRITE standing in that union
	// with neither ever opened, and downgrading to one of them would name a
	// mode the client never held (RFC 7530 Section 16.19.4).
	if openState.openedAccessModes&shareModeBit(newShareAccess) == 0 {
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_INVAL,
			Message: "OPEN_DOWNGRADE to a share_access mode no OPEN asked for",
		}
	}
	if newShareDeny & ^openState.ShareDeny != 0 {
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_INVAL,
			Message: "OPEN_DOWNGRADE cannot add share_deny bits",
		}
	}

	// newShareAccess must be non-zero (must have at least read or write)
	if newShareAccess == 0 {
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_INVAL,
			Message: "OPEN_DOWNGRADE share_access cannot be zero",
		}
	}

	// Update share modes
	openState.ShareAccess = newShareAccess
	openState.ShareDeny = newShareDeny

	// Forget the opened modes this downgrade drops: a later OPEN_DOWNGRADE may
	// only name one this one kept. Downgrading to a single mode leaves that
	// mode as the only one opened; BOTH still covers all three, so it drops
	// nothing.
	if newShareAccess != types.OPEN4_SHARE_ACCESS_BOTH {
		openState.openedAccessModes = shareModeBit(newShareAccess)
	}

	// Increment stateid seqid
	openState.Stateid.Seqid = nextSeqID(openState.Stateid.Seqid)

	// Update owner seqid
	owner.LastSeqID = seqid

	resultStateid := openState.Stateid

	logger.Debug("DowngradeOpen: share modes updated",
		"client_id", owner.ClientID,
		"new_access", newShareAccess,
		"new_deny", newShareDeny,
		"stateid_seqid", resultStateid.Seqid)

	return &OpenSeqResult{
		Stateid:       resultStateid,
		OwnerClientID: owner.ClientID,
		OwnerData:     owner.OwnerData,
	}, nil
}

// GetOpenState returns the OpenState for a given stateid "other" field,
// or nil if not found. Used for read-only lookups that don't need validation.

func (sm *StateManager) GetOpenState(other [types.NFS4_OTHER_SIZE]byte) *OpenState {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.openStateByOther[other]
}

// ============================================================================
// Lease Operations (, Task 4)
// ============================================================================

// renewPrincipalAllowed reports whether principal may renew record's lease.
//
// RFC 7530 Section 16.28.5 names exactly two permitted callers: the principal
// that established the client ID via SETCLIENTID_CONFIRM, and any principal
// that currently has an OPEN file on the server under that client ID. A RENEW
// from anyone else MUST be rejected with NFS4ERR_ACCESS.
//
// This does not put the lease out of a stranger's reach, and is not meant to.
// Section 9.5 has DELEGPURGE, LOCK, LOCKT, OPEN and RELEASE_LOCKOWNER renew
// every lease of the client whose clientid they carry, with no principal
// restriction -- ValidateAndRenewClient does that on the OPEN path. The
// restriction is scoped to the one operation whose only effect is the renewal.
//
// The second caller is what keeps a multi-user mount working: one client ID
// covers every user on the client, and the RFC expects a lease held on behalf
// of all of them to be renewable by any of them.
//
// One caller is admitted that the RFC does not name: a record with no
// principal recorded. SETCLIENTID under AUTH_NONE stores no identity, and there
// is nothing to compare a later RENEW against.
//
// Root gets no exemption. It would have to be an exemption for the string
// "uid:0" rather than for a verified machine credential, because Principal()
// renders a GSS-resolved uid and a client-asserted AUTH_SYS uid identically and
// RENEW carries no filehandle for an export's sec= policy to judge -- so it
// would hand any AUTH_SYS peer claiming uid 0 the lease of a client established
// under Kerberos. Nothing needs it: the Linux client renews under the same
// machine credential it established the client ID with, so the equality above
// already matches, and a client that falls back to a state owner's credential
// is covered by the open-holder scan below.
//
// Caller must hold sm.mu.
