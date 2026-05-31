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

// reservedState is the per-reservation phase status returned to a replay
// (FLAGS_REPLAY_OPERATION) that arrives while the original conflicting CREATE
// for the same CreateGuid has not yet established a cached open. The
// reservation is two-phase (MS-SMB2 §3.3.5.9; Samba bug 14449
// smb2.replay.dhv2-pending1*):
//
//   - Phase A — the original CREATE is still pending on a share-mode conflict
//     (parked on a lease/oplock break, or waiting inline). A replay returns
//     Status = STATUS_FILE_NOT_AVAILABLE.
//
//   - Phase B — the conflict resolved to a terminal *failure* (the holder
//     released the cached handle via a break-ack but kept the file open, so the
//     share-mode violation stands). The original CREATE returned
//     STATUS_SHARING_VIOLATION. A replay must keep returning that SAME terminal
//     status from the reservation rather than re-running the full CREATE (which
//     would re-park and time out). UpdateReservedStatus performs the A→B
//     transition; the reservation is then cleared at session teardown
//     (ForgetSession) or by the TTL backstop below, never leaking.
//
// At is the time the reservation was taken or last transitioned, used by the
// TTL backstop so a Phase-B reservation that is somehow never explicitly
// cleared cannot brick all future opens of that CreateGuid indefinitely.
type reservedState struct {
	Status types.Status
	At     time.Time
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
	reserved map[reserveKey]reservedState
}

// NewCreateReplayCache builds an empty cache.
func NewCreateReplayCache() *CreateReplayCache {
	return &CreateReplayCache{
		entries:  make(map[[16]byte]*CachedCreateResponse),
		reserved: make(map[reserveKey]reservedState),
	}
}

// Reserve marks a CreateGuid as having an in-progress (Phase A, pending)
// original CREATE for the given session. While reserved in Phase A, a replayed
// CREATE for the same CreateGuid resolves to STATUS_FILE_NOT_AVAILABLE. A zero
// CreateGuid is ignored. Reserve is idempotent: re-reserving an already-reserved
// guid resets it to Phase A (it never clobbers a Phase-B terminal status into
// existence, but a fresh original CREATE attempt legitimately restarts Phase A).
func (c *CreateReplayCache) Reserve(sessionID uint64, createGuid [16]byte) {
	if createGuid == ([16]byte{}) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reserved[reserveKey{SessionID: sessionID, CreateGuid: createGuid}] = reservedState{
		Status: types.StatusFileNotAvailable,
		At:     time.Now(),
	}
}

// UpdateReservedStatus transitions an existing reservation from Phase A
// (pending → STATUS_FILE_NOT_AVAILABLE) to Phase B (the original conflicting
// CREATE resolved to a terminal failure status, e.g. STATUS_SHARING_VIOLATION).
// A subsequent replay then returns that terminal status from the reservation
// instead of re-running CREATE. It is a no-op when the guid is not currently
// reserved (the reservation was already released on a success path), so it can
// never resurrect a cleared reservation. The timestamp is refreshed so the TTL
// backstop applies to the Phase-B window from its start.
func (c *CreateReplayCache) UpdateReservedStatus(sessionID uint64, createGuid [16]byte, status types.Status) {
	if createGuid == ([16]byte{}) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := reserveKey{SessionID: sessionID, CreateGuid: createGuid}
	if _, ok := c.reserved[key]; !ok {
		return
	}
	c.reserved[key] = reservedState{Status: status, At: time.Now()}
}

// Release clears an in-progress reservation once the parked CREATE has
// reached a terminal SUCCESS state (response stored). Idempotent. Phase-B
// (terminal-failure) reservations are NOT released here — they persist so a
// later replay returns the cached terminal status — and are reaped instead by
// ForgetSession (session teardown) or the TTL backstop.
func (c *CreateReplayCache) Release(sessionID uint64, createGuid [16]byte) {
	if createGuid == ([16]byte{}) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.reserved, reserveKey{SessionID: sessionID, CreateGuid: createGuid})
}

// IsReserved reports whether a CreateGuid's original CREATE is currently
// reserved (Phase A or B) for the given session and has not aged past the TTL
// backstop. Expired reservations are reaped on access.
func (c *CreateReplayCache) IsReserved(sessionID uint64, createGuid [16]byte) bool {
	_, ok := c.ReservedStatus(sessionID, createGuid)
	return ok
}

// ReservedStatus returns the phase status a replay must receive while the
// CreateGuid is reserved (STATUS_FILE_NOT_AVAILABLE in Phase A, the stored
// terminal status in Phase B), or ok=false on no/expired reservation. A
// reservation older than replayCacheTTL is reaped and treated as absent so a
// Phase-B reservation can never brick the guid indefinitely.
func (c *CreateReplayCache) ReservedStatus(sessionID uint64, createGuid [16]byte) (types.Status, bool) {
	if createGuid == ([16]byte{}) {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := reserveKey{SessionID: sessionID, CreateGuid: createGuid}
	st, ok := c.reserved[key]
	if !ok {
		return 0, false
	}
	if time.Since(st.At) > replayCacheTTL {
		delete(c.reserved, key)
		return 0, false
	}
	return st.Status, true
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
