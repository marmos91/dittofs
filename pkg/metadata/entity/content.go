package entity

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"math"
	"time"
)

// Content identities.
type (
	// ChunkHash is a keyed hash of a chunk's plaintext under its namespace's
	// chunk-ID key. The zero hash names no chunk: a zero ref's bytes are all
	// zeros.
	ChunkHash [32]byte
	// BlockName names one remote object, minted once per put attempt.
	BlockName [32]byte
	// SnapshotCut numbers a share's snapshots in order; 0 is before the first.
	SnapshotCut uint64
	// NamespaceID names a remote key namespace: the partition every content
	// record is counted in.
	NamespaceID [16]byte
)

func (h ChunkHash) IsZero() bool     { return h == ChunkHash{} }
func (h ChunkHash) String() string   { return hex.EncodeToString(h[:]) }
func (n BlockName) String() string   { return hex.EncodeToString(n[:]) }
func (n NamespaceID) IsZero() bool   { return n == NamespaceID{} }
func NewNamespaceID() NamespaceID    { return newID() }
func (n NamespaceID) String() string { return FileID(n).String() }

// JournalVersion orders a file's content writes: 128 bits, an epoch and then
// a counter, compared as one unsigned number. The epoch is zero in this
// format.
type JournalVersion [16]byte

// VersionOf is the version with counter n and a zero epoch.
func VersionOf(n uint64) JournalVersion {
	var v JournalVersion
	binary.BigEndian.PutUint64(v[8:], n)
	return v
}

func (v JournalVersion) Epoch() uint64   { return binary.BigEndian.Uint64(v[:8]) }
func (v JournalVersion) Counter() uint64 { return binary.BigEndian.Uint64(v[8:]) }
func (v JournalVersion) IsZero() bool    { return v == JournalVersion{} }

// Compare orders two versions as unsigned 128-bit numbers.
func (v JournalVersion) Compare(o JournalVersion) int { return bytes.Compare(v[:], o[:]) }

func (v JournalVersion) Less(o JournalVersion) bool { return v.Compare(o) < 0 }

func MaxVersion(a, b JournalVersion) JournalVersion {
	if a.Less(b) {
		return b
	}
	return a
}

// NamespaceGen is one namespace a share's refs are counted in, and its
// generation: a share has one outside a re-home and two during one.
type NamespaceGen struct {
	Generation uint32
	Namespace  NamespaceID
}

// Extent is a run of bytes [Offset, Offset+Length).
type Extent struct {
	Offset, Length int64
}

// EndOfFile is the end of an extent that runs to the end of the file: a
// truncate's or a release's removal.
const EndOfFile int64 = math.MaxInt64

func (e Extent) End() int64 {
	if e.Length > EndOfFile-e.Offset {
		return EndOfFile
	}
	return e.Offset + e.Length
}

func (e Extent) IsEmpty() bool { return e.Length <= 0 }

func (e Extent) Overlaps(o Extent) bool {
	return !e.IsEmpty() && !o.IsEmpty() && e.Offset < o.End() && o.Offset < e.End()
}

func (e Extent) Contains(o Extent) bool {
	return !o.IsEmpty() && e.Offset <= o.Offset && o.End() <= e.End()
}

// Intersect is the part of e that o covers; empty when they do not overlap.
func (e Extent) Intersect(o Extent) Extent {
	start, end := max(e.Offset, o.Offset), min(e.End(), o.End())
	if end <= start {
		return Extent{Offset: start}
	}
	return Extent{Offset: start, Length: end - start}
}

// ExtentOf is the extent [start, end).
func ExtentOf(start, end int64) Extent { return Extent{Offset: start, Length: end - start} }

// ChunkRef says these bytes of this file are that run of that chunk's bytes:
// [Skip, Skip+Length) of Chunk, at Offset in the file. A zero Chunk is a zero
// ref, all zeros, never counted. Died is zero for a live ref; a ref a snapshot
// still sees after it was superseded has Died set to its successor's Born.
type ChunkRef struct {
	File           FileID
	Offset, Length int64
	Chunk          ChunkHash
	Skip           int64
	Oldest, Newest JournalVersion // the journal versions the bytes came from
	Born, Died     SnapshotCut
	NSGen          uint32 // the namespace generation the chunk is counted in
}

func (c ChunkRef) End() int64      { return c.Offset + c.Length }
func (c ChunkRef) Extent() Extent  { return Extent{Offset: c.Offset, Length: c.Length} }
func (c ChunkRef) IsLive() bool    { return c.Died == 0 }
func (c ChunkRef) IsZeroRef() bool { return c.Chunk.IsZero() }

// VisibleAt reports whether the snapshot at cut k sees this ref: born before
// k, and still live at k.
func (c ChunkRef) VisibleAt(k SnapshotCut) bool {
	return c.Born < k && (c.IsLive() || k <= c.Died)
}

