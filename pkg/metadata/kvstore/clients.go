package kvstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// ErrRecordChanged is a conditional client write whose expected record is no
// longer the stored one: the call writes nothing and returns the current one.
var ErrRecordChanged = errors.New("kvstore: client record changed")

// MaxAdapter bounds a client record's Adapter bytes: a write over it is
// refused as invalid.
const MaxAdapter = 4 << 10

// Clients is open state's view of its durable client records: one per
// client instance, found by ID or by its protocol and name, with one key per
// shard it holds state in, whose value is that shard's Done instance. Every
// ID comes from NextID(IDClient), issued before the record is written, since
// a client may be told its ID before it confirms. The decisions — which Boot
// is a reboot, which principal may take a name — are open state's; this view
// keeps the records consistent with each other.
type Clients struct{ s *Store }

func (s *Store) Clients() Clients { return Clients{s} }

// Create records a new client with its name index row. A name already
// recorded under its protocol, or an ID already recorded, is refused as
// existing: a reboot replaces its record through Reboot. Shards is not
// written: a client's shard marks change only through MarkShard and
// DropShards.
func (c Clients) Create(ctx context.Context, cl entity.ClientRecord) error {
	if err := validClient(cl); err != nil {
		return err
	}
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		name := clientNameKey(cl.Protocol, cl.Name)
		if _, ok, err := get(tx, name); err != nil {
			return err
		} else if ok {
			return merrs.NewAlreadyExistsError(fmt.Sprintf("%s client %x", cl.Protocol, cl.Name))
		}
		if err := tx.Set(name, encodeClientID(cl.ID)); err != nil {
			return err
		}
		return putClient(tx, cl)
	})
}

// Reboot replaces old, the record of a client that came back with a new
// Boot, by cl under its own new ID: the name index moves to it, and old goes
// with its shard marks, in one transaction. A reboot keeps the protocol and
// name and changes the Boot. Shards is not written: the new ID holds no
// state anywhere.
func (c Clients) Reboot(ctx context.Context, old entity.ClientID, cl entity.ClientRecord) error {
	if err := validClient(cl); err != nil {
		return err
	}
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		prev, err := clientIn(tx, old)
		if err != nil {
			return err
		}
		if prev.Protocol != cl.Protocol || !bytes.Equal(prev.Name, cl.Name) || prev.Boot == cl.Boot || cl.ID == old {
			return merrs.NewInvalidArgumentError("a reboot keeps the client's protocol and name and changes its boot and ID")
		}
		// decision: a reboot deletes the old record and its shard marks at once,
		// because on one node the table has already released the old ID's state
		// everywhere. Once a shard's primary can be another node, keep the old
		// record marked Ended until every primary its Shards names has
		// acknowledged, or the fan-out loses its list.
		if err := dropClient(tx, prev, false); err != nil {
			return err
		}
		if err := tx.Set(clientNameKey(cl.Protocol, cl.Name), encodeClientID(cl.ID)); err != nil {
			return err
		}
		return putClient(tx, cl)
	})
}

// Get reads one client record, its shard marks included.
func (c Clients) Get(ctx context.Context, id entity.ClientID) (cl entity.ClientRecord, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		cl, err = clientIn(r, id)
		return err
	})
	return cl, err
}

