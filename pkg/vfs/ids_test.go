package vfs_test

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/vfs"
)

func stateID(t testing.TB, s string) [16]byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 16 {
		t.Fatalf("bad vector %q", s)
	}
	return [16]byte(b)
}

// The stateid vectors are spelled byte by byte from the wire layout: seqid,
// then other's type and instance byte, its three shard bytes and its eight
// token bytes.
func TestStateIDGolden(t *testing.T) {
	cases := []struct {
		name string
		wire string
		ref  vfs.OpenRef
	}{
		{
			name: "open",
			// seqid 7; type 1 | instance 5<<3 = 0x29; shard 0x0a0b0c; token 0x0102030405060708
			wire: "00000007" + "29" + "0a0b0c" + "0102030405060708",
			ref:  vfs.OpenRef{Kind: vfs.RefOpened, State: vfs.StateRef{Type: vfs.StateOpen, Instance: 5, Shard: 0x0a0b0c, Token: 0x0102030405060708, Seq: 7}},
		},
		{
			name: "lock state at the widest shard and instance",
			// seqid 0xfffffffe; type 2 | instance 31<<3 = 0xfa; shard 0xffffff
			wire: "fffffffe" + "fa" + "ffffff" + "fedcba9876543210",
			ref:  vfs.OpenRef{Kind: vfs.RefLocked, State: vfs.StateRef{Type: vfs.StateLock, Instance: 31, Shard: 0xffffff, Token: 0xfedcba9876543210, Seq: 0xfffffffe}},
		},
		{
			name: "delegation",
			// seqid 1; type 3, instance 0; shard 1; token 0x8000000000000001
			wire: "00000001" + "03" + "000001" + "8000000000000001",
			ref:  vfs.OpenRef{Kind: vfs.RefDelegated, State: vfs.StateRef{Type: vfs.StateGrant, Shard: 1, Token: 0x8000000000000001, Seq: 1}},
		},
		{
			name: "anonymous",
			wire: "00000000" + "00" + "000000" + "0000000000000000",
			ref:  vfs.OpenRef{Kind: vfs.RefAnonymous},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := stateID(t, c.wire)
			got, err := vfs.DecodeStateID(b)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Kind != c.ref.Kind || got.State != c.ref.State || got.Open != 0 || got.Handle != nil {
				t.Fatalf("decode = %+v, want %+v", got, c.ref)
			}
			if enc, err := vfs.EncodeStateID(c.ref.State); err != nil || enc != b {
				t.Fatalf("encode = %x, %v; want %x", enc, err, b)
			}
		})
	}
}

func TestStateIDMalformed(t *testing.T) {
	for name, wire := range map[string]string{
		"type zero, not all zeros": "00000000" + "28" + "000001" + "0000000000000001",
		"current stateid":          "00000001" + "00" + "000000" + "0000000000000000",
		"watch type":               "00000001" + "04" + "000001" + "0000000000000001",
		"type 5":                   "00000001" + "05" + "000001" + "0000000000000001",
		"type 6":                   "00000001" + "06" + "000001" + "0000000000000001",
		"type 7":                   "00000001" + "07" + "000001" + "0000000000000001",
		"all ones":                 "ffffffff" + "ff" + "ffffff" + "ffffffffffffffff",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := vfs.DecodeStateID(stateID(t, wire))
			var se *vfs.StateError
			if !errors.As(err, &se) || se.Kind != vfs.StateBadState {
				t.Fatalf("err = %v, want StateError BadState", err)
			}
		})
	}
}

// The FileId vectors put the shard in the high 24 bits of the persistent
// half and the OpenID in the low 40.
func TestFileIDGolden(t *testing.T) {
	cases := []struct {
		name       string
		persistent uint64
		shard      vfs.ShardID
		open       vfs.OpenID
	}{
		{"shard and open", 0x0a0b0c_0102030405, 0x0a0b0c, 0x0102030405},
		{"shard zero", 0x000000_0000000001, 0, 1},
		{"widest", 0xffffff_ffffffffff, 0xffffff, 0xffffffffff},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := vfs.DecodeFileID(c.persistent)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			want := vfs.OpenRef{Kind: vfs.RefOpened, State: vfs.StateRef{Shard: c.shard}, Open: c.open}
			if got.Kind != want.Kind || got.State != want.State || got.Open != want.Open {
				t.Fatalf("decode = %+v, want %+v", got, want)
			}
			if enc, err := vfs.EncodeFileID(want); err != nil || enc != c.persistent {
				t.Fatalf("encode = %#x, %v; want %#x", enc, err, c.persistent)
			}
		})
	}
}

