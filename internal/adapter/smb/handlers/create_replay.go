package handlers

import (
	"encoding/binary"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// CREATE replay: durable-handle and lease replay caches, response caching,
// and the create-context strip helpers.
// storeCreateReplayIfApplicable mirrors the cache.Store call at the
// bottom of completeCreateAfterBreak for CREATE return paths that
// bypass it (notably handlePipeCreate and handleOpenRootCreate). It is
// the single seam used by those bypass paths so a successful CREATE
// carrying a DH2Q CreateGuid is recorded into the replay cache; a
// FLAGS_REPLAY_OPERATION retry within the replay window can then return
// the cached result. Cache.Store is itself a no-op when CreateGuid is
// zero or when resp.Status != StatusSuccess, so it is safe to call
// unconditionally here (MS-SMB2 §3.3.5.9).
func (h *Handler) storeCreateReplayIfApplicable(ctx *SMBHandlerContext, req *CreateRequest, resp *CreateResponse) {
	if h.CreateReplayCache == nil || resp == nil || resp.Status != types.StatusSuccess {
		return
	}
	createGuid := dh2qCreateGuid(req)
	if createGuid == ([16]byte{}) {
		return
	}
	// These bypass paths (pipe / open-root) have no lease- or oplock-bearing
	// Open to refresh on replay, so the cached snapshot is replayed verbatim.
	h.CreateReplayCache.Store(ctx.SessionID, createGuid, resp, nil)
}

// resolveCreateReplay applies the SMB3 DH2Q CreateGuid de-duplication
// contract (MS-SMB2 §3.3.5.9; Samba smb2srv_open_lookup_replay_cache +
// the replay block in source3/smbd/smb2_create.c). It returns
// (resp, true) when the CREATE has been handled by the replay path and
// the caller must return resp verbatim; (nil, false) when the request
// should fall through to the normal CREATE path.
//
// Three outcomes for a CREATE whose DH2Q CreateGuid matches a live open
// in the per-session cache:
//
//   - FLAGS_REPLAY_OPERATION set → replay. The original open is returned
//     (same FileId). The lease/oplock state in the response is rebuilt
//     from the CURRENT open state, not the create-time snapshot, so a
//     lease upgraded after the original CREATE replays back the upgraded
//     state (replay-dhv2-lease1/2). Lease replays additionally validate
//     against the live open: a replay request carrying a lease whose key
//     differs from the open's, or replaying a lease over an open that is
//     not lease-backed, returns ACCESS_DENIED (replay-dhv2-lease3 /
//     oplock-lease).
//
//   - FLAGS_REPLAY_OPERATION clear but CreateGuid matches a cached open →
//     DUPLICATE_OBJECTID. A second non-replay CREATE for an in-flight
//     CreateGuid is a protocol violation.
//
//   - No cache match → fall through.

func (h *Handler) resolveCreateReplay(ctx *SMBHandlerContext, req *CreateRequest) (*CreateResponse, bool) {
	if h.CreateReplayCache == nil {
		return nil, false
	}
	createGuid := dh2qCreateGuid(req)
	if createGuid == ([16]byte{}) {
		return nil, false
	}

	entry := h.CreateReplayCache.LookupEntry(ctx.SessionID, createGuid)
	if entry == nil {
		// No completed entry yet. A replay that arrives while the original
		// CREATE for this CreateGuid is still parked on a pending
		// oplock/lease break must fail fast with STATUS_FILE_NOT_AVAILABLE
		// rather than block on the same break (Samba FWP_RESERVED /
		// FILE_NOT_AVAILABLE slot states). The original parked request keeps
		// running and completes on its own timeline. Checked after the
		// completed-entry lookup so a parked CREATE that has just finished
		// (entry present, reservation not yet cleared) replays the open
		// rather than returning FILE_NOT_AVAILABLE.
		if ctx.IsReplay && h.CreateReplayCache.IsReserved(ctx.SessionID, createGuid) {
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusFileNotAvailable}}, true
		}
		return nil, false
	}

	// A duplicate CreateGuid without the replay flag is rejected
	// (MS-SMB2 §3.3.5.9.12 / Samba NT_STATUS_DUPLICATE_OBJECTID).
	if !ctx.IsReplay {
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusDuplicateObjectid}}, true
	}

	// Shallow copy so per-response stamping never mutates the cache entry.
	resp := *entry.Response

	// Lease replays are validated and state-refreshed against the live
	// open. A replay carrying an RqLs context goes through the lease path;
	// a replay requesting a plain oplock (or none) echoes the REQUESTED
	// oplock level and re-derives durability for it (replay-dhv2-oplock2).
	if FindCreateContext(req.CreateContexts, LeaseContextTagRequest) != nil {
		if status := h.refreshReplayLease(ctx, req, entry, &resp); status != types.StatusSuccess {
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: status}}, true
		}
	} else {
		refreshReplayOplock(req, &resp)
	}
	return &resp, true
}

