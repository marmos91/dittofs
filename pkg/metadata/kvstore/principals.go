package kvstore

import (
	"context"
	"errors"
	"iter"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// Principals is what authentication and the authorisation chokepoint read.
type Principals struct{ s *Store }

func (s *Store) Principals() Principals { return Principals{s} }

// ByName finds the user or group of that name. Names are unique per kind
// only: a user and its same-named private group are the POSIX norm.
func (p Principals) ByName(ctx context.Context, kind entity.PrincipalKind, name string) (entity.Principal, error) {
	if kind != entity.PrincipalUser && kind != entity.PrincipalGroup {
		return entity.Principal{}, merrs.NewInvalidArgumentError("only users and groups have names")
	}
	var out entity.Principal
	err := p.s.kv.View(ctx, func(r kv.Reader) error {
		b, ok, err := get(r, nameKey(nameKind(kind), name))
		if err != nil {
			return err
		}
		if !ok {
			return merrs.NewNotFoundError(name, kind.String())
		}
		id, err := decodeID(b)
		out = entity.Principal{Kind: kind, ID: id}
		return err
	})
	return out, err
}

func (p Principals) ByProtocolID(ctx context.Context, id entity.ProtocolID) (entity.Principal, error) {
	var out entity.Principal
	err := p.s.kv.View(ctx, func(r kv.Reader) error {
		b, err := r.Get(protocolKey(id))
		if errors.Is(err, kv.ErrNotFound) {
			return merrs.NewNotFoundError(id.Scheme.String()+":"+id.Value, "principal")
		}
		if err != nil {
			return err
		}
		out, err = decodePrincipal(b)
		return err
	})
	return out, err
}

// Groups yields every group p is a member of, directly or through nested
// groups, each once, nearest first. A cycle of nested groups ends the walk
// rather than loop.
func (p Principals) Groups(ctx context.Context, of entity.Principal) iter.Seq2[entity.Principal, error] {
	return func(yield func(entity.Principal, error) bool) {
		var groups []entity.Principal
		err := p.s.kv.View(ctx, func(r kv.Reader) (err error) {
			groups, err = groupsOf(r, of)
			return err
		})
		if err != nil {
			yield(entity.Principal{}, err)
			return
		}
		for _, g := range groups {
			if !yield(g, nil) {
				return
			}
		}
	}
}

// ponytail: collects the whole transitive closure before yielding, so memory
// grows with the number of groups one principal is in, not with group sizes.
// Stream it if a principal's groups ever number in the thousands.
func groupsOf(r kv.Reader, of entity.Principal) ([]entity.Principal, error) {
	seen := map[entity.PrincipalID]bool{of.ID: true}
	var out []entity.Principal
	queue := []entity.PrincipalID{of.ID}
	for len(queue) > 0 {
		member := queue[0]
		queue = queue[1:]
		prefix := memberOfPrefix(member)
		for kvp, err := range r.Scan(prefix, nil) {
			if err != nil {
				return nil, err
			}
			gid, _, err := keys.ReadID(kvp.Key[len(prefix):], keys.IDLen)
			if err != nil {
				return nil, err
			}
			id := entity.PrincipalID(gid)
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, entity.Principal{Kind: entity.PrincipalGroup, ID: id})
			queue = append(queue, id)
		}
	}
	return out, nil
}

// Grant is p's access to share once signed in, so authenticated: a grant
// naming p itself decides, even one of none; without one, the widest grant
// naming any of p's groups does; without one, a grant naming authenticated
// users, then one naming everyone. No grant at all is none.
func (p Principals) Grant(ctx context.Context, share entity.ShareID, of entity.Principal) (entity.ShareAccess, error) {
	var access entity.ShareAccess
	err := p.s.kv.View(ctx, func(r kv.Reader) error {
		groups, err := groupsOf(r, of)
		if err != nil {
			return err
		}
		access, err = grantFor(r, share, entity.Identity{Principal: of, Groups: groups, Authenticated: true})
		return err
	})
	return access, err
}

func grantIn(r kv.Reader, share entity.ShareID, p entity.PrincipalID) (entity.ShareGrant, bool, error) {
	b, err := r.Get(grantKey(share, p))
	if errors.Is(err, kv.ErrNotFound) {
		return entity.ShareGrant{}, false, nil
	}
	if err != nil {
		return entity.ShareGrant{}, false, err
	}
	g, err := decodeGrant(share, p, b)
	return g, err == nil, err
}

// User is the user record of p: what resolution reads to refuse a disabled
// or deleted user. A deleted one is not found.
func (p Principals) User(ctx context.Context, of entity.Principal) (entity.User, error) {
	var u entity.User
	err := p.s.kv.View(ctx, func(r kv.Reader) error {
		if of.Kind != entity.PrincipalUser {
			return merrs.NewNotFoundError(of.ID.String(), "user")
		}
		b, ok, err := get(r, userKey(of.ID))
		if err != nil || !ok {
			return notFound(err, of)
		}
		u, err = decodeUser(of.ID, b)
		return err
	})
	return u, err
}
