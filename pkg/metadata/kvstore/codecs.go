package kvstore

import (
	"errors"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv/codec"
)

// Every control-plane codec is at version 1. A field the key carries is not
// repeated in the value: the decoders take it from the caller.
const v1 = 1

// errMalformed is a value whose fields decode but name no valid choice.
var errMalformed = errors.New("kvstore: malformed value")

func open(b []byte) (*codec.Reader, error) {
	r, _, err := codec.NewReader(b, v1)
	return r, err
}

func putPrincipal(w *codec.Writer, p entity.Principal) *codec.Writer {
	return w.Uint8(uint8(p.Kind)).Fixed(p.ID[:])
}

func readPrincipal(r *codec.Reader) entity.Principal {
	p := entity.Principal{Kind: entity.PrincipalKind(r.Uint8())}
	copy(p.ID[:], r.Fixed(16))
	return p
}

func putIDs(w *codec.Writer, ids entity.ProtocolIDs) {
	w.Len(len(ids))
	for _, id := range ids {
		w.Uint8(uint8(id.Scheme)).String(id.Value)
	}
}

func readIDs(r *codec.Reader) entity.ProtocolIDs {
	n := r.Len()
	if n == 0 {
		return nil
	}
	ids := make(entity.ProtocolIDs, n)
	for i := range ids {
		ids[i] = entity.ProtocolID{Scheme: entity.ProtocolScheme(r.Uint8()), Value: r.String()}
	}
	return ids
}

func encodeUser(u entity.User) []byte {
	w := codec.NewWriter(v1).String(u.Name)
	putIDs(w, u.IDs)
	return putPrincipal(w, u.Primary).Bool(u.Disabled).Fixed(u.Secret[:]).Encode()
}

func decodeUser(id entity.PrincipalID, b []byte) (entity.User, error) {
	r, err := open(b)
	if err != nil {
		return entity.User{}, err
	}
	u := entity.User{Principal: entity.Principal{Kind: entity.PrincipalUser, ID: id}, Name: r.String(), IDs: readIDs(r)}
	u.Primary = readPrincipal(r)
	u.Disabled = r.Bool()
	copy(u.Secret[:], r.Fixed(16))
	return u, r.Done()
}

func encodeGroup(g entity.Group) []byte {
	w := codec.NewWriter(v1).String(g.Name)
	putIDs(w, g.IDs)
	return w.Encode()
}

func decodeGroup(id entity.PrincipalID, b []byte) (entity.Group, error) {
	r, err := open(b)
	if err != nil {
		return entity.Group{}, err
	}
	g := entity.Group{Principal: entity.Principal{Kind: entity.PrincipalGroup, ID: id}, Name: r.String(), IDs: readIDs(r)}
	return g, r.Done()
}

// A membership row's value is the member's kind, so a walk knows which
// members are nested groups without reading them. Both directions carry it.
func encodeKind(k entity.PrincipalKind) []byte { return codec.NewWriter(v1).Uint8(uint8(k)).Encode() }

func decodeKind(b []byte) (entity.PrincipalKind, error) {
	r, err := open(b)
	if err != nil {
		return 0, err
	}
	k := entity.PrincipalKind(r.Uint8())
	return k, r.Done()
}

// encodeID is a name or path index row's value: the ID it names.
func encodeID(id [16]byte) []byte { return codec.NewWriter(v1).Fixed(id[:]).Encode() }

func decodeID(b []byte) ([16]byte, error) {
	var id [16]byte
	r, err := open(b)
	if err != nil {
		return id, err
	}
	copy(id[:], r.Fixed(16))
	return id, r.Done()
}

// encodePrincipal is a protocol-ID row's value: a SID names a user or a
// group, so the row carries the kind as well as the ID.
func encodePrincipal(p entity.Principal) []byte { return putPrincipal(codec.NewWriter(v1), p).Encode() }

func decodePrincipal(b []byte) (entity.Principal, error) {
	r, err := open(b)
	if err != nil {
		return entity.Principal{}, err
	}
	p := readPrincipal(r)
	return p, r.Done()
}

// encodeShareInfo writes the fields a share put owns; State is its own record.
func encodeShareInfo(s entity.Share) []byte {
	return codec.NewWriter(v1).String(s.Name).String(s.Path).Fixed(s.Root[:]).Encode()
}

func decodeShareInfo(id entity.ShareID, b []byte) (entity.Share, error) {
	r, err := open(b)
	if err != nil {
		return entity.Share{}, err
	}
	s := entity.Share{ID: id, Name: r.String(), Path: r.String()}
	copy(s.Root[:], r.Fixed(16))
	return s, r.Done()
}

func encodeState(s entity.ShareState) []byte { return codec.NewWriter(v1).Uint8(uint8(s)).Encode() }

func decodeState(b []byte) (entity.ShareState, error) {
	r, err := open(b)
	if err != nil {
		return 0, err
	}
	s := entity.ShareState(r.Uint8())
	return s, r.Done()
}

func encodeGrant(g entity.ShareGrant) []byte {
	return codec.NewWriter(v1).Uint8(uint8(g.Principal.Kind)).Uint8(uint8(g.Access)).Uint64(g.Version).Encode()
}

