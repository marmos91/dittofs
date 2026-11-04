package engine

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// instanceID is the process instance, drawn at random when the engine is
// built, so an engine built again in the same process answers other
// verifiers.
func instanceID() [16]byte {
	var id [16]byte
	_, _ = rand.Read(id[:]) // crypto/rand.Read never returns an error
	return id
}

// primary is what a binding knows of the node serving its share's shard: the
// node and its epoch. The shard's incarnation, raised each time this node or a
// journal begins serving the shard, is the binding's own, as its attach wrote
// it.
type primary struct {
	node      entity.NodeID
	nodeEpoch uint64
}

// verifier is the write verifier of a file of share b, sampled now.
//
// decision: the verifier is the first 8 bytes of a SHA-256 over the node, the
// node epoch, the process instance, the shard's incarnation and the loss
// generation of the share's journal, in that order, each at a fixed width, so
// no two inputs cancel and no input is left out: it changes exactly when one of
// them does, never with the clock. Revisit if the verifier must be decoded back
// into its inputs.
//
// ponytail: every sample reads the journal's Stats() for its loss generation,
// one call per write and per Commit. Add a journal call returning the
// generation alone if a profile shows Stats() on the write path.
func (e *Engine) verifier(b *binding) vfserr.Verifier {
	var in [16 + 8 + 16 + 8 + 8]byte
	copy(in[0:16], b.primary.node[:])
	binary.BigEndian.PutUint64(in[16:24], b.primary.nodeEpoch)
	copy(in[24:40], e.instance[:])
	binary.BigEndian.PutUint64(in[40:48], b.incarnation)
	binary.BigEndian.PutUint64(in[48:56], b.j.Stats().LossGeneration)
	sum := sha256.Sum256(in[:])
	var v vfserr.Verifier
	copy(v[:], sum[:])
	return v
}
