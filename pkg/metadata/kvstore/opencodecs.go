package kvstore

import (
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv/codec"
)

func encodeClient(c entity.ClientRecord) []byte {
	w := codec.NewWriter(v1).Uint8(uint8(c.Protocol)).Bytes(c.Name).Fixed(c.Boot[:])
	putPrincipal(w, c.Owner).Bool(c.Protect).Uint8(uint8(c.Recovery)).
		Fixed(c.Lease.Node[:]).Fixed(c.Reach.Callback[:]).Bool(c.Reach.Down).Len(len(c.Reach.Sessions))
	for _, n := range c.Reach.Sessions {
		w.Fixed(n[:])
	}
	return w.Bool(c.Ended).Bytes(c.Adapter).Encode()
}

// decodeClient reads a client record's value; its shard marks are keys of
// their own, which the caller adds.
func decodeClient(id entity.ClientID, b []byte) (entity.ClientRecord, error) {
	r, err := open(b)
	if err != nil {
		return entity.ClientRecord{}, err
	}
	c := entity.ClientRecord{ID: id, Protocol: entity.Protocol(r.Uint8()), Name: r.Bytes()}
	copy(c.Boot[:], r.Fixed(8))
	c.Owner, c.Protect, c.Recovery = readPrincipal(r), r.Bool(), entity.RecoveryKind(r.Uint8())
	copy(c.Lease.Node[:], r.Fixed(16))
	copy(c.Reach.Callback[:], r.Fixed(16))
	c.Reach.Down = r.Bool()
	if n := r.Len(); n > 0 {
		c.Reach.Sessions = make([]entity.NodeID, n)
		for i := range c.Reach.Sessions {
			copy(c.Reach.Sessions[i][:], r.Fixed(16))
		}
	}
	c.Ended, c.Adapter = r.Bool(), r.Bytes()
	return c, r.Done()
}

func encodeClientID(id entity.ClientID) []byte {
	return codec.NewWriter(v1).Uint64(uint64(id)).Encode()
}

func decodeClientID(b []byte) (entity.ClientID, error) {
	r, err := open(b)
	if err != nil {
		return 0, err
	}
	id := entity.ClientID(r.Uint64())
	return id, r.Done()
}

func encodeOpen(o entity.OpenRecord) []byte {
	w := codec.NewWriter(v1).Uint64(uint64(o.Client))
	putPrincipal(w, o.Principal).Uint32(uint32(o.Access)).Bool(o.Privileged).Uint32(uint32(o.Deny)).
		Uint8(uint8(o.Durability)).Bool(o.DeleteOnClose)
	putPrincipal(w, o.DeleteBy).Bool(o.Lease != nil)
	if l := o.Lease; l != nil {
		w.Fixed(l.Key[:]).Fixed(l.ParentKey[:]).Uint8(l.State).Uint32(uint32(l.Epoch)).Bool(l.V2)
	}
	w.Uint8(uint8(o.Suspended)).Fixed(o.Via[:]).Bytes(o.Name).Uint32(o.StateSeq).Uint64(o.Nonce).Uint64(o.LossSeen).
		Uint64(o.Session.Node).Uint64(o.Session.NodeEpoch).Uint64(o.Session.ID).
		Uint8(uint8(o.Kind)).Fixed(o.CloneDst[:]).Fixed(o.CloneVersion[:]).
		Fixed(o.CreateGUID[:]).Fixed(o.AppInstance[:]).Uint64(o.AppVersion[0]).Uint64(o.AppVersion[1]).
		Int64(int64(o.Timeout)).Fixed(o.LockSeq[:]).Uint32(uint32(o.ChannelSeq))
	return w.Encode()
}

func decodeOpen(file entity.FileID, id entity.OpenID, b []byte) (entity.OpenRecord, error) {
	r, err := open(b)
	if err != nil {
		return entity.OpenRecord{}, err
	}
	o := entity.OpenRecord{ID: id, File: file, Client: entity.ClientID(r.Uint64())}
	o.Principal = readPrincipal(r)
	o.Access, o.Privileged, o.Deny = entity.AccessMask(r.Uint32()), r.Bool(), entity.AccessMask(r.Uint32())
	o.Durability, o.DeleteOnClose = entity.Durability(r.Uint8()), r.Bool()
	o.DeleteBy = readPrincipal(r)
	if r.Bool() {
		l := &entity.Lease{}
		copy(l.Key[:], r.Fixed(16))
		copy(l.ParentKey[:], r.Fixed(16))
		l.State, l.Epoch, l.V2 = r.Uint8(), uint16(r.Uint32()), r.Bool()
		o.Lease = l
	}
	o.Suspended, o.Via, o.Name = entity.TimeMask(r.Uint8()), readFileID(r), r.Bytes()
	o.StateSeq, o.Nonce, o.LossSeen = r.Uint32(), r.Uint64(), r.Uint64()
	o.Session = entity.SessionRef{Node: r.Uint64(), NodeEpoch: r.Uint64(), ID: r.Uint64()}
	o.Kind, o.CloneDst, o.CloneVersion = entity.OpenKind(r.Uint8()), readFileID(r), readVersion(r)
	copy(o.CreateGUID[:], r.Fixed(16))
	copy(o.AppInstance[:], r.Fixed(16))
	o.AppVersion = [2]uint64{r.Uint64(), r.Uint64()}
	o.Timeout = time.Duration(r.Int64())
	copy(o.LockSeq[:], r.Fixed(64))
	o.ChannelSeq = uint16(r.Uint32())
	return o, r.Done()
}

func encodeLock(l entity.LockRecord) []byte {
	return codec.NewWriter(v1).Uint64(uint64(l.Owner.Client)).Bytes(l.Owner.Owner).Uint64(l.Length).Bool(l.Exclusive).Encode()
}

func decodeLock(o entity.OpenID, offset uint64, id entity.LockID, b []byte) (entity.LockRecord, error) {
	r, err := open(b)
	if err != nil {
		return entity.LockRecord{}, err
	}
	l := entity.LockRecord{ID: id, Open: o, Offset: offset}
	l.Owner = entity.LockOwner{Client: entity.ClientID(r.Uint64()), Owner: r.Bytes()}
	l.Length, l.Exclusive = r.Uint64(), r.Bool()
	return l, r.Done()
}

func encodeDeletePending(d entity.DeleteRecord) []byte {
	w := codec.NewWriter(v1).Fixed(d.Parent[:]).Bytes(d.Name)
	return putPrincipal(w, d.By).Uint32(uint32(d.Access)).Bool(d.Privileged).Encode()
}

func decodeDeletePending(b []byte) (entity.DeleteRecord, error) {
	r, err := open(b)
	if err != nil {
		return entity.DeleteRecord{}, err
	}
	d := entity.DeleteRecord{Parent: readFileID(r), Name: r.Bytes(), By: readPrincipal(r)}
	d.Access, d.Privileged = entity.AccessMask(r.Uint32()), r.Bool()
	return d, r.Done()
}
