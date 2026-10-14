package kvstore

import (
	"fmt"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv/codec"
)

func readVersion(r *codec.Reader) (v entity.JournalVersion) {
	copy(v[:], r.Fixed(16))
	return v
}

// errEpoch is a version whose epoch half is set, which this format refuses.
var errEpoch = fmt.Errorf("%w: journal version with a nonzero epoch", errMalformed)

// checkEpochs finishes a decode, refusing any version with an epoch.
func checkEpochs(r *codec.Reader, vs ...entity.JournalVersion) error {
	for _, v := range vs {
		if v.Epoch() != 0 {
			return errEpoch
		}
	}
	return r.Done()
}

// A ref's value: the key holds its file and offset, and a live ref's died is
// zero.
func encodeRef(c entity.ChunkRef) []byte {
	return codec.NewWriter(v1).Fixed(c.Chunk[:]).Uint32(c.NSGen).Uint64(uint64(c.Skip)).Uint64(uint64(c.Length)).
		Fixed(c.Oldest[:]).Fixed(c.Newest[:]).Uint64(uint64(c.Born)).Encode()
}

func decodeRef(file entity.FileID, offset int64, b []byte) (entity.ChunkRef, error) {
	r, err := open(b)
	if err != nil {
		return entity.ChunkRef{}, err
	}
	c := entity.ChunkRef{File: file, Offset: offset}
	copy(c.Chunk[:], r.Fixed(32))
	c.NSGen = r.Uint32()
	c.Skip, c.Length = int64(r.Uint64()), int64(r.Uint64())
	c.Oldest, c.Newest = readVersion(r), readVersion(r)
	c.Born = entity.SnapshotCut(r.Uint64())
	return c, checkEpochs(r, c.Oldest, c.Newest)
}

// hole is one extent below size never written, or deallocated, the version
// of the operation that made it, which tells a removal not done the holes it
// removes from the ones made after it, and born, the cut the transaction
// that wrote it read, which dates it for snapshots. A piece cut from a hole
// keeps both.
type hole struct {
	entity.Extent
	Version entity.JournalVersion
	Born    entity.SnapshotCut
}

func encodeHole(h hole) []byte {
	return codec.NewWriter(v1).Uint64(uint64(h.End())).Fixed(h.Version[:]).Uint64(uint64(h.Born)).Encode()
}

func decodeHole(start int64, b []byte) (hole, error) {
	r, err := open(b)
	if err != nil {
		return hole{}, err
	}
	h := hole{Extent: entity.ExtentOf(start, int64(r.Uint64()))}
	h.Version = readVersion(r)
	h.Born = entity.SnapshotCut(r.Uint64())
	return h, checkEpochs(r, h.Version)
}

// overwrite is one extent a committed write replaced committed content over,
// at the newest version that did.
type overwrite struct {
	entity.Extent
	Version entity.JournalVersion
	Born    entity.SnapshotCut
}

func encodeOverwrite(o overwrite) []byte {
	return codec.NewWriter(v1).Uint64(uint64(o.End())).Fixed(o.Version[:]).Uint64(uint64(o.Born)).Encode()
}

func decodeOverwrite(start int64, b []byte) (overwrite, error) {
	r, err := open(b)
	if err != nil {
		return overwrite{}, err
	}
	o := overwrite{Extent: entity.ExtentOf(start, int64(r.Uint64()))}
	o.Version = readVersion(r)
	o.Born = entity.SnapshotCut(r.Uint64())
	return o, checkEpochs(r, o.Version)
}

// A removal's value; its version is in its key.
func encodeRemoval(m entity.Removal) []byte {
	w := codec.NewWriter(v1).Uint64(uint64(m.Extent.Offset)).Uint64(uint64(m.Extent.End())).
		Uint8(uint8(m.Kind)).Uint64(uint64(m.Cut)).Uint64(uint64(m.Cursor)).Bool(m.Done).Uint64(uint64(m.OvSpan)).Bool(m.HoleCredit)
	if m.Kind == entity.RemoveRelease {
		w.Fixed(m.FileVersion[:])
	}
	w.Bool(m.Clone != nil)
	if c := m.Clone; c != nil {
		w.Fixed(c.SrcShare[:]).Fixed(c.Src[:]).Uint64(uint64(c.SrcOff)).Uint64(uint64(c.DstOff)).Uint64(uint64(c.Len)).Fixed(c.AsOf[:])
	}
	return w.Encode()
}

func decodeRemoval(version entity.JournalVersion, b []byte) (entity.Removal, error) {
	r, err := open(b)
	if err != nil {
		return entity.Removal{}, err
	}
	m := entity.Removal{Version: version}
	start, end := int64(r.Uint64()), int64(r.Uint64())
	m.Extent = entity.ExtentOf(start, end)
	m.Kind = entity.RemovalKind(r.Uint8())
	m.Cut = entity.SnapshotCut(r.Uint64())
	m.Cursor = int64(r.Uint64())
	m.Done = r.Bool()
	m.OvSpan = int64(r.Uint64())
	m.HoleCredit = r.Bool()
	if m.Kind == entity.RemoveRelease {
		m.FileVersion = readVersion(r)
	}
	if r.Bool() {
		c := &entity.CloneSpec{SrcShare: entity.ShareID(readFileID(r)), Src: readFileID(r)}
		c.SrcOff, c.DstOff, c.Len = int64(r.Uint64()), int64(r.Uint64()), int64(r.Uint64())
		c.AsOf = readVersion(r)
		m.Clone = c
		return m, checkEpochs(r, version, c.AsOf)
	}
	return m, checkEpochs(r, version, m.FileVersion)
}