// ByName finds the client a protocol's client name belongs to, after any
// restart: a client names itself by its own string, not by an ID the server
// gave.
func (c Clients) ByName(ctx context.Context, p entity.Protocol, name []byte) (cl entity.ClientRecord, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		b, ok, err := get(r, clientNameKey(p, name))
		if err != nil {
			return err
		}
		if !ok {
			return merrs.NewNotFoundError(fmt.Sprintf("%s client %x", p, name), "client")
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

// Put rewrites a client record from prev, the record the caller read, to
// next. When the stored record is no longer prev it writes nothing and
// returns ErrRecordChanged with the current record; when next changes no
// field it writes nothing. Its ID, protocol, name and Boot never change, so
// prev and next naming two IDs is refused as invalid before any read, and
// its shard marks change only through MarkShard and DropShards: Shards is
// neither compared nor written here.
func (c Clients) Put(ctx context.Context, prev, next entity.ClientRecord) (cur entity.ClientRecord, err error) {
	if err := validClient(next); err != nil {
		return entity.ClientRecord{}, err
	}
	if prev.ID != next.ID {
		return entity.ClientRecord{}, merrs.NewInvalidArgumentError("a put names one client in prev and next")
	}
	err = c.s.kv.Update(ctx, func(tx kv.Txn) error {
		if cur, err = clientIn(tx, next.ID); err != nil {
			return err
		}
		if !bytes.Equal(encodeClient(cur), encodeClient(prev)) {
			return ErrRecordChanged
		}
		if cur.Protocol != next.Protocol || !bytes.Equal(cur.Name, next.Name) || cur.Boot != next.Boot {
			return merrs.NewInvalidArgumentError("a client's protocol, name and boot are fixed")
		}
		next.Shards = cur.Shards
		if err := setClient(tx, cur, next); err != nil {
			return err
		}
		cur = next
		return nil
	})
	return cur, err
}

// AddSession records that node holds a session of the client and writes next
// as its Adapter bytes, if they still equal prev, the bytes the caller last
// read; otherwise it writes nothing and returns ErrRecordChanged with the
// current record. So a replayed setup call, on any node or after a restart,
// finds the first call's bytes rather than recording a second session.
func (c Clients) AddSession(ctx context.Context, id entity.ClientID, node entity.NodeID, prev, next []byte) (cur entity.ClientRecord, err error) {
	if len(next) > MaxAdapter {
		return entity.ClientRecord{}, merrs.NewInvalidArgumentError(fmt.Sprintf("adapter bytes over %d", MaxAdapter))
	}
	err = c.s.kv.Update(ctx, func(tx kv.Txn) error {
		if cur, err = clientIn(tx, id); err != nil {
			return err
		}
		if !bytes.Equal(cur.Adapter, prev) {
			return ErrRecordChanged
		}
		up := cur
		up.Adapter = next
		if !slices.Contains(cur.Reach.Sessions, node) {
			up.Reach.Sessions = append(slices.Clip(cur.Reach.Sessions), node)
		}
		if err := setClient(tx, cur, up); err != nil {
			return err
		}
		cur = up
		return nil
	})
	return cur, err
}

// MarkShard writes the client's mark in one shard: when it first takes state
// there, and when it finishes reclaiming there. The write commits before the
// answer granting the client its first state in the shard.
//
// ponytail: a client's shard list is unbounded. Record a client past a
// bound as holding state in every shard of the share, which widens its grace
// and never loses a reclaim, once shards exist beyond one per share.
func (c Clients) MarkShard(ctx context.Context, id entity.ClientID, m entity.ShardMark) error {
	if m.Shard >= entity.ShardLimit {
		return merrs.NewInvalidArgumentError(fmt.Sprintf("shard %d past the 24-bit shard space", m.Shard))
	}
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		if _, err := clientIn(tx, id); err != nil {
			return err
		}
		return tx.Set(clientShardKey(id, m.Shard), encodeCounter(m.Done))
	})
}

// DropShards deletes every shard mark of the client, as an administrator's
// revoke does, so a reclaim after a restart finds none and is refused.
func (c Clients) DropShards(ctx context.Context, id entity.ClientID) error {
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		if _, err := clientIn(tx, id); err != nil {
			return err
		}
		return deleteUnder(tx, clientShardsPrefix(id))
	})
}

// Delete removes a client record with its name row and shard marks: an
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

