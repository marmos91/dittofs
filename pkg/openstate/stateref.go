package openstate

import (
	"crypto/rand"
	"encoding/binary"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// ShardID names the shard a file's state is held under.
type ShardID uint64

// StateType is what a state reference names.
type StateType uint8

const (
	StateOpen StateType = iota + 1
	StateLock
	StateGrant
)

// StateRef names one piece of state without a file handle: its type, its
// shard, the low bits of the grace instance it was minted under, and a token
// of 64 random bits unique within the shard. It resolves by shard, then token.
// A client that sees one cannot derive another's, so it is a bearer ID with
// room: it carries no counter.
type StateRef struct {
	Type     StateType
	Shard    ShardID
	Instance uint8
	Token    uint64
}

type refKey struct {
	shard ShardID
	token uint64
}

// holder is what a state reference resolves to.
type holder struct {
	typ    StateType
	file   entity.FileID
	client entity.ClientID
}

// shardGrace is a shard's grace state, as a miss is judged against it. Set
// when the shard enters and leaves grace.
type shardGrace struct {
	instance uint8
	inGrace  bool
}

// mint issues a reference to a new piece of state of file f, held by client
// c, under file f's step, which e is.
func (t *Table) mint(e *entry, f entity.FileID, c entity.ClientID, s ShardID, typ StateType) StateRef {
	t.idx.Lock()
	defer t.idx.Unlock()
	ref := StateRef{Type: typ, Shard: s, Instance: t.shards[s].instance}
	for {
		var b [8]byte
		_, _ = rand.Read(b[:]) // never fails: crypto/rand aborts the process instead
		ref.Token = binary.LittleEndian.Uint64(b[:])
		if _, taken := t.refs[refKey{s, ref.Token}]; !taken {
			break
		}
	}
	t.refs[refKey{s, ref.Token}] = holder{typ: typ, file: f, client: c}
	e.states++
	return ref
}

// release drops the piece of state ref names, under its file's step, which e
// is. The reference then resolves as a miss.
func (t *Table) release(e *entry, ref StateRef) {
	t.idx.Lock()
	defer t.idx.Unlock()
	if _, ok := t.refs[refKey{ref.Shard, ref.Token}]; ok {
		delete(t.refs, refKey{ref.Shard, ref.Token})
		e.states--
	}
}

// resolve finds the state ref names, for client c. A non-zero f is the file
// the request's handle names, and must be the state's. Every call names its
// client and only that client's state.
func (t *Table) resolve(c entity.ClientID, f entity.FileID, ref StateRef) (holder, error) {
	t.idx.Lock()
	defer t.idx.Unlock()
	h, ok := t.refs[refKey{ref.Shard, ref.Token}]
	if !ok || h.typ != ref.Type || ref.Instance != t.shards[ref.Shard].instance {
		return holder{}, t.miss(ref)
	}
	if !f.IsZero() && h.file != f {
		return holder{}, ErrBadState
	}
	if h.client != c {
		return holder{}, ErrNotYours
	}
	return h, nil
}

// miss judges a reference that names no record. It is stale when it may name
// state the shard lost, so the client reclaims: while the shard is in grace,
// or when it was minted under an earlier grace instance. Any other miss is
// bad: released state, a removed share's and a forged reference resolve to
// nothing alike, since nothing maps a released reference back to its client.
// The caller holds idx.
func (t *Table) miss(ref StateRef) error {
	g := t.shards[ref.Shard]
	if g.inGrace || ref.Instance != g.instance {
		return ErrStaleState
	}
	return ErrBadState
}
