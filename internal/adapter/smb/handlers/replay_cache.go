package handlers

import (
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// SMB3 replay protection (MS-SMB2 §3.3.5.2.5, §3.3.5.9).
//
// When a client retransmits a request whose response was lost (e.g., the
// channel carrying the response failed before delivery), it MAY set
// SMB2_FLAGS_REPLAY_OPERATION on the retried header. The server must
// recognise this and return the previously-computed result for the
// original request instead of executing it fresh — otherwise an idempotent
// CREATE that already established the open trips STATUS_SHARING_VIOLATION
// on the replay, and a LOCK that was already acquired trips
// STATUS_LOCK_NOT_GRANTED.
//
// Two replay caches are needed:
//
//  1. CREATE replay-cache, keyed by SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2
//     CreateGuid. The cache holds the cached CREATE response (raw body +
//     status + FileID) for a short window after a successful V2 CREATE.
//     A second CREATE with the same CreateGuid and FLAGS_REPLAY_OPERATION
//     returns the cached response.
//
//  2. LOCK replay-cache, keyed by (FileID, LockSequenceIndex) and
//     remembering the last LockSequenceNumber accepted for that slot.
//     LockSequence is a packed 32-bit field on the SMB2_LOCK request:
//     high 4 bits hold the index (0–15), low 28 bits the sequence number.
//     On a replay with the same (FileID, Index, Number), the server
//     returns the cached status.
//
// The caches are intentionally small and bounded by an eviction TTL —
// they cover the narrow "response in flight died" window, not long-term
// session state. A miss falls through to the normal handler path; the
// caller is responsible for storing a fresh entry on success.

// replayCacheTTL bounds how long a CREATE replay cache entry stays
// valid after the original response was generated. Samba uses a
// 600-second window (`source3/smbd/smb2_create.c:replay_cache_purge`).
// We mirror that here so the test client's retry window aligns.
const replayCacheTTL = 600 * time.Second

// maxCreateReplayEntries caps the CREATE replay cache so a misbehaving
// client cannot grow it unbounded. Old entries are pruned lazily on
// each store call.
const maxCreateReplayEntries = 4096

// CachedCreateResponse holds the materialised wire-level CREATE
// response that should be returned on replay. We cache the full
// CreateResponse (not just the encoded body) so the caller can still
// access FileID for compound-chain FileID propagation. SessionID is
// recorded so session teardown can flush related entries and so a
// CreateGuid collision across sessions cannot hijack another open.
//
// OpenFile references the live Open established by the original CREATE.
// A replayed DH2Q CREATE must reflect the CURRENT state of that Open,
// not a create-time snapshot — Samba rebuilds the lease response from
// op->compat->lease->lease (source3/smbd/smb2_create.c). In particular
// a lease that was upgraded (RH→RWH) by a later CREATE on the same key
// must replay back the upgraded state (smbtorture replay-dhv2-lease1/2).
// The reference also carries the original oplock type and lease key so
// the replay path can apply Samba's lease/oplock-mismatch ACCESS_DENIED
// gates (replay-dhv2-lease3 / oplock-lease).
type CachedCreateResponse struct {
	SessionID uint64
	Response  *CreateResponse
	OpenFile  *OpenFile
	StoredAt  time.Time
}

// reserveKey scopes an in-progress CreateGuid reservation per session,
// so a CreateGuid collision across sessions cannot make one session's
// parked CREATE shadow another's.
type reserveKey struct {
	SessionID  uint64
	CreateGuid [16]byte
}

// CreateReplayCache stores CREATE responses keyed by DH2Q CreateGuid
// so a replayed CREATE (FLAGS_REPLAY_OPERATION) returns the original
// result instead of re-executing. CreateGuid is client-generated and
// globally unique per durable open, so it is sufficient as the key.
//
// It also tracks CreateGuids whose original CREATE is still in progress
// — parked on a pending oplock/lease break (MS-SMB2 §3.3.5.9). A replay
// that arrives while the original is still parked must return
// STATUS_FILE_NOT_AVAILABLE immediately rather than block or time out
// (smbtorture smb2.replay.replay-dhv2-pending* / *-vs-{oplock,lease}).
// Samba models this with the FWP_RESERVED / FILE_NOT_AVAILABLE slot
// states in smb2srv_open_lookup_replay_cache.
type CreateReplayCache struct {
	mu       sync.Mutex
	entries  map[[16]byte]*CachedCreateResponse
	reserved map[reserveKey]struct{}
}

// NewCreateReplayCache builds an empty cache.
func NewCreateReplayCache() *CreateReplayCache {
	return &CreateReplayCache{
		entries:  make(map[[16]byte]*CachedCreateResponse),
		reserved: make(map[reserveKey]struct{}),
	}
}

// Reserve marks a CreateGuid as having an in-progress (parked) original
// CREATE for the given session. While reserved, a replayed CREATE for
// the same CreateGuid resolves to STATUS_FILE_NOT_AVAILABLE. A zero
// CreateGuid is ignored.
func (c *CreateReplayCache) Reserve(sessionID uint64, createGuid [16]byte) {
	if createGuid == ([16]byte{}) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reserved[reserveKey{SessionID: sessionID, CreateGuid: createGuid}] = struct{}{}
}

// Release clears an in-progress reservation once the parked CREATE has
// reached a terminal state (success stored, or failed). Idempotent.
func (c *CreateReplayCache) Release(sessionID uint64, createGuid [16]byte) {
	if createGuid == ([16]byte{}) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.reserved, reserveKey{SessionID: sessionID, CreateGuid: createGuid})
}

