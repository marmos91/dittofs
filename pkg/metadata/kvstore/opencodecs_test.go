package kvstore

import (
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv/codec"
)

func hexU32(n uint32) string {
	return hex.EncodeToString([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}

func openCodecCases() []codecCase {
	alice := entity.Principal{Kind: entity.PrincipalUser, ID: pid}
	ph, gh := "01"+hex.EncodeToString(pid[:]), "02"+hex.EncodeToString(gid[:])
	cl := entity.Client{ID: 7, Protocol: entity.ProtocolNFSv4, Owner: []byte("host"), Verifier: [8]byte{7: 1},
		Principal: alice, MachCred: &entity.MachCred{Principal: group, Ops: []byte{0x0F}}, CSSeq: 3, CSReply: []byte{0xAB}, Revoked: true}
	smb := entity.Client{ID: 8, Protocol: entity.ProtocolSMB, Owner: []byte{0x11, 0x22}, CSReply: []byte{}}
	sh := entity.ClientShard{Shard: 2, Reclaimed: 5, LossPending: true, Revoked: true, Notify: []byte("n")}
	op := entity.Open{ID: 9, Client: 7, Principal: alice, File: fid, Access: 0x1F, Privileged: true, Deny: 0x02,
		Durability: entity.DurabilityPersistent, DeleteOnClose: true, DeleteBy: group,
		Lease:     &entity.Lease{Key: [16]byte{15: 1}, ParentKey: [16]byte{15: 2}, State: 7, Epoch: 4},
		Suspended: entity.TimeModify, Via: entity.FileID{15: 3}, StateSeq: 2, Nonce: 0xDEAD, LossSeen: 6,
		Session: entity.SessionRef{Node: 1, NodeEpoch: 2, ID: 3},
		Kind:    entity.OpenCloneSource, CloneDst: entity.FileID{15: 4}, CloneVersion: entity.VersionOf(5),
		CreateGUID: [16]byte{15: 5}, AppInstance: [16]byte{15: 6}, AppVersion: [2]uint64{1, 2},
		Timeout: time.Minute, LockSeq: [64]uint8{0: 0x13}, ChannelSeq: 8}
	lk := entity.Lock{ID: 4, Open: 9, Owner: entity.LockOwner{Client: 7, Owner: []byte("o")}, Offset: 100, Length: entity.EndOfFile, Exclusive: true}
	dp := entity.DeletePending{Parent: entity.FileID{15: 1}, Name: []byte("a"), By: alice, Access: 0x10000, Privileged: true}
	f16 := func(b byte) string { return strings.Repeat("00", 15) + hex.EncodeToString([]byte{b}) }
	return []codecCase{
		{"client", cl, encodeClient(cl),
			"01" + "01" + "04" + hex.EncodeToString([]byte("host")) + "0000000000000001" + ph + "01" + gh + "010f" +
				hexU32(3) + "01ab" + "01",
			func(b []byte) (any, error) { return decodeClient(7, b) }},
		{"smb client", smb, encodeClient(smb),
			"01" + "02" + "021122" + "0000000000000000" + "00" + strings.Repeat("00", 16) + "00" + hexU32(0) + "00" + "00",
			func(b []byte) (any, error) { return decodeClient(8, b) }},
		{"client shard", sh, encodeClientShard(sh), "01" + hexU32(5) + "01" + "01" + "016e",
			func(b []byte) (any, error) { return decodeClientShard(2, b) }},
		{"client id", entity.ClientID(7), encodeClientID(7), "01" + hexU64(7),
			func(b []byte) (any, error) { return decodeClientID(b) }},
		{"open", op, encodeOpen(op),
			"01" + hexU64(7) + ph + hexU32(0x1F) + "01" + hexU32(2) + "02" + "01" + gh +
				"01" + f16(1) + f16(2) + "07" + hexU32(4) +
				"01" + f16(3) + hexU32(2) + hexU64(0xDEAD) + hexU64(6) + hexU64(1) + hexU64(2) + hexU64(3) +
				"01" + f16(4) + v16(5) + f16(5) + f16(6) + hexU64(1) + hexU64(2) +
				hexU64(uint64(time.Minute)) + "13" + strings.Repeat("00", 63) + hexU32(8),
			func(b []byte) (any, error) { return decodeOpen(fid, 9, b) }},
		{"lock", lk, encodeLock(lk), "01" + hexU64(7) + "016f" + hexU64(uint64(entity.EndOfFile)) + "01",
			func(b []byte) (any, error) { return decodeLock(9, 100, 4, b) }},
		{"delete pending", dp, encodeDeletePending(dp), "01" + f16(1) + "0161" + ph + hexU32(0x10000) + "01",
			func(b []byte) (any, error) { return decodeDeletePending(b) }},
	}
}

func TestOpenCodecs(t *testing.T) {
	for _, c := range openCodecCases() {
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

func TestOpenKeyGolden(t *testing.T) {
	s, f := hex.EncodeToString(sid[:]), hex.EncodeToString(fid[:])
	for _, c := range []struct {
		name string
		key  []byte
		want string
	}{
		{"open", openKey(sid, fid, 9), "01" + s + f + "0e" + hexU64(9)},
		{"lock", lockKey(sid, fid, 9, 100, 4), "01" + s + f + "0e" + hexU64(9) + "01" + hexU64(100) + hexU64(4)},
		{"durable-open index", durableOpenIndexKey(sid, 2, fid, 9), "02" + s + "1b" + hexU64(2) + f + hexU64(9)},
		{"delete pending", deletePendingKey(sid, fid), "01" + s + f + "0f"},
		{"client", clientKey(7), "35" + hexU64(7)},
		{"client shard", clientShardKey(7, 2), "35" + hexU64(7) + "01" + hexU64(2)},
		{"client owner", clientOwnerKey(entity.ProtocolSMB, []byte{0x11, 0x00}), "36" + "02" + "1100ff" + "0001"},
		{"nsm", nsmKey(), "2a"},
		{"ids", idKey(idOpen), "3a02"},
	} {
		if got := hex.EncodeToString(c.key); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}
