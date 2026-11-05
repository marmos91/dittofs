package kvstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// Lifecycle is the engine's store access outside block metadata's views: a
// node's devices' journal identities, the installation's tags, the shares'
// version limits and the shards' incarnations (RFC 16 §3).
type Lifecycle struct {
	s *Store
}

// Lifecycle is the engine's view of the store outside block metadata.
func (s *Store) Lifecycle() Lifecycle {
	return Lifecycle{s}
}

// journalRecord is a journal's J‖journal value: the node and device it lives
// on.
type journalRecord struct {
	Node   entity.NodeID
	Device entity.JournalDir
}

func encodeJournal(j journalRecord) []byte {
	return codec.NewWriter(v1).Fixed(j.Node[:]).String(string(j.Device)).Encode()
}

func decodeJournal(b []byte) (journalRecord, error) {
	r, err := open(b)
	if err != nil {
		return journalRecord{}, err
	}

	var j journalRecord
	copy(j.Node[:], r.Fixed(len(j.Node)))
	j.Device = entity.JournalDir(r.String())

	return j, r.Done()
}

// encodeJournalIndex is a JD‖node‖device value: the journal identity on that
// node's device.
func encodeJournalIndex(id [16]byte) []byte {
	return codec.NewWriter(v1).Fixed(id[:]).Encode()
}

func decodeJournalIndex(b []byte) ([16]byte, error) {
	r, err := open(b)
	if err != nil {
		return [16]byte{}, err
	}

	var id [16]byte
	copy(id[:], r.Fixed(len(id)))

	return id, r.Done()
}

// removalStep is how far a share's removal has gone on one shard, as its
// SH‖shard‖rm row records it (RFC 16 §2.3.2, §4.2), in the order the steps
// run. A shard with no row has not started its step 3.
type removalStep uint8

const (
	stepThree removalStep = iota + 1
	stepOffloaded
	stepForgotten
	stepFour
	stepDone
)

// decodeRemovalStep reads an rm row's step: the first field after the codec
// version. The row may hold more after it, as step 4's cursor, which is that
// step's to read, so trailing bytes are not refused.
func decodeRemovalStep(b []byte) (removalStep, error) {
	r, err := open(b)
	if err != nil {
		return 0, err
	}

	f := r.Fixed(1)

	if f == nil {
		return 0, codec.ErrTruncated
	}

	step := removalStep(f[0])

	if step < stepThree || step > stepDone {
		return 0, fmt.Errorf("%w: removal step %d", errMalformed, step)
	}

	return step, nil
}

func journalKey(id [16]byte) []byte {
	return keys.ID([]byte{keys.KindJ}, id[:])
}

func journalIndexKey(node entity.NodeID, device entity.JournalDir) []byte {
	return keys.String(keys.ID([]byte{keys.KindJD}, node[:]), []byte(device))
}

func shardRemovalKey(shard uint64) []byte {
	return append(shardKey(shard), keys.SHRm)
}

// JournalID is the identity recorded for node's device, for the engine's
// Open; zero when none is, which Open refuses. The device is matched byte for
// byte as RecordJournal recorded it.
func (l Lifecycle) JournalID(
	ctx context.Context,
	node entity.NodeID,
	device entity.JournalDir,
) (id [16]byte, err error) {
	err = l.s.kv.View(ctx, func(r kv.Reader) error {
		b, ok, err := get(r, journalIndexKey(node, device))
		if err != nil || !ok {
			return err
		}

		id, err = decodeJournalIndex(b)

		return err
	})

	return id, err
}