// IsReserved reports whether a CreateGuid's original CREATE is currently
// parked for the given session.
func (c *CreateReplayCache) IsReserved(sessionID uint64, createGuid [16]byte) bool {
	if createGuid == ([16]byte{}) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.reserved[reserveKey{SessionID: sessionID, CreateGuid: createGuid}]
	return ok
}

// Store records the response for a successful V2 CREATE keyed by
// CreateGuid. Only success responses are cached — a replayed failed
// CREATE should run through the normal handler path and may even
// succeed the second time. openFile is the live Open the CREATE
// established; it is consulted on replay to rebuild the current
// lease/oplock state (may be nil for paths that have no Open, in which
// case the cached snapshot is replayed verbatim).
func (c *CreateReplayCache) Store(sessionID uint64, createGuid [16]byte, resp *CreateResponse, openFile *OpenFile) {
	if createGuid == ([16]byte{}) || resp == nil || resp.Status != types.StatusSuccess {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	c.entries[createGuid] = &CachedCreateResponse{
		SessionID: sessionID,
		Response:  resp,
		OpenFile:  openFile,
		StoredAt:  time.Now(),
	}
}

// Lookup returns the cached response if one exists for the given
// CreateGuid + session and has not expired. Replay is scoped per
// session — a different session's CreateGuid collision (vanishingly
// unlikely but possible) must not hijack another session's open.
func (c *CreateReplayCache) Lookup(sessionID uint64, createGuid [16]byte) *CreateResponse {
	if e := c.LookupEntry(sessionID, createGuid); e != nil {
		return e.Response
	}
	return nil
}

// LookupEntry returns the full cache entry (response + live Open) for
// the given CreateGuid + session, or nil on miss/expiry. The create
// path uses this to rebuild the current lease/oplock state on replay
// and to distinguish a replay (FLAGS_REPLAY_OPERATION set) from a
// duplicate non-replay CREATE (→ DUPLICATE_OBJECTID).
func (c *CreateReplayCache) LookupEntry(sessionID uint64, createGuid [16]byte) *CachedCreateResponse {
	if createGuid == ([16]byte{}) {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[createGuid]
	if !ok || entry.SessionID != sessionID {
		return nil
	}
	if time.Since(entry.StoredAt) > replayCacheTTL {
		delete(c.entries, createGuid)
		return nil
	}
	return entry
}

// Forget drops the cached entry for CreateGuid. Called when the open
// is closed cleanly — the cache window is only meaningful while a
// retry could still arrive.
func (c *CreateReplayCache) Forget(createGuid [16]byte) {
	if createGuid == ([16]byte{}) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, createGuid)
}

// ForgetSession drops all entries for the given session. Called by
// session teardown so a long-lived ServerGUID-stable replay cache
// does not survive logoff.
func (c *CreateReplayCache) ForgetSession(sessionID uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if e.SessionID == sessionID {
			delete(c.entries, k)
		}
	}
	for k := range c.reserved {
		if k.SessionID == sessionID {
			delete(c.reserved, k)
		}
	}
}

