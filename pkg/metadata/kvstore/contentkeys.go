package kvstore

import (
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// Keys of block metadata. Per-file records sit under the file's key with the
// namespace records; content-addressed ones under their namespace.

func holeKey(share entity.ShareID, file entity.FileID, start int64) []byte {
	return keys.Uint64(fileSub(share, file, keys.FH), uint64(start))
}

func holesPrefix(share entity.ShareID, file entity.FileID) []byte {
	return fileSub(share, file, keys.FH)
}

func overwriteKey(share entity.ShareID, file entity.FileID, start int64) []byte {
	return keys.Uint64(fileSub(share, file, keys.FOv), uint64(start))
}

func overwritesPrefix(share entity.ShareID, file entity.FileID) []byte {
	return fileSub(share, file, keys.FOv)
}

func refKey(share entity.ShareID, file entity.FileID, offset int64) []byte {
	return keys.Uint64(fileSub(share, file, keys.FRef), uint64(offset))
}

func refsPrefix(share entity.ShareID, file entity.FileID) []byte {
	return fileSub(share, file, keys.FRef)
}

func removalsPrefix(share entity.ShareID, file entity.FileID) []byte {
	return fileSub(share, file, keys.FRm)
}

// The two fences: the (shard, epoch) the file's existence and offload
// commits must carry.
func existenceFenceKey(share entity.ShareID, file entity.FileID) []byte {
	return fileSub(share, file, keys.FFx)
}

func offloadFenceKey(share entity.ShareID, file entity.FileID) []byte {
	return fileSub(share, file, keys.FFo)
}

func namespacesKey(share entity.ShareID) []byte { return shareKey(share, keys.SNsg) }

func chunkKey(ns entity.NamespaceID, h entity.ChunkHash) []byte {
	return keys.ID(keys.ID([]byte{keys.KindC}, ns[:]), h[:])
}

func blockKey(ns entity.NamespaceID, name entity.BlockName) []byte {
	return keys.ID(keys.ID([]byte{keys.KindB}, ns[:]), name[:])
}

func intentKey(ns entity.NamespaceID, name entity.BlockName) []byte {
	return keys.ID(keys.ID([]byte{keys.KindI}, ns[:]), name[:])
}

func intentsPrefix(ns entity.NamespaceID) []byte { return keys.ID([]byte{keys.KindI}, ns[:]) }

// reverseKey is one ref's reverse index entry: which refs name this chunk.
func reverseKey(ns entity.NamespaceID, h entity.ChunkHash, share entity.ShareID, file entity.FileID, offset int64, died entity.SnapshotCut) []byte {
	k := keys.ID(keys.ID([]byte{keys.KindCR}, ns[:]), h[:])
	k = keys.ID(keys.ID(k, share[:]), file[:])
	return keys.Uint64(keys.Uint64(k, uint64(offset)), uint64(died))
}

func reversePrefix(ns entity.NamespaceID, h entity.ChunkHash) []byte {
	return keys.ID(keys.ID([]byte{keys.KindCR}, ns[:]), h[:])
}

// retiredKey files a retired block by when it may be deleted, store time in
// unsigned nanoseconds, so the deleter scans what is due in order.
func retiredKey(ns entity.NamespaceID, notBefore time.Time, name entity.BlockName) []byte {
	k := keys.Uint64(keys.ID([]byte{keys.KindBR}, ns[:]), uint64(notBefore.UnixNano()))
	return keys.ID(k, name[:])
}

func retiredPrefix(ns entity.NamespaceID) []byte { return keys.ID([]byte{keys.KindBR}, ns[:]) }

// deletedKey files a block whose delete is recorded, awaiting prune.
func deletedKey(ns entity.NamespaceID, name entity.BlockName) []byte {
	return keys.ID(keys.ID([]byte{keys.KindBD}, ns[:]), name[:])
}

func deletedPrefix(ns entity.NamespaceID) []byte { return keys.ID([]byte{keys.KindBD}, ns[:]) }

func gcPauseKey(ns entity.NamespaceID) []byte {
	return append(keys.ID([]byte{keys.KindNS}, ns[:]), keys.NSGc, keys.GcPause)
}