// refreshReplayOplock applies Samba's plain-oplock replay rule
// (smbd_smb2_create_replay): the replay response echoes the REQUESTED
// oplock level of the replay request and only carries a DH2Q durable
// grant blob if that requested oplock would itself qualify for V2
// durability (Batch). The original open's held oplock is untouched.
//
// For replay-dhv2-oplock1/3 the replay re-requests the same Batch oplock
// so the cached snapshot already matches and this is a no-op. For
// replay-dhv2-oplock2 the replay requests NONE over a Batch open: the
// response must report oplock_level=NONE, durable_open_v2=false, and drop
// the DH2Q response context (smbtorture asserts exactly these).

func refreshReplayOplock(req *CreateRequest, resp *CreateResponse) {
	// A lease-backed cached response (the open holds a lease, OplockLevel
	// 0xFF) is left untouched here: an oplock-less replay against a
	// lease-backed open does not re-key the open's lease, and the lease
	// response context stands. Only plain-oplock cached responses echo the
	// requested level.
	if resp.OplockLevel == OplockLevelLease {
		return
	}

	resp.OplockLevel = req.OplockLevel

	// Re-derive V2 durability for the requested oplock. Only a Batch oplock
	// qualifies a non-lease open for V2 durability (MS-SMB2 §3.3.5.9.10).
	// A weaker/none requested oplock drops the durable grant: strip the
	// DH2Q response context so out.durable_open_v2 reads false and timeout 0.
	if req.OplockLevel != OplockLevelBatch {
		resp.CreateContexts = stripCreateContext(resp.CreateContexts, DurableHandleV2RequestTag)
	}
}

// stripCreateContext returns a copy of contexts with every entry named
// tag removed. Returns the original slice when nothing matches (no
// allocation on the common path). Never mutates the input backing array,
// so a cached response shared across replays is safe.

func stripCreateContext(contexts []CreateContext, tag string) []CreateContext {
	hasTag := false
	for i := range contexts {
		if contexts[i].Name == tag {
			hasTag = true
			break
		}
	}
	if !hasTag {
		return contexts
	}
	out := make([]CreateContext, 0, len(contexts))
	for i := range contexts {
		if contexts[i].Name != tag {
			out = append(out, contexts[i])
		}
	}
	return out
}

// refreshReplayLease applies Samba's replay-with-lease rules to a DH2Q
// CREATE replay (the `if (state->rqls != NULL)` block in
// source3/smbd/smb2_create.c). When the replay request carries an RqLs
// (lease) context it:
//
//   - requires the live open to be lease-backed (else ACCESS_DENIED);
//   - requires the replay's lease key to equal the open's (else
//     ACCESS_DENIED);
//   - rewrites the lease response context to the CURRENT lease state and
//     epoch read from the LeaseManager, so an upgrade applied after the
//     original CREATE is reflected on replay.
//
// It returns StatusSuccess when the response may be returned (possibly
// mutated) or the ACCESS_DENIED status to reject with. A replay request
// without a lease context, or an entry with no associated open, is a
// no-op success — the cached snapshot stands.