// RecordJournal records journal id as living on node's device, the store's
// part of creating or adopting a journal: the initialisation step calls it
// after the journal's Init, and a recovery import for each journal it adopts.
// The engine never does. In one transaction it writes J‖journal with the node
// and device and JD‖node‖device with the identity. Recording a device again,
// as its re-initialisation does, deletes the J‖journal of the identity the
// device's index named. The device is kept as given: the bootstrap, which
// builds the paths, names each device one way.
//
// decision: recording a journal that J‖journal places on another node or
// directory also deletes that older index key, so each journal keeps one and
// the old node or path never opens it. RFC 16 says nothing on that key;
// withdraw this if it settles it.
//
// decision: a re-initialised device rewrites no shard record, because no
// shard record names its primary's journal yet. Withdraw this when the shard
// record carries that identity: this call must then rewrite it in the same
// transaction.
func (s *Store) RecordJournal(
	ctx context.Context,
	id [16]byte,
	node entity.NodeID,
	device entity.JournalDir,
) error {
	return s.kv.Update(ctx, func(tx kv.Txn) error {
		index := journalIndexKey(node, device)

		b, ok, err := get(tx, index)
		if err != nil {
			return err
		}

		if ok {
			old, err := decodeJournalIndex(b)
			if err != nil {
				return err
			}

			if err := tx.Delete(journalKey(old)); err != nil {
				return err
			}
		}

		b, ok, err = get(tx, journalKey(id))
		if err != nil {
			return err
		}

		if ok {
			prev, err := decodeJournal(b)
			if err != nil {
				return err
			}

			// When the journal stays where it was, this is the index key
			// itself, which the Set below writes again.
			if err := tx.Delete(journalIndexKey(prev.Node, prev.Device)); err != nil {
				return err
			}
		}

		rec := encodeJournal(journalRecord{
			Node:   node,
			Device: device,
		})

		if err := tx.Set(journalKey(id), rec); err != nil {
			return err
		}

		return tx.Set(index, encodeJournalIndex(id))
	})
}

// Tags is every share record of the installation, attached or not, being
// removed or retired, with its journal tag and how far its removal has gone,
// read from every one of its shards' rm rows (RFC 16 §2.3.2): Forgotten when
// every row is at forgotten or later, Offloaded when every row is at
// offloaded or later, and Serving otherwise, a shard with no row included.
//
// decision: start learns the removals and the installation's tags from this
// one store read, keyed by ShareID because a resumed removal's Drop names only
// the share, rather than from the node handing them to Start: the source
// RFC 16 §3 names. RFC 16 §3 still calls it a proposal, settled when the
// engine's start is built; withdraw this if it takes the other reading.
//
// ponytail: seeks once per share in the store and reads each one's shard
// rows, as nothing indexes the share records by tag. Nothing writes an rm row
// yet, as only a removal's mark runs, so every share reads Serving until the
// removal's per-shard steps record their progress there in this format. One
// View reads every share, so on a backend whose transactions have an age
// limit (FoundationDB's five seconds) an installation with enough shares
// fails it, and start fails with it; read in batches of shares, each its own
// View, once one does.
func (l Lifecycle) Tags(ctx context.Context) (map[entity.ShareID]entity.TagRecord, error) {
	var out map[entity.ShareID]entity.TagRecord
	err := l.s.kv.View(ctx, func(r kv.Reader) error {
		out = map[entity.ShareID]entity.TagRecord{}
		prefix := []byte{keys.KindS}
		var after []byte

		for {
			id, found, err := nextShare(r, prefix, after)
			if err != nil {
				return err
			}

			if !found {
				return nil
			}

			after = shareKey(id, 0xFF) // past every key of the share: no sub-kind is 0xFF

			b, ok, err := get(r, shareKey(id, keys.SInfo))
			if err != nil {
				return err
			}

			if !ok {
				continue
			}

			sh, err := decodeShareInfo(id, b)
			if err != nil {
				return err
			}

			state, err := tagState(r, id)
			if err != nil {
				return err
			}

			out[id] = entity.TagRecord{
				Tag:   sh.Tag,
				State: state,
			}
		}
	})

	return out, err
}

