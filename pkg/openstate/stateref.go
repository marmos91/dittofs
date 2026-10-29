package openstate

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// ShardID names the shard a file's state is held under: the store's, whose
// low 24 bits are usable, so an installation numbers at most 2^24 shards.
type ShardID = entity.ShardID

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

// revokedRef is a reference in its client's revoked set: what it named, and
// why it was revoked.
type revokedRef struct {
	client entity.ClientID
	typ    StateType
	cause  RevokeReason
}

// holder is what a state reference resolves to.
type holder struct {
	typ    StateType
	file   entity.FileID
	client entity.ClientID
	seq    uint32 // the state's current sequence; the first is 1
}

// mint issues a reference to a new piece of state of e's file, held by client
// c, under that file's step. Its token is 64 bits read from Deps.Rand, outside
// idx; a failed read mints nothing.
//
// ponytail: one mutex guards the reference index for every file, so mints and
// resolves on different files serialise on it. Split it by shard like files
// when step-wait time on unrelated files shows contention on idx.
func (t *Table) mint(e *entry, c entity.ClientID, s ShardID, typ StateType) (StateRef, error) {
	return t.mintUnder(e, c, s, typ, nil)
}

// mintUnder is mint, running under, if set, with idx held once the token is
// free and before the reference is added: an error from it adds nothing. A
// minted reference carries the low bits of its shard's grace instance.
func (t *Table) mintUnder(e *entry, c entity.ClientID, s ShardID, typ StateType, under func(StateRef) error) (StateRef, error) {
	var b [8]byte
	for {
		if _, err := io.ReadFull(t.d.Rand, b[:]); err != nil {
			return StateRef{}, fmt.Errorf("openstate: minting a reference: %w", err)
		}
		k := refKey{s, binary.LittleEndian.Uint64(b[:])}
		t.idx.Lock()
		if _, taken := t.refs[k]; taken {
			t.idx.Unlock()
			continue
		}
		inst, _ := t.instance(s)
		ref := StateRef{Type: typ, Shard: s, Instance: uint8(inst & 31), Token: k.token, Seq: 1}
		if under != nil {
			if err := under(ref); err != nil {
				t.idx.Unlock()
				return StateRef{}, err
			}
		}
		t.refs[k] = holder{typ: typ, file: e.file, client: c, seq: 1}
		t.idx.Unlock()
		e.states++
		return ref, nil
	}
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
	return t.resolveLocked(c, f, ref)
}

// resolveLocked is resolve, with idx held.
func (t *Table) resolveLocked(c entity.ClientID, f entity.FileID, ref StateRef) (holder, error) {
	k := refKey{ref.Shard, ref.Token}
	h, ok := t.refs[k]
	if r, gone := t.revoked[k]; !ok && gone {
		switch {
		case r.typ != ref.Type:
			return holder{}, ErrBadState
		case r.client != c:
			return holder{}, ErrNotYours
		}
		return holder{}, &RevokedError{r.cause}
	}
	if !ok {
		return holder{}, t.missed(ref)
	}
	if h.typ != ref.Type || (!f.IsZero() && h.file != f) {
		return holder{}, ErrBadState
	}
	if h.client != c {
		return holder{}, ErrNotYours
	}
	if err := seqErr(h.seq, ref.Seq); err != nil {
		return holder{}, err
	}
	return h, nil
}

// instance is shard s's grace instance; ok is false for a shard not begun.
func (t *Table) instance(s ShardID) (uint64, bool) {
	t.cmu.Lock()
	defer t.cmu.Unlock()
	if sh := t.shards[s]; sh != nil {
		return sh.instance, true
	}
	return 0, false
}

// missed judges a reference whose record is not held: stale when it may name
// state its shard lost, as in grace or minted under another grace instance,
// so the client reclaims; bad otherwise, as for a released, freed or forged
// one, or one of a shard not begun, which defaults to no instance.
//
// decision: a miss in a shard not begun is ErrBadState, while a minting call
// there is ErrDelay. The service begins every shard it serves before it
// answers anything, so a reference naming one it does not serve was forged or
// names a removed share's state, and a retry would never end; a minting call
// can only race a takeover. Answer ErrDelay if shards are found to move
// between nodes, where a reference can reach a node about to serve it.
func (t *Table) missed(ref StateRef) error {
	t.cmu.Lock()
	defer t.cmu.Unlock()
	sh := t.shards[ref.Shard]
	if sh != nil && (sh.inGrace(t.d.Clock.Elapsed()) || uint8(sh.instance&31) != ref.Instance) {
		return ErrStaleState
	}
	return ErrBadState
}

// seqErr judges a presented reference sequence against the state's current
// one: zero is the current one, an older one ErrOldState, a newer one
// ErrBadState.
func seqErr(cur, presented uint32) error {
	if presented == 0 || presented == cur {
		return nil
	}
	// Sequences wrap, so older is judged on the distance, not the value.
	if int32(cur-presented) > 0 {
		return ErrOldState
	}
	return ErrBadState
}
