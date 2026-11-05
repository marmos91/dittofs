package kvstore

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
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
