package kvstore

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// ErrLimits is a backend whose limits cannot hold a record this store
// writes at its worst case, or a batch of one item.
var ErrLimits = errors.New("kvstore: backend limits too small")

// maxCarried is the most chunks one block may carry, the block format's cap.
const maxCarried = 1024

// cost is one key's worst case: its encoded key, its value, and the
// conflict range the backend charges for reading or guarding the key, which
// is the key again.
type cost struct{ key, value int }

func (c cost) bytes() int { return 2*c.key + c.value }

func keyCost(key, value []byte) cost { return cost{len(key), len(value)} }

// worstCosts are the worst cases of every key kind a batch writes, each from
// the real encoders at their largest values.
func worstCosts() (perKey []cost, fixed []cost) {
	var (
		share = entity.ShareID{}
		file  = entity.FileID{}
		ns    = entity.NamespaceID{}
		h     = entity.ChunkHash{}
		name  = entity.BlockName{}
		big   = int64(math.MaxInt64)
		v     = entity.VersionOf(math.MaxUint64)
	)
	carried := make([]entity.Carried, maxCarried)
	block := entity.Block{State: entity.BlockRetired, NotBefore: time.Unix(1<<40, 0), DeadAt: time.Unix(1<<40, 0),
		Live: big, Dead: big, Size: big, Carried: carried}
	ref := entity.ChunkRef{Length: big, Skip: big, Oldest: v, Newest: v, Born: math.MaxUint64, NSGen: math.MaxUint32}
	perKey = []cost{
		keyCost(refKey(share, file, big), encodeRef(ref)),
		keyCost(reverseKey(ns, h, share, file, big, math.MaxUint64), encodeEmpty()),
		keyCost(chunkKey(ns, h), encodeChunk(chunkRecord{Chunk: entity.Chunk{Position: big, Length: big, Refcount: big}, Stamp: math.MaxUint64})),
		keyCost(blockKey(ns, name), encodeBlock(block)),
		keyCost(retiredKey(ns, time.Unix(1<<40, 0), name), encodeEmpty()),
		keyCost(deletedKey(ns, name), encodeDeletedAt(time.Unix(1<<40, 0))),
		keyCost(overwriteKey(share, file, big), encodeOverwrite(overwrite{Extent: entity.Extent{Offset: big}, Version: v, Born: math.MaxUint64})),
		keyCost(holeKey(share, file, big), encodeHole(hole{Extent: entity.Extent{Offset: big}, Version: v})),
	}
	removal := entity.Removal{Version: v, Extent: entity.Extent{Offset: big, Length: 0}, Kind: entity.RemoveClone, Cut: math.MaxUint64,
		Cursor: big, Clone: &entity.CloneSpec{SrcOff: big, DstOff: big, Len: big, AsOf: v}}
	fixed = []cost{
		keyCost(removalKey(share, file, v), encodeRemoval(removal)),
		keyCost(existenceFenceKey(share, file), encodeFence(fence{math.MaxUint64, math.MaxUint64})),
		keyCost(offloadFenceKey(share, file), encodeFence(fence{math.MaxUint64, math.MaxUint64})),
		{key: len(fileKey(share, file)), value: 1 << 10}, // the File record a batch may rewrite
	}
	return perKey, fixed
}

// keyBudget is K: the most keys one batch may write or delete, from the
// limits the backend reports, never configured.
//
//	K = min(⌊(T − F) / b⌋, E − e)
//
// with T and E the transaction's byte and entry limits, b the worst cost of
// one key, F and e the bytes and entries of a batch's fixed records. It
// refuses a backend whose key or value limit is below a record's worst case.
func keyBudget(l kv.TxnLimits) (int, error) {
	perKey, fixed := worstCosts()
	b, F := 0, 0
	for _, c := range perKey {
		if c.key > l.Key || c.value > l.Value {
			return 0, fmt.Errorf("%w: a record of %d+%d bytes past the key or value limit", ErrLimits, c.key, c.value)
		}
		b = max(b, c.bytes())
	}
	for _, c := range fixed {
		if c.key > l.Key || c.value > l.Value {
			return 0, fmt.Errorf("%w: a record of %d+%d bytes past the key or value limit", ErrLimits, c.key, c.value)
		}
		F += c.bytes()
	}
	k := (l.TxnBytes - F) / b
	if l.TxnEntries > 0 {
		k = min(k, l.TxnEntries-len(fixed))
	}
	// The smallest item, one ref piece under one overwrite record piece,
	// must fit, or no batch makes progress.
	if k < minItemKeys {
		return 0, fmt.Errorf("%w: a batch holds %d keys, an item needs %d", ErrLimits, k, minItemKeys)
	}
	return k, nil
}

// minItemKeys is the keys of the smallest batch item: a ref dropped, its
// reverse key, its chunk record, its block record with the block's two GC
// index keys, and one overwrite record over it.
const minItemKeys = 7
