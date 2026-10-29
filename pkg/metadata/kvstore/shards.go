package kvstore

import (
	"context"
	"fmt"
	"slices"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/codec"
)

// Shard reads a shard's record: its incarnation and grace instance, which
// the start passes on to the shard's recovery. A shard never raised reads
// as zero in both.
func (s *Store) Shard(ctx context.Context, id entity.ShardID) (sh entity.Shard, err error) {
	err = s.kv.View(ctx, func(r kv.Reader) (err error) {
		sh, err = shardIn(r, id)
		return err
	})
	return sh, err
}

// RaiseIncarnations raises each named shard's incarnation by one, in one
// transaction, before the node answers any open-state request for them, so
// no incarnation and no grace instance repeats across starts. A shard named
// twice is raised once; one past the 24-bit shard space refuses the call.
//
// decision: every raise also opens a new grace instance, GraceInstance =
// Incarnation, because a single node's start always loses the shard's open
// state: the table lives in the process that stopped. Only a start calls it.
// A journal attach and a handover that ships the table raise the incarnation
// and keep the grace instance, so neither may come through here, or every
// client of the shard would reclaim for nothing.
//
// ponytail: every shard goes in one transaction, with no split by the key
// budget, and it raises nothing else. The start transaction also raises the
// node epoch: move this raise into it when the node record lands. Splitting it
// once a node serves more shards than one transaction holds needs the
// one-transaction start rule changed first.
func (s *Store) RaiseIncarnations(ctx context.Context, shards []entity.ShardID) error {
	ids := slices.Clone(shards)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, id := range ids {
		if id >= entity.ShardLimit {
			return merrs.NewInvalidArgumentError(fmt.Sprintf("shard %d past the 24-bit shard space", id))
		}
	}
	return s.kv.Update(ctx, func(tx kv.Txn) error {
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
		}
		return nil
	})
}

func shardIn(r kv.Reader, id entity.ShardID) (entity.Shard, error) {
	b, ok, err := get(r, shardKey(uint64(id)))
	if err != nil || !ok {
		return entity.Shard{ID: id}, err
	}
	return decodeShard(id, b)
}

func encodeShard(sh entity.Shard) []byte {
	return codec.NewWriter(v1).Uint64(sh.Incarnation).Uint64(sh.GraceInstance).Encode()
}

// decodeShard reads a shard record's value; its ID is its key's.
func decodeShard(id entity.ShardID, b []byte) (entity.Shard, error) {
	r, err := open(b)
	if err != nil {
		return entity.Shard{}, err
	}
	sh := entity.Shard{ID: id, Incarnation: r.Uint64(), GraceInstance: r.Uint64()}
	return sh, r.Done()
}