// nextShare is the first share with a key under prefix past after.
func nextShare(r kv.Reader, prefix, after []byte) (id entity.ShareID, found bool, err error) {
	for row, err := range r.Scan(prefix, after) {
		if err != nil {
			return id, false, err
		}

		if len(row.Key) < 1+len(id) {
			return id, false, merrs.NewInvalidArgumentError("a share key shorter than its ID")
		}

		copy(id[:], row.Key[1:])

		return id, true, nil
	}

	return id, false, nil
}

// tagState is how far share's removal has gone: the least step any of its
// shards' rm rows reached, a shard with no row counting as not started.
func tagState(r kv.Reader, share entity.ShareID) (entity.TagState, error) {
	least, sharded := stepDone, false

	for row, err := range r.Scan(shardsPrefix(share), nil) {
		if err != nil {
			return 0, err
		}

		shard, rest, err := keys.ReadUint64(row.Key[len(shardsPrefix(share)):])
		if err != nil || len(rest) != 0 {
			return 0, merrs.NewInvalidArgumentError("a share's shard row does not decode")
		}

		sharded = true

		b, ok, err := get(r, shardRemovalKey(shard))
		if err != nil {
			return 0, err
		}

		if !ok {
			return entity.TagServing, nil
		}

		step, err := decodeRemovalStep(b)
		if err != nil {
			return 0, err
		}

		least = min(least, step)
	}

	switch {
	case !sharded || least < stepOffloaded:
		return entity.TagServing, nil
	case least < stepForgotten:
		return entity.TagOffloaded, nil
	}

	return entity.TagForgotten, nil
}

// VersionLimits is each share's version limit, for the floor the engine's
// Open is given; zero for a share none was written for.
//
// ponytail: one View reads every named share's limit: Tags' age-limit ceiling.
func (l Lifecycle) VersionLimits(
	ctx context.Context,
	shares []entity.ShareID,
) (map[entity.ShareID]entity.JournalVersion, error) {
	var out map[entity.ShareID]entity.JournalVersion
	err := l.s.kv.View(ctx, func(r kv.Reader) error {
		out = make(map[entity.ShareID]entity.JournalVersion, len(shares))

		for _, id := range shares {
			b, ok, err := get(r, versionLimitKey(id))
			if err != nil {
				return err
			}

			var v entity.JournalVersion

			if ok {
				if v, err = decodeVersion(b); err != nil {
					return err
				}
			}

			out[id] = v
		}

		return nil
	})

	return out, err
}

