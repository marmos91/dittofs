package kvstore

import (
	"context"
	"iter"
	"slices"
	"strconv"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// ControlPlane is the management API's view. Every write names what it
// changes: a user rename writes the user record and its name rows, and
// nothing under any file's prefix.
type ControlPlane struct{ s *Store }

func (s *Store) ControlPlane() ControlPlane { return ControlPlane{s} }

// Users lists users in name order, resuming after the name after.
func (c ControlPlane) Users(ctx context.Context, after string) iter.Seq2[entity.User, error] {
	return pages(ctx, c.s.kv, []byte{keys.KindNX, keys.NXUser}, nameCursor(keys.NXUser, after),
		func(r kv.Reader, row kv.KeyValue) (entity.User, error) {
			id, b, err := byIndex(r, row, userKey)
			if err != nil {
				return entity.User{}, err
			}
			return decodeUser(id, b)
		})
}

// Groups lists groups in name order, resuming after the name after.
func (c ControlPlane) Groups(ctx context.Context, after string) iter.Seq2[entity.Group, error] {
	return pages(ctx, c.s.kv, []byte{keys.KindNX, keys.NXGroup}, nameCursor(keys.NXGroup, after),
		func(r kv.Reader, row kv.KeyValue) (entity.Group, error) {
			id, b, err := byIndex(r, row, groupKey)
			if err != nil {
				return entity.Group{}, err
			}
			return decodeGroup(id, b)
		})
}

func nameCursor(kind byte, after string) []byte {
	if after == "" {
		return nil
	}
	return nameKey(kind, after)
}

// byIndex follows a name index row to the record it names.
func byIndex(r kv.Reader, row kv.KeyValue, key func(entity.PrincipalID) []byte) (entity.PrincipalID, []byte, error) {
	id, err := decodeID(row.Value)
	if err != nil {
		return id, nil, err
	}
	b, err := r.Get(key(id))
	return id, b, err
}

func (c ControlPlane) PutUser(ctx context.Context, u entity.User) error {
	if u.Principal.Kind != entity.PrincipalUser || u.Principal.ID.IsZero() || u.Name == "" {
		return merrs.NewInvalidArgumentError("a user needs a user principal and a name")
	}
	if !u.Primary.IsZero() && u.Primary.Kind != entity.PrincipalGroup {
		return merrs.NewInvalidArgumentError("a user's primary group must be a group")
	}
	if err := validIDs(u.IDs, entity.SchemeUID, entity.SchemeSID, entity.SchemeKerberos); err != nil {
		return err
	}
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		key := userKey(u.Principal.ID)
		b, existed, err := get(tx, key)
		if err != nil {
			return err
		}
		var old entity.User
		if existed {
			if old, err = decodeUser(u.Principal.ID, b); err != nil {
				return err
			}
		}
		if err := reindex(tx, u.Principal, existed, old.Name, u.Name, old.IDs, u.IDs); err != nil {
			return err
		}
		return tx.Set(key, encodeUser(u))
	})
}

func (c ControlPlane) PutGroup(ctx context.Context, g entity.Group) error {
	if g.Principal.Kind != entity.PrincipalGroup || g.Principal.ID.IsZero() || g.Name == "" {
		return merrs.NewInvalidArgumentError("a group needs a group principal and a name")
	}
	if err := validIDs(g.IDs, entity.SchemeGID, entity.SchemeSID); err != nil {
		return err
	}
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		key := groupKey(g.Principal.ID)
		b, existed, err := get(tx, key)
		if err != nil {
			return err
		}
		var old entity.Group
		if existed {
			if old, err = decodeGroup(g.Principal.ID, b); err != nil {
				return err
			}
		}
		if err := reindex(tx, g.Principal, existed, old.Name, g.Name, old.IDs, g.IDs); err != nil {
			return err
		}
		return tx.Set(key, encodeGroup(g))
	})
}

// rootID and anonymousID are the uid and gid of root and of the anonymous
// caller, held by rule rather than as records.
const (
	rootID      = 0
	anonymousID = 65534
)

