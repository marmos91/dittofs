package kvstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// Clients is open state's view of its durable client records: one per
// client instance, found by ID or by the client's own name for itself, and
// one per shard the client has held state in. The decisions — which
// verifier is a reboot, which principal may take an owner — are the open
// state service's; this view keeps the records consistent with each other.
type Clients struct{ s *Store }

func (s *Store) Clients() Clients { return Clients{s} }

// Create records a new client under a freshly issued ID, with its owner
// index row, and returns it. An owner already recorded under its protocol
// is refused as existing: a reboot replaces its record through Reboot.
func (c Clients) Create(ctx context.Context, cl entity.Client) (entity.Client, error) {
	if len(cl.Owner) == 0 || cl.Protocol == 0 {
		return entity.Client{}, merrs.NewInvalidArgumentError("a client needs a protocol and an owner")
	}
	id, err := c.s.NextID(ctx, IDClient)
	if err != nil {
		return entity.Client{}, err
	}
	cl.ID = entity.ClientID(id)
	return cl, c.s.kv.Update(ctx, func(tx kv.Txn) error {
		owner := clientOwnerKey(cl.Protocol, cl.Owner)
		if _, ok, err := get(tx, owner); err != nil {
			return err
		} else if ok {
			return merrs.NewAlreadyExistsError(fmt.Sprintf("%s client %x", cl.Protocol, cl.Owner))
		}
		if err := tx.Set(owner, encodeClientID(cl.ID)); err != nil {
			return err
		}
		return tx.Set(clientKey(cl.ID), encodeClient(cl))
	})
}

// Reboot replaces old, the record of a client that came back with a new
// boot verifier, by cl under a new ID: the owner index moves to it, and old
// goes with its shard records, in one transaction.
func (c Clients) Reboot(ctx context.Context, old entity.ClientID, cl entity.Client) (entity.Client, error) {
	id, err := c.s.NextID(ctx, IDClient)
	if err != nil {
		return entity.Client{}, err
	}
	cl.ID = entity.ClientID(id)
	return cl, c.s.kv.Update(ctx, func(tx kv.Txn) error {
		prev, err := clientIn(tx, old)
		if err != nil {
			return err
		}
		if prev.Protocol != cl.Protocol || !bytes.Equal(prev.Owner, cl.Owner) {
			return merrs.NewInvalidArgumentError("a reboot keeps the client's protocol and owner")
		}
		if err := dropClient(tx, prev, false); err != nil {
			return err
		}
		if err := tx.Set(clientOwnerKey(cl.Protocol, cl.Owner), encodeClientID(cl.ID)); err != nil {
			return err
		}
		return tx.Set(clientKey(cl.ID), encodeClient(cl))
	})
}

// Get reads one client record.
func (c Clients) Get(ctx context.Context, id entity.ClientID) (cl entity.Client, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		cl, err = clientIn(r, id)
		return err
	})
	return cl, err
}

// ByOwner finds the client a protocol's owner name belongs to, after any
// restart: EXCHANGE_ID, SETCLIENTID and an SMB negotiate name a client this
// way, not by an ID the server gave.
func (c Clients) ByOwner(ctx context.Context, p entity.Protocol, owner []byte) (cl entity.Client, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		b, ok, err := get(r, clientOwnerKey(p, owner))
		if err != nil {
			return err
		}
		if !ok {
			return merrs.NewNotFoundError(fmt.Sprintf("%s client %x", p, owner), "client")
		}
		id, err := decodeClientID(b)
		if err != nil {
			return err
		}
		cl, err = clientIn(r, id)
		return err
	})
	return cl, err
}

// Put rewrites a client record, and writes nothing when no field changed:
// the record is rewritten only when one does. Its protocol and owner, which
// the index names it by, never change.
func (c Clients) Put(ctx context.Context, cl entity.Client) error {
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		prev, err := clientIn(tx, cl.ID)
		if err != nil {
			return err
		}
		if prev.Protocol != cl.Protocol || !bytes.Equal(prev.Owner, cl.Owner) {
			return merrs.NewInvalidArgumentError("a client's protocol and owner are fixed")
		}
		if b := encodeClient(cl); !bytes.Equal(b, encodeClient(prev)) {
			return tx.Set(clientKey(cl.ID), b)
		}
		return nil
	})
}

