package kvstore

import (
	"encoding/hex"
	"errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv/codec"
)

var fid = entity.FileID{0: 0xF1, 15: 0x01}

func fileCodecCases() []codecCase {
	t0 := time.Unix(1_800_000_000, 123).UTC()
	old := time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC) // SMB's FILETIME zero, before 1678
	f := entity.File{
		ID: fid, Share: sid, Type: entity.Symlink,
		Owner: entity.Principal{Kind: entity.PrincipalUser, ID: pid}, Group: group,
		Mode: 0o4755, Flags: entity.FlagHidden | entity.FlagImmutable,
		Birth: old, Modify: t0, Change: t0, // Access left zero
		Version: entity.VersionOf(9), Size: 5, Charged: 4096, Applied: entity.VersionOf(7), MaxSpan: 6, MaxOverwrite: 8, Nlink: 2, Subdirs: 3,
		Shard: 3, Project: 4, Number: 42,
		CreateVerifier: []byte("verifier"), Target: []byte("/a"),
	}
	acl := entity.ACL{
		Flags:   entity.ACLProtected,
		Entries: []entity.ACE{{Type: entity.ACEAllow, Flags: entity.ACEFileInherit, Mask: entity.ReadData, Who: entity.Everyone}},
		Audit:   []entity.ACE{{Type: entity.ACEAudit, Flags: entity.ACEFailedAccess, Mask: entity.Delete, Who: group}},
	}
	e := entity.Entry{Parent: fid, Name: []byte("README"), Child: entity.FileID{15: 2}, Type: entity.Regular}
	d := delta{Modify: t0, Change: t0, Version: entity.VersionOf(11), Cut: 1, Subdirs: -1}
	uq := entity.Quota{Share: sid, Principal: entity.Principal{Kind: entity.PrincipalUser, ID: pid},
		Hard: entity.Limit{Bytes: 10}, Soft: entity.Limit{Bytes: 8}, Advisory: entity.Limit{Files: 5000}, Grace: time.Hour}
	pq := entity.Quota{Share: sid, Project: 7, Hard: entity.Limit{Files: 1000}}
	return []codecCase{
		{"file", f, encodeFile(f),
			"01" + "03" + "01aa000000000000000000000000000001" + "02bb000000000000000000000000000002" +
				"000009ed" + "00000011" +
				"fffffffd49ef6f00" + "00000000" + "8000000000000000" + "00000000" + // birth 1601, access zero
				"000000006b49d200" + "0000007b" + "000000006b49d200" + "0000007b" +
				"00000000000000000000000000000009" + "0000000000000005" + "0000000000001000" +
				"00000000000000000000000000000007" + "0000000000000006" + "0000000000000008" + "00000002" + "0000000000000003" +
				"00000000000000000000000000000000" + "0000000000000003" + "0000000000000004" + "000000000000002a" +
				"087665726966696572" + "022f61" + "00000000" + "00000000" + "00000000000000000000000000000000",
			func(b []byte) (any, error) { return decodeFile(sid, fid, b) }},
		{"quota", uq, encodeQuota(uq),
			"01" + "01" + hexU64(10) + hexU64(0) + hexU64(8) + hexU64(0) + hexU64(0) + hexU64(5000) + hexU64(uint64(time.Hour)),
			func(b []byte) (any, error) { return decodeQuota(sid, append([]byte{quotaPrincipal}, pid[:]...), b) }},
		{"project quota", pq, encodeQuota(pq),
			"01" + "00" + hexU64(0) + hexU64(1000) + strings.Repeat(hexU64(0), 4) + hexU64(0),
			func(b []byte) (any, error) { return decodeQuota(sid, keys.Uint64([]byte{quotaProject}, 7), b) }},
		{"soft exceeded since", t0, encodeSince(t0), "01" + "000000006b49d200" + "0000007b",
			func(b []byte) (any, error) { return decodeSince(b) }},
		{"entry", e, encodeEntry(e),
			"01" + "06524541444d45" + "00000000000000000000000000000002" + "01",
			func(b []byte) (any, error) { return decodeEntry(fid, b) }},
		{"acl", acl, encodeACL(acl),
			"01" + "01" +
				"01" + "01" + "00000001" + "00000001" + "03" + "00000000000000000000000000000003" +
				"01" + "03" + "00000020" + "00010000" + "02" + "bb000000000000000000000000000002",
			func(b []byte) (any, error) { return decodeACL(b) }},
		{"xattr", entity.Xattr{File: fid, Name: []byte("user.k"), Value: []byte("v")}, encodeXattr([]byte("v")), "01" + "0176",
			func(b []byte) (any, error) { return decodeXattr(fid, []byte("user.k"), b) }},
		{"delta", d, encodeDelta(d),
			"01" + "000000006b49d200" + "0000007b" + "000000006b49d200" + "0000007b" + "0000000000000000000000000000000b" + "0000000000000001" + "ffffffffffffffff",
			func(b []byte) (any, error) { return decodeDelta(b) }},
	}
}

func TestFileCodecs(t *testing.T) {
	for _, c := range fileCodecCases() {
		t.Run(c.name, func(t *testing.T) {
			if got := hex.EncodeToString(c.enc); got != c.golden {
				t.Errorf("encoding\n got %s\nwant %s", got, c.golden)
			}
			got, err := c.dec(c.enc)
			if err != nil || !reflect.DeepEqual(got, c.value) {
				t.Fatalf("round trip:\n got %+v, %v\nwant %+v", got, err, c.value)
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