// fence is the (shard, epoch) a file's commits on one path must carry.
type fence struct{ Shard, Epoch uint64 }

func encodeFence(f fence) []byte { return codec.NewWriter(v1).Uint64(f.Shard).Uint64(f.Epoch).Encode() }

func decodeFence(b []byte) (fence, error) {
	r, err := open(b)
	if err != nil {
		return fence{}, err
	}
	f := fence{Shard: r.Uint64(), Epoch: r.Uint64()}
	return f, r.Done()
}

// shareNamespaces is a share's namespaces, each with its generation, and the
// generation new puts carve under.
type shareNamespaces struct {
	Namespaces []entity.NamespaceGen
	Write      uint32
}

func encodeNamespaces(n shareNamespaces) []byte {
	w := codec.NewWriter(v1).Len(len(n.Namespaces))
	for _, g := range n.Namespaces {
		w.Uint32(g.Generation).Fixed(g.Namespace[:])
	}
	return w.Uint32(n.Write).Encode()
}

func decodeNamespaces(b []byte) (shareNamespaces, error) {
	r, err := open(b)
	if err != nil {
		return shareNamespaces{}, err
	}
	var n shareNamespaces
	if c := r.Len(); c > 0 {
		n.Namespaces = make([]entity.NamespaceGen, c)
		for i := range n.Namespaces {
			n.Namespaces[i].Generation = r.Uint32()
			copy(n.Namespaces[i].Namespace[:], r.Fixed(16))
		}
	}
	n.Write = r.Uint32()
	return n, r.Done()
}

// A chunk's value, with its change stamp: replaced by every transaction that
// changes a ref naming the chunk, so the audit can tell it changed.
type chunkRecord struct {
	entity.Chunk
	Stamp uint64
}

func encodeChunk(c chunkRecord) []byte {
	return codec.NewWriter(v1).Fixed(c.Block[:]).Uint64(uint64(c.Position)).Uint64(uint64(c.Length)).
		Uint64(uint64(c.Refcount)).Uint64(c.Stamp).Encode()
}

func decodeChunk(h entity.ChunkHash, b []byte) (chunkRecord, error) {
	r, err := open(b)
	if err != nil {
		return chunkRecord{}, err
	}
	c := chunkRecord{Chunk: entity.Chunk{Hash: h}}
	copy(c.Block[:], r.Fixed(32))
	c.Position, c.Length, c.Refcount = int64(r.Uint64()), int64(r.Uint64()), int64(r.Uint64())
	c.Stamp = r.Uint64()
	return c, r.Done()
}

func encodeBlock(b entity.Block) []byte {
	w := codec.NewWriter(v1).Uint8(uint8(b.State))
	putTime(w, b.NotBefore)
	w.Uint64(uint64(b.Live)).Uint64(uint64(b.Dead))
	putTime(w, b.DeadAt)
	w.Uint64(uint64(b.Size)).Uint8(b.Generation).Len(len(b.Carried))
	for _, c := range b.Carried {
		w.Fixed(c.Hash[:]).Uint64(uint64(c.Length))
	}
	return w.Encode()
}

func decodeBlock(name entity.BlockName, v []byte) (entity.Block, error) {
	r, err := open(v)
	if err != nil {
		return entity.Block{}, err
	}
	b := entity.Block{Name: name, State: entity.BlockState(r.Uint8()), NotBefore: readTime(r)}
	b.Live, b.Dead = int64(r.Uint64()), int64(r.Uint64())
	b.DeadAt = readTime(r)
	b.Size = int64(r.Uint64())
	b.Generation = r.Uint8()
	if n := r.Len(); n > 0 {
		b.Carried = make([]entity.Carried, n)
		for i := range b.Carried {
			copy(b.Carried[i].Hash[:], r.Fixed(32))
			b.Carried[i].Length = int64(r.Uint64())
		}
	}
	return b, r.Done()
}

// IntentDomain is what a put intent's epoch is the epoch of.
type IntentDomain uint8

const (
	DomainShard IntentDomain = iota + 1
	DomainGC
)

// Intent is a put intent: the name may still be put and committed, under
// the epoch of its domain; for a shard, by the primary named.
type Intent struct {
	Domain    IntentDomain
	ID        uint64
	Epoch     uint64
	Node      [16]byte
	NodeEpoch uint64
}

func encodeIntent(i Intent) []byte {
	return codec.NewWriter(v1).Uint8(uint8(i.Domain)).Uint64(i.ID).Uint64(i.Epoch).Fixed(i.Node[:]).Uint64(i.NodeEpoch).Encode()
}

func decodeIntent(b []byte) (Intent, error) {
	r, err := open(b)
	if err != nil {
		return Intent{}, err
	}
	i := Intent{Domain: IntentDomain(r.Uint8()), ID: r.Uint64(), Epoch: r.Uint64()}
	copy(i.Node[:], r.Fixed(16))
	i.NodeEpoch = r.Uint64()
	return i, r.Done()
}

// encodeEmpty is the value of an index key that holds nothing but itself.
func encodeEmpty() []byte { return codec.NewWriter(v1).Encode() }

// encodeDeletedAt is a deleted-block index key's value: when the delete
// succeeded, zero while it has not.
func encodeDeletedAt(t time.Time) []byte { return putTime(codec.NewWriter(v1), t).Encode() }