// ByShard lists every client with a mark in shard s, its marks included:
// the clients a shard beginning grace tells to reclaim. A record deleted
// between the scan and its read is skipped.
//
// ponytail: a scan of every client record and mark, since a mark's key puts
// the shard last; a shard runs it once, when its grace begins. Add a
// shard-first index, CLS‖shard‖clientID, written and deleted with each mark,
// if a profile of a shard beginning grace shows the scan.
func (c Clients) ByShard(ctx context.Context, s entity.ShardID) iter.Seq2[entity.ClientRecord, error] {
	mark := keys.Uint64([]byte{keys.CLSh}, uint64(s))
	ids := pages(ctx, c.s.kv, []byte{keys.KindCL}, nil, func(_ kv.Reader, row kv.KeyValue) (entity.ClientID, error) {
		id, rest, err := keys.ReadUint64(row.Key[1:])
		if err != nil {
			return 0, fmt.Errorf("%w: client key", errMalformed)
		}
		if !bytes.Equal(rest, mark) {
			return 0, nil // the record itself, or a mark in another shard
		}
		return entity.ClientID(id), nil
	})
	return func(yield func(entity.ClientRecord, error) bool) {
		for id, err := range ids {
			if err != nil {
				yield(entity.ClientRecord{}, err)
				return
			}
			if id == 0 {
				continue
			}
			cl, err := c.Get(ctx, id)
			if merrs.IsNotFoundError(err) {
				continue
			}
			if !yield(cl, err) || err != nil {
				return
			}
		}
	}
}

// validClient checks what every written record must hold. An owner mapped to
// the anonymous principal is recorded as zero, and protects nothing.
func validClient(cl entity.ClientRecord) error {
	switch {
	case cl.ID == 0 || cl.Protocol == 0 || len(cl.Name) == 0:
		return merrs.NewInvalidArgumentError("a client needs an ID, a protocol and a name")
	case len(cl.Adapter) > MaxAdapter:
		return merrs.NewInvalidArgumentError(fmt.Sprintf("adapter bytes over %d", MaxAdapter))
	case cl.Protect && cl.Owner == (entity.Principal{}):
		return merrs.NewInvalidArgumentError("state protection needs an owner")
	}
	return nil
}

// putClient writes a new record, without its shard marks, refusing an ID already
// recorded: an ID is issued once, so another record under it is a caller's
// mistake that would overwrite a live client.
func putClient(tx kv.Txn, cl entity.ClientRecord) error {
	if _, ok, err := get(tx, clientKey(cl.ID)); err != nil {
		return err
	} else if ok {
		return merrs.NewAlreadyExistsError(fmt.Sprintf("client %d", cl.ID))
	}
	return tx.Set(clientKey(cl.ID), encodeClient(cl))
}

// setClient rewrites a record only when one of its fields changes.
func setClient(tx kv.Txn, cur, next entity.ClientRecord) error {
	if b := encodeClient(next); !bytes.Equal(b, encodeClient(cur)) {
		return tx.Set(clientKey(next.ID), b)
	}
	return nil
}

func clientIn(r kv.Reader, id entity.ClientID) (entity.ClientRecord, error) {
	b, err := r.Get(clientKey(id))
	if errors.Is(err, kv.ErrNotFound) {
		return entity.ClientRecord{}, merrs.NewNotFoundError(fmt.Sprintf("client %d", id), "client")
	}
	if err != nil {
		return entity.ClientRecord{}, err
	}
	cl, err := decodeClient(id, b)
	if err != nil {
		return entity.ClientRecord{}, err
	}
	pre := clientShardsPrefix(id)
	for row, err := range r.Scan(pre, nil) {
		if err != nil {
			return entity.ClientRecord{}, err
		}
		shard, rest, err := keys.ReadUint64(row.Key[len(pre):])
		if err != nil || len(rest) != 0 || shard >= uint64(entity.ShardLimit) {
			return entity.ClientRecord{}, fmt.Errorf("%w: client shard key", errMalformed)
		}
		done, err := decodeCounter(row.Value)
		if err != nil {
			return entity.ClientRecord{}, err
		}
		cl.Shards = append(cl.Shards, entity.ShardMark{Shard: entity.ShardID(shard), Done: done})
	}
	return cl, nil
}

// dropClient deletes a client's record and shard marks, and its name row
// when name is set; a reboot keeps the row for the record replacing it.
func dropClient(tx kv.Txn, cl entity.ClientRecord, name bool) error {
	if name {
		if err := tx.Delete(clientNameKey(cl.Protocol, cl.Name)); err != nil {
			return err
		}
	}
	if err := deleteUnder(tx, clientShardsPrefix(cl.ID)); err != nil {
		return err
	}
	return tx.Delete(clientKey(cl.ID))
}
