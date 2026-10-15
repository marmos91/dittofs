package kvstore

import (
	"crypto/rand"
	"encoding/binary"
	"sync/atomic"
)

// processID stands for the writer's node ID and epoch in the unique part of
// a delta key: drawn once per process, so two processes, or two runs of one,
// never share it.
//
// ponytail: random, since the store has no node identity yet. Put the
// node's ID and epoch here once a node is registered at start, as the
// format has it.
var processID = func() (p [24]byte) {
	_, _ = rand.Read(p[:])
	return p
}()

var uniqueSeq atomic.Uint64

// nextUnique is a delta key's unique part: the process's ID and the next of
// its sequence, unique without a system call per delta.
func nextUnique() (u [32]byte) {
	copy(u[:], processID[:])
	binary.BigEndian.PutUint64(u[24:], uniqueSeq.Add(1))
	return u
}