func (h *Handler) refreshReplayLease(ctx *SMBHandlerContext, req *CreateRequest, entry *CachedCreateResponse, resp *CreateResponse) types.Status {
	leaseCtx := FindCreateContext(req.CreateContexts, LeaseContextTagRequest)
	if leaseCtx == nil || entry.OpenFile == nil {
		return types.StatusSuccess
	}
	lcc, err := DecodeLeaseCreateContext(leaseCtx.Data)
	if err != nil {
		// Malformed lease context on a replay is treated like the
		// non-lease path (the cached snapshot stands); the original
		// CREATE already validated the lease.
		return types.StatusSuccess
	}

	open := entry.OpenFile

	// Samba: replay with a lease is only allowed against an open that
	// itself holds a lease, and only with the same lease key.
	if open.OplockLevel != OplockLevelLease || open.LeaseKey != lcc.LeaseKey {
		return types.StatusAccessDenied
	}

	// Refresh the RqLs response context to the open's CURRENT lease state
	// (e.g. RH→RWH after a later upgrading CREATE on the same key).
	if h.LeaseManager == nil {
		return types.StatusSuccess
	}
	state, epoch, found := h.LeaseManager.GetLeaseState(ctx.Context, lock.FileHandle(open.MetadataHandle), open.ShareName, open.LeaseKey)
	if !found {
		return types.StatusSuccess
	}
	// resp is a shallow copy of the cached response, so resp.CreateContexts
	// still shares the cached entry's backing array. Clone the slice before
	// rewriting an element so we never mutate (or race another replay on)
	// the cached entry. rewriteLeaseResponseState itself returns fresh bytes.
	for i := range resp.CreateContexts {
		if resp.CreateContexts[i].Name != LeaseContextTagResponse {
			continue
		}
		contexts := make([]CreateContext, len(resp.CreateContexts))
		copy(contexts, resp.CreateContexts)
		contexts[i].Data = rewriteLeaseResponseState(contexts[i].Data, state, epoch)
		resp.CreateContexts = contexts
		break
	}
	return types.StatusSuccess
}

// rewriteLeaseResponseState patches the LeaseState (and, for a V2
// response, the Epoch) of an already-encoded RqLs response context in
// place without disturbing the lease key, flags, parent key, or wire
// version. Both the V1 (32-byte) and V2 (52-byte) layouts place the
// 32-bit LeaseState at offset 16; the V2 layout places the 16-bit Epoch
// at offset 48 (MS-SMB2 §2.2.14.2.10). A buffer too short for either
// layout is returned unchanged. It returns a fresh slice so the cached
// entry's bytes are never mutated.

func rewriteLeaseResponseState(data []byte, state uint32, epoch uint16) []byte {
	const (
		leaseStateOff = 16
		v2EpochOff    = 48
	)
	if len(data) < leaseStateOff+4 {
		return data
	}
	out := make([]byte, len(data))
	copy(out, data)
	binary.LittleEndian.PutUint32(out[leaseStateOff:leaseStateOff+4], state)
	if len(out) >= v2EpochOff+2 {
		binary.LittleEndian.PutUint16(out[v2EpochOff:v2EpochOff+2], epoch)
	}
	return out
}

// dh2qCreateGuid extracts the CreateGuid from a CREATE request's
// SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2 context. Returns the zero
// GUID when the context is missing, malformed, or carries a zero
// CreateGuid — callers must treat that as "no replay keying
// possible" (MS-SMB2 §2.2.13.2.11).

func dh2qCreateGuid(req *CreateRequest) [16]byte {
	dh2qCtx := FindCreateContext(req.CreateContexts, DurableHandleV2RequestTag)
	if dh2qCtx == nil {
		return [16]byte{}
	}
	_, _, createGuid, err := DecodeDH2QRequest(dh2qCtx.Data)
	if err != nil {
		return [16]byte{}
	}
	return createGuid
}

// handlePipeCreate handles CREATE on IPC$ for named pipes.
// Named pipes are used for DCE/RPC communication, e.g., srvsvc for share enumeration.
