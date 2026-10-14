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

var (
	nsid  = entity.NamespaceID{0: 0xD0, 15: 0x01}
	hash  = entity.ChunkHash{0: 0xAB, 31: 0x01}
	bname = entity.BlockName{0: 0xBC, 31: 0x02}
)

func v16(n uint64) string { return strings.Repeat("0", 16) + hexU64(n) }
func hexU64(n uint64) string {
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = byte(n)
		n >>= 8
	}
	return hex.EncodeToString(b)
}

func contentCodecCases() []codecCase {
	t0 := time.Unix(1_800_000_000, 0).UTC()
	ref := entity.ChunkRef{File: fid, Offset: 4096, Length: 1 << 20, Chunk: hash, Skip: 512,
		Oldest: entity.VersionOf(1), Newest: entity.VersionOf(2), Born: 3, NSGen: 1}
	rm := entity.Removal{Version: entity.VersionOf(4), Extent: entity.Extent{Offset: 3 << 20, Length: entity.EndOfFile - 3<<20},
		Kind: entity.RemoveClone, Cut: 2, Cursor: 3 << 20, OvSpan: 5, HoleCredit: true,
		Clone: &entity.CloneSpec{SrcShare: sid, Src: fid, SrcOff: 0, DstOff: 3 << 20, Len: 1 << 20, AsOf: entity.VersionOf(9)}}
	release := entity.Removal{Version: entity.VersionOf(8), Extent: entity.ExtentOf(0, entity.EndOfFile),
		Kind: entity.RemoveRelease, Done: true, FileVersion: entity.VersionOf(6)}
	blk := entity.Block{Name: bname, State: entity.BlockRetired, NotBefore: t0, Live: 0, Dead: 10, DeadAt: t0,
		Size: 4 << 20, Generation: 1, Carried: []entity.Carried{{Hash: hash, Length: 100}}}
	return []codecCase{
		{"ref", ref, encodeRef(ref),
			"01" + hex.EncodeToString(hash[:]) + "00000001" + hexU64(512) + hexU64(1<<20) + v16(1) + v16(2) + hexU64(3),
			func(b []byte) (any, error) { return decodeRef(fid, 4096, b) }},
		{"hole", hole{entity.ExtentOf(10, 20), entity.VersionOf(5), 7}, encodeHole(hole{entity.ExtentOf(10, 20), entity.VersionOf(5), 7}),
			"01" + hexU64(20) + v16(5) + hexU64(7),
			func(b []byte) (any, error) { return decodeHole(10, b) }},
		{"overwrite", overwrite{Extent: entity.ExtentOf(1<<20, 2<<20), Version: entity.VersionOf(3), Born: 1},
			encodeOverwrite(overwrite{Extent: entity.ExtentOf(1<<20, 2<<20), Version: entity.VersionOf(3), Born: 1}),
			"01" + hexU64(2<<20) + v16(3) + hexU64(1),
			func(b []byte) (any, error) { return decodeOverwrite(1<<20, b) }},
		{"removal", rm, encodeRemoval(rm),
			"01" + hexU64(3<<20) + hexU64(uint64(entity.EndOfFile)) + "04" + hexU64(2) + hexU64(3<<20) + "00" + hexU64(5) + "01" + "01" +
				hex.EncodeToString(sid[:]) + hex.EncodeToString(fid[:]) + hexU64(0) + hexU64(3<<20) + hexU64(1<<20) + v16(9),
			func(b []byte) (any, error) { return decodeRemoval(entity.VersionOf(4), b) }},
		{"release removal", release, encodeRemoval(release),
			"01" + hexU64(0) + hexU64(uint64(entity.EndOfFile)) + "03" + hexU64(0) + hexU64(0) + "01" + hexU64(0) + "00" + v16(6) + "00",
			func(b []byte) (any, error) { return decodeRemoval(entity.VersionOf(8), b) }},
		{"fence", fence{Shard: 0, Epoch: 7}, encodeFence(fence{Epoch: 7}), "01" + hexU64(0) + hexU64(7),
			func(b []byte) (any, error) { return decodeFence(b) }},
		{"namespaces", shareNamespaces{Namespaces: []entity.NamespaceGen{{Generation: 1, Namespace: nsid}}, Write: 1},
			encodeNamespaces(shareNamespaces{Namespaces: []entity.NamespaceGen{{Generation: 1, Namespace: nsid}}, Write: 1}),
			"01" + "01" + "00000001" + hex.EncodeToString(nsid[:]) + "00000001",
			func(b []byte) (any, error) { return decodeNamespaces(b) }},
		{"chunk", chunkRecord{Chunk: entity.Chunk{Hash: hash, Block: bname, Position: 64, Length: 100, Refcount: 2}, Stamp: 9},
			encodeChunk(chunkRecord{Chunk: entity.Chunk{Hash: hash, Block: bname, Position: 64, Length: 100, Refcount: 2}, Stamp: 9}),
			"01" + hex.EncodeToString(bname[:]) + hexU64(64) + hexU64(100) + hexU64(2) + hexU64(9),
			func(b []byte) (any, error) { return decodeChunk(hash, b) }},
		{"block", blk, encodeBlock(blk),
			"01" + "01" + hexU64(1_800_000_000) + "00000000" + hexU64(0) + hexU64(10) + hexU64(1_800_000_000) + "00000000" +
				hexU64(4<<20) + "01" + "01" + hex.EncodeToString(hash[:]) + hexU64(100),
			func(b []byte) (any, error) { return decodeBlock(bname, b) }},
		{"intent", Intent{Domain: DomainShard, ID: 0, Epoch: 41, NodeEpoch: 3}, encodeIntent(Intent{Domain: DomainShard, Epoch: 41, NodeEpoch: 3}),
			"01" + "01" + hexU64(0) + hexU64(41) + strings.Repeat("00", 16) + hexU64(3),
			func(b []byte) (any, error) { return decodeIntent(b) }},
	}
}

