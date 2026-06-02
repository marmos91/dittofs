package nfs

import (
	"sync"
	"testing"
	"time"

	nfs_types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
)

// dispatchThroughDRC models the dispatch wiring: a cacheable request flows
// through the DRC (replay/in-progress/miss), and on a miss the handler runs and
// its reply is recorded. handler reports (reply, cacheable). This mirrors
// handleNFSProcedure so the tests prove the wiring contract, not just the map.
func dispatchThroughDRC(d *duplicateRequestCache, srcAddr string, xid uint32, body []byte, handler func() (reply []byte, cacheable bool)) (reply []byte, dropped bool) {
	switch res, cached := d.lookup(srcAddr, xid, body); res {
	case drcReplay:
		return cached, false
	case drcInProgressDup:
		return nil, true
	}
	r, cacheable := handler()
	if !cacheable {
		d.abort(srcAddr, xid, body)
		return nil, false
	}
	d.record(srcAddr, xid, body, r)
	return r, false
}

// TestDRC_ReplaysNonIdempotentReply proves a retransmitted REMOVE replays the
// original success reply instead of re-executing into NFS3ERR_NOENT.
//
// This is the TDD anchor: the handler returns the success reply on the FIRST
// call and a NOENT reply on any SUBSEQUENT call. Without the DRC the second
// dispatch would surface NOENT; with it the cached success reply is replayed.
func TestDRC_ReplaysNonIdempotentReply(t *testing.T) {
	d := newDuplicateRequestCache()

	const srcAddr = "10.0.0.1:1010"
	const xid = uint32(0xAABBCCDD)
	body := []byte("remove(dir-fh, \"file\")")

	successReply := []byte{0, 0, 0, 0} // NFS3OK
	noentReply := []byte{0, 0, 0, 2}   // NFS3ERR_NOENT

	calls := 0
	handler := func() ([]byte, bool) {
		calls++
		if calls == 1 {
			return successReply, true
		}
		return noentReply, true // re-execution: file already gone
	}

	// Original request.
	got, dropped := dispatchThroughDRC(d, srcAddr, xid, body, handler)
	if dropped {
		t.Fatal("original request unexpectedly dropped")
	}
	if string(got) != string(successReply) {
		t.Fatalf("original reply = %v, want success %v", got, successReply)
	}

	// Retransmit (same srcAddr+xid+body).
	got, dropped = dispatchThroughDRC(d, srcAddr, xid, body, handler)
	if dropped {
		t.Fatal("retransmit unexpectedly dropped")
	}
	if string(got) != string(successReply) {
		t.Fatalf("retransmit reply = %v, want replayed success %v (re-executed into NOENT?)", got, successReply)
	}
	if calls != 1 {
		t.Fatalf("handler invoked %d times, want 1 (DRC must not re-execute)", calls)
	}
}

// TestDRC_ReplaysAcrossProcedures covers RENAME/MKDIR/LINK with the same
// "succeed then fail on replay" shape, ensuring each non-idempotent op replays.
func TestDRC_ReplaysAcrossProcedures(t *testing.T) {
	for _, proc := range []uint32{
		nfs_types.NFSProcRename,
		nfs_types.NFSProcMkdir,
		nfs_types.NFSProcLink,
	} {
		if !isCacheable(proc) {
			t.Fatalf("proc %d expected cacheable", proc)
		}
		d := newDuplicateRequestCache()
		body := []byte{byte(proc), 1, 2, 3}
		ok := []byte{0, 0, 0, 0}
		errReply := []byte{0, 0, 0, 17} // e.g. NFS3ERR_EXIST

		calls := 0
		h := func() ([]byte, bool) {
			calls++
			if calls == 1 {
				return ok, true
			}
			return errReply, true
		}
		_, _ = dispatchThroughDRC(d, "c:1", 7, body, h)
		got, _ := dispatchThroughDRC(d, "c:1", 7, body, h)
		if string(got) != string(ok) {
			t.Fatalf("proc %d: replay = %v, want %v", proc, got, ok)
		}
		if calls != 1 {
			t.Fatalf("proc %d: handler called %d times, want 1", proc, calls)
		}
	}
}

// TestDRC_IdempotentOpsBypass proves GETATTR/READ/LOOKUP/WRITE/READDIR are not
// cacheable and so never enter the cache.
func TestDRC_IdempotentOpsBypass(t *testing.T) {
	idempotent := []uint32{
		nfs_types.NFSProcGetAttr,
		nfs_types.NFSProcRead,
		nfs_types.NFSProcLookup,
		nfs_types.NFSProcWrite,
		nfs_types.NFSProcReadDir,
		nfs_types.NFSProcReadDirPlus,
		nfs_types.NFSProcAccess,
		nfs_types.NFSProcFsStat,
		nfs_types.NFSProcCommit,
	}
	for _, p := range idempotent {
		if isCacheable(p) {
			t.Fatalf("idempotent proc %d must not be cacheable", p)
		}
	}

	// And the cacheable set is exactly the non-idempotent procedures.
	for _, p := range []uint32{
		nfs_types.NFSProcSetAttr,
		nfs_types.NFSProcCreate,
		nfs_types.NFSProcMkdir,
		nfs_types.NFSProcSymlink,
		nfs_types.NFSProcMknod,
		nfs_types.NFSProcRemove,
		nfs_types.NFSProcRmdir,
		nfs_types.NFSProcRename,
		nfs_types.NFSProcLink,
	} {
		if !isCacheable(p) {
			t.Fatalf("non-idempotent proc %d must be cacheable", p)
		}
	}
}

