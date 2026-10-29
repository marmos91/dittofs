package kvstore

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

func TestKeyGolden(t *testing.T) {
	p, g, s := "aa000000000000000000000000000001", "bb000000000000000000000000000002", "cc000000000000000000000000000003"
	for _, c := range []struct {
		name string
		key  []byte
		want string
	}{
		{"user", userKey(pid), "20" + p},
		{"group", principalKey(group), "21" + g},
		{"member", memberKey(gid, pid), "22" + g + p},
		{"member rev", memberRevKey(pid, gid), "23" + p + g},
		{"user name", nameKey(keys.NXUser, "alice"), "2501" + "616c696365" + "0001"},
		{"share name folds", nameKey(keys.NXShare, "Photos"), "2503" + "50484f544f53" + "0001"},
		{"path", pathKey("/a"), "26" + "2f61" + "0001"},
		{"paths below", pathsBelow("/a"), "26" + "2f61" + "2f"},
		{"protocol id", protocolKey(entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: "a@R"}), "2b04" + "614052" + "0001"},
		{"netgroup", netgroupKey("lab"), "27" + "6c6162" + "0001"},
		{"share info", shareKey(sid, keys.SInfo), "02" + s + "01"},
		{"grant", grantKey(sid, gid), "02" + s + "04" + g},
		{"grant rev", grantRevKey(gid, sid), "24" + g + s},
		{"setting, installation", settingKey(entity.SettingScope{Kind: entity.ScopeInstallation}, "k"), "3201" + "6b0001"},
		{"setting, share", settingKey(entity.SettingScope{Kind: entity.ScopeShare, ID: sid}, "k"), "3205" + s + "6b0001"},
		{"share list", shareListKey, "28"},
		{"installation", installationKey, "29"},
		{"settings generation", cfgGenKey, "33"},
	} {
		if got := hex.EncodeToString(c.key); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// pathsBelow covers exactly the paths strictly below its argument.
func TestPathsBelow(t *testing.T) {
	below := pathsBelow("/home")
	for path, want := range map[string]bool{
		"/home/alice": true, "/home/a/b": true,
		"/home": false, "/homes": false, "/hom": false, "/home-x": false,
	} {
		if got := bytes.HasPrefix(pathKey(path), below); got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
}

func TestFileKeyGolden(t *testing.T) {
	s, f := "cc000000000000000000000000000003", "f1000000000000000000000000000001"
	for _, c := range []struct {
		name string
		key  []byte
		want string
	}{
		{"file", fileKey(sid, fid), "01" + s + f},
		{"acl", aclKey(sid, fid), "01" + s + f + "01"},
		{"xattr", xattrKey(sid, fid, []byte("user.k")), "01" + s + f + "02" + "757365722e6b" + "0001"},
		{"entry", entryKey(sid, fid, 7, []byte("A")), "01" + s + f + "04" + "0000000000000007" + "41" + "0001"},
		{"delta", deltaKey(sid, fid, [32]byte{31: 1}), "01" + s + f + "05" + "0000000000000000000000000000000000000000000000000000000000000001"},
		{"release", releaseKey(sid, fid), "01" + s + f + "0d"},
		{"numbers", numberKey(sid), "02" + s + "16"},
	} {
		if got := hex.EncodeToString(c.key); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// The cursor after a digest sorts after every entry at that digest, whatever
// its name, and before every entry at the next.
func TestEntriesAfter(t *testing.T) {
	after := entriesAfter(sid, fid, 7)
	for _, name := range [][]byte{{}, {0}, {0xFF}, {0xFF, 0xFF, 0xFF}, []byte("zzz")} {
		if k := entryKey(sid, fid, 7, name); bytes.Compare(k, after) >= 0 {
			t.Errorf("digest 7, name %x sorts at or after the cursor", name)
		}
		if k := entryKey(sid, fid, 8, name); bytes.Compare(k, after) <= 0 {
			t.Errorf("digest 8, name %x sorts at or before the cursor", name)
		}
	}
}

// Describe names a key's parts, from the builders the store writes with.
func TestKeyDescribe(t *testing.T) {
	sh, f := entity.ShareID{0: 0x3f, 1: 0xa2, 2: 0xc1, 3: 0xd0}, entity.FileID{0: 0x9b, 1: 0x1e, 2: 0x44, 3: 0xa7}
	for _, c := range []struct {
		key  []byte
		want string
	}{
		{fileKey(sh, f), "F‖share=3fa2c1d0‖file=9b1e44a7"},
		{entryKey(sh, f, 0xabcdef01, []byte("README.md")), `F‖share=3fa2c1d0‖file=9b1e44a7‖e‖2882400001‖"README.md"`},
		{removalKey(sh, f, entity.VersionOf(7)), "F‖share=3fa2c1d0‖file=9b1e44a7‖rm‖v7"},
		{lockKey(sh, f, 9, 100, 4), "F‖share=3fa2c1d0‖file=9b1e44a7‖op‖9‖l‖100‖4"},
		{removalIndexKey(sh, 2, f, entity.VersionOf(7)), "S‖share=3fa2c1d0‖rr‖2‖file=9b1e44a7‖v7"},
		{clientShardKey(7, 2), "CL‖7‖sh‖2"},
		{shardKey(3), "SH‖3"},
		{clientNameKey(entity.ProtocolSMB, []byte("host")), `CLO‖4‖"host"`},
		{[]byte{0xFE, 1}, "?fe01"},
	} {
		if got := keys.Describe(c.key); got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}
}