// Clip is the part of the ref over e, its chunk run adjusted to match; a ref
// with zero Length when they do not overlap.
func (c ChunkRef) Clip(e Extent) ChunkRef {
	part := c.Extent().Intersect(e)
	c.Skip += part.Offset - c.Offset
	c.Offset, c.Length = part.Offset, part.Length
	return c
}

// Chunk is one content-addressed chunk and where its bytes are: the block
// that carries it, and its body's position and length there.
type Chunk struct {
	Hash     ChunkHash
	Block    BlockName
	Position int64
	Length   int64
	Refcount int64 // live refs plus history refs
}

// Block is one remote object.
type Block struct {
	Name       BlockName
	State      BlockState
	NotBefore  time.Time // store time: the earliest the deleter may delete it, set at retirement
	Live       int64     // chunk records naming it with a nonzero refcount
	Dead       int64     // bytes of carried chunks dead here
	DeadAt     time.Time // store time Dead last grew
	Size       int64     // the block's encoded bytes
	Carried    []Carried // every chunk it carries, in order; written once
	Generation uint8     // 0 from offload; one above its highest source from compaction
}

// Retired reports a block no referenced chunk record names: retired, or on
// its way past it.
func (b Block) Retired() bool { return b.Live == 0 && b.State != BlockLive }

type BlockState uint8

const (
	BlockLive BlockState = iota
	BlockRetired
	BlockDeleted
)

var blockStates = []string{BlockLive: "live", BlockRetired: "retired", BlockDeleted: "deleted"}

func (s BlockState) String() string                { return enumString(blockStates, s) }
func (s BlockState) MarshalText() ([]byte, error)  { return enumMarshal(blockStates, s) }
func (s *BlockState) UnmarshalText(b []byte) error { return enumUnmarshal(blockStates, s, b) }

// Carried is one chunk a block carries and its body's encoded length there.
type Carried struct {
	Hash   ChunkHash
	Length int64
}

// Class is what metadata says of an offset below a file's size.
type Class uint8

const (
	// Hole: never written, or deallocated. Reads as zeros.
	Hole Class = iota + 1
	// Uncarved: written, with no current chunk. Only the journal can serve
	// it; without it the offset is Lost.
	Uncarved
	// Carved: a current ref covers it.
	Carved
)

var classes = []string{Hole: "hole", Uncarved: "uncarved", Carved: "carved"}

func (c Class) String() string                { return enumString(classes, c) }
func (c Class) MarshalText() ([]byte, error)  { return enumMarshal(classes, c) }
func (c *Class) UnmarshalText(b []byte) error { return enumUnmarshal(classes, c, b) }

// Span is one run of a covering lookup or an allocation answer.
type Span struct {
	Extent
	Class     Class
	Ref       ChunkRef       // the covering ref, clipped to the span, when Carved
	Overwrite JournalVersion // when an overwrite record made it Uncarved, that record's version
	Applied   JournalVersion // the file's applied when the lookup read it
}

// RemovalKind is the operation a removal record recorded.
type RemovalKind uint8

const (
	RemoveTruncate RemovalKind = iota + 1
	RemoveDeallocate
	RemoveRelease
	RemoveClone
)

var removalKinds = []string{RemoveTruncate: "truncate", RemoveDeallocate: "deallocate", RemoveRelease: "release", RemoveClone: "clone"}

func (k RemovalKind) String() string                { return enumString(removalKinds, k) }
func (k RemovalKind) MarshalText() ([]byte, error)  { return enumMarshal(removalKinds, k) }
func (k *RemovalKind) UnmarshalText(b []byte) error { return enumUnmarshal(removalKinds, k, b) }

// Removal is an extent a truncate, deallocate, release or clone removed, at
// the journal version of that operation, and how far dropping its refs has
// got. While it is not done it masks what it has not yet dropped.
type Removal struct {
	Version JournalVersion
	Extent  Extent
	Kind    RemovalKind
	Cut     SnapshotCut // the cut its phase 1 read
	Cursor  int64       // refs below this offset are dropped
	Done    bool
	Clone   *CloneSpec // Kind RemoveClone only
}

// Masks reports whether the removal hides content at version v over e: it is
// not done, overlaps e, and is newer than v.
func (r Removal) Masks(v JournalVersion, e Extent) bool {
	return !r.Done && r.Extent.Overlaps(e) && v.Less(r.Version)
}

// CloneSpec is what a clone adopts from, carried on its destination's
// removal so the clone resumes from that record alone.
type CloneSpec struct {
	Src            FileID
	SrcOff, DstOff int64
	Len            int64
	AsOf           JournalVersion // the source journal position the source was read at
}
