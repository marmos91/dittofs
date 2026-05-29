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
type CachedCreateResponse struct {
	SessionID uint64
	Response  *CreateResponse
	StoredAt  time.Time
}

// CreateReplayCache stores CREATE responses keyed by DH2Q CreateGuid
// so a replayed CREATE (FLAGS_REPLAY_OPERATION) returns the original
// result instead of re-executing. CreateGuid is client-generated and
// globally unique per durable open, so it is sufficient as the key.
type CreateReplayCache struct {
	mu      sync.Mutex
	entries map[[16]byte]*CachedCreateResponse
}

// NewCreateReplayCache builds an empty cache.
func NewCreateReplayCache() *CreateReplayCache {
	return &CreateReplayCache{
		entries: make(map[[16]byte]*CachedCreateResponse),
	}
}

// Store records the response for a successful V2 CREATE keyed by
// CreateGuid. Only success responses are cached — a replayed failed
// CREATE should run through the normal handler path and may even
// succeed the second time.
func (c *CreateReplayCache) Store(sessionID uint64, createGuid [16]byte, resp *CreateResponse) {
	if createGuid == ([16]byte{}) || resp == nil || resp.Status != types.StatusSuccess {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	c.entries[createGuid] = &CachedCreateResponse{
		SessionID: sessionID,
		Response:  resp,
		StoredAt:  time.Now(),
	}
}

// Lookup returns the cached response if one exists for the given
// CreateGuid + session and has not expired. Replay is scoped per
// session — a different session's CreateGuid collision (vanishingly
// unlikely but possible) must not hijack another session's open.
func (c *CreateReplayCache) Lookup(sessionID uint64, createGuid [16]byte) *CreateResponse {
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
	return entry.Response
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