// Delete removes a client record with its owner row and shard records: an
// expired or destroyed client, whose state is already released.
func (c Clients) Delete(ctx context.Context, id entity.ClientID) error {
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		cl, err := clientIn(tx, id)
		if err != nil {
			return err
		}
		return dropClient(tx, cl, true)
	})
}

// CreateSession applies one NFSv4.1 CREATE_SESSION's sequence ID in the
// client's record. A replay of the last one applied, the same sequence ID,
// returns its cached reply; the next one records reply as the new cache and
// returns nil; any other is misordered. A replay that reaches another node,
// or arrives after a restart, so gets the first reply, not a second session.
func (c Clients) CreateSession(ctx context.Context, id entity.ClientID, seq uint32, reply []byte) (cached []byte, err error) {
	err = c.s.kv.Update(ctx, func(tx kv.Txn) error {
		cached = nil
		cl, err := clientIn(tx, id)
		if err != nil {
			return err
		}
		switch seq {
		case cl.CSSeq:
			cached = slices.Clone(cl.CSReply)
			return nil
		case cl.CSSeq + 1:
			cl.CSSeq, cl.CSReply = seq, reply
			return tx.Set(clientKey(id), encodeClient(cl))
		default:
			return entity.ErrSeqMisordered
		}
	})
	return cached, err
}

// PutShard writes the client's record in one shard: when it first takes
// state there, and when its reclaim or loss state there changes.
//
// ponytail: a client's shard list is unbounded. Record a client past a
// bound as holding state in every shard of the share, which widens its grace
// and never loses a reclaim, once shards exist beyond one per share.
func (c Clients) PutShard(ctx context.Context, id entity.ClientID, sh entity.ClientShard) error {
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		if _, err := clientIn(tx, id); err != nil {
			return err
		}
		return tx.Set(clientShardKey(id, sh.Shard), encodeClientShard(sh))
	})
}

// Shards lists the client's shard records in shard order: where it may
// reclaim.
func (c Clients) Shards(ctx context.Context, id entity.ClientID) (out []entity.ClientShard, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		out = nil
		pre := clientShardsPrefix(id)
		for row, err := range r.Scan(pre, nil) {
			if err != nil {
				return err
			}
			shard, err := shardOf(pre, row.Key)
			if err != nil {
				return err
			}
			sh, err := decodeClientShard(shard, row.Value)
			if err != nil {
				return err
			}
			out = append(out, sh)
		}
		return nil
	})
	return out, err
}

func shardOf(pre, key []byte) (uint64, error) {
	shard, rest, err := keys.ReadUint64(key[len(pre):])
	if err != nil || len(rest) != 0 {
		return 0, fmt.Errorf("%w: client shard key", errMalformed)
	}
	return shard, nil
}

func clientIn(r kv.Reader, id entity.ClientID) (entity.Client, error) {
	b, err := r.Get(clientKey(id))
	if errors.Is(err, kv.ErrNotFound) {
		return entity.Client{}, merrs.NewNotFoundError(fmt.Sprintf("client %d", id), "client")
	}
	if err != nil {
		return entity.Client{}, err
	}
	return decodeClient(id, b)
}

// dropClient deletes a client's record and shard records, and its owner row
// when owner is set; a reboot keeps the row for the record replacing it.
func dropClient(tx kv.Txn, cl entity.Client, owner bool) error {
	if owner {
		if err := tx.Delete(clientOwnerKey(cl.Protocol, cl.Owner)); err != nil {
			return err
		}
	}
	if err := deleteUnder(tx, clientShardsPrefix(cl.ID)); err != nil {
		return err
	}
	return tx.Delete(clientKey(cl.ID))
}