func TestContentCodecs(t *testing.T) {
	for _, c := range contentCodecCases() {
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

// A version with its epoch set does not decode in this format.
func TestEpochRefused(t *testing.T) {
	v := entity.VersionOf(3)
	v[0] = 1
	b := encodeOverwrite(overwrite{Extent: entity.ExtentOf(0, 1), Version: v})
	if _, err := decodeOverwrite(0, b); !errors.Is(err, errMalformed) {
		t.Fatalf("epoch decoded: %v", err)
	}
}

func TestContentKeyGolden(t *testing.T) {
	s, f, n := hex.EncodeToString(sid[:]), hex.EncodeToString(fid[:]), hex.EncodeToString(nsid[:])
	h, b := hex.EncodeToString(hash[:]), hex.EncodeToString(bname[:])
	for _, c := range []struct {
		name string
		key  []byte
		want string
	}{
		{"hole", holeKey(sid, fid, 7), "01" + s + f + "06" + hexU64(7)},
		{"overwrite", overwriteKey(sid, fid, 7), "01" + s + f + "08" + hexU64(7)},
		{"removal", removalKey(sid, fid, entity.VersionOf(7)), "01" + s + f + "07" + v16(7)},
		{"ref", refKey(sid, fid, 7), "01" + s + f + "09" + hexU64(7)},
		{"existence fence", existenceFenceKey(sid, fid), "01" + s + f + "0b"},
		{"offload fence", offloadFenceKey(sid, fid), "01" + s + f + "0c"},
		{"namespaces", namespacesKey(sid), "02" + s + "03"},
		{"chunk", chunkKey(nsid, hash), "10" + n + h},
		{"reverse", reverseKey(nsid, hash, sid, fid, 7, 0), "11" + n + h + s + f + hexU64(7) + hexU64(0)},
		{"block", blockKey(nsid, bname), "12" + n + b},
		{"retired", retiredKey(nsid, time.Unix(0, 5), bname), "13" + n + hexU64(5) + b},
		{"deleted", deletedKey(nsid, bname), "14" + n + b},
		{"intent", intentKey(nsid, bname), "16" + n + b},
		{"gc pause", gcPauseKey(nsid), "17" + n + "0106"},
	} {
		if got := hex.EncodeToString(c.key); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}