// validIDs refuses a scheme the principal's kind may not carry, an empty
// spelling, a UID or GID not in canonical decimal, and a repeated ID: each
// spelling has one index row, so it must have one form. It also refuses uid
// and gid 0 and 65534, which are root's and anonymous's by rule and never a
// stored principal's, so no record can claim root's privileges or stand in for
// the anonymous caller.
func validIDs(ids entity.ProtocolIDs, schemes ...entity.ProtocolScheme) error {
	for i, id := range ids {
		if !slices.Contains(schemes, id.Scheme) || id.Value == "" {
			return merrs.NewInvalidArgumentError("protocol ID " + id.Scheme.String() + " not valid here")
		}
		if id.Scheme == entity.SchemeUID || id.Scheme == entity.SchemeGID {
			n, err := strconv.ParseUint(id.Value, 10, 32)
			if err != nil || strconv.FormatUint(n, 10) != id.Value {
				return merrs.NewInvalidArgumentError(id.Scheme.String() + " " + id.Value + " is not a canonical decimal")
			}
			if n == rootID || n == anonymousID {
				return merrs.NewInvalidArgumentError(id.Scheme.String() + " " + id.Value + " is reserved")
			}
		}
		if slices.Contains(ids[:i], id) {
			return merrs.NewInvalidArgumentError("protocol ID repeated")
		}
	}
	return nil
}

// reindex moves p's name and protocol-ID rows from the old record's to the
// new one's, refusing a name or ID another principal holds. Each row is read
// before it is claimed, so of two principals racing for one row, the second
// to commit conflicts and then finds it taken. A row p already holds is left
// unwritten.
func reindex(tx kv.Txn, p entity.Principal, existed bool, oldName, newName string, oldIDs, newIDs entity.ProtocolIDs) error {
	kind := nameKind(p.Kind)
	if !existed || oldName != newName {
		if err := claim(tx, nameKey(kind, newName), "name "+newName, p, encodeID(p.ID), decodeIDAs(p.Kind)); err != nil {
			return err
		}
		if existed {
			if err := tx.Delete(nameKey(kind, oldName)); err != nil {
				return err
			}
		}
	}
	for _, id := range newIDs {
		if !slices.Contains(oldIDs, id) {
			if err := claim(tx, protocolKey(id), id.Scheme.String()+" "+id.Value, p, encodePrincipal(p), decodePrincipal); err != nil {
				return err
			}
		}
	}
	for _, id := range oldIDs {
		if !slices.Contains(newIDs, id) {
			if err := tx.Delete(protocolKey(id)); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeIDAs(kind entity.PrincipalKind) func([]byte) (entity.Principal, error) {
	return func(b []byte) (entity.Principal, error) {
		id, err := decodeID(b)
		return entity.Principal{Kind: kind, ID: id}, err
	}
}

// claim writes an index row for p, refusing one that names another
// principal; what names the row in the refusal.
func claim(tx kv.Txn, key []byte, what string, p entity.Principal, value []byte, decode func([]byte) (entity.Principal, error)) error {
	b, ok, err := get(tx, key)
	if err != nil {
		return err
	}
	if ok {
		holder, err := decode(b)
		if err != nil {
			return err
		}
		if holder != p {
			return merrs.NewAlreadyExistsError(what)
		}
		return nil
	}
	return tx.Set(key, value)
}

func (c ControlPlane) DeleteUser(ctx context.Context, p entity.Principal) error {
	if p.Kind != entity.PrincipalUser {
		return merrs.NewInvalidArgumentError("not a user")
	}
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		b, ok, err := get(tx, userKey(p.ID))
		if err != nil || !ok {
			return notFound(err, p)
		}
		u, err := decodeUser(p.ID, b)
		if err != nil {
			return err
		}
		return deletePrincipal(tx, p, userKey(p.ID), u.Name, u.IDs)
	})
}

func (c ControlPlane) DeleteGroup(ctx context.Context, p entity.Principal) error {
	if p.Kind != entity.PrincipalGroup {
		return merrs.NewInvalidArgumentError("not a group")
	}
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		b, ok, err := get(tx, groupKey(p.ID))
		if err != nil || !ok {
			return notFound(err, p)
		}
		g, err := decodeGroup(p.ID, b)
		if err != nil {
			return err
		}
		// Its members' rows, both directions.
		if err := dropPairs(tx, membersPrefix(p.ID), func(m []byte) []byte {
			return memberRevKey(entity.PrincipalID(m), p.ID)
		}); err != nil {
			return err
		}
		return deletePrincipal(tx, p, groupKey(p.ID), g.Name, g.IDs)
	})
}

