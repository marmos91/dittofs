package kvstore

import (
	"encoding/hex"
	"errors"
	"net/netip"
	"reflect"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv/codec"
)

var (
	pid   = entity.PrincipalID{0: 0xAA, 15: 0x01}
	gid   = entity.PrincipalID{0: 0xBB, 15: 0x02}
	sid   = entity.ShareID{0: 0xCC, 15: 0x03}
	group = entity.Principal{Kind: entity.PrincipalGroup, ID: gid}
)

// codecCase is one entity, its encoding pinned, and a decoder of it.
type codecCase struct {
	name   string
	value  any
	enc    []byte
	golden string
	dec    func([]byte) (any, error)
}

func codecCases() []codecCase {
	user := entity.User{
		Principal: entity.Principal{Kind: entity.PrincipalUser, ID: pid},
		Name:      "alice",
		IDs:       entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "1000"}},
		Primary:   group,
		Disabled:  true,
		Secret:    entity.SecretRef{15: 9},
	}
	grp := entity.Group{Principal: group, Name: "staff"}
	share := entity.Share{ID: sid, Name: "Photos", Path: "/photos", Root: entity.FileID{15: 7}}
	grant := entity.ShareGrant{Share: sid, Principal: group, Access: entity.AccessReadWrite, Version: 3}
	policy := entity.ExportPolicy{
		Share:       sid,
		Flavors:     []entity.AuthFlavor{entity.FlavorKerberos, entity.FlavorSys},
		MinKerberos: entity.Krb5i,
		Clients: []entity.ClientRule{
			{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix("10.0.0.0/8")}, Access: entity.AccessRead},
			{Match: entity.ClientMatch{Netgroup: "lab"}, Access: entity.AccessReadWrite},
		},
		Squash:    entity.SquashRoot,
		Anonymous: entity.Everyone,
		SMB:       entity.SMBEncrypt | entity.SMBHidden,
		Version:   4,
	}
	ng := entity.Netgroup{Name: "lab", Members: []string{"a.example", "10.1.0.0/16"}, Version: 2}
	in := entity.Installation{ID: [16]byte{15: 1}, Instance: [16]byte{15: 2}, Generation: []byte{5}}
	set := entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeInstallation}, Key: "k", Value: []byte("v"), Generation: 6, Schema: 1}
	return []codecCase{
		{"user", user, encodeUser(user),
			"01" + "05616c696365" + "01" + "0104" + "31303030" + "02" + "bb000000000000000000000000000002" + "01" + "00000000000000000000000000000009",
			func(b []byte) (any, error) { return decodeUser(pid, b) }},
		{"group", grp, encodeGroup(grp), "01" + "057374616666" + "00",
			func(b []byte) (any, error) { return decodeGroup(gid, b) }},
		{"kind", entity.PrincipalGroup, encodeKind(entity.PrincipalGroup), "0102",
			func(b []byte) (any, error) { return decodeKind(b) }},
		{"id", [16]byte(sid), encodeID(sid), "01" + "cc000000000000000000000000000003",
			func(b []byte) (any, error) { return decodeID(b) }},
		{"share", share, encodeShareInfo(share),
			"01" + "0650686f746f73" + "072f70686f746f73" + "00000000000000000000000000000007",
			func(b []byte) (any, error) { return decodeShareInfo(sid, b) }},
		{"state", entity.ShareQuiesced, encodeState(entity.ShareQuiesced), "0102",
			func(b []byte) (any, error) { return decodeState(b) }},
		{"grant", grant, encodeGrant(grant), "01" + "02" + "02" + "0000000000000003",
			func(b []byte) (any, error) { return decodeGrant(sid, gid, b) }},
		{"policy", policy, encodePolicy(policy),
			"01" + "02" + "0302" + "02" + "02" +
				"01" + "05" + "0a00000008" + "01" +
				"02" + "036c6162" + "02" +
				"01" + "03" + "00000000000000000000000000000003" + "03" + "0000000000000004",
			func(b []byte) (any, error) { return decodePolicy(sid, b) }},
		{"netgroup", ng, encodeNetgroup(ng),
			"01" + "02" + "09612e6578616d706c65" + "0b31302e312e302e302f3136" + "0000000000000002",
			func(b []byte) (any, error) { return decodeNetgroup("lab", b) }},
		{"installation", in, encodeInstallation(in),
			"01" + "00000000000000000000000000000001" + "00000000000000000000000000000002" + "0105",
			func(b []byte) (any, error) { return decodeInstallation(b) }},
		{"counter", uint64(7), encodeCounter(7), "01" + "0000000000000007",
			func(b []byte) (any, error) { return decodeCounter(b) }},
		{"setting", set, encodeSetting(set), "01" + "0176" + "0000000000000006" + "00000001",
			func(b []byte) (any, error) { return decodeSetting(set.Scope, "k", b) }},
	}
}

func TestCodecs(t *testing.T) {
	for _, c := range codecCases() {
		t.Run(c.name, func(t *testing.T) {
			if got := hex.EncodeToString(c.enc); got != c.golden {
				t.Errorf("encoding\n got %s\nwant %s", got, c.golden)
			}
			got, err := c.dec(c.enc)
			if err != nil || !reflect.DeepEqual(got, c.value) {
				t.Fatalf("round trip: %+v, %v", got, err)
			}
			if _, err := c.dec(append([]byte{2}, c.enc[1:]...)); !errors.Is(err, codec.ErrVersion) {
				t.Errorf("unknown version: %v", err)
			}
			if _, err := c.dec(append(c.enc, 0)); !errors.Is(err, codec.ErrTrailing) {
				t.Errorf("trailing byte: %v", err)
			}
			for n := range len(c.enc) {
				if _, err := c.dec(c.enc[:n]); err == nil {
					t.Errorf("truncated to %d bytes: decoded", n)
				}
			}
		})
	}
}

func TestPolicyUnknownMatchTag(t *testing.T) {
	b := codec.NewWriter(v1).Len(0).Uint8(0).Len(1).Uint8(9).Encode()
	if _, err := decodePolicy(sid, b); !errors.Is(err, errMalformed) {
		t.Errorf("unknown tag: %v", err)
	}
}