// RaiseVersionLimits raises each share's version limit to its value in to,
// a tracked max-write: it reads the stored limit in the transaction that
// writes it and never lowers it (RFC 8 §2.7). The shares go in batches of
// half the key budget, so a batch writes at most K/2 keys and reads three
// keys a share with tracking (its info, its state and its limit), and no
// transaction holds every share. A share being removed or retired is
// skipped, since nothing attaches it again and it must not fail the raise for
// the shares batched with it, and so is one with no record, which a removal
// leaves at its last step (RFC 16 §2.3.2), its ShareID never reused. A batch
// that fails leaves the earlier ones raised: a raise is safe to run again.
func (l Lifecycle) RaiseVersionLimits(
	ctx context.Context,
	to map[entity.ShareID]entity.JournalVersion,
) error {
	ids := make([]entity.ShareID, 0, len(to))

	for id := range to {
		ids = append(ids, id)
	}

	slices.SortFunc(ids, func(a, b entity.ShareID) int {
		return bytes.Compare(a[:], b[:])
	})

	batch := max(1, l.s.k/2)

	for chunk := range slices.Chunk(ids, batch) {
		err := l.s.kv.Update(ctx, func(tx kv.Txn) error {
			for _, id := range chunk {
				err := raiseVersionLimitIn(tx, id, to[id])
				if errors.Is(err, entity.ErrStale) || merrs.IsNotFoundError(err) {
					continue
				}

				if err != nil {
					return err
				}
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// raiseVersionLimitIn raises share's limit to v in tx, a tracked max-write.
// It refuses a share being removed or retired with ErrStale and one with no
// record as not found, writing nothing. The state is read first: a removal
// deletes the share's other records before it (RFC 16 §2.3.2, step 6), so a
// share part way through that step reads stale, not missing.
func raiseVersionLimitIn(tx kv.Txn, share entity.ShareID, v entity.JournalVersion) error {
	if err := liveShare(tx, share); err != nil {
		return err
	}

	_, ok, err := get(tx, shareKey(share, keys.SInfo))
	if err != nil {
		return err
	}

	if !ok {
		return merrs.NewNotFoundError(share.String(), "share")
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
}

// RaiseAtStart raises each named shard's incarnation by one, in one
// transaction, before the node answers any open-state request for them, so
// no incarnation and no grace instance repeats across starts. A shard named
// twice is raised once; one past the 24-bit shard space refuses the call. It
// returns the incarnation it wrote for each shard.
//
// decision: this raise also opens a new grace instance, GraceInstance =
// Incarnation, because the shard's open state is lost: a single node's start
// loses every shard's, the table living in the process that stopped, and a
// shard this process has not raised yet lost its with that restart, or is
// new, cloned or restored and has none (RFC 14 §4.4). A shard whose open
// state this process holds is raised by RaiseAtAttach instead, which keeps the
// grace instance, or every client of the shard would reclaim for nothing.
// Revisit when a handover ships the table, which keeps it too.
//
// ponytail: every shard goes in one transaction, with no split by the key
// budget, and it raises nothing else. The start transaction also raises the
// node epoch: move this raise into it when the node record lands. Splitting it
// once a node serves more shards than one transaction holds needs the
// one-transaction start rule changed first.
func (l Lifecycle) RaiseAtStart(
	ctx context.Context,
	shards []entity.ShardID,
) (map[entity.ShardID]uint64, error) {
	ids := slices.Clone(shards)
	slices.Sort(ids)
	ids = slices.Compact(ids)

	for _, id := range ids {
		if id >= entity.ShardLimit {
			msg := fmt.Sprintf("shard %d past the 24-bit shard space", id)

			return nil, merrs.NewInvalidArgumentError(msg)
		}
	}

	var out map[entity.ShardID]uint64
	err := l.s.kv.Update(ctx, func(tx kv.Txn) error {
		out = make(map[entity.ShardID]uint64, len(ids))

		for _, id := range ids {
			sh, err := shardIn(tx, id)
			if err != nil {
				return err
			}

			sh.Incarnation++
			sh.GraceInstance = sh.Incarnation

			if err := tx.Set(shardKey(uint64(id)), encodeShard(sh)); err != nil {
				return err
			}

			out[id] = sh.Incarnation
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// RaiseAtAttach raises the shard's incarnation by one and keeps its grace
// instance, for an attach of a shard whose open state this process holds,
// and returns the incarnation it wrote. That shard was raised already, by
// this process or by the node whose handover shipped its table (RFC 16 §3),
// so one with no shard record is refused as not found, as is one past the
// 24-bit shard space, and neither writes anything.
func (l Lifecycle) RaiseAtAttach(ctx context.Context, shard entity.ShardID) (inc uint64, err error) {
	if shard >= entity.ShardLimit {
		msg := fmt.Sprintf("shard %d past the 24-bit shard space", shard)

		return 0, merrs.NewInvalidArgumentError(msg)
	}

	err = l.s.kv.Update(ctx, func(tx kv.Txn) error {
		_, ok, err := get(tx, shardKey(uint64(shard)))
		if err != nil {
			return err
		}

		if !ok {
			return merrs.NewNotFoundError(fmt.Sprint(shard), "shard")
		}

		sh, err := shardIn(tx, shard)
		if err != nil {
			return err
		}

		sh.Incarnation++
		inc = sh.Incarnation

		return tx.Set(shardKey(uint64(shard)), encodeShard(sh))
	})

	return inc, err
}
