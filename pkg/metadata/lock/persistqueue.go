package lock

import "sync"

// Lock-store writes are ordered by ticket, not by lm.mu.
//
// Every mutation the Manager persists used to make its store call while still
// holding lm.mu. That is what made "mutex order == store order" true, and that
// ordering is the whole reason the reorder/resurrection class does not exist:
// the store can never observe a lock's PutLock after the DeleteLock that
// released it, nor an acquire that lm.mu already ordered behind a bulk delete.
//
// It also meant one store round-trip serialized the entire share. With a
// remote-backed lock store the manager ran at one lock operation per round-trip
// regardless of how many clients or cores were pushing it, because the lock
// operations queued on lm.mu behind somebody else's network call.
//
// So the store call moves out of the critical section, and the ordering moves
// onto tickets. A mutation still applies to the in-memory maps under lm.mu, but
// instead of calling the store it takes a ticket on the lane its file hashes to
// and records the call as a pending op. The op runs after lm.mu is released, in
// its lane's ticket order, and the caller does not return until its own ops have
// run — so persistence is still synchronous with respect to the client, and per
// file the store still sees mutations in exactly the order lm.mu applied them.
// What changes is that two different files' round-trips now overlap.
//
// Operations that are not scoped to one file (a client-wide bulk delete) take a
// ticket on every lane and so act as a barrier: nothing crosses them in either
// direction.
//
// Deadlock-freedom: tickets are issued under lm.mu, so if critical section A
// precedes critical section B then A's ticket precedes B's on every lane they
// share. An op only ever waits on a strictly earlier ticket, and "earlier" is
// the total order lm.mu already imposed, so the wait-for graph cannot cycle.

// ponytail: fixed 16 lanes, hashed by file. The count bounds how many store
// round-trips can be in flight for one share; make it configurable, or scale it
// off GOMAXPROCS, only if a profile shows lane collisions rather than the lock
// store itself as the limit.
const persistLaneCount = 16

// persistLane is a ticket lock. Tickets are handed out under lm.mu (next) and
// retired by the goroutine that ran the op (done); an op holds its lane for the
// whole store call, which is what keeps a lane's writes ordered and serialized.
type persistLane struct {
	mu   sync.Mutex
	cond *sync.Cond
	next uint64
	done uint64
}

func (l *persistLane) acquire(seq uint64) {
	l.mu.Lock()
	for l.done != seq {
		l.cond.Wait()
	}
	l.mu.Unlock()
}

func (l *persistLane) release() {
	l.mu.Lock()
	l.done++
	l.cond.Broadcast()
	l.mu.Unlock()
}

// persistOp is one store call together with the lane tickets it must hold to
// run. A file-scoped op holds one; a barrier holds all of them.
type persistOp struct {
	lanes []*persistLane
	seqs  []uint64
	run   func()
}

func (op persistOp) exec() {
	for i, lane := range op.lanes {
		lane.acquire(op.seqs[i])
	}
	// Retire on the way out even if the store panics: a ticket that is never
	// retired wedges its lane, and every later write to those files with it.
	defer func() {
		for _, lane := range op.lanes {
			lane.release()
		}
	}()
	op.run()
}

// laneFor maps a file key to its lane. FNV-1a over the key, inlined to avoid
// allocating a hash per persisted mutation.
func (lm *Manager) laneFor(fileKey string) *persistLane {
	h := uint32(2166136261)
	for i := 0; i < len(fileKey); i++ {
		h ^= uint32(fileKey[i])
		h *= 16777619
	}
	return &lm.persistLanes[h%persistLaneCount]
}

// enqueuePersistLocked records a store call scoped to one file, to run once
// lm.mu is released. Caller must hold lm.mu and must have already applied the
// in-memory mutation.
func (lm *Manager) enqueuePersistLocked(fileKey string, run func()) {
	lane := lm.laneFor(fileKey)
	lm.pendingPersist = append(lm.pendingPersist, persistOp{
		lanes: []*persistLane{lane},
		seqs:  []uint64{lane.next},
		run:   run,
	})
	lane.next++
}

// enqueuePersistBarrierLocked records a store call that is not scoped to a
// single file. It takes a ticket on every lane, so it runs after everything
// lm.mu ordered before it and before everything lm.mu ordered after it, across
// all files. Caller must hold lm.mu.
func (lm *Manager) enqueuePersistBarrierLocked(run func()) {
	op := persistOp{
		lanes: make([]*persistLane, persistLaneCount),
		seqs:  make([]uint64, persistLaneCount),
		run:   run,
	}
	for i := range lm.persistLanes {
		lane := &lm.persistLanes[i]
		op.lanes[i] = lane
		op.seqs[i] = lane.next
		lane.next++
	}
	lm.pendingPersist = append(lm.pendingPersist, op)
}

// unlock releases lm.mu and then runs the store calls this critical section
// queued, in ticket order. Every site that acquires lm.mu for writing must
// release it through here rather than calling lm.mu.Unlock directly, otherwise
// its persists sit in the queue until some unrelated goroutine happens to
// unlock and the caller returns before its own write reached the store.
func (lm *Manager) unlock() {
	ops := lm.pendingPersist
	lm.pendingPersist = nil
	lm.mu.Unlock()

	for _, op := range ops {
		op.exec()
	}
}
