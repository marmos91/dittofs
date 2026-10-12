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
