package openstate

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// ShardID names the shard a file's state is held under. 24 of its bits are
// usable, so an installation numbers at most 2^24 shards.
type ShardID uint32

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
// The table keeps no grace instance, so Instance is minted zero and not read.
// A client that sees one cannot derive another's, so it is a bearer ID with
// room: it carries no counter. Seq is the sequence presented with it, judged
// against the state's own: zero is the current one, an older one ErrOldState,
// a newer one ErrBadState.
type StateRef struct {
	Type     StateType
	Shard    ShardID
	Instance uint8
	Token    uint64
	Seq      uint32
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
	seq    uint32 // the state's current sequence; the first is 1
}

// mint issues a reference to a new piece of state of e's file, held by client
// c, under that file's step. Its token is 64 bits read from
// Deps.Rand; a failed read mints nothing.
func (t *Table) mint(e *entry, c entity.ClientID, s ShardID, typ StateType) (StateRef, error) {
	t.idx.Lock()
	defer t.idx.Unlock()
	ref := StateRef{Type: typ, Shard: s}
	for {
		var b [8]byte
		if _, err := io.ReadFull(t.d.Rand, b[:]); err != nil {
			return StateRef{}, fmt.Errorf("openstate: minting a reference: %w", err)
		}
		ref.Token = binary.LittleEndian.Uint64(b[:])
		if _, taken := t.refs[refKey{s, ref.Token}]; !taken {
			break
		}
	}
	t.refs[refKey{s, ref.Token}] = holder{typ: typ, file: e.file, client: c, seq: 1}
	e.states++
	ref.Seq = 1
	return ref, nil
}

// advance raises the sequence of the state ref names, under its file's step,
// which e must be, and returns the new one. It runs in the step that applies an
// upgrade, a downgrade, a close or a lock change. It wraps from the highest
// value to 1, never to zero, which means the current one. A reference that
// names no record is ErrBadState, and adds none.
func (t *Table) advance(e *entry, ref StateRef) (uint32, error) {
	t.idx.Lock()
	defer t.idx.Unlock()
	k := refKey{ref.Shard, ref.Token}
	h, ok := t.refs[k]
	if !ok || h.file != e.file {
		return 0, ErrBadState
	}
	if h.seq++; h.seq == 0 {
		h.seq = 1
	}
	t.refs[k] = h
	return h.seq, nil
}

// release drops the piece of state ref names, under its file's step, which e
// must be: a reference to another file's state releases nothing. The
// reference then resolves as a miss.
func (t *Table) release(e *entry, ref StateRef) {
	t.idx.Lock()
	defer t.idx.Unlock()
	if h, ok := t.refs[refKey{ref.Shard, ref.Token}]; ok && h.file == e.file {
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
	if !ok {
		// decision: a miss is always ErrBadState, never stale: the
		// table keeps no grace state, so no miss can be told to name state a
		// shard lost rather than released or forged state. Judge a miss
		// against the shard's grace instance once the table records one.
		return holder{}, ErrBadState
	}
	if h.typ != ref.Type || (!f.IsZero() && h.file != f) {
		return holder{}, ErrBadState
	}
	if h.client != c {
		return holder{}, ErrNotYours
	}
	if ref.Seq != 0 && ref.Seq != h.seq {
		// Sequences wrap, so older is judged on the distance, not the value.
		if int32(h.seq-ref.Seq) > 0 {
			return holder{}, ErrOldState
		}
		return holder{}, ErrBadState
	}
	return h, nil
}
