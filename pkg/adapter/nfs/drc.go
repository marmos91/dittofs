package nfs

import (
	"hash/crc32"
	"sync"
	"time"

	nfs_types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
)

// ============================================================================
// NFSv3 Duplicate-Request Cache (DRC)
// ============================================================================
//
// On a hard NFS mount a client that times out an RPC retransmits the *same*
// request (same XID). For idempotent procedures (GETATTR, LOOKUP, READ, WRITE,
// READDIR, ...) re-executing is harmless. For non-idempotent procedures
// (REMOVE, RMDIR, RENAME, non-exclusive CREATE, MKDIR, LINK, SYMLINK, MKNOD,
// SETATTR-with-guard) re-execution produces a spurious error: the second
// REMOVE returns NFS3ERR_NOENT, the second MKDIR/CREATE returns NFS3ERR_EXIST,
// a guarded SETATTR returns NFS3ERR_NOT_SYNC, etc. The client surfaces that as
// a real failure even though its original op succeeded.
//
// This cache mirrors the Linux server reply cache (fs/nfsd/nfscache.c): it
// records the encoded reply of a completed non-idempotent op keyed by
// (source address, XID, request-body checksum) and, on a confirmed duplicate,
// replays the recorded bytes instead of re-invoking the handler.
//
// Why all three key fields: the data path is TCP, but an RPC-timeout retransmit
// on a hard mount can arrive on a *reconnected* connection with a fresh source
// port, and a client is free to reuse an XID for a brand-new request once the
// old one is believed complete. XID alone is therefore not collision-safe. The
// request-body checksum (as in nfscache.c) disambiguates an XID reused for a
// genuinely different request from a true retransmit of the same bytes.
//
// Idempotent procedures bypass this cache entirely and are never recorded —
// caching their (often large) replies would waste memory for no correctness
// benefit. CREATE-exclusive is already idempotent via its create verifier
// (the metadata store stores an IdempotencyToken), so a retransmit re-resolves
// to the same file; it flows through the non-idempotent CREATE path here
// harmlessly (recording its reply is correct and cheap).

const (
	// drcMaxEntries bounds the cache. The Linux reply cache scales its hash
	// table with RAM but caps the working set in the low thousands; 4096 covers
	// the in-flight + recently-completed non-idempotent ops of many concurrent
	// clients while bounding memory to a few MB of small reply blobs.
	drcMaxEntries = 4096

	// drcTTL is how long a completed reply is retained for replay. NFS client
	// retransmit timeouts are on the order of seconds and back off; a few
	// seconds of retention catches the retransmit window without holding stale
	// replies. Mirrors the short lifetime of nfsd reply-cache DONE entries.
	drcTTL = 8 * time.Second
)

// drcState is the lifecycle of a cache entry, mirroring nfsd's RC_INPROG /
// RC_REPLY distinction.
type drcState uint8

const (
	// drcInProgress: the original request is still executing. A duplicate that
	// arrives in this window is dropped (no reply written) — the in-flight
	// original will produce the single authoritative reply.
	drcInProgress drcState = iota

	// drcDone: the original completed and its reply bytes are cached for replay.
	drcDone
)

// drcKey identifies a request. Two requests are "the same" iff all three match.
type drcKey struct {
	srcAddr  string // client source address (host:port)
	xid      uint32 // RPC transaction id
	checksum uint32 // CRC-32 of the request body (disambiguates XID reuse)
}

// drcEntry is a cached request slot.
type drcEntry struct {
	state    drcState
	reply    []byte    // encoded reply bytes (valid only when state == drcDone)
	inserted time.Time // for TTL eviction
}

// drcLookupResult tells the dispatch path what to do for an incoming request.
type drcLookupResult uint8

const (
	// drcMiss: not seen before; the caller registered an in-progress entry and
	// MUST run the handler, then call Record with the reply.
	drcMiss drcLookupResult = iota

	// drcReplay: a completed duplicate; the caller MUST return the cached reply
	// and MUST NOT run the handler.
	drcReplay

	// drcInProgressDup: a duplicate of a still-executing request; the caller
	// MUST drop the request (write nothing) and let the original reply.
	drcInProgressDup
)

// duplicateRequestCache is a server-wide bounded reply cache for non-idempotent
// NFSv3 procedures. Safe for concurrent use.
type duplicateRequestCache struct {
	mu         sync.Mutex
	entries    map[drcKey]*drcEntry
	maxEntries int
	ttl        time.Duration
	now        func() time.Time // injectable clock for tests
}

func newDuplicateRequestCache() *duplicateRequestCache {
	return &duplicateRequestCache{
		entries:    make(map[drcKey]*drcEntry),
		maxEntries: drcMaxEntries,
		ttl:        drcTTL,
		now:        time.Now,
	}
}

