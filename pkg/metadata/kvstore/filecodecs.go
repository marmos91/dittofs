package kvstore

import (
	"math"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv/codec"
)

// A time is its Unix seconds and nanoseconds, so a client may set any time
// it can express, before 1678 included. The zero time is the minimum
// seconds value, which no client can set.
var zeroTime int64 = math.MinInt64

func putTime(w *codec.Writer, t time.Time) *codec.Writer {
	if t.IsZero() {
		return w.Uint64(uint64(zeroTime)).Uint32(0)
	}
	return w.Uint64(uint64(t.Unix())).Uint32(uint32(t.Nanosecond()))
}

func readTime(r *codec.Reader) time.Time {
	sec, nsec := int64(r.Uint64()), r.Uint32()
	if sec == zeroTime {
		return time.Time{}
	}
	return time.Unix(sec, int64(nsec)).UTC()
}

func readFileID(r *codec.Reader) (id entity.FileID) {
	copy(id[:], r.Fixed(16))
	return id
}

// encodeFile writes every field but the ID and share, which the key holds.
func encodeFile(f entity.File) []byte {
	w := codec.NewWriter(v1).Uint8(uint8(f.Type))
	putPrincipal(w, f.Owner)
	putPrincipal(w, f.Group)
	w.Uint32(f.Mode).Uint32(uint32(f.Flags))
	for _, t := range []time.Time{f.Birth, f.Access, f.Modify, f.Change} {
		putTime(w, t)
	}
	return w.Fixed(f.Version[:]).Uint64(uint64(f.Size)).Uint64(uint64(f.Charged)).Fixed(f.Applied[:]).
		Uint64(uint64(f.MaxSpan)).
		Uint32(f.Nlink).Fixed(f.Parent[:]).Uint64(f.Shard).Uint64(f.Project).Uint64(f.Number).
		Bytes(f.CreateVerifier).Bytes(f.Target).Uint32(f.Device.Major).Uint32(f.Device.Minor).
		Fixed(f.StreamOf[:]).Encode()
}

func decodeFile(share entity.ShareID, id entity.FileID, b []byte) (entity.File, error) {
	r, err := open(b)
	if err != nil {
		return entity.File{}, err
	}
	f := entity.File{ID: id, Share: share, Type: entity.FileType(r.Uint8())}
	f.Owner = readPrincipal(r)
	f.Group = readPrincipal(r)
	f.Mode = r.Uint32()
	f.Flags = entity.FileFlags(r.Uint32())
	f.Birth, f.Access, f.Modify, f.Change = readTime(r), readTime(r), readTime(r), readTime(r)
	f.Version = readVersion(r)
	f.Size = int64(r.Uint64())
	f.Charged = int64(r.Uint64())
	f.Applied = readVersion(r)
	f.MaxSpan = int64(r.Uint64())
	f.Nlink = r.Uint32()
	f.Parent = readFileID(r)
	f.Shard, f.Project, f.Number = r.Uint64(), r.Uint64(), r.Uint64()
	f.CreateVerifier = cloneOrNil(r.Bytes())
	f.Target = cloneOrNil(r.Bytes())
	f.Device = entity.DeviceID{Major: r.Uint32(), Minor: r.Uint32()}
	f.StreamOf = readFileID(r)
	return f, checkEpochs(r, f.Version, f.Applied)
}

func cloneOrNil(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return append([]byte(nil), b...)
}

// An entry's value is the name as given, its child and the child's type; the
// key holds only the folded name.
func encodeEntry(e entity.Entry) []byte {
	return codec.NewWriter(v1).Bytes(e.Name).Fixed(e.Child[:]).Uint8(uint8(e.Type)).Encode()
}

func decodeEntry(parent entity.FileID, b []byte) (entity.Entry, error) {
	r, err := open(b)
	if err != nil {
		return entity.Entry{}, err
	}
	e := entity.Entry{Parent: parent, Name: cloneOrNil(r.Bytes()), Child: readFileID(r)}
	e.Type = entity.FileType(r.Uint8())
	return e, r.Done()
}

func putACEs(w *codec.Writer, es []entity.ACE) {
	w.Len(len(es))
	for _, e := range es {
		w.Uint8(uint8(e.Type)).Uint32(uint32(e.Flags)).Uint32(uint32(e.Mask))
		putPrincipal(w, e.Who)
	}
}

func readACEs(r *codec.Reader) []entity.ACE {
	n := r.Len()
	if n == 0 {
		return nil
	}
	es := make([]entity.ACE, n)
	for i := range es {
		es[i] = entity.ACE{Type: entity.ACEType(r.Uint8()), Flags: entity.ACEFlags(r.Uint32()), Mask: entity.AccessMask(r.Uint32())}
		es[i].Who = readPrincipal(r)
	}
	return es
}

func encodeACL(a entity.ACL) []byte {
	w := codec.NewWriter(v1).Uint8(uint8(a.Flags))
	putACEs(w, a.Entries)
	putACEs(w, a.Audit)
	return w.Encode()
}

func decodeACL(b []byte) (entity.ACL, error) {
	r, err := open(b)
	if err != nil {
		return entity.ACL{}, err
	}
	a := entity.ACL{Flags: entity.ACLFlags(r.Uint8())}
	a.Entries = readACEs(r)
	a.Audit = readACEs(r)
	return a, r.Done()
}

func encodeXattr(value []byte) []byte { return codec.NewWriter(v1).Bytes(value).Encode() }

func decodeXattr(file entity.FileID, name, b []byte) (entity.Xattr, error) {
	r, err := open(b)
	if err != nil {
		return entity.Xattr{}, err
	}
	x := entity.Xattr{File: file, Name: append([]byte(nil), name...), Value: append([]byte{}, r.Bytes()...)}
	return x, r.Done()
}

// delta is one directory time change not yet folded into the directory's
// record: the times and version the change set, and the cut it was written
// after.
type delta struct {
	Modify, Change time.Time
	Version        entity.JournalVersion
	Cut            uint64
}

func encodeDelta(d delta) []byte {
	w := codec.NewWriter(v1)
	putTime(w, d.Modify)
	putTime(w, d.Change)
	return w.Fixed(d.Version[:]).Uint64(d.Cut).Encode()
}

func decodeDelta(b []byte) (delta, error) {
	r, err := open(b)
	if err != nil {
		return delta{}, err
	}
	d := delta{Modify: readTime(r), Change: readTime(r)}
	d.Version, d.Cut = readVersion(r), r.Uint64()
	return d, checkEpochs(r, d.Version)
}

// A pending release holds nothing but its existence.
func encodeRelease() []byte { return codec.NewWriter(v1).Encode() }
