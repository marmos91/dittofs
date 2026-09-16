package nfs

import (
	"context"
	"fmt"
	"hash/crc32"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	nfs_types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/logger"
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
	// drcMaxEntries bounds the cache across all shards. The Linux reply cache
	// scales its hash table with RAM but caps the working set in the low
	// thousands; 4096 covers the in-flight + recently-completed non-idempotent
	// ops of many concurrent clients while bounding memory to a few MB of small
	// reply blobs. Split evenly across drcShardCount shards.
	drcMaxEntries = 4096

	// drcShardCount partitions the cache so unrelated clients/XIDs don't
	// serialize on one lock. Power of two so shard selection is a mask.
	drcShardCount = 32
)

// Compile-time guards: drcShardCount must be a power of two (mask routing) and
// must divide drcMaxEntries evenly (per-shard cap). A bad edit makes these
// constant conversions negative, which fails to compile.
const (
	_ = uint(-(drcShardCount & (drcShardCount - 1)))
	_ = uint(-(drcMaxEntries % drcShardCount))
)

const (
	// drcTTL is how long a completed reply is retained for replay. NFS client
	// retransmit timeouts are on the order of seconds and back off; a few
	// seconds of retention catches the retransmit window without holding stale
	// replies. Mirrors the short lifetime of nfsd reply-cache DONE entries.
	drcTTL = 8 * time.Second

	// drcInProgressTTL bounds how long a reservation is allowed to answer
	// duplicates with a drop. A duplicate arriving while the original is
	// genuinely still executing must be dropped, so this cannot be the reply
	// TTL: it has to exceed the slowest handler that can hold a slot, or a
	// retransmission would re-run a non-idempotent operation the server is
	// still performing. It is bounded above by wanting a wedged request to
	// come back on its own within a support call rather than lasting as long
	// as the connection does, which is what an unreleased reservation would
	// otherwise do.
	//
	// decision: what the bound retires is the entry, not the request. A
	// handler that never returns has each expiry start another execution of
	// it, and because an entry carries no owner, the first executor's release
	// can drop a successor's reservation and its record can promote one — so
	// the request can also be answered twice on one XID. All of that needs a
	// handler that has already overrun this bound, where the alternative is
	// the request staying dropped for as long as the connection lives.
	//
	// ponytail: no owner token on an entry, which is what would make each of
	// those exact rather than merely rare. Add one if a handler is ever
	// expected to overrun this bound.
	drcInProgressTTL = 2 * time.Minute
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

// drcShard is one lock-partitioned slice of the cache.
type drcShard struct {
	mu      sync.Mutex
	entries map[drcKey]*drcEntry
}

// duplicateRequestCache is a server-wide bounded reply cache for non-idempotent
// NFSv3 procedures. It is partitioned into drcShardCount independently-locked
// shards keyed by client/XID. Safe for concurrent use.
type duplicateRequestCache struct {
	shards        [drcShardCount]drcShard
	maxPerShard   int
	ttl           time.Duration
	inProgressTTL time.Duration
	now           func() time.Time // injectable clock for tests
}

func newDuplicateRequestCache() *duplicateRequestCache {
	d := &duplicateRequestCache{
		maxPerShard:   drcMaxEntries / drcShardCount,
		ttl:           drcTTL,
		inProgressTTL: drcInProgressTTL,
		now:           time.Now,
	}
	for i := range d.shards {
		d.shards[i].entries = make(map[drcKey]*drcEntry)
	}
	return d
}

// shard routes a request to its lock partition by hashing the client identity
// and XID (FNV-1a). The body checksum is deliberately excluded so retransmits
// of the same (client, XID) always land on the same shard.
func (d *duplicateRequestCache) shard(srcAddr string, xid uint32) *drcShard {
	const (
		fnvOffset32 = 2166136261
		fnvPrime32  = 16777619
	)
	h := uint32(fnvOffset32)
	for i := 0; i < len(srcAddr); i++ {
		h = (h ^ uint32(srcAddr[i])) * fnvPrime32
	}
	for i := 0; i < 4; i++ {
		h = (h ^ (xid & 0xff)) * fnvPrime32
		xid >>= 8
	}
	return &d.shards[h&(drcShardCount-1)]
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

// boundFor returns how long an entry in this state stays valid: a recorded
// reply is worth replaying for the retransmit window, while a reservation has
// to outlive the request it is held for.
func (d *duplicateRequestCache) boundFor(state drcState) time.Duration {
	if state == drcInProgress {
		return d.inProgressTTL
	}
	return d.ttl
}

// lookup classifies an incoming request and, on a miss, atomically reserves an
// in-progress slot so concurrent duplicates are detected. The key is built from
// the source address, XID and a checksum of the request body.
//
// Callers must only invoke lookup for cacheable procedures (see isCacheable).
func (d *duplicateRequestCache) lookup(srcAddr string, xid uint32, body []byte) (drcLookupResult, []byte) {
	key := drcKey{srcAddr: srcAddr, xid: xid, checksum: crc32.ChecksumIEEE(body)}
	s := d.shard(srcAddr, xid)

	s.mu.Lock()
	defer s.mu.Unlock()

	if e, ok := s.entries[key]; ok {
		fresh := d.now().Sub(e.inserted) < d.boundFor(e.state)
		switch {
		case e.state == drcInProgress && fresh:
			return drcInProgressDup, nil
		case e.state == drcDone && fresh:
			return drcReplay, e.reply
		default:
			// Past its bound: expire lazily and fall through so the request is
			// treated as a fresh one. For a DONE entry that keeps a long-after
			// XID reuse from being answered with a false replay; for an
			// in-progress one it is the only thing that reclaims a reservation
			// that was never released, since evictIfNeeded sweeps only a shard
			// that is already at its cap.
			delete(s.entries, key)
		}
	}

	d.evictIfNeeded(s)
	s.entries[key] = &drcEntry{state: drcInProgress, inserted: d.now()}
	return drcMiss, nil
}

// record promotes the in-progress slot for (srcAddr, xid, body) to DONE with the
// encoded reply. It is a no-op if the slot was evicted in the meantime. The
// reply is copied so the caller may reuse the backing buffer.
func (d *duplicateRequestCache) record(srcAddr string, xid uint32, body []byte, reply []byte) {
	key := drcKey{srcAddr: srcAddr, xid: xid, checksum: crc32.ChecksumIEEE(body)}
	s := d.shard(srcAddr, xid)

	cp := make([]byte, len(reply))
	copy(cp, reply)

	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok {
		// Slot evicted under cap pressure while the handler ran; re-insert as
		// DONE so a retransmit still replays.
		d.evictIfNeeded(s)
		s.entries[key] = &drcEntry{state: drcDone, reply: cp, inserted: d.now()}
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
	s := d.shard(srcAddr, xid)

	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key]; ok && e.state == drcInProgress {
		delete(s.entries, key)
	}
}

// evictIfNeeded enforces the per-shard entry cap. The cap is per-shard by
// design (drcMaxEntries/drcShardCount): a global counter would re-introduce the
// cross-shard contention the sharding removes. It first drops entries past
// their own bound; if still at capacity it evicts the oldest entry
// (approximate LRU by insertion/completion time). Caller must hold s.mu.
//
// The sweep judges a reservation by drcInProgressTTL rather than by the reply
// TTL, or a full shard would retire a reservation seconds into the request it
// is holding and let the retransmission run a second copy of it — the bound
// drcInProgressTTL exists to keep well clear of.
func (d *duplicateRequestCache) evictIfNeeded(s *drcShard) {
	if len(s.entries) < d.maxPerShard {
		return
	}

	now := d.now()
	for k, e := range s.entries {
		if now.Sub(e.inserted) >= d.boundFor(e.state) {
			delete(s.entries, k)
		}
	}
	if len(s.entries) < d.maxPerShard {
		return
	}

	// Still full: evict one entry to make room, oldest first but preferring a
	// recorded reply over a reservation. Dropping a reply costs a
	// retransmission its replay; dropping a reservation lets a duplicate run a
	// second copy of a request that is still executing, which is worse. A
	// shard holding nothing but reservations has no such choice.
	var victim drcKey
	var victimAge time.Time
	var found, victimDone bool
	for k, e := range s.entries {
		done := e.state == drcDone
		if victimDone && !done {
			continue
		}
		if !found || (done && !victimDone) || e.inserted.Before(victimAge) {
			victim, victimAge, found, victimDone = k, e.inserted, true, done
		}
	}
	if found {
		delete(s.entries, victim)
	}
}

// withDRC runs fn under the duplicate-request cache.
//
// The cache has a three-phase protocol — look up, reserve, then either record
// the reply or release the reservation — and getting the last phase wrong is
// not a visible failure: a reservation left behind answers every later
// retransmission of that exact request with a silent drop until it ages past
// drcInProgressTTL, which is minutes of a wedged request rather than a
// refused one. Holding the protocol in one place is what makes the release
// unconditional: it is deferred, so it covers early returns and a handler that
// panics and is recovered per request, and it is a no-op once the reply has
// been recorded.
//
// fn reports whether its reply may be recorded. That decision stays with the
// caller because the programs disagree on it: NFSv3 caches whatever reply it
// produced, while NFSv4.0 caches only a reply the COMPOUND marked cacheable
// and only when the transport call itself succeeded. Deciding it here would
// silently change one of them.
func (c *NFSConnection) withDRC(
	ctx context.Context,
	call *rpc.RPCCallMessage,
	data []byte,
	clientAddr string,
	eligible bool,
	label string,
	fn func() (reply []byte, recordable bool, err error),
) ([]byte, error) {
	if c.server.drc == nil || !eligible {
		reply, _, err := fn()
		return reply, err
	}

	switch res, cached := c.server.drc.lookup(clientAddr, call.XID, data); res {
	case drcReplay:
		logger.DebugCtx(ctx, label+": duplicate request replayed from DRC",
			"client", clientAddr,
			"xid", fmt.Sprintf("0x%x", call.XID))
		return cached, nil

	case drcInProgressDup:
		// The original is still executing and owns the XID. Write nothing
		// rather than a second reply on the same XID: the in-flight request
		// produces the single authoritative one.
		logger.DebugCtx(ctx, label+": duplicate of in-flight request dropped",
			"client", clientAddr,
			"xid", fmt.Sprintf("0x%x", call.XID))
		return nil, errDropReply
	}

	// drcMiss reserved an in-progress slot; release it however this returns.
	defer c.server.drc.abort(clientAddr, call.XID, data)

	reply, recordable, err := fn()
	if recordable {
		c.server.drc.record(clientAddr, call.XID, data, reply)
	}
	return reply, err
}
