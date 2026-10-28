package vfs

import "encoding/binary"

// The codecs of every name of state a client presents back. Each is a pure
// function shared by every adapter: decoding never touches the store, refuses
// only malformed bytes, and leaves stale against bad to the service, when the
// decoded value is used. A Handle needs none, being opaque, and a ClientID,
// a Cursor and an SMB lease key are carried on the wire as they are.

// openIDBits is the width of an OpenID: with a 24-bit shard it fills the
// persistent half of an SMB FileId.
const openIDBits = 40

// EncodeStateID spells r as an NFSv4 stateid: its seqid, big-endian, then
// the 12 bytes of other: byte 0 holds the type in its low 3 bits and the
// grace instance's low 5 bits in its high 5, bytes 1-3 the shard, big-endian,
// and bytes 4-11 the token, big-endian. The zero StateRef is the anonymous
// stateid, all zeros. r's shard is below ShardLimit, as every shard is.
func EncodeStateID(r StateRef) [16]byte {
	var b [16]byte
	binary.BigEndian.PutUint32(b[0:4], r.Seq)
	b[4] = uint8(r.Type)&7 | r.Instance<<3
	b[5], b[6], b[7] = byte(r.Shard>>16), byte(r.Shard>>8), byte(r.Shard)
	binary.BigEndian.PutUint64(b[8:16], r.Token)
	return b
}

// DecodeStateID reads a stateid as EncodeStateID spells it, its seqid as
// State.Seq. All zeros is the anonymous open. Any other stateid whose type is
// not an open, a lock state or a delegation is StateError BadState: the
// special stateids other than all zeros are the adapter's to recognise
// before decoding.
func DecodeStateID(b [16]byte) (OpenRef, error) {
	if b == ([16]byte{}) {
		return OpenRef{Kind: RefAnonymous}, nil
	}
	r := StateRef{
		Type:     StateType(b[4] & 7),
		Instance: b[4] >> 3,
		Shard:    ShardID(b[5])<<16 | ShardID(b[6])<<8 | ShardID(b[7]),
		Token:    binary.BigEndian.Uint64(b[8:16]),
		Seq:      binary.BigEndian.Uint32(b[0:4]),
	}
	switch r.Type {
	case StateOpen:
		return OpenRef{Kind: RefOpened, State: r}, nil
	case StateLock:
		return OpenRef{Kind: RefLocked, State: r}, nil
	case StateGrant:
		return OpenRef{Kind: RefDelegated, State: r}, nil
	}
	return OpenRef{}, &StateError{Kind: StateBadState}
}

// EncodeFileID spells an SMB open as its FileId's persistent half: the
// shard in the high 24 bits, the OpenID in the low 40. ref's shard is below
// ShardLimit and its OpenID below 2^40, as every one is.
func EncodeFileID(ref OpenRef) uint64 {
	return uint64(ref.State.Shard)<<openIDBits | uint64(ref.Open)&(1<<openIDBits-1)
}

// DecodeFileID reads a FileId's persistent half as EncodeFileID spells it.
// One naming OpenID zero, which no SMB open has, is StateError BadState.
//
// decision: the persistent half has no room for grace-instance bits, so the
// decoded State.Instance is always zero and a FileId cannot say which grace
// instance its open was minted under. Take bits from the shard or the OpenID
// if a missing open's FileId must be told stale from bad by instance.
func DecodeFileID(persistent uint64) (OpenRef, error) {
	id := OpenID(persistent & (1<<openIDBits - 1))
	if id == 0 {
		return OpenRef{}, &StateError{Kind: StateBadState}
	}
	return OpenRef{Kind: RefOpened, State: StateRef{Shard: ShardID(persistent >> openIDBits)}, Open: id}, nil
}