// Len returns the number of cached entries (test / metrics).
func (c *CreateReplayCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// pruneLocked evicts expired entries and, if the cap is still
// exceeded, drops oldest-first until under the cap. Cheap because
// the cache is bounded small.
func (c *CreateReplayCache) pruneLocked() {
	now := time.Now()
	for k, e := range c.entries {
		if now.Sub(e.StoredAt) > replayCacheTTL {
			delete(c.entries, k)
		}
	}
	if len(c.entries) < maxCreateReplayEntries {
		return
	}
	// Over-cap: drop oldest. Linear scan since the cap is small.
	var oldestKey [16]byte
	var oldestAt time.Time
	for k, e := range c.entries {
		if oldestAt.IsZero() || e.StoredAt.Before(oldestAt) {
			oldestAt = e.StoredAt
			oldestKey = k
		}
	}
	delete(c.entries, oldestKey)
}

// ============================================================================
// LOCK replay protection (MS-SMB2 §3.3.5.14)
// ============================================================================

// LockSequenceIndexMax bounds the 28-bit LockSequenceIndex (bucket)
// in the SMB2_LOCK request LockSequence packed value (MS-SMB2
// §2.2.26). Samba caps the array at 64 buckets
// (`SMB2_LOCK_SEQUENCE_ARRAY_SIZE`); we mirror that to avoid an
// unbounded per-open footprint when a client picks large bucket
// numbers. Indices outside [1, LockSequenceIndexMax] are not tracked.
const LockSequenceIndexMax uint32 = 64

// CachedLockResponse holds the last LockSequenceNumber stored at a
// given (FileID, Index) bucket together with the status returned for
// that request. On replay with matching number, the server returns
// Status verbatim (MS-SMB2 §3.3.5.14 step 4).
type CachedLockResponse struct {
	Number uint8
	Status types.Status
}

// lockReplayKey indexes the LOCK replay cache by FileID + bucket.
type lockReplayKey struct {
	FileID [16]byte
	Index  uint32
}

// LockReplayCache stores the last (LockSequenceNumber, status) pair
// per (FileID, LockSequenceIndex). Bounded implicitly by the per-open
// 16-slot cap × max concurrent opens.
type LockReplayCache struct {
	mu      sync.RWMutex
	entries map[lockReplayKey]CachedLockResponse
}

// NewLockReplayCache builds an empty cache.
func NewLockReplayCache() *LockReplayCache {
	return &LockReplayCache{entries: make(map[lockReplayKey]CachedLockResponse)}
}

// UnpackLockSequence extracts (index, number) from the packed
// LockSequence field per MS-SMB2 §2.2.26: low 4 bits =
// LockSequenceNumber (the byte stored in the slot), upper 28 bits =
// LockSequenceIndex (the slot/bucket number, 1-based; 0 = "not
// tracked"). Mirrors Samba `smb2_lock_recv`: `value = in & 0xF`,
// `bucket = in >> 4`, replay tracked only when `bucket > 0`.
func UnpackLockSequence(packed uint32) (index uint32, number uint8, enabled bool) {
	number = uint8(packed & 0xF)
	index = packed >> 4
	enabled = index > 0
	return index, number, enabled
}

// Lookup returns the cached status if the bucket already holds the
// same LockSequenceNumber. Caller passes the unpacked (index, number).
// Returns ok=false when no match: the LOCK should execute normally
// and the result stored via Store.
func (c *LockReplayCache) Lookup(fileID [16]byte, index uint32, number uint8) (types.Status, bool) {
	if index == 0 || index > LockSequenceIndexMax {
		return 0, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[lockReplayKey{FileID: fileID, Index: index}]
	if !ok {
		return 0, false
	}
	if entry.Number != number {
		return 0, false
	}
	return entry.Status, true
}

// Store records the (Number, Status) pair for the given bucket.
// Subsequent calls with a different Number for the same bucket
// overwrite — the bucket tracks the LATEST sequence, not history.
func (c *LockReplayCache) Store(fileID [16]byte, index uint32, number uint8, status types.Status) {
	if index == 0 || index > LockSequenceIndexMax {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[lockReplayKey{FileID: fileID, Index: index}] = CachedLockResponse{
		Number: number,
		Status: status,
	}
}

// ForgetFile drops all buckets for the given FileID. Called by
// CLOSE so the cache footprint is freed with the handle.
func (c *LockReplayCache) ForgetFile(fileID [16]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.FileID == fileID {
			delete(c.entries, k)
		}
	}
}

// Len returns the number of cached entries (test / metrics).
func (c *LockReplayCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}
