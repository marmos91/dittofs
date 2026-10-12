// Package kvstore is the metadata store over a kv.KV: the records, their keys
// and codecs, and the views over them.
package kvstore

import (
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// Keys of the control-plane records. A key names principals by ID only; the
// kind of a member or a grant's principal is in the record's value.

func userKey(id entity.PrincipalID) []byte  { return keys.ID([]byte{keys.KindU}, id[:]) }
func groupKey(id entity.PrincipalID) []byte { return keys.ID([]byte{keys.KindG}, id[:]) }

// principalKey is the User or Group record of p.
func principalKey(p entity.Principal) []byte {
	if p.Kind == entity.PrincipalGroup {
		return groupKey(p.ID)
	}
	return userKey(p.ID)
}

func memberKey(group, member entity.PrincipalID) []byte {
	return keys.ID(keys.ID([]byte{keys.KindM}, group[:]), member[:])
}

func membersPrefix(group entity.PrincipalID) []byte { return keys.ID([]byte{keys.KindM}, group[:]) }

func memberRevKey(member, group entity.PrincipalID) []byte {
	return keys.ID(keys.ID([]byte{keys.KindMR}, member[:]), group[:])
}

func memberOfPrefix(member entity.PrincipalID) []byte {
	return keys.ID([]byte{keys.KindMR}, member[:])
}

// nameKey is the name index row of a user, group or share name. A share's
// name is keyed by its fold, so two names equal under it collide.
func nameKey(kind byte, name string) []byte {
	if kind == keys.NXShare {
		name = entity.FoldShareName(name)
	}
	return keys.String([]byte{keys.KindNX, kind}, []byte(name))
}

func nameKind(k entity.PrincipalKind) byte {
	if k == entity.PrincipalGroup {
		return keys.NXGroup
	}
	return keys.NXUser
}

func pathKey(path string) []byte { return keys.String([]byte{keys.KindNP}, []byte(path)) }

// pathsBelow is the prefix of every path index row strictly below path: its
// encoding without the terminator, then the separator.
func pathsBelow(path string) []byte {
	k := pathKey(path)
	return append(k[:len(k)-2], '/')
}

func protocolKey(id entity.ProtocolID) []byte {
	return keys.String([]byte{keys.KindPX, byte(id.Scheme)}, []byte(id.Value))
}

func netgroupKey(name string) []byte { return keys.String([]byte{keys.KindNG}, []byte(name)) }

func shareKey(id entity.ShareID, sub byte) []byte {
	return append(keys.ID([]byte{keys.KindS}, id[:]), sub)
}

func grantKey(share entity.ShareID, p entity.PrincipalID) []byte {
	return keys.ID(shareKey(share, keys.SG), p[:])
}

func grantRevKey(p entity.PrincipalID, share entity.ShareID) []byte {
	return keys.ID(keys.ID([]byte{keys.KindGR}, p[:]), share[:])
}

func grantsOfPrefix(p entity.PrincipalID) []byte { return keys.ID([]byte{keys.KindGR}, p[:]) }

// settingKey tags the scope with its kind; installation scope has no ID.
func settingKey(scope entity.SettingScope, key string) []byte {
	return keys.String(scopePrefix(scope), []byte(key))
}

func scopePrefix(scope entity.SettingScope) []byte {
	k := []byte{keys.KindCFG, byte(scope.Kind)}
	if scope.Kind != entity.ScopeInstallation {
		k = keys.ID(k, scope.ID[:])
	}
	return k
}

var (
	shareListKey    = []byte{keys.KindSL}
	installationKey = []byte{keys.KindIN}
	cfgGenKey       = []byte{keys.KindCFGGEN}
	tagKey          = []byte{keys.KindTG}
)

// Keys of a file's records, all under F‖share‖file.

func fileKey(share entity.ShareID, id entity.FileID) []byte {
	return keys.ID(keys.ID([]byte{keys.KindF}, share[:]), id[:])
}

func fileSub(share entity.ShareID, id entity.FileID, sub byte) []byte {
	return append(fileKey(share, id), sub)
}

func aclKey(share entity.ShareID, id entity.FileID) []byte { return fileSub(share, id, keys.FAcl) }

func xattrKey(share entity.ShareID, id entity.FileID, name []byte) []byte {
	return keys.String(fileSub(share, id, keys.FX), name)
}

func xattrsPrefix(share entity.ShareID, id entity.FileID) []byte { return fileSub(share, id, keys.FX) }

// entryKey is one entry of dir, ordered by the digest of its folded key and
// then by the key.
func entryKey(share entity.ShareID, dir entity.FileID, digest uint64, folded []byte) []byte {
	return keys.String(keys.Uint64(fileSub(share, dir, keys.FE), digest), folded)
}

func entriesPrefix(share entity.ShareID, dir entity.FileID) []byte {
	return fileSub(share, dir, keys.FE)
}

// entriesAfter is the scan cursor that resumes a listing after every entry
// whose digest is cookie or below: the bare digest cookie+1 sorts before
// every entry key at that digest, which all carry a name after it, and after
// every key of a lower digest.
func entriesAfter(share entity.ShareID, dir entity.FileID, cookie uint64) []byte {
	return keys.Uint64(entriesPrefix(share, dir), cookie+1)
}

// deltaKey is one directory time delta, unique to the transaction that
// wrote it.
func deltaKey(share entity.ShareID, dir entity.FileID, unique [32]byte) []byte {
	return append(fileSub(share, dir, keys.FT), unique[:]...)
}

func deltasPrefix(share entity.ShareID, dir entity.FileID) []byte {
	return fileSub(share, dir, keys.FT)
}

func releaseKey(share entity.ShareID, id entity.FileID) []byte { return fileSub(share, id, keys.FRel) }

// filesPrefix covers every file record of a share.
func filesPrefix(share entity.ShareID) []byte { return keys.ID([]byte{keys.KindF}, share[:]) }

func numberKey(share entity.ShareID) []byte { return shareKey(share, keys.SFnc) }
