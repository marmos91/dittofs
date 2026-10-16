package kvstore

import (
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv/codec"
)

func encodeClient(c entity.Client) []byte {
	w := codec.NewWriter(v1).Uint8(uint8(c.Protocol)).Bytes(c.Owner).Fixed(c.Verifier[:])
	putPrincipal(w, c.Principal).Bool(c.MachCred != nil)
	if m := c.MachCred; m != nil {
		putPrincipal(w, m.Principal).Bytes(m.Ops)
	}
	return w.Uint32(c.CSSeq).Bytes(c.CSReply).Bool(c.Revoked).Encode()
}

func decodeClient(id entity.ClientID, b []byte) (entity.Client, error) {
	r, err := open(b)
	if err != nil {
		return entity.Client{}, err
	}
	c := entity.Client{ID: id, Protocol: entity.Protocol(r.Uint8()), Owner: r.Bytes()}
	copy(c.Verifier[:], r.Fixed(8))
	c.Principal = readPrincipal(r)
	if r.Bool() {
		c.MachCred = &entity.MachCred{Principal: readPrincipal(r), Ops: r.Bytes()}
	}
	c.CSSeq, c.CSReply, c.Revoked = r.Uint32(), r.Bytes(), r.Bool()
	return c, r.Done()
}

func encodeClientShard(s entity.ClientShard) []byte {
	return codec.NewWriter(v1).Uint32(s.Reclaimed).Bool(s.LossPending).Bool(s.Revoked).Bytes(s.Notify).Encode()
}

func decodeClientShard(shard uint64, b []byte) (entity.ClientShard, error) {
	r, err := open(b)
	if err != nil {
		return entity.ClientShard{}, err
	}
	s := entity.ClientShard{Shard: shard, Reclaimed: r.Uint32(), LossPending: r.Bool(), Revoked: r.Bool(), Notify: r.Bytes()}
	return s, r.Done()
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

func encodeOpen(o entity.Open) []byte {
	w := codec.NewWriter(v1).Uint64(uint64(o.Client))
	putPrincipal(w, o.Principal).Uint32(uint32(o.Access)).Bool(o.Privileged).Uint32(uint32(o.Deny)).
		Uint8(uint8(o.Durability)).Bool(o.DeleteOnClose)
	putPrincipal(w, o.DeleteBy).Bool(o.Lease != nil)
	if l := o.Lease; l != nil {
		w.Fixed(l.Key[:]).Fixed(l.ParentKey[:]).Uint8(l.State).Uint32(uint32(l.Epoch))
	}
	w.Uint8(uint8(o.Suspended)).Fixed(o.Via[:]).Uint32(o.StateSeq).Uint64(o.Nonce).Uint64(o.LossSeen).
		Uint64(o.Session.Node).Uint64(o.Session.NodeEpoch).Uint64(o.Session.ID).
		Uint8(uint8(o.Kind)).Fixed(o.CloneDst[:]).Fixed(o.CloneVersion[:]).
		Fixed(o.CreateGUID[:]).Fixed(o.AppInstance[:]).Uint64(o.AppVersion[0]).Uint64(o.AppVersion[1]).
		Int64(int64(o.Timeout)).Fixed(o.LockSeq[:]).Uint32(uint32(o.ChannelSeq))
	return w.Encode()
}

func decodeOpen(file entity.FileID, id entity.OpenID, b []byte) (entity.Open, error) {
	r, err := open(b)
	if err != nil {
		return entity.Open{}, err
	}
	o := entity.Open{ID: id, File: file, Client: entity.ClientID(r.Uint64())}
	o.Principal = readPrincipal(r)
	o.Access, o.Privileged, o.Deny = entity.AccessMask(r.Uint32()), r.Bool(), entity.AccessMask(r.Uint32())
	o.Durability, o.DeleteOnClose = entity.Durability(r.Uint8()), r.Bool()
	o.DeleteBy = readPrincipal(r)
	if r.Bool() {
		l := &entity.Lease{}
		copy(l.Key[:], r.Fixed(16))
		copy(l.ParentKey[:], r.Fixed(16))
		l.State, l.Epoch = r.Uint8(), uint16(r.Uint32())
		o.Lease = l
	}
	o.Suspended, o.Via = entity.TimeMask(r.Uint8()), readFileID(r)
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

func encodeLock(l entity.Lock) []byte {
	return codec.NewWriter(v1).Uint64(uint64(l.Owner.Client)).Bytes(l.Owner.Owner).Uint64(uint64(l.Length)).Bool(l.Exclusive).Encode()
}

func decodeLock(o entity.OpenID, offset int64, id entity.LockID, b []byte) (entity.Lock, error) {
	r, err := open(b)
	if err != nil {
		return entity.Lock{}, err
	}
	l := entity.Lock{ID: id, Open: o, Offset: offset}
	l.Owner = entity.LockOwner{Client: entity.ClientID(r.Uint64()), Owner: r.Bytes()}
	l.Length, l.Exclusive = int64(r.Uint64()), r.Bool()
	return l, r.Done()
}

func encodeDeletePending(d entity.DeletePending) []byte {
	w := codec.NewWriter(v1).Fixed(d.Parent[:]).Bytes(d.Name)
	return putPrincipal(w, d.By).Uint32(uint32(d.Access)).Bool(d.Privileged).Encode()
}

func decodeDeletePending(b []byte) (entity.DeletePending, error) {
	r, err := open(b)
	if err != nil {
		return entity.DeletePending{}, err
	}
	d := entity.DeletePending{Parent: readFileID(r), Name: r.Bytes(), By: readPrincipal(r)}
	d.Access, d.Privileged = entity.AccessMask(r.Uint32()), r.Bool()
	return d, r.Done()
}