func TestFileIDMalformed(t *testing.T) {
	for _, p := range []uint64{0, 0x0a0b0c_0000000000} {
		_, err := vfs.DecodeFileID(p)
		var se *vfs.StateError
		if !errors.As(err, &se) || se.Kind != vfs.StateBadState {
			t.Fatalf("%#x: err = %v, want StateError BadState", p, err)
		}
	}
}

// A value whose fields do not fit is refused, never spelled as another.
func TestEncodeRefusesWhatDoesNotFit(t *testing.T) {
	for name, r := range map[string]vfs.StateRef{
		"shard past 24 bits":   {Type: vfs.StateOpen, Shard: 0x1000001, Token: 1},
		"shard at the limit":   {Type: vfs.StateOpen, Shard: entity.ShardLimit, Token: 1},
		"instance past 5 bits": {Type: vfs.StateOpen, Instance: 32, Token: 1},
		"watch, no stateid's":  {Type: vfs.StateWatch, Token: 1},
		"type past the 3 bits": {Type: 9, Token: 1},
	} {
		if b, err := vfs.EncodeStateID(r); !errors.Is(err, vfs.ErrInvalid) {
			t.Errorf("stateid, %s: %x, %v; want ErrInvalid", name, b, err)
		}
	}
	for name, ref := range map[string]vfs.OpenRef{
		"shard past 24 bits":  {Kind: vfs.RefOpened, State: vfs.StateRef{Shard: 0x1000001}, Open: 1},
		"shard at the limit":  {Kind: vfs.RefOpened, State: vfs.StateRef{Shard: entity.ShardLimit}, Open: 1},
		"OpenID past 40 bits": {Kind: vfs.RefOpened, Open: 1 << 40},
		"OpenID zero":         {Kind: vfs.RefOpened, State: vfs.StateRef{Shard: 1}},
		"a lock state":        {Kind: vfs.RefLocked, State: vfs.StateRef{Type: vfs.StateLock, Shard: 1, Instance: 7, Token: 99}, Open: 5},
		"an open's stateid":   {Kind: vfs.RefOpened, State: vfs.StateRef{Type: vfs.StateOpen, Shard: 1, Token: 99}, Open: 5},
		"anonymous":           {Kind: vfs.RefAnonymous, Open: 5},
		"a grace instance":    {Kind: vfs.RefOpened, State: vfs.StateRef{Shard: 1, Instance: 7}, Open: 5},
		"a state type":        {Kind: vfs.RefOpened, State: vfs.StateRef{Type: vfs.StateOpen, Shard: 1}, Open: 5},
		"a token":             {Kind: vfs.RefOpened, State: vfs.StateRef{Shard: 1, Token: 99}, Open: 5},
		"a sequence":          {Kind: vfs.RefOpened, State: vfs.StateRef{Shard: 1, Seq: 3}, Open: 5},
	} {
		if p, err := vfs.EncodeFileID(ref); !errors.Is(err, vfs.ErrInvalid) {
			t.Errorf("FileId, %s: %#x, %v; want ErrInvalid", name, p, err)
		}
	}
}

// The fuzz targets take bytes: the fuzzer moves an integer argument only a
// little at a time, so the type byte and the shard bits would never leave
// their seeds.
func FuzzDecodeStateID(f *testing.F) {
	f.Add(make([]byte, 16))
	seed := stateID(f, "00000007"+"29"+"0a0b0c"+"0102030405060708")
	f.Add(seed[:])
	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) != 16 {
			return
		}
		b := [16]byte(in)
		ref, err := vfs.DecodeStateID(b)
		if err != nil {
			return
		}
		if enc, err := vfs.EncodeStateID(ref.State); err != nil || enc != b {
			t.Fatalf("%x decoded and re-encoded as %x, %v", b, enc, err)
		}
	})
}

func FuzzDecodeFileID(f *testing.F) {
	f.Add(binary.BigEndian.AppendUint64(nil, 0x0a0b0c_0102030405))
	f.Add(make([]byte, 8))
	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) != 8 {
			return
		}
		p := binary.BigEndian.Uint64(in)
		ref, err := vfs.DecodeFileID(p)
		if err != nil {
			return
		}
		if enc, err := vfs.EncodeFileID(ref); err != nil || enc != p {
			t.Fatalf("%#x decoded and re-encoded as %#x, %v", p, enc, err)
		}
	})
}
