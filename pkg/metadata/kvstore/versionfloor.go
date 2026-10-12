package kvstore

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/codec"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// floorBuckets is B, the buckets a share's version index spreads over, so
// its commits, which all write near the top of the version order, land in B
// key ranges rather than one. It is part of the store format.
const floorBuckets = 16

// floorBucket hashes a file's identity into its bucket. A minted ID ends in
// random bits, so its last byte is already a uniform hash of it.
func floorBucket(id entity.FileID) uint64 { return uint64(id[15]) % floorBuckets }

func floorBucketPrefix(share entity.ShareID, bucket uint64) []byte {
	return keys.Uint64(shareKey(share, keys.SVf), bucket)
}

// floorKey is a File's entry in the version index, at its stored Version.
func floorKey(share entity.ShareID, file entity.FileID, v entity.JournalVersion) []byte {
	return keys.ID(keys.Version(floorBucketPrefix(share, floorBucket(file)), v), file[:])
}

// deltaFloorKey is an unfolded directory delta's entry, under the delta's
// own unique suffix.
func deltaFloorKey(share entity.ShareID, dir entity.FileID, v entity.JournalVersion, unique [32]byte) []byte {
	return append(floorKey(share, dir, v), unique[:]...)
}

// putFile writes a File record and, when its Version changed from was, moves
// its version-index entry in the same transaction: every transaction that
// raises a Version goes through here, so the index never misses one. A zero
// was is a file with no entry yet.
func putFile(tx kv.Txn, was entity.JournalVersion, f entity.File) error {
	if err := tx.Set(fileKey(f.Share, f.ID), encodeFile(f)); err != nil {
		return err
	}
	return moveFloor(tx, f.Share, f.ID, was, f.Version)
}

// moveFloor moves a file's version-index entry from was to v.
func moveFloor(tx kv.Txn, share entity.ShareID, file entity.FileID, was, v entity.JournalVersion) error {
	if was == v {
		return nil
	}
	if !was.IsZero() {
		if err := tx.Delete(floorKey(share, file, was)); err != nil {
			return err
		}
	}
	return tx.Set(floorKey(share, file, v), encodeEmpty())
}

// VersionFloor is the highest version the store records for any file of
// the shares: every File's Version and every unfolded directory delta's,
// which bound every ref's newest and every applied. A journal opens above
// it, and raises its counter above a share's floor before it serves a share
// it did not serve when it opened. It reads the highest key of each bucket:
// B seeks per share, whatever the share's size.
func (s *Store) VersionFloor(ctx context.Context, shares []entity.ShareID) (floor entity.JournalVersion, err error) {
	err = s.kv.View(ctx, func(r kv.Reader) error {
		for _, share := range shares {
			for b := range uint64(floorBuckets) {
				pre := floorBucketPrefix(share, b)
				// Every version's epoch half is zero, so 0xFF sorts above
				// every key under the bucket.
				last, ok, err := r.Before(pre, append(pre, 0xFF))
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				v, _, err := keys.ReadVersion(last.Key[len(pre):])
				if err != nil {
					return err
				}
				floor = entity.MaxVersion(floor, v)
			}
		}
		return nil
	})
	return floor, err
}

func versionLimitKey(share entity.ShareID) []byte { return shareKey(share, keys.SVl) }

// VersionLimit is the share's version limit: its journal issues versions
// only below it, so a crash skips the rest of a block and never repeats a
// version a client saw. Zero when none was written.
func (s *Store) VersionLimit(ctx context.Context, share entity.ShareID) (v entity.JournalVersion, err error) {
	err = s.kv.View(ctx, func(r kv.Reader) error {
		b, ok, err := get(r, versionLimitKey(share))
		if err != nil || !ok {
			return err
		}
		v, err = decodeVersion(b)
		return err
	})
	return v, err
}

// RaiseVersionLimit raises the share's version limit to v, in its own
// transaction, before the journal draws at or above the old one. A limit is
// never lowered: a v at or below the stored one changes nothing.
func (s *Store) RaiseVersionLimit(ctx context.Context, share entity.ShareID, v entity.JournalVersion) error {
	return s.kv.Update(ctx, func(tx kv.Txn) error {
		if _, ok, err := get(tx, shareKey(share, keys.SInfo)); err != nil {
			return err
		} else if !ok {
			return merrs.NewNotFoundError(share.String(), "share")
		}
		if err := liveShare(tx, share); err != nil {
			return err
		}
		b, ok, err := get(tx, versionLimitKey(share))
		if err != nil {
			return err
		}
		if ok {
			old, err := decodeVersion(b)
			if err != nil {
				return err
			}
			if !old.Less(v) {
				return nil
			}
		}
		return tx.Set(versionLimitKey(share), encodeVersion(v))
	})
}

func encodeVersion(v entity.JournalVersion) []byte { return codec.NewWriter(v1).Fixed(v[:]).Encode() }

func decodeVersion(b []byte) (entity.JournalVersion, error) {
	r, err := open(b)
	if err != nil {
		return entity.JournalVersion{}, err
	}
	v := readVersion(r)
	return v, checkEpochs(r, v)
}