func decodeGrant(share entity.ShareID, p entity.PrincipalID, b []byte) (entity.ShareGrant, error) {
	r, err := open(b)
	if err != nil {
		return entity.ShareGrant{}, err
	}
	g := entity.ShareGrant{Share: share, Principal: entity.Principal{Kind: entity.PrincipalKind(r.Uint8()), ID: p}}
	g.Access = entity.ShareAccess(r.Uint8())
	g.Version = r.Uint64()
	return g, r.Done()
}

// Client match tags.
const (
	matchCIDR     = 1
	matchNetgroup = 2
)

func encodePolicy(p entity.ExportPolicy) []byte {
	w := codec.NewWriter(v1).Len(len(p.Flavors))
	for _, f := range p.Flavors {
		w.Uint8(uint8(f))
	}
	w.Uint8(uint8(p.MinKerberos)).Len(len(p.Clients))
	for _, c := range p.Clients {
		if c.Match.CIDR.IsValid() {
			b, _ := c.Match.CIDR.MarshalBinary()
			w.Uint8(matchCIDR).Bytes(b)
		} else {
			w.Uint8(matchNetgroup).String(c.Match.Netgroup)
		}
		w.Uint8(uint8(c.Access))
	}
	w.Uint8(uint8(p.Squash))
	return putPrincipal(w, p.Anonymous).Uint8(uint8(p.SMB)).Uint64(p.Version).Encode()
}

func decodePolicy(share entity.ShareID, b []byte) (entity.ExportPolicy, error) {
	r, err := open(b)
	if err != nil {
		return entity.ExportPolicy{}, err
	}
	p := entity.ExportPolicy{Share: share}
	if n := r.Len(); n > 0 {
		p.Flavors = make([]entity.AuthFlavor, n)
		for i := range p.Flavors {
			p.Flavors[i] = entity.AuthFlavor(r.Uint8())
		}
	}
	p.MinKerberos = entity.KerberosLevel(r.Uint8())
	if n := r.Len(); n > 0 {
		p.Clients = make([]entity.ClientRule, n)
		for i := range p.Clients {
			c := &p.Clients[i]
			switch r.Uint8() {
			case matchCIDR:
				if err := c.Match.CIDR.UnmarshalBinary(r.Bytes()); err != nil && r.Done() == nil {
					return entity.ExportPolicy{}, errMalformed
				}
			case matchNetgroup:
				c.Match.Netgroup = r.String()
			default:
				if r.Done() == nil {
					return entity.ExportPolicy{}, errMalformed
				}
				return entity.ExportPolicy{}, r.Done()
			}
			c.Access = entity.ShareAccess(r.Uint8())
		}
	}
	p.Squash = entity.Squash(r.Uint8())
	p.Anonymous = readPrincipal(r)
	p.SMB = entity.SMBShareFlags(r.Uint8())
	p.Version = r.Uint64()
	return p, r.Done()
}

func encodeNetgroup(g entity.Netgroup) []byte {
	w := codec.NewWriter(v1).Len(len(g.Members))
	for _, m := range g.Members {
		w.String(m)
	}
	return w.Uint64(g.Version).Encode()
}

func decodeNetgroup(name string, b []byte) (entity.Netgroup, error) {
	r, err := open(b)
	if err != nil {
		return entity.Netgroup{}, err
	}
	g := entity.Netgroup{Name: name}
	if n := r.Len(); n > 0 {
		g.Members = make([]string, n)
		for i := range g.Members {
			g.Members[i] = r.String()
		}
	}
	g.Version = r.Uint64()
	return g, r.Done()
}

func encodeInstallation(in entity.Installation) []byte {
	return codec.NewWriter(v1).Fixed(in.ID[:]).Fixed(in.Instance[:]).Bytes(in.Generation).Encode()
}

func decodeInstallation(b []byte) (entity.Installation, error) {
	r, err := open(b)
	if err != nil {
		return entity.Installation{}, err
	}
	var in entity.Installation
	copy(in.ID[:], r.Fixed(16))
	copy(in.Instance[:], r.Fixed(16))
	if g := r.Bytes(); len(g) > 0 {
		in.Generation = append([]byte(nil), g...)
	}
	return in, r.Done()
}

// encodeCounter is ShareList's and the settings generation's value.
func encodeCounter(v uint64) []byte { return codec.NewWriter(v1).Uint64(v).Encode() }

func decodeCounter(b []byte) (uint64, error) {
	r, err := open(b)
	if err != nil {
		return 0, err
	}
	v := r.Uint64()
	return v, r.Done()
}

func encodeSetting(s entity.Setting) []byte {
	return codec.NewWriter(v1).Bytes(s.Value).Uint64(s.Generation).Uint32(s.Schema).Encode()
}

func decodeSetting(scope entity.SettingScope, key string, b []byte) (entity.Setting, error) {
	r, err := open(b)
	if err != nil {
		return entity.Setting{}, err
	}
	s := entity.Setting{Scope: scope, Key: key}
	if v := r.Bytes(); len(v) > 0 {
		s.Value = append([]byte(nil), v...)
	}
	s.Generation = r.Uint64()
	s.Schema = r.Uint32()
	return s, r.Done()
}