func notFound(err error, p entity.Principal) error {
	if err != nil {
		return err
	}
	return merrs.NewNotFoundError(p.String(), "principal")
}

// deletePrincipal removes p's record, its index rows, the groups it is a
// member of, its grants and its quotas. Files keep naming p, as an unknown
// principal. The membership and grant rows are found by scan, so their ranges are
// guarded: a membership or grant added for p after this transaction's
// snapshot makes one of the two conflict, rather than leave a row naming a
// principal that no longer exists.
//
// ponytail: one transaction for every row, so a principal in more groups or
// with more grants than one transaction holds fails with kv.ErrTooLarge.
// Delete in batches sized from the store's limits, the record last, when a
// principal can reach that many rows.
func deletePrincipal(tx kv.Txn, p entity.Principal, record []byte, name string, ids entity.ProtocolIDs) error {
	if err := dropQuotas(tx, p); err != nil {
		return err
	}
	if err := dropPairs(tx, memberOfPrefix(p.ID), func(g []byte) []byte {
		return memberKey(entity.PrincipalID(g), p.ID)
	}); err != nil {
		return err
	}
	if err := dropPairs(tx, grantsOfPrefix(p.ID), func(share []byte) []byte {
		return grantKey(entity.ShareID(share), p.ID)
	}); err != nil {
		return err
	}
	if err := tx.Delete(nameKey(nameKind(p.Kind), name)); err != nil {
		return err
	}
	for _, id := range ids {
		if err := tx.Delete(protocolKey(id)); err != nil {
			return err
		}
	}
	return tx.Delete(record)
}

// dropPairs deletes every row under prefix, whose key ends in one ID, and
// the row twin names for that ID: a two-way index's other direction. The
// prefix is guarded first.
func dropPairs(tx kv.Txn, prefix []byte, twin func(id []byte) []byte) error {
	if err := tx.GuardRange(prefix); err != nil {
		return err
	}
	var rows [][]byte
	for row, err := range tx.Scan(prefix, nil) {
		if err != nil {
			return err
		}
		rows = append(rows, slices.Clone(row.Key))
	}
	for _, k := range rows {
		id, _, err := keys.ReadID(k[len(prefix):], keys.IDLen)
		if err != nil {
			return err
		}
		if err := tx.Delete(k); err != nil {
			return err
		}
		if err := tx.Delete(twin(id)); err != nil {
			return err
		}
	}
	return nil
}

// AddMember adds a user or a nested group to a group. Both must exist; the
// reads that check so conflict with a deletion of either.
func (c ControlPlane) AddMember(ctx context.Context, m entity.Membership) error {
	if m.Group.Kind != entity.PrincipalGroup || (m.Member.Kind != entity.PrincipalUser && m.Member.Kind != entity.PrincipalGroup) || m.Group == m.Member {
		return merrs.NewInvalidArgumentError("a membership is a user or another group in a group")
	}
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		for _, p := range []entity.Principal{m.Group, m.Member} {
			if _, ok, err := get(tx, principalKey(p)); err != nil || !ok {
				return notFound(err, p)
			}
		}
		kind := encodeKind(m.Member.Kind)
		if err := tx.Set(memberKey(m.Group.ID, m.Member.ID), kind); err != nil {
			return err
		}
		return tx.Set(memberRevKey(m.Member.ID, m.Group.ID), kind)
	})
}

// RemoveMember removes both rows of a membership; removing one that does not
// exist is not an error.
func (c ControlPlane) RemoveMember(ctx context.Context, m entity.Membership) error {
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		if err := tx.Delete(memberKey(m.Group.ID, m.Member.ID)); err != nil {
			return err
		}
		return tx.Delete(memberRevKey(m.Member.ID, m.Group.ID))
	})
}