// TestDRC_DifferentChecksumIsNewRequest proves a same-srcAddr+same-XID request
// with a different body is treated as a fresh request (no false replay). This
// is the reconnect/XID-reuse disambiguation that XID alone cannot provide.
func TestDRC_DifferentChecksumIsNewRequest(t *testing.T) {
	d := newDuplicateRequestCache()
	const srcAddr = "10.0.0.2:2020"
	const xid = uint32(42)

	replyA := []byte{0, 0, 0, 0}
	replyB := []byte{0, 0, 0, 13}

	calls := 0
	mk := func(r []byte) func() ([]byte, bool) {
		return func() ([]byte, bool) { calls++; return r, true }
	}

	got, _ := dispatchThroughDRC(d, srcAddr, xid, []byte("remove A"), mk(replyA))
	if string(got) != string(replyA) {
		t.Fatalf("first reply = %v, want %v", got, replyA)
	}
	// Same xid, different body -> must run the handler, not replay replyA.
	got, _ = dispatchThroughDRC(d, srcAddr, xid, []byte("remove B"), mk(replyB))
	if string(got) != string(replyB) {
		t.Fatalf("different-checksum reply = %v, want fresh %v (false replay?)", got, replyB)
	}
	if calls != 2 {
		t.Fatalf("handler called %d times, want 2 (both are distinct requests)", calls)
	}
}

// TestDRC_InProgressDuplicateDropped proves a duplicate arriving while the
// original is still executing is dropped (no double reply / double execution).
func TestDRC_InProgressDuplicateDropped(t *testing.T) {
	d := newDuplicateRequestCache()
	const srcAddr = "10.0.0.3:3030"
	const xid = uint32(99)
	body := []byte("rmdir(...)")

	// Reserve the in-progress slot (simulates original mid-flight).
	res, _ := d.lookup(srcAddr, xid, body)
	if res != drcMiss {
		t.Fatalf("first lookup = %v, want drcMiss", res)
	}
	// Duplicate while in progress.
	res, _ = d.lookup(srcAddr, xid, body)
	if res != drcInProgressDup {
		t.Fatalf("concurrent duplicate lookup = %v, want drcInProgressDup", res)
	}
	// After completion it replays.
	d.record(srcAddr, xid, body, []byte{1, 2, 3, 4})
	res, reply := d.lookup(srcAddr, xid, body)
	if res != drcReplay || string(reply) != string([]byte{1, 2, 3, 4}) {
		t.Fatalf("post-completion lookup = (%v,%v), want replay of recorded reply", res, reply)
	}
}

// TestDRC_TTLEviction proves a DONE entry past the TTL is treated as a fresh
// request (no replay), using an injected clock.
func TestDRC_TTLEviction(t *testing.T) {
	d := newDuplicateRequestCache()
	now := time.Unix(1000, 0)
	d.now = func() time.Time { return now }

	const srcAddr = "10.0.0.4:4040"
	const xid = uint32(7)
	body := []byte("remove(...)")

	d.lookup(srcAddr, xid, body)
	d.record(srcAddr, xid, body, []byte{0, 0, 0, 0})

	// Within TTL: replays.
	now = now.Add(d.ttl - time.Nanosecond)
	if res, _ := d.lookup(srcAddr, xid, body); res != drcReplay {
		t.Fatalf("within TTL lookup = %v, want drcReplay", res)
	}
	// Past TTL: fresh request (miss).
	now = now.Add(2 * d.ttl)
	if res, _ := d.lookup(srcAddr, xid, body); res != drcMiss {
		t.Fatalf("past-TTL lookup = %v, want drcMiss", res)
	}
}

// TestDRC_CapEviction proves the entry cap is enforced (no unbounded growth).
func TestDRC_CapEviction(t *testing.T) {
	d := newDuplicateRequestCache()
	d.maxEntries = 8
	now := time.Unix(2000, 0)
	d.now = func() time.Time { return now }

	for i := 0; i < 100; i++ {
		now = now.Add(time.Millisecond) // distinct ages for oldest-eviction
		body := []byte{byte(i), byte(i >> 8)}
		d.lookup("c:1", uint32(i), body)
		d.record("c:1", uint32(i), body, []byte{0})
	}

	d.mu.Lock()
	n := len(d.entries)
	d.mu.Unlock()
	if n > d.maxEntries {
		t.Fatalf("cache holds %d entries, exceeds cap %d", n, d.maxEntries)
	}
}

// TestDRC_ConcurrentAccess exercises the cache under -race with many goroutines
// hitting overlapping and distinct keys.
func TestDRC_ConcurrentAccess(t *testing.T) {
	d := newDuplicateRequestCache()
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				xid := uint32((g*500 + i) % 64) // forces key collisions
				body := []byte{byte(i), byte(g)}
				if res, _ := d.lookup("c:1", xid, body); res == drcMiss {
					d.record("c:1", xid, body, []byte{byte(i)})
				}
			}
		}(g)
	}
	wg.Wait()
}