// drcCachedProcs is the set of non-idempotent NFSv3 procedures whose replies
// are cached. Every other procedure bypasses the cache.
var drcCachedProcs = map[uint32]struct{}{
	nfs_types.NFSProcSetAttr: {}, // guarded SETATTR is non-idempotent (NFS3ERR_NOT_SYNC on replay)
	nfs_types.NFSProcCreate:  {},
	nfs_types.NFSProcMkdir:   {},
	nfs_types.NFSProcSymlink: {},
	nfs_types.NFSProcMknod:   {},
	nfs_types.NFSProcRemove:  {},
	nfs_types.NFSProcRmdir:   {},
	nfs_types.NFSProcRename:  {},
	nfs_types.NFSProcLink:    {},
}

// isCacheable reports whether a procedure's reply should flow through the DRC.
// Idempotent procedures (GETATTR, LOOKUP, READ, WRITE, READDIR, ...) return false
// and never touch the cache.
func isCacheable(procedure uint32) bool {
	_, ok := drcCachedProcs[procedure]
	return ok
}

// lookup classifies an incoming request and, on a miss, atomically reserves an
// in-progress slot so concurrent duplicates are detected. The key is built from
// the source address, XID and a checksum of the request body.
//
// Callers must only invoke lookup for cacheable procedures (see isCacheable).
func (d *duplicateRequestCache) lookup(srcAddr string, xid uint32, body []byte) (drcLookupResult, []byte) {
	key := drcKey{srcAddr: srcAddr, xid: xid, checksum: crc32.ChecksumIEEE(body)}

	d.mu.Lock()
	defer d.mu.Unlock()

	if e, ok := d.entries[key]; ok {
		switch {
		case e.state == drcInProgress:
			return drcInProgressDup, nil
		case d.now().Sub(e.inserted) < d.ttl:
			return drcReplay, e.reply
		default:
			// DONE but past TTL: expire lazily and fall through so a long-after
			// XID reuse is treated as a fresh request, not a false replay.
			delete(d.entries, key)
		}
	}

	d.evictIfNeeded()
	d.entries[key] = &drcEntry{state: drcInProgress, inserted: d.now()}
	return drcMiss, nil
}

// record promotes the in-progress slot for (srcAddr, xid, body) to DONE with the
// encoded reply. It is a no-op if the slot was evicted in the meantime. The
// reply is copied so the caller may reuse the backing buffer.
func (d *duplicateRequestCache) record(srcAddr string, xid uint32, body []byte, reply []byte) {
	key := drcKey{srcAddr: srcAddr, xid: xid, checksum: crc32.ChecksumIEEE(body)}

	cp := make([]byte, len(reply))
	copy(cp, reply)

	d.mu.Lock()
	defer d.mu.Unlock()

	e, ok := d.entries[key]
	if !ok {
		// Slot evicted under cap pressure while the handler ran; re-insert as
		// DONE so a retransmit still replays.
		d.evictIfNeeded()
		d.entries[key] = &drcEntry{state: drcDone, reply: cp, inserted: d.now()}
		return
	}
	e.state = drcDone
	e.reply = cp
	e.inserted = d.now()
}

// abort drops the in-progress slot for a request whose handler did not produce
// a cacheable reply (e.g. decode failure). Without this an errored op would
// leave a permanent in-progress slot that swallows later legitimate retries.
func (d *duplicateRequestCache) abort(srcAddr string, xid uint32, body []byte) {
	key := drcKey{srcAddr: srcAddr, xid: xid, checksum: crc32.ChecksumIEEE(body)}

	d.mu.Lock()
	defer d.mu.Unlock()
	if e, ok := d.entries[key]; ok && e.state == drcInProgress {
		delete(d.entries, key)
	}
}

// evictIfNeeded enforces the entry cap. It first drops TTL-expired entries; if
// still at capacity it evicts the oldest entry (approximate LRU by insertion/
// completion time). Caller must hold d.mu.
func (d *duplicateRequestCache) evictIfNeeded() {
	if len(d.entries) < d.maxEntries {
		return
	}

	now := d.now()
	for k, e := range d.entries {
		if now.Sub(e.inserted) >= d.ttl {
			delete(d.entries, k)
		}
	}
	if len(d.entries) < d.maxEntries {
		return
	}

	// Still full: evict the single oldest entry to make room.
	var oldestKey drcKey
	var oldest time.Time
	first := true
	for k, e := range d.entries {
		if first || e.inserted.Before(oldest) {
			oldestKey, oldest, first = k, e.inserted, false
		}
	}
	if !first {
		delete(d.entries, oldestKey)
	}
}
